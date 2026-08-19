// Package tasks provides module-level functionality for tasks.
// input: start/stop commands, runner callbacks, permanent/retryable errors, cancellation signals
// output: task state transitions across STARTING/RUNNING/RETRY/FAILED/STOPPING lifecycle
// pos: scheduler execution lifecycle orchestration excluding lease-renew loop details
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"go.uber.org/zap"
)

func (s *Scheduler) StartTask(id string) error {
	s.mu.Lock()

	// Step 1: 校验任务存在、状态可启动、source 最小配置可用。
	task, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return ErrTaskNotFound
	}
	canClaimDispatched := task.State == StateStarting &&
		task.OwnerWorkerID == "" &&
		task.Epoch == 0 &&
		task.RunID == "" &&
		s.runner != nil &&
		s.leaseManager != nil
	// 仅允许 claim “干净的 dispatch STARTING 任务”，避免误接管非预期中间态。
	if task.State != StateCreated && task.State != StateStopped && task.State != StateRetryBackoff && !canClaimDispatched {
		s.mu.Unlock()
		return fmt.Errorf("cannot start from state %s", task.State)
	}
	// Scheduler 在 start 前强制校验最小 source config。
	if task.Source.Host == "" || task.Source.Port == 0 || task.Source.User == "" {
		s.mu.Unlock()
		return ErrInvalidSourceConfig
	}
	// Step 2: control-plane dispatch-only 分支（本地无 runner）。
	// cluster control-plane 允许 dispatch-only start：仅写入 STARTING，由 worker 接管执行。
	if s.runner == nil {
		if s.leaseManager == nil {
			// 非 cluster dispatch 场景仍要求本地 runner，防止误标记状态。
			s.mu.Unlock()
			return ErrRunnerNotConfigured
		}
		task.State = StateStarting
		task.LastError = ""
		task.OwnerWorkerID = ""
		task.Epoch = 0
		task.RunID = ""
		task.UpdatedAt = time.Now()
		s.tasks[id] = task
		s.appendEventLocked(id, "TASK_START_DISPATCHED", "task start dispatched to worker", "")
		if err := s.persistTaskLocked(task); err != nil {
			s.mu.Unlock()
			return err
		}
		s.mu.Unlock()
		return nil
	}
	if s.leaseManager != nil && s.clusterWorkerID == "" {
		s.mu.Unlock()
		return ErrClusterWorkerIDRequired
	}

	// Step 3: worker 执行分支，先 acquire lease，再进入 STARTING。
	if s.leaseManager != nil {
		leaseCtx, cancelLease := s.withLeaseTimeout(context.Background())
		epoch, acquired, err := s.leaseManager.Acquire(leaseCtx, id, s.clusterWorkerID, s.leaseTTL)
		cancelLease()
		if err != nil {
			s.mu.Unlock()
			return err
		}
		if !acquired {
			s.mu.Unlock()
			return ErrLeaseNotAcquired
		}
		task.OwnerWorkerID = s.clusterWorkerID
		task.Epoch = epoch
		task.RunID = fmt.Sprintf("%s-%d", id, time.Now().UnixNano())
	}

	task.State = StateStarting
	task.UpdatedAt = time.Now()
	// 注意：这里仅表示“已发起启动流程”，不是“runner 已 ready”。
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_STARTED", "task started", "")
	if err := s.persistTaskLocked(task); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	// Step 4: 启动 run/renew goroutine，进入真实执行期。
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	s.mu.Lock()
	if oldCancel, ok := s.cancels[id]; ok {
		// 防御性处理：task 快速重启时替换旧的 cancel function。
		oldCancel()
	}
	s.cancels[id] = cancel
	s.runs[id] = done
	taskForRun := s.tasks[id]
	s.mu.Unlock()

	if s.leaseManager != nil && taskForRun.Epoch > 0 {
		go s.renewLeaseLoop(ctx, id, taskForRun.OwnerWorkerID, taskForRun.Epoch)
	}

	go s.runTask(ctx, id, taskForRun, done)
	return nil
}

