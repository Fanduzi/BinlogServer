// Package tasks provides module-level functionality for tasks.
// input: start/stop commands, metadata source policy, runner callbacks, typed source errors, cancellation signals, ListStartingUnownedTasks, ExpiredLeaseTaskLister
// output: guarded start/stop, a cluster run launched with the owner and epoch StartTask just acquired, a same-owner reclaim that does not Acquire, refusal to run a cluster dump at epoch 0, a control-plane stop that stays STOPPING while another worker owns the lease, cancellation of a local dump when the shared row is STOPPING or STOPPED, an idle store reload that keeps this process's pending dump, refusal to start or stop a read-only on-disk backup, a higher open epoch for an adopted leftover directory, ClaimRunnableTasks (remote stop, starting, expired, owned idle) that leaves a live owned run alone and does not Acquire a lease this worker already holds, expired-lease takeover that errors when lookup is missing, FAILED on the first error that is not on the retry allowlist (SOURCE_UNREACHABLE budget of 10, transient metadata text, OBJECT_PURGE_FAILED) including an unclassified runner error and MySQL 1236 with no stored GTID, lease release with desired_run STOP and failed_spec_revision, a lease-epoch handoff that stops this runner and releases only this epoch without writing FAILED or RETRY_BACKOFF, bounded SOURCE_UNREACHABLE retry read and written with retry_attempt and consecutive_source_failures on the task row, reset only by runner ready and operator Start, a retry that stops when the stored row is already a newer Stop, an error during that Stop (including a KILL that could not reach the source) that stays STOPPED with pending_dump_cleanup instead of FAILED, cancellation orchestration, and a run-exit done close that happens before the scheduler lock is released. The holder renews its lease until Close returns. A worker that did not hold the dump does not write STOPPED for it; an expired lease fences the published connection id. Cluster schema 3 refuses to take over another worker's dump until migration 000004. Start KILL a pending connection before opening a dump.
// pos: scheduler execution loop delegating state mutations to scheduler_transitions.go
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

const maxConsecutiveRetryableSourceFailures = 10

func (s *Scheduler) releaseTaskLease(taskID, owner string, epoch int64) {
	if s.leaseManager == nil || owner == "" || epoch <= 0 {
		return
	}
	ctx, cancel := s.withLeaseTimeout(context.Background())
	released, err := s.leaseManager.Release(ctx, taskID, owner, epoch)
	cancel()
	if err != nil || !released {
		log.Printf("lease release failed task=%s owner=%s epoch=%d released=%v err=%v", taskID, owner, epoch, released, err)
	}
}

// taskStart distinguishes an operator Start from a claim.
// resetBudget is true only for operator Start. A claim keeps the counters.
// acquireLease is false when this worker already holds an unexpired lease.
type taskStart struct {
	resetBudget  bool
	acquireLease bool
}

func (s *Scheduler) StartTask(id string) error {
	if err := s.writeStartIntent(id); err != nil {
		return err
	}
	// The control plane has no runner. The worker loop is the only starter.
	// A process that has a runner converges now so all-in-one does not wait for the tick.
	if s.runner == nil {
		return nil
	}
	_, err := s.convergeTask(id)
	return err
}

// resumeHeldTask starts one run at the epoch this worker already holds.
// It does not call Acquire, so a claim loop on an owned task does not bump epoch.
func (s *Scheduler) resumeHeldTask(id string) error {
	return s.startTask(id, taskStart{resetBudget: false, acquireLease: false})
}