// ClaimStartingTasks 让在线 worker 在常驻状态下接管 control-plane dispatch 的 STARTING 任务。
func (s *Scheduler) ClaimStartingTasks() (int, error) {
	// 常见误解：
	// 这里不是“抢占 RUNNING 任务”，而是只处理 dispatch 出去且仍为 STARTING 的任务。
	// 真正是否能执行仍要依赖 StartTask 内部 lease Acquire 结果。
	s.mu.Lock()
	store := s.store
	runner := s.runner
	leaseManager := s.leaseManager
	s.mu.Unlock()

	if store == nil || runner == nil || leaseManager == nil {
		return 0, nil
	}

	readCtx, cancelRead := s.withReadTimeout(context.Background())
	list, err := store.ListTasks(readCtx)
	cancelRead()
	if err != nil {
		return 0, err
	}

	// 基于持久化快照做 best-effort 认领；
	// 并发竞争由 StartTask 内的状态校验 + lease Acquire 结果保证安全。
	claimed := 0
	for _, item := range list {
		if item.State != StateStarting {
			continue
		}
		if !s.prepareStartingTaskClaim(item) {
			continue
		}
		if err := s.StartTask(item.ID); err != nil {
			// 竞争窗口下可能被其他 worker 先拿到 lease，或状态已变；这里按 best-effort 跳过。
			continue
		}
		claimed++
	}
	return claimed, nil
}

// prepareStartingTaskClaim 把 store 里的 STARTING 任务注入内存，并过滤本机不可接管场景。
func (s *Scheduler) prepareStartingTaskClaim(item Task) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if done, ok := s.runs[item.ID]; ok && !isClosed(done) {
		// 本机已有活跃 run goroutine，拒绝重复接管。
		return false
	}

	if current, ok := s.tasks[item.ID]; ok {
		if current.State == StateRunning || current.State == StateRetryBackoff || current.State == StateLeaseDegraded || current.State == StateStopping {
			// 本地状态已进入执行/停止路径，不用 store 快照覆盖。
			return false
		}
	}

	s.tasks[item.ID] = item
	return true
}

// MarkRetryableError 将任务标记为 RETRY_BACKOFF 并记录错误信息。
func (s *Scheduler) MarkRetryableError(id, msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return ErrTaskNotFound
	}
	if task.State != StateRunning && task.State != StateStarting {
		return fmt.Errorf("cannot mark retryable error from state %s", task.State)
	}

	task.State = StateRetryBackoff
	task.LastError = msg
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_RETRY_BACKOFF", "task entered retry backoff", msg)
	if err := s.persistTaskLocked(task); err != nil {
		return err
	}
	return nil
}

// StopTask 请求停止任务（两阶段：STOPPING -> STOPPED）。
func (s *Scheduler) StopTask(id string) error {
	s.mu.Lock()

	task, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return ErrTaskNotFound
	}
	if task.State != StateRunning && task.State != StateRetryBackoff && task.State != StateStarting && task.State != StateLeaseDegraded {
		s.mu.Unlock()
		return fmt.Errorf("cannot stop from state %s", task.State)
	}

	done, hasRun := s.runs[id]
	if cancel, ok := s.cancels[id]; ok {
		cancel()
		delete(s.cancels, id)
	}

	task.State = StateStopping
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	// 常见误解：
	// “调用 StopTask 后应立刻看到 STOPPED”并不成立。这里先写 STOPPING，
	// 只有 run goroutine 真正退出后才会转为 STOPPED，确保状态语义等于“执行已结束”。
	// 两阶段停止：先对外可见 STOPPING，再等待 run goroutine defer 收敛到 STOPPED。
	s.appendEventLocked(id, "TASK_STOPPING", "task stopping", "")
	if err := s.persistTaskLocked(task); err != nil {
		s.mu.Unlock()
		return err
	}

	// 没有运行中的 goroutine（或已经退出）时，直接收敛到 STOPPED。
	if !hasRun || isClosed(done) {
		if err := s.markStoppedLocked(id); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	s.mu.Unlock()
	return nil
}

// GetTask 读取任务详情；必要时会从 store 刷新。

func (s *Scheduler) runTask(ctx context.Context, id string, task Task, done chan struct{}) {
	defer func() {
		var (
			releaseOwner string
			releaseEpoch int64
		)
		s.mu.Lock()
		if currentDone, ok := s.runs[id]; ok && currentDone == done {
			delete(s.runs, id)
		}
		// 收到 stop 请求后，直到执行 goroutine 真退出才收敛到 STOPPED。
		// 这样 API 层的 STOPPED 表示“执行路径已结束”，而不是“仅发出停止请求”。
		if currentTask, ok := s.tasks[id]; ok && currentTask.State == StateStopping {
			_ = s.markStoppedLocked(id)
			if s.leaseManager != nil && currentTask.OwnerWorkerID != "" && currentTask.Epoch > 0 {
				releaseOwner = currentTask.OwnerWorkerID
				releaseEpoch = currentTask.Epoch
			}
		}
		s.mu.Unlock()
		if s.leaseManager != nil && releaseOwner != "" && releaseEpoch > 0 {
			releaseCtx, cancelRelease := s.withLeaseTimeout(context.Background())
			released, err := s.leaseManager.Release(releaseCtx, id, releaseOwner, releaseEpoch)
			cancelRelease()
			if err != nil || !released {
				log.Printf("lease release on run exit failed task=%s owner=%s epoch=%d released=%v err=%v", id, releaseOwner, releaseEpoch, released, err)
			}
		}
		// 常见误解：
		// done 不是“任务开始执行”的信号，而是“本轮执行完全结束”的信号。
		// StopTask/状态收敛逻辑依赖这个 close 时机判断是否可标记 STOPPED。
		close(done)
	}()

	// Step 1: 调用 runRunner 执行一次会话；错误则进入退避重试。
	attempt := 0
	for {
		// runRunner 是一次“会话级”执行：内部会一直拉 binlog，直到 stop 或报错才返回。
		err := s.runRunner(ctx, id, task)
		if err == nil || errors.Is(err, context.Canceled) {
			return
		}

		s.mu.Lock()
		current, ok := s.tasks[id]
		if !ok {
			s.mu.Unlock()
			return
		}
		if current.State == StateStopped || current.State == StateStopping {
			s.mu.Unlock()
			return
		}

		errMsg := err.Error()
		zap.L().Error("runner error", zap.String("task_id", id), zap.Error(err))
		s.appendEventLocked(id, "TASK_RUNNER_ERROR", "runner error", errMsg)
		if IsPermanent(err) {
			_ = s.markFailedLocked(id, errMsg)
			s.mu.Unlock()
			return
		}

		current.State = StateRetryBackoff
		current.LastError = errMsg
		current.UpdatedAt = time.Now()
		s.tasks[id] = current
		s.appendEventLocked(id, "TASK_RETRY_BACKOFF", "task entered retry backoff", errMsg)
		_ = s.persistTaskLocked(current)
		s.mu.Unlock()

		// Step 2: 指数退避等待，避免瞬时故障导致热重试风暴。
		attempt++
		// 使用 exponential backoff，保护 source DB 并避免热重试。
		delay := s.retryDelay(attempt)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		s.mu.Lock()
		current, ok = s.tasks[id]
		if !ok {
			s.mu.Unlock()
			return
		}
		if current.State == StateStopped || current.State == StateStopping {
			s.mu.Unlock()
			return
		}
		// Step 3: 重试前先回到 STARTING，等待下一轮 runner ready 回调。
		current.State = StateStarting
		current.UpdatedAt = time.Now()
		s.tasks[id] = current
		// 重试前先回到 STARTING，等 runner onReady 后再切 RUNNING。
		s.appendEventLocked(id, "TASK_RETRYING", "retrying runner", "")
		_ = s.persistTaskLocked(current)
		task = current
		s.mu.Unlock()
	}
}