// startTask 启动任务。resetBudget 仅操作员 Start 为真，计数器归零。
// 认领续跑读 backup_tasks 上的计数，不归零。
func (s *Scheduler) startTask(id string, opts taskStart) error {
	s.mu.Lock()

	if done, ok := s.runs[id]; ok && !isClosed(done) {
		s.mu.Unlock()
		return errLocalRunOpen
	}

	// 控制面把停止留在 STOPPING 后，内存可能还停在那里，而行已经被 worker 收成 STOPPED。
	// 只在这一态回读，避免把测试里故意领先 store 的内存抄本盖掉。
	if current, ok := s.tasks[id]; ok && current.State == StateStopping {
		if err := s.reloadIdleTaskFromStoreLocked(id); err != nil {
			s.mu.Unlock()
			return err
		}
	}

	// Step 1: 校验任务存在、状态可启动、source 最小配置可用。
	task, ok := s.tasks[id]
	if !ok {
		err := readOnlyDiskBackup(s.store, s.dataDir, id)
		s.mu.Unlock()
		if err != nil {
			return err
		}
		return ErrTaskNotFound
	}
	hasLiveRun := false
	if done, ok := s.runs[id]; ok && !isClosed(done) {
		hasLiveRun = true
	}
	canClaimDispatched := task.State == StateStarting &&
		task.OwnerWorkerID == "" &&
		task.Epoch == 0 &&
		task.RunID == "" &&
		s.runner != nil &&
		s.leaseManager != nil
	canTakeoverExpired := s.runner != nil && s.leaseManager != nil && !hasLiveRun &&
		(task.State == StateRunning || task.State == StateLeaseDegraded)
	canResumeIdle := s.runner != nil && !hasLiveRun &&
		(task.State == StateRunning || task.State == StateLeaseDegraded || task.State == StateStarting || task.State == StateRetryBackoff) &&
		(s.leaseManager == nil || task.OwnerWorkerID == s.clusterWorkerID)
	// 仅允许 claim “干净的 dispatch STARTING 任务”，避免误接管非预期中间态。
	// 过期 RUNNING/LEASE_DEGRADED 可在 Acquire 成功后接管；Acquire 失败则不改状态。
	// 本机空闲的 active 任务（开机接上 / 单机无租约表）走 canResumeIdle，不先 Stop。
	if task.State != StateCreated && task.State != StateStopped && task.State != StateRetryBackoff && task.State != StateFailed && !canClaimDispatched && !canTakeoverExpired && !canResumeIdle {
		s.mu.Unlock()
		return fmt.Errorf("cannot start from state %s", task.State)
	}
	// Scheduler 在 start 前强制校验最小 source config。
	if task.Source.Host == "" || task.Source.Port == 0 || task.Source.User == "" {
		s.mu.Unlock()
		return ErrInvalidSourceConfig
	}
	if err := s.validateMetadataSourceEndpoint(task.Source); err != nil {
		s.mu.Unlock()
		return err
	}
	if opts.resetBudget {
		task.RetryAttempt = 0
		task.ConsecutiveSourceFailures = 0
		task.DesiredRun = TaskDesiredRun
	}
	// A spec-0 active row still says desired STOP. The loop is starting it
	// from the observed state. Persist RUN so the next read does not cancel it.
	if effectiveDesired(task) == TaskDesiredRun {
		task.DesiredRun = TaskDesiredRun
	}
	// Step 2: control-plane dispatch-only 分支（本地无 runner）。
	// cluster control-plane 允许 dispatch-only start：仅写入 STARTING，由 worker 接管执行。
	if s.runner == nil {
		if s.leaseManager == nil {
			// 非 cluster dispatch 场景仍要求本地 runner，防止误标记状态。
			s.mu.Unlock()
			return ErrRunnerNotConfigured
		}
		if err := s.markStartDispatchedLocked(task); err != nil {
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

	// Step 3: worker 执行分支。只有所有权真正变化时才 Acquire。
	// 本进程已经持有未过期租约时不调用 Acquire，epoch 保持不变。
	if opts.acquireLease && s.refusesUnfencedTakeoverLocked(task) {
		owner, epoch := task.OwnerWorkerID, task.Epoch
		lease := s.leaseManager
		s.mu.Unlock()
		if lease != nil && owner != "" && epoch > 0 {
			leaseCtx, cancelLease := s.withLeaseTimeout(context.Background())
			held, err := lease.Verify(leaseCtx, id, owner, epoch)
			cancelLease()
			if err != nil || held {
				return ErrLeaseNotAcquired
			}
		}
		s.mu.Lock()
		task, ok = s.tasks[id]
		if !ok {
			s.mu.Unlock()
			return ErrTaskNotFound
		}
		if !s.refusesUnfencedTakeoverLocked(task) {
			s.mu.Unlock()
			return s.startTask(id, opts)
		}
		if task.State == StateRetryBackoff && task.LastError == ClusterDumpFenceSchemaMessage && task.DesiredRun == TaskDesiredRun {
			s.mu.Unlock()
			return nil
		}
		log.Printf("task=%s %s", id, ClusterDumpFenceSchemaMessage)
		task.DesiredRun = TaskDesiredRun
		if err := s.markRetryBackoffLocked(task, ClusterDumpFenceSchemaMessage); err != nil {
			s.mu.Unlock()
			return err
		}
		s.mu.Unlock()
		return nil
	}
	if s.leaseManager != nil && opts.acquireLease {
		previousOwner := task.OwnerWorkerID
		previousState := task.State
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
		if previousState == StateRunning || previousState == StateLeaseDegraded {
			s.appendEventLocked(id, "TASK_LEASE_TAKEOVER", "claimed expired lease", previousOwner)
			log.Printf("claimed expired lease task=%s previous_owner=%s previous_state=%s epoch=%d", id, previousOwner, previousState, epoch)
		}
	} else if s.leaseManager != nil {
		task.RunID = fmt.Sprintf("%s-%d", id, time.Now().UnixNano())
	}

	pending := s.leftoverDumpLocked(id, task)
	killer := s.dumpKiller
	if pending.ConnectionID == 0 && s.persistsPendingDumpLocked() {
		store := s.store
		s.mu.Unlock()
		readCtx, cancelRead := s.withReadTimeout(context.Background())
		fresh, readErr := store.GetTask(readCtx, id)
		cancelRead()
		s.mu.Lock()
		if current, ok := s.tasks[id]; ok {
			current.OwnerWorkerID = task.OwnerWorkerID
			current.Epoch = task.Epoch
			current.RunID = task.RunID
			current.DesiredRun = task.DesiredRun
			current.SpecRevision = task.SpecRevision
			current.Source = task.Source
			task = current
		}
		if readErr == nil && fresh.PendingDumpCleanup != nil && fresh.PendingDumpCleanup.ConnectionID != 0 {
			pending = *fresh.PendingDumpCleanup
			s.rememberPendingDumpLocked(id, pending)
		} else if again := s.leftoverDumpLocked(id, task); again.ConnectionID != 0 {
			pending = again
		}
	}
	if pending.ConnectionID != 0 {
		s.mu.Unlock()
		killErr := s.killLeftover(task, pending, killer)
		s.mu.Lock()
		if current, ok := s.tasks[id]; ok {
			current.OwnerWorkerID = task.OwnerWorkerID
			current.Epoch = task.Epoch
			current.RunID = task.RunID
			current.DesiredRun = task.DesiredRun
			current.SpecRevision = task.SpecRevision
			current.Source = task.Source
			task = current
		}
		if done, open := s.runs[id]; open && !isClosed(done) {
			s.mu.Unlock()
			return errLocalRunOpen
		}
		if killErr != nil {
			task.DesiredRun = TaskDesiredRun
			if err := s.markRetryBackoffLocked(task, DumpCleanupWarning(pending.ConnectionID)); err != nil {
				s.mu.Unlock()
				return err
			}
			s.mu.Unlock()
			return nil
		}
	}

	if err := s.prepareDiskResumeEpochLocked(&task); err != nil {
		owner, epoch := task.OwnerWorkerID, task.Epoch
		s.flushPendingEventsLocked()
		s.mu.Unlock()
		s.releaseTaskLease(id, owner, epoch)
		return err
	}

	// 注意：这里仅表示“已发起启动流程”，不是“runner 已 ready”。
	// task 是这次 Acquire 之后的抄本。markStartingLocked 按值接收，不会把
	// STARTING 写回这里，但 owner/epoch/runID 已经在上面写好。落库会放开锁，
	// 并发的 GetTask 可能把旧行写进 s.tasks；runner 仍用这一份，不用落库期间的内存。
	// applied_spec_revision 是这次打开的 spec。下一次 spec 更大才关掉再开一条。
	task.State = StateStarting
	task.LastError = ""
	task.AppliedSpecRevision = task.SpecRevision
	task.UpdatedAt = time.Now()
	if err := s.markStartingLocked(task); err != nil {
		s.flushPendingEventsLocked()
		s.mu.Unlock()
		return err
	}
	// 先登记 run 再放开锁。之后的 store 读要么看到这次 publish，要么因 publish
	// 世代已经前进而不覆盖 owner/epoch；有 run 时 STOPPED 旧行也不会被当成“还没启动”。
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	if _, open := s.runs[id]; open && !isClosed(s.runs[id]) {
		cancel()
		s.mu.Unlock()
		return errLocalRunOpen
	}
	s.cancels[id] = cancel
	s.runs[id] = done
	if s.runSpec == nil {
		s.runSpec = make(map[string]int64)
	}
	s.runSpec[id] = task.SpecRevision
	s.mu.Unlock()

	var stopRenew func()
	if s.leaseManager != nil && task.Epoch > 0 {
		renewCtx, renewCancel := context.WithCancel(context.Background())
		renewStopped := make(chan struct{})
		go func() {
			defer close(renewStopped)
			s.renewLeaseLoop(renewCtx, id, task.OwnerWorkerID, task.Epoch)
		}()
		stopRenew = func() {
			renewCancel()
			select {
			case <-renewStopped:
			case <-time.After(s.internalLeaseTimeout + time.Second):
				log.Printf("lease renew did not stop task=%s", id)
			}
		}
	}

	go s.runTask(ctx, id, task, done, opts.resetBudget, stopRenew)
	return nil
}

// refusesUnfencedTakeoverLocked reports a cluster start that would open a dump
// another worker may still hold, with no shared connection id to KILL first.
// Caller holds s.mu. Single-process schema 3 does not set dumpFenceRequired.
func (s *Scheduler) refusesUnfencedTakeoverLocked(task Task) bool {
	if !s.dumpFenceRequired || s.persistsPendingDumpLocked() {
		return false
	}
	if task.OwnerWorkerID == "" || task.OwnerWorkerID == s.clusterWorkerID {
		return false
	}
	return true
}

// prepareDiskResumeEpochLocked raises an adopted task's epoch above every
// leftover .open.e* file so the new segment sorts after them. Caller holds s.mu.
func (s *Scheduler) prepareDiskResumeEpochLocked(task *Task) error {
	if task == nil || !task.KeepLocalSegments {
		return nil
	}
	next, err := diskNextOpenEpoch(s.dataDir, task.ID)
	if err != nil {
		return err
	}
	if s.leaseManager == nil {
		if task.Epoch < next {
			task.Epoch = next
		}
		return nil
	}
	ml, ok := s.leaseManager.(*MemoryLease)
	if !ok {
		if task.Epoch < next {
			return fmt.Errorf("cannot resume on-disk segments at epoch %d", next)
		}
		return nil
	}
	const maxAdvance = 10000
	advanced := 0
	for task.Epoch < next {
		if advanced >= maxAdvance {
			return fmt.Errorf("cannot resume on-disk segments at epoch %d", next)
		}
		advanced++
		prev := task.Epoch
		owner := task.OwnerWorkerID
		if owner == "" {
			owner = s.clusterWorkerID
		}
		s.releaseTaskLease(task.ID, owner, prev)
		leaseCtx, cancel := s.withLeaseTimeout(context.Background())
		epoch, acquired, acquireErr := ml.Acquire(leaseCtx, task.ID, s.clusterWorkerID, s.leaseTTL)
		cancel()
		if acquireErr != nil {
			return acquireErr
		}
		if !acquired || epoch <= prev {
			return fmt.Errorf("cannot resume on-disk segments at epoch %d", next)
		}
		task.Epoch = epoch
		task.OwnerWorkerID = s.clusterWorkerID
	}
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
	list, err := store.ListStartingUnownedTasks(readCtx)
	cancelRead()
	if err != nil {
		return 0, err
	}

	// 基于 STARTING + 空 owner 的定向查询做 best-effort 认领；
	// 并发竞争由 StartTask 内的状态校验 + lease Acquire 结果保证安全。
	claimed := 0
	for _, item := range list {
		if item.State != StateStarting {
			continue
		}
		if !s.prepareStartingTaskClaim(item) {
			continue
		}
		started, err := s.convergeTask(item.ID)
		if err != nil || !started {
			// 竞争窗口下可能被其他 worker 先拿到 lease，或状态已变；这里按 best-effort 跳过。
			continue
		}
		claimed++
	}
	return claimed, nil
}

// ClaimExpiredTasks 让在线 worker 接管租约已过期的 RUNNING/LEASE_DEGRADED/RETRY_BACKOFF 任务。
// 租约已过期的 STOPPING 只收成 STOPPED，不在这里重新拉起。
func (s *Scheduler) ClaimExpiredTasks() (int, error) {
	s.mu.Lock()
	store := s.store
	runner := s.runner
	leaseManager := s.leaseManager
	s.mu.Unlock()

	if store == nil || runner == nil || leaseManager == nil {
		return 0, nil
	}
	lister, ok := store.(ExpiredLeaseTaskLister)
	if !ok {
		return 0, ErrExpiredLeaseLookupNotAvailable
	}

	readCtx, cancelRead := s.withReadTimeout(context.Background())
	list, err := lister.ListTasksWithExpiredLease(readCtx)
	cancelRead()
	if err != nil {
		return 0, err
	}

	claimed := 0
	for _, item := range list {
		if item.State == StateStopping {
			s.completeIdleStop(item)
			continue
		}
		if !isExpiredLeaseTakeoverState(item.State) {
			continue
		}
		if !s.prepareExpiredTaskClaim(item) {
			continue
		}
		started, err := s.convergeTask(item.ID)
		if err != nil || !started {
			// 竞争窗口下可能被其他 worker 先拿到 lease，或本机仍持有有效执行；按 best-effort 跳过。
			continue
		}
		log.Printf("claimed expired-lease task=%s previous_owner=%s previous_state=%s", item.ID, item.OwnerWorkerID, item.State)
		claimed++
	}
	return claimed, nil
}

func isExpiredLeaseTakeoverState(state State) bool {
	return state == StateRunning || state == StateLeaseDegraded || state == StateRetryBackoff
}

// ClaimRunnableTasks 把该本 Worker 跑的任务跑起来：没人要的 STARTING、过期租约、以及自己名下还空着的。
// 同一轮先看共享行：本进程还在拉流时，行已经是 STOPPING 或 STOPPED 就取消这次执行。
func (s *Scheduler) ClaimRunnableTasks() (int, error) {
	if _, err := s.ReconcileLegacyDesiredRun(context.Background()); err != nil {
		return 0, err
	}
	s.applyRemoteStops()
	claimed, err := s.ClaimStartingTasks()
	if err != nil {
		return claimed, err
	}
	expired, err := s.ClaimExpiredTasks()
	claimed += expired
	if err != nil {
		return claimed, err
	}
	owned, err := s.claimOwnedIdleTasks()
	return claimed + owned, err
}

func (s *Scheduler) claimOwnedIdleTasks() (int, error) {
	s.mu.Lock()
	leaseManager := s.leaseManager
	workerID := s.clusterWorkerID
	ids := make([]string, 0, len(s.tasks))
	for id := range s.tasks {
		ids = append(ids, id)
	}
	s.mu.Unlock()

	claimed := 0
	for _, id := range ids {
		s.mu.Lock()
		task, ok := s.tasks[id]
		if !ok || !isClaimableActiveState(task.State) {
			s.mu.Unlock()
			continue
		}
		if leaseManager != nil && task.OwnerWorkerID != "" && task.OwnerWorkerID != workerID {
			s.mu.Unlock()
			continue
		}
		if done, running := s.runs[id]; running && !isClosed(done) {
			// 本进程还在拉流或退避。再开一条会在上一条 Close 返回之前连上源库。
			// spec 变化由 applyRemoteIntent 取消，等这条执行退出再开。
			opened := s.runSpec[id]
			spec := task.SpecRevision
			desired := task.DesiredRun
			state := task.State
			s.mu.Unlock()
			if effectiveDesired(Task{DesiredRun: desired, State: state}) == TaskDesiredStop || spec > opened {
				s.cancelLocal(id)
			}
			continue
		}
		s.mu.Unlock()
		// 本 worker 已经持有未过期租约时只把执行拉起来，不走 Acquire。
		started, err := s.convergeTask(id)
		if err != nil {
			if errors.Is(err, ErrInvalidSourceConfig) {
				_ = s.StopTask(id)
			}
			continue
		}
		if !started {
			continue
		}
		claimed++
	}
	return claimed, nil
}

func isClaimableActiveState(state State) bool {
	return state == StateRunning || state == StateStarting || state == StateRetryBackoff || state == StateLeaseDegraded
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

// prepareExpiredTaskClaim 把 store 里租约已过期的任务注入内存，并过滤本机仍在执行的场景。
func (s *Scheduler) prepareExpiredTaskClaim(item Task) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if done, ok := s.runs[item.ID]; ok && !isClosed(done) {
		// 本机已有活跃 run goroutine，拒绝重复接管。
		return false
	}
	if current, ok := s.tasks[item.ID]; ok && current.State == StateStopping {
		// 本地已进入停止路径，不用 store 快照覆盖。
		return false
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

	return s.markRetryBackoffLocked(task, msg)
}

// StopTask 请求停止任务（两阶段：STOPPING -> STOPPED）。
func (s *Scheduler) StopTask(id string) error {
	s.mu.Lock()

	// 控制面内存常常还是 dispatch 时的 STARTING。先按 store 里的主人来停，
	// 避免用空 owner 的旧抄本把正在跑的行写成 STOPPED。
	if err := s.reloadIdleTaskFromStoreLocked(id); err != nil {
		s.mu.Unlock()
		return err
	}

	task, ok := s.tasks[id]
	if !ok {
		err := readOnlyDiskBackup(s.store, s.dataDir, id)
		s.mu.Unlock()
		if err != nil {
			return err
		}
		return ErrTaskNotFound
	}
	if task.State != StateRunning && task.State != StateRetryBackoff && task.State != StateStarting && task.State != StateLeaseDegraded {
		s.mu.Unlock()
		return fmt.Errorf("cannot stop from state %s", task.State)
	}
	task.SpecRevision++
	task.DesiredRun = TaskDesiredStop
	s.tasks[id] = task

	done, hasRun := s.runs[id]
	if cancel, ok := s.cancels[id]; ok {
		cancel()
		delete(s.cancels, id)
	}

	// 常见误解：
	// “调用 StopTask 后应立刻看到 STOPPED”并不成立。这里先写 STOPPING，
	// 只有 run goroutine 真正退出后才会转为 STOPPED，确保状态语义等于“执行已结束”。
	// 两阶段停止：先对外可见 STOPPING，再等待 run goroutine defer 收敛到 STOPPED。
	if err := s.markStoppingLocked(task); err != nil {
		s.mu.Unlock()
		return err
	}

	// 没有本进程的执行时，别人还占着租约就不能写成 STOPPED：dump 还在那个 worker 上。
	// 行留在 STOPPING，主人下一次认领会取消拉流，再由那边的退出路径收成 STOPPED。
	if !hasRun || isClosed(done) {
		if s.leaseManager != nil && task.OwnerWorkerID != "" && task.Epoch > 0 && task.OwnerWorkerID != s.clusterWorkerID {
			s.mu.Unlock()
			return nil
		}
		owner, epoch := task.OwnerWorkerID, task.Epoch
		if err := s.markStoppedLocked(id); err != nil {
			s.mu.Unlock()
			return err
		}
		s.mu.Unlock()
		s.releaseTaskLease(id, owner, epoch)
		return nil
	}
	s.mu.Unlock()
	return nil
}

// reloadIdleTaskFromStoreLocked 在本进程没有活着的 run 时，用 store 行替换内存抄本。
// 没有 pending_dump_cleanup 列时，本进程登记的残留连接号留在抄本上。
// 调用方持有 s.mu。store 没有这行时保持内存不变。
func (s *Scheduler) reloadIdleTaskFromStoreLocked(id string) error {
	if s.store == nil {
		return nil
	}
	if done, ok := s.runs[id]; ok && !isClosed(done) {
		return nil
	}
	seen := s.persisted[id].published
	store := s.store
	s.mu.Unlock()
	item, err := s.readStoredTask(store, id)
	s.mu.Lock()
	if err != nil {
		if errors.Is(err, ErrTaskNotFound) {
			return nil
		}
		return err
	}
	if s.staleStoreReadLocked(id, seen) {
		return nil
	}
	if done, ok := s.runs[id]; ok && !isClosed(done) {
		return nil
	}
	s.tasks[id] = s.overlayPendingDumpLocked(item)
	if n, convErr := strconv.Atoi(item.ID); convErr == nil && n > s.seq {
		s.seq = n
	}
	return nil
}

func (s *Scheduler) readStoredTask(store TaskStore, id string) (Task, error) {
	ctx, cancel := s.withReadTimeout(context.Background())
	defer cancel()
	return store.GetTask(ctx, id)
}

// applyRemoteStops 让控制面写进共享行的停止落到本进程的 dump 上。
func (s *Scheduler) applyRemoteStops() {
	if s.store == nil {
		return
	}
	s.mu.Lock()
	store := s.store
	live := make([]string, 0, len(s.runs))
	for id, done := range s.runs {
		if !isClosed(done) {
			live = append(live, id)
		}
	}
	stopping := make([]string, 0)
	for id, task := range s.tasks {
		if task.State == StateStopping {
			stopping = append(stopping, id)
		}
	}
	s.mu.Unlock()

	for _, id := range live {
		item, err := s.readStoredTask(store, id)
		if err != nil {
			log.Printf("remote stop read task=%s err=%v", id, err)
			continue
		}
		s.mu.Lock()
		if item.DesiredRun == TaskDesiredRun && item.SpecRevision > s.runSpec[id] && item.State != StateStopping && item.State != StateStopped {
			if cancel, ok := s.cancels[id]; ok {
				cancel()
				delete(s.cancels, id)
				log.Printf("cancelled local dump for spec change task=%s spec=%d applied=%d", id, item.SpecRevision, s.runSpec[id])
			}
			s.mu.Unlock()
			continue
		}
		s.noteRemoteStopLocked(item)
		s.flushPendingEventsLocked()
		s.mu.Unlock()
	}
	for _, id := range stopping {
		item, err := s.readStoredTask(store, id)
		if err != nil {
			if !errors.Is(err, ErrTaskNotFound) {
				log.Printf("remote stop read task=%s err=%v", id, err)
			}
			continue
		}
		if item.State != StateStopping {
			continue
		}
		s.completeIdleStop(item)
	}
}

// noteRemoteStopLocked 在共享行已经要求停止时取消本进程的 dump。
// 内存里的 owner/epoch 留着，等 run 退出再放租约。调用方持有 s.mu。
// 返回 true 表示这行不能覆盖当前执行的内存抄本。
func (s *Scheduler) noteRemoteStopLocked(stored Task) bool {
	done, ok := s.runs[stored.ID]
	if !ok || isClosed(done) {
		return false
	}
	stopRequested := stored.DesiredRun == TaskDesiredStop || stored.State == StateStopping || stored.State == StateStopped
	if !stopRequested {
		return false
	}
	if stored.DesiredRun == TaskDesiredRun && stored.State != StateStopping && stored.State != StateStopped {
		return false
	}
	current, exists := s.tasks[stored.ID]
	if !exists {
		current = stored
	}
	changed := false
	if stored.DesiredRun == TaskDesiredStop && current.DesiredRun != TaskDesiredStop {
		current.DesiredRun = TaskDesiredStop
		changed = true
	}
	if current.State != StateStopping && current.State != StateStopped {
		current.State = StateStopping
		current.UpdatedAt = time.Now()
		changed = true
		s.appendEventLocked(stored.ID, "TASK_STOPPING", "stop requested by control plane", "")
	}
	if changed {
		s.tasks[stored.ID] = current
	}
	if cancel, ok := s.cancels[stored.ID]; ok {
		cancel()
		delete(s.cancels, stored.ID)
		log.Printf("cancelled local dump for remote stop task=%s store_state=%s", stored.ID, stored.State)
	}
	return true
}

// completeIdleStop 把没有本进程执行的 STOPPING 收成 STOPPED。
// 租约仍被别的 worker 持有时不动。没关上的 dump 先记成 pending，不写成已经 Close。
func (s *Scheduler) completeIdleStop(task Task) {
	if task.State != StateStopping {
		return
	}
	_ = s.settleForeignStop(task)
}

// settleForeignStop writes STOPPED only when this process is not leaving a
// dump behind. A held connection is fenced into pending cleanup. A cluster
// without migration 000004 does not finish another worker's dump.
func (s *Scheduler) settleForeignStop(task Task) error {
	if task.State == StateStopped || task.State == StateCreated || task.State == StateFailed {
		return nil
	}
	s.mu.Lock()
	if current, ok := s.tasks[task.ID]; ok && current.State == StateStopped {
		s.mu.Unlock()
		return nil
	}
	if done, ok := s.runs[task.ID]; ok && !isClosed(done) {
		s.noteRemoteStopLocked(task)
		s.flushPendingEventsLocked()
		s.mu.Unlock()
		return nil
	}
	owner := task.OwnerWorkerID
	epoch := task.Epoch
	if current, ok := s.tasks[task.ID]; ok {
		if current.OwnerWorkerID != "" {
			owner = current.OwnerWorkerID
			epoch = current.Epoch
		}
		if marker := s.leftoverDumpLocked(task.ID, current); marker.ConnectionID != 0 {
			copied := marker
			task.PendingDumpCleanup = &copied
		}
	}
	if task.PendingDumpCleanup == nil || task.PendingDumpCleanup.ConnectionID == 0 {
		if marker := s.leftoverDumpLocked(task.ID, task); marker.ConnectionID != 0 {
			copied := marker
			task.PendingDumpCleanup = &copied
		}
	}
	self := s.clusterWorkerID
	lease := s.leaseManager
	s.mu.Unlock()

	if owner != "" && epoch > 0 && owner != self && lease != nil {
		ctx, cancel := s.withLeaseTimeout(context.Background())
		held, err := lease.Verify(ctx, task.ID, owner, epoch)
		cancel()
		if err != nil || held {
			return nil
		}
	}

	s.mu.Lock()
	if current, ok := s.tasks[task.ID]; ok && current.State == StateStopped {
		s.mu.Unlock()
		return nil
	}
	if done, ok := s.runs[task.ID]; ok && !isClosed(done) {
		s.noteRemoteStopLocked(task)
		s.flushPendingEventsLocked()
		s.mu.Unlock()
		return nil
	}
	marker := DumpCleanup{}
	if task.PendingDumpCleanup != nil {
		marker = *task.PendingDumpCleanup
	}
	if current, ok := s.tasks[task.ID]; ok {
		if again := s.leftoverDumpLocked(task.ID, current); again.ConnectionID != 0 {
			marker = again
		}
	}
	s.mu.Unlock()

	if marker.Held && marker.ConnectionID != 0 {
		s.mu.Lock()
		current := task
		if mem, ok := s.tasks[task.ID]; ok {
			current = mem
		}
		copied := marker
		current.PendingDumpCleanup = &copied
		s.tasks[task.ID] = current
		s.rememberPendingDumpLocked(task.ID, marker)
		s.mu.Unlock()
		src := task.Source
		if marker.Host != "" {
			src.Host = marker.Host
			src.Port = marker.Port
		}
		s.noteDumpCleanup(task.ID, src, marker.ConnectionID, errors.New("lease expired before dump close"))
		return nil
	}
	if marker.ConnectionID != 0 && owner != "" && owner != self {
		return nil
	}
	if marker.ConnectionID == 0 && s.dumpFenceRequired && !s.persistsPendingDumpLocked() && owner != "" && owner != self {
		log.Printf("task=%s %s", task.ID, ClusterDumpFenceSchemaMessage)
		s.mu.Lock()
		current := task
		if mem, ok := s.tasks[task.ID]; ok {
			current = mem
		}
		if current.State != StateStopped && current.State != StateFailed {
			current.LastError = ClusterDumpFenceSchemaMessage
			current.DesiredRun = TaskDesiredStop
			current.State = StateStopping
			current.UpdatedAt = time.Now()
			s.tasks[task.ID] = current
			if err := s.persistTaskLocked(current); err != nil {
				log.Printf("persist task transition failed task=%s state=%s err=%v", task.ID, StateStopping, err)
			}
		}
		s.mu.Unlock()
		return nil
	}

	s.mu.Lock()
	if current, ok := s.tasks[task.ID]; ok && current.State == StateStopped {
		s.mu.Unlock()
		return nil
	}
	if done, ok := s.runs[task.ID]; ok && !isClosed(done) {
		s.noteRemoteStopLocked(task)
		s.flushPendingEventsLocked()
		s.mu.Unlock()
		return nil
	}
	base := task
	if current, ok := s.tasks[task.ID]; ok {
		base = current
	}
	base.DesiredRun = TaskDesiredStop
	base.State = StateStopping
	if base.OwnerWorkerID == "" {
		base.OwnerWorkerID = owner
		base.Epoch = epoch
	}
	s.tasks[task.ID] = base
	releaseOwner, releaseEpoch := base.OwnerWorkerID, base.Epoch
	if err := s.markStoppedLocked(task.ID); err != nil {
		s.mu.Unlock()
		log.Printf("persist task transition failed task=%s state=%s err=%v", task.ID, StateStopped, err)
		return err
	}
	s.mu.Unlock()
	s.releaseTaskLease(task.ID, releaseOwner, releaseEpoch)
	return nil
}

func (s *Scheduler) runTask(ctx context.Context, id string, task Task, done chan struct{}, resetBudget bool, stopRenew func()) {
	openedSpec := task.AppliedSpecRevision
	defer func() {
		if stopRenew != nil {
			stopRenew()
		}
		var (
			releaseOwner string
			releaseEpoch int64
			reopen       bool
		)
		s.mu.Lock()
		// Run 已经返回，同步器的 Close 也已经返回。这里才允许写 STOPPED。
		fresh, ferr := s.intentLocked(id)
		if newer, ok := s.wantReopenLocked(fresh, ferr, openedSpec); ok {
			reopen = true
			s.keepOwnerForReopenLocked(id, task, newer)
		} else if mem, ok := s.tasks[id]; ok && mem.State != StateFailed {
			stop := mem.State == StateStopping || mem.State == StateStopped
			if ferr == nil && fresh.DesiredRun == TaskDesiredStop && fresh.State != StateFailed {
				stop = true
			}
			// Start 可能在这次读之后才把 desired 写成 RUN。写 STOPPED 前再读一次。
			if stop && mem.State != StateStopped {
				fresh, ferr = s.intentLocked(id)
				if newer, ok := s.wantReopenLocked(fresh, ferr, openedSpec); ok {
					reopen = true
					stop = false
					s.keepOwnerForReopenLocked(id, task, newer)
				}
			}
			if stop {
				if mem.State != StateStopped {
					logTransitionPersistError(id, StateStopped, s.markStoppedLocked(id))
				}
				if s.leaseManager != nil && task.OwnerWorkerID != "" && task.Epoch > 0 {
					releaseOwner = task.OwnerWorkerID
					releaseEpoch = task.Epoch
				}
			}
		}
		if currentDone, ok := s.runs[id]; ok && currentDone == done {
			delete(s.runs, id)
			delete(s.runSpec, id)
		}
		// done 在放下锁之前关闭。下一次 StartSync 要等这个 Close 完成。
		close(done)
		s.mu.Unlock()
		s.releaseTaskLease(id, releaseOwner, releaseEpoch)
		if reopen {
			_, _ = s.convergeTask(id)
		}
	}()

	// Step 1: 调用 runRunner 执行一次会话；错误则进入退避重试。
	// 计数以任务行上的列为准。操作员 Start 已经把它们写成 0。
	// 升级后列仍是 0、事件里还有连续失败时，只把那一段抄进列一次。
	attempt := int(task.RetryAttempt)
	consecutiveSourceFailures := int(task.ConsecutiveSourceFailures)
	if resetBudget {
		attempt = 0
		consecutiveSourceFailures = 0
	} else if s.store != nil && task.SpecRevision == 0 && attempt == 0 && consecutiveSourceFailures == 0 {
		if seeded := s.legacyUnreachableStreak(id); seeded > 0 {
			consecutiveSourceFailures = seeded
			s.persistRetryBudget(id, attempt, consecutiveSourceFailures)
		}
	}
	for {
		// runRunner 是一次“会话级”执行：内部会一直拉 binlog，直到 stop 或报错才返回。
		ready, err := s.runRunner(ctx, id, task)
		if ready {
			attempt = 0
			consecutiveSourceFailures = 0
		}
		if err == nil || errors.Is(err, context.Canceled) {
			return
		}

		s.mu.Lock()
		current, ok := s.tasks[id]
		if !ok {
			s.mu.Unlock()
			return
		}
		// Stop wins over the allowlist. A stored newer Stop, or STOPPING/STOPPED
		// already in memory, ends this run as STOPPED. That includes a Stop
		// whose KILL could not reach the source: the runner may also return an
		// error that is not SOURCE_UNREACHABLE, and that error must not write FAILED.
		if current.State == StateStopped || current.State == StateStopping || s.runSupersededLocked(id, openedSpec) || s.adoptNewerStopLocked(id, openedSpec) {
			s.mu.Unlock()
			return
		}

		err = classifyRunError(err)
		errMsg := err.Error()
		zap.L().Error("runner error", zap.String("task_id", id), zap.Error(err))
		if IsLeaseHandoff(err) {
			// This epoch no longer owns the task. Writing FAILED or RETRY_BACKOFF
			// would persist this worker's row over the new owner. Stop, drop
			// only this epoch, and leave the shared row.
			if cancel, ok := s.cancels[id]; ok {
				cancel()
				delete(s.cancels, id)
			}
			current.State = StateStopped
			current.OwnerWorkerID = ""
			current.Epoch = 0
			current.RunID = ""
			current.LastError = ""
			current.UpdatedAt = time.Now()
			s.tasks[id] = current
			s.appendEventLocked(id, "TASK_LEASE_YIELDED", "runner stopped; another owner holds the lease", errMsg)
			s.flushPendingEventsLocked()
			s.mu.Unlock()
			return
		}
		s.appendEventLocked(id, "TASK_RUNNER_ERROR", "runner error", errMsg)
		// Allowlist only: SOURCE_UNREACHABLE (budget 10), transient metadata
		// text, and OBJECT_PURGE_FAILED. Anything else fails once.
		if !retryAllowed(err) {
			s.failRunLocked(id, &current, attempt, consecutiveSourceFailures, errMsg)
			return
		}
		if IsSourceUnreachable(err) {
			consecutiveSourceFailures++
			if consecutiveSourceFailures >= maxConsecutiveRetryableSourceFailures {
				s.failRunLocked(id, &current, attempt, consecutiveSourceFailures, errMsg)
				return
			}
		} else {
			consecutiveSourceFailures = 0
		}

		attempt++
		rememberRetryBudget(&current, attempt, consecutiveSourceFailures)
		logTransitionPersistError(id, StateRetryBackoff, s.markRetryBackoffLocked(current, errMsg))
		s.mu.Unlock()

		// Step 2: 指数退避等待，避免瞬时故障导致热重试风暴。
		// attempt 来自已落库的 retry_attempt，重启后不会从 1 开始。
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
		if current.State == StateStopped || current.State == StateStopping || s.runSupersededLocked(id, openedSpec) || s.adoptNewerStopLocked(id, openedSpec) {
			s.mu.Unlock()
			return
		}
		// Step 3: 重试前先回到 STARTING，等待下一轮 runner ready 回调。
		// 重试前先回到 STARTING，等 runner onReady 后再切 RUNNING。
		logTransitionPersistError(id, StateStarting, s.markRetryingLocked(&current))
		task = current
		s.mu.Unlock()
	}
}

// runRunner 负责把 Scheduler 状态机与 Runner 生命周期对齐。
func (s *Scheduler) runRunner(ctx context.Context, id string, task Task) (bool, error) {
	if s.leaseManager != nil && task.Epoch <= 0 {
		return false, NewPermanentError(CodeEpochNotAcquired, "this start did not keep its lease epoch. Start the task again")
	}
	var ready atomic.Bool
	// Step 1: 定义 ready 回调，把任务状态收敛到 RUNNING。
	onReady := func() {
		ready.Store(true)
		s.mu.Lock()
		defer s.mu.Unlock()

		logTransitionPersistError(id, StateRunning, s.markRunnerReadyLocked(id))
	}

	if n, ok := s.runner.(runnerWithNotify); ok {
		// 类型断言：如果 runner 支持 RunWithNotify，就走精确 ready 语义。
		err := n.RunWithNotify(ctx, task, onReady)
		return ready.Load(), err
	}
	// Step 2: 兼容旧 runner（无 notify），采用乐观 ready 语义。
	// 向后兼容旧 runner：没有 notify 能力时，在 Run 前乐观置为 RUNNING。
	// 该路径可能出现“短暂 RUNNING 后立即失败”；失败会在 runTask 的错误分支回收状态。
	onReady()
	// 乐观状态不等于 runner 明确 ready，不重置连续源失败计数。
	return false, s.runner.Run(ctx, task)
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

// failRunLocked writes FAILED for this run, releases the lease, and unlocks s.mu.
// Caller holds s.mu. One TASK_FAILED is appended by markFailedLocked.
func (s *Scheduler) failRunLocked(id string, current *Task, attempt, consecutive int, errMsg string) {
	owner, epoch := current.OwnerWorkerID, current.Epoch
	rememberRetryBudget(current, attempt, consecutive)
	s.tasks[id] = *current
	logTransitionPersistError(id, StateFailed, s.markFailedLocked(id, errMsg))
	s.mu.Unlock()
	s.releaseTaskLease(id, owner, epoch)
}

func rememberRetryBudget(task *Task, attempt, consecutive int) {
	if task == nil {
		return
	}
	if attempt < 0 {
		attempt = 0
	}
	if consecutive < 0 {
		consecutive = 0
	}
	task.RetryAttempt = int64(attempt)
	task.ConsecutiveSourceFailures = int64(consecutive)
}

func (s *Scheduler) persistRetryBudget(id string, attempt, consecutive int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.tasks[id]
	if !ok {
		return
	}
	rememberRetryBudget(&current, attempt, consecutive)
	s.tasks[id] = current
	logTransitionPersistError(id, current.State, s.persistTaskLocked(current))
}

// legacyUnreachableStreak copies a pre-column SOURCE_UNREACHABLE streak once.
// Call it only when both budget columns are still 0. A non-zero column is the
// budget; this does not run again. ponytail: last 200 stored events. The cap is 10.
func (s *Scheduler) legacyUnreachableStreak(id string) int {
	s.mu.Lock()
	mem := append([]TaskEvent(nil), s.events[id]...)
	store := s.eventStore
	s.mu.Unlock()
	if memoryHasRunnerError(mem) || store == nil {
		return consecutiveUnreachableStreak(mem, false)
	}
	ctx, cancel := s.withReadTimeout(context.Background())
	stored, err := store.ListEvents(ctx, id, 200)
	cancel()
	if err != nil {
		log.Printf("source failure streak read task=%s err=%v", id, err)
		return consecutiveUnreachableStreak(mem, false)
	}
	if len(stored) == 0 {
		return consecutiveUnreachableStreak(mem, false)
	}
	// ListEvents is oldest-first. The 200-row window is the newest rows
	// (ORDER BY id DESC LIMIT), then reversed. Walking that slice newest-first
	// hits the earliest TASK_STARTED and returns 0.
	return consecutiveUnreachableStreak(stored, false)
}

func memoryHasRunnerError(events []TaskEvent) bool {
	for _, ev := range events {
		if ev.Type == "TASK_RUNNER_ERROR" {
			return true
		}
	}
	return false
}

// consecutiveUnreachableStreak counts SOURCE_UNREACHABLE runner errors after the
// latest ready run, operator start, or other runner error.
// newestFirst is true when index 0 is the newest event. The in-memory log and
// MySQL ListEvents are both oldest-first, so callers pass false.
// The newest event is skipped when it is TASK_STARTED: startTask writes that
// before the run reads the streak, and it is this claim, not an operator reset.
func consecutiveUnreachableStreak(events []TaskEvent, newestFirst bool) int {
	n := len(events)
	count := 0
	for i := 0; i < n; i++ {
		ev := events[i]
		if !newestFirst {
			ev = events[n-1-i]
		}
		if i == 0 && (ev.Type == "TASK_STARTED" || ev.Type == "TASK_START_DISPATCHED") {
			continue
		}
		switch ev.Type {
		case "TASK_RUNNER_ERROR":
			if strings.HasPrefix(ev.Detail, CodeSourceUnreachable) {
				count++
				continue
			}
			return count
		case "TASK_RETRY_BACKOFF", "TASK_RETRYING":
			continue
		case "TASK_RUNNING", "TASK_STARTED", "TASK_FAILED", "TASK_STOPPED", "TASK_START_DISPATCHED":
			return count
		default:
			continue
		}
	}
	return count
}

// Restore 从持久化层恢复任务到内存视图。