// runRunner 负责把 Scheduler 状态机与 Runner 生命周期对齐。
func (s *Scheduler) runRunner(ctx context.Context, id string, task Task) error {
	// Step 1: 定义 ready 回调，把任务状态收敛到 RUNNING。
	onReady := func() {
		s.mu.Lock()
		defer s.mu.Unlock()

		current, ok := s.tasks[id]
		if !ok {
			return
		}
		if current.State == StateStopped || current.State == StateStopping {
			return
		}
		if current.State == StateRunning {
			return
		}

		current.State = StateRunning
		current.LastError = ""
		current.UpdatedAt = time.Now()
		s.tasks[id] = current
		s.appendEventLocked(id, "TASK_RUNNING", "runner is running", "")
		_ = s.persistTaskLocked(current)
	}

	if n, ok := s.runner.(runnerWithNotify); ok {
		// 类型断言：如果 runner 支持 RunWithNotify，就走精确 ready 语义。
		return n.RunWithNotify(ctx, task, onReady)
	}
	// Step 2: 兼容旧 runner（无 notify），采用乐观 ready 语义。
	// 向后兼容旧 runner：没有 notify 能力时，在 Run 前乐观置为 RUNNING。
	// 该路径可能出现“短暂 RUNNING 后立即失败”；失败会在 runTask 的错误分支回收状态。
	onReady()
	return s.runner.Run(ctx, task)
}

// renewLeaseLoop 在 cluster 模式下周期续租，并处理降级/失租停机。

func (s *Scheduler) failSafeStopLocked(id, eventType, message string) {
	task, ok := s.tasks[id]
	if !ok {
		return
	}
	if task.State == StateStopping || task.State == StateStopped {
		return
	}
	task.State = StateStopping
	task.LastError = message
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, eventType, message, "")
	_ = s.persistTaskLocked(task)
	if cancel, ok := s.cancels[id]; ok {
		// 这里只发取消信号；最终 STOPPED 由 runTask defer 统一收敛。
		cancel()
		delete(s.cancels, id)
	}
}

// markFailedLocked 将任务收敛到 FAILED，用于不可恢复的源库/配置错误。
func (s *Scheduler) markFailedLocked(id, message string) error {
	task, ok := s.tasks[id]
	if !ok {
		return nil
	}
	if task.State == StateFailed {
		task.LastError = message
		s.tasks[id] = task
		return nil
	}
	task.State = StateFailed
	task.LastError = message
	task.OwnerWorkerID = ""
	task.Epoch = 0
	task.RunID = ""
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_FAILED", "task failed", message)
	if cancel, ok := s.cancels[id]; ok {
		cancel()
		delete(s.cancels, id)
	}
	return s.persistTaskLocked(task)
}

// markStoppedLocked 将任务收敛到最终 STOPPED 并清理运行时 ownership 字段。
func (s *Scheduler) markStoppedLocked(id string) error {
	task, ok := s.tasks[id]
	if !ok {
		return nil
	}
	if task.State == StateStopped {
		return nil
	}
	task.State = StateStopped
	// STOPPED 是“无执行归属”的稳定终态，清空运行时 ownership 字段。
	task.OwnerWorkerID = ""
	task.Epoch = 0
	task.RunID = ""
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_STOPPED", "task stopped", "")
	return s.persistTaskLocked(task)
}

// isClosed 判断 channel 是否已关闭（nil 视为已关闭）。
func isClosed(ch <-chan struct{}) bool {
	if ch == nil {
		return true
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// Restore 从持久化层恢复任务到内存视图。
