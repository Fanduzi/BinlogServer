// Package tasks provides module-level functionality for tasks.
// input: locked task snapshots plus runner and lease lifecycle signals
// output: private state/event/persistence transitions plus best-effort persistence failure logs; STOPPED keeps the latest stored source config, the newer Stop spec when a stale snapshot would leave the row STOPPING, and, when pending_dump_cleanup is not a column, this process's dump marker; RUNNING clears the retry budget; FAILED sets desired_run STOP and failed_spec_revision; a fail-safe stop leaves FAILED unchanged; STOPPING/STOPPED from lease-loss paths write through persistTaskFencedLocked (#360 / P2-6a)
// note P2-6c (#421): markStoppedLocked never writes STOPPED over a stored FAILED row and fences STOPPED with this run's (owner, epoch), not the row read back
// pos: centralized lifecycle transition recipes shared by scheduler orchestration loops
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"log"
	"time"

	"binlog_server/internal/healthstore"
)

func (s *Scheduler) markStartDispatchedLocked(task Task) error {
	task.LastError = ""
	task.OwnerWorkerID = ""
	task.Epoch = 0
	task.RunID = ""
	task.UpdatedAt = time.Now()
	return s.commitTransitionLocked(task, StateStarting, transitionRecord{Action: "start", Type: "TASK_START_DISPATCHED", Message: "task start dispatched to worker"})
}

func (s *Scheduler) markStartingLocked(task Task) error {
	task.State = StateStarting
	task.LastError = ""
	task.UpdatedAt = time.Now()
	s.tasks[task.ID] = task
	s.appendEventLocked(task.ID, "TASK_STARTED", "task started", "")
	return s.persistTaskLocked(task)
}

func (s *Scheduler) markRetryBackoffLocked(task Task, message string) error {
	task.State = StateRetryBackoff
	task.LastError = message
	task.UpdatedAt = time.Now()
	s.tasks[task.ID] = task
	s.appendEventLocked(task.ID, "TASK_RETRY_BACKOFF", "task entered retry backoff", message)
	return s.persistTaskLocked(task)
}

func (s *Scheduler) markRetryingLocked(task *Task) error {
	task.State = StateStarting
	task.UpdatedAt = time.Now()
	s.tasks[task.ID] = *task
	s.appendEventLocked(task.ID, "TASK_RETRYING", "retrying runner", "")
	return s.persistTaskLocked(*task)
}

func (s *Scheduler) markRunnerReadyLocked(id string) error {
	task, ok := s.tasks[id]
	if !ok || task.State == StateStopped || task.State == StateStopping || task.State == StateRunning {
		return nil
	}
	task.State = StateRunning
	task.LastError = ""
	task.RetryAttempt = 0
	task.ConsecutiveSourceFailures = 0
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_RUNNING", "runner is running", "")
	return s.persistTaskLocked(task)
}

func (s *Scheduler) markLeaseDegradedLocked(id, message string, now time.Time) error {
	task, ok := s.tasks[id]
	if !ok || task.State == StateStopping || task.State == StateStopped {
		return nil
	}
	if task.State != StateLeaseDegraded {
		task.State = StateLeaseDegraded
		s.appendEventLocked(id, "TASK_LEASE_DEGRADED", "lease renew failed", message)
	}
	task.LastError = message
	task.UpdatedAt = now
	s.tasks[id] = task
	return s.persistTaskLocked(task)
}

func (s *Scheduler) markLeaseRenewedLocked(id string) error {
	task, ok := s.tasks[id]
	if !ok || task.State != StateLeaseDegraded {
		return nil
	}
	task.State = StateRunning
	task.LastError = ""
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_LEASE_RENEWED", "lease renewed", "")
	return s.persistTaskLocked(task)
}

func (s *Scheduler) failSafeStopLocked(id, eventType, message string) error {
	task, ok := s.tasks[id]
	// FAILED already released the lease. A renew that observes that release
	// must not rewrite the row as STOPPING.
	if !ok || task.State == StateStopping || task.State == StateStopped || task.State == StateFailed {
		return nil
	}
	owner, epoch := task.OwnerWorkerID, task.Epoch
	task.State = StateStopping
	task.LastError = message
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, eventType, message, "")
	// P2-6a / #360: fence the STOPPING write on task_leases so a stale owner
	// after lease loss cannot overwrite a newer holder's row.
	err := s.persistTaskFencedLocked(task, owner, epoch)
	if errors.Is(err, healthstore.ErrFenced) {
		s.yieldLostLeaseLocked(id, epoch)
		err = nil
	}
	if cancel, ok := s.cancels[id]; ok {
		// 这里只发取消信号；最终 STOPPED 由 runTask defer 统一收敛。
		cancel()
		delete(s.cancels, id)
	}
	return err
}

func logTransitionPersistError(id string, state State, err error) {
	if err != nil {
		log.Printf("persist task transition failed task=%s state=%s err=%v", id, state, err)
	}
}

// markFailedLocked 将任务收敛到 FAILED，用于不可恢复的源库/配置错误。
// StartTask 允许从 FAILED 再次启动，方便值班改完密码/配置后重试。
func (s *Scheduler) markFailedLocked(id, message string) error {
	task, ok := s.tasks[id]
	if !ok {
		return nil
	}
	if task.State == StateFailed {
		task.LastError = message
		task.DesiredRun = TaskDesiredStop
		task.FailedSpecRevision = task.SpecRevision
		s.tasks[id] = task
		return s.persistTaskLocked(task)
	}
	task.State = StateFailed
	task.DesiredRun = TaskDesiredStop
	task.FailedSpecRevision = task.SpecRevision
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
// 有 store 时用最新行上的配置（含密码），只改状态和归属，避免这次收尾把控制面刚写的配置盖回去。
// 正在落库的 Stop 可能已经把 spec_revision 加一。读到的旧行 spec 更小，不能拿它写 STOPPED：
// 元数据库在 spec 更小时会留下那次 Stop 的 STOPPING。
func (s *Scheduler) markStoppedLocked(id string) error {
	task, ok := s.tasks[id]
	if !ok || task.State == StateStopped {
		return nil
	}
	// The fence identity is this run's (owner, epoch), never the row read
	// back below: a row another epoch already ended has no owner, and
	// fencing with that empty identity would skip the fence (#421).
	fenceOwner, fenceEpoch := task.OwnerWorkerID, task.Epoch
	if s.store != nil {
		store := s.store
		currentEpoch := task.Epoch
		s.mu.Unlock()
		ctx, cancel := s.withReadTimeout(context.Background())
		fresh, err := store.GetTask(ctx, id)
		cancel()
		s.mu.Lock()
		current, ok := s.tasks[id]
		if !ok || current.State == StateStopped {
			return nil
		}
		task = s.keepNewerStopSpecLocked(id, current)
		if task.DesiredRun == TaskDesiredRun && task.SpecRevision > task.AppliedSpecRevision {
			return nil
		}
		if err == nil && fresh.SpecRevision >= task.SpecRevision {
			// A newer operator Start already asked for another dump. Do not cover that row with STOPPED.
			if fresh.DesiredRun == TaskDesiredRun && fresh.SpecRevision > task.AppliedSpecRevision {
				return nil
			}
			if fresh.State == StateFailed {
				// FAILED leaves only by an operator Start; a stop that
				// ends a run never writes STOPPED over it (§24.4).
				s.tasks[id] = s.overlayPendingDumpLocked(fresh)
				return nil
			}
			active := fresh.State == StateRunning || fresh.State == StateStarting || fresh.State == StateRetryBackoff || fresh.State == StateLeaseDegraded
			if active && fresh.Epoch != 0 && currentEpoch != 0 && fresh.Epoch != currentEpoch {
				s.tasks[id] = s.overlayPendingDumpLocked(fresh)
				return nil
			}
			kept := DumpCleanup{}
			if current.PendingDumpCleanup != nil {
				kept = *current.PendingDumpCleanup
			}
			task = fresh
			// Schema 3 reads this row without the column. Keep the marker the
			// runner recorded before this STOPPED write replaced the struct.
			if kept.ConnectionID != 0 && !s.persistsPendingDumpLocked() {
				if marker, ok := s.pendingDumps[id]; !ok || marker.ConnectionID == 0 {
					s.rememberPendingDumpLocked(id, kept)
				}
			}
			task = s.overlayPendingDumpLocked(task)
		}
	}
	if err := s.writeStoppedLocked(id, task, fenceOwner, fenceEpoch); err != nil {
		return err
	}
	return s.finishStoredStopLocked(id)
}

// keepNewerStopSpecLocked copies a Stop spec that is already queued for the
// store. Caller holds s.mu. A GetTask that started earlier still has the
// previous spec; writing STOPPED at that spec does not apply.
func (s *Scheduler) keepNewerStopSpecLocked(id string, task Task) Task {
	pending := s.persisted[id].task
	if pending.ID != id || pending.SpecRevision <= task.SpecRevision {
		return task
	}
	stop := pending.DesiredRun == TaskDesiredStop || pending.State == StateStopping || pending.State == StateStopped
	if !stop {
		return task
	}
	task.SpecRevision = pending.SpecRevision
	task.DesiredRun = TaskDesiredStop
	return task
}

// writeStoppedLocked writes STOPPED fenced by the run's (owner, epoch); an
// empty identity falls back to the unfenced write (standalone).
func (s *Scheduler) writeStoppedLocked(id string, task Task, owner string, epoch int64) error {
	if owner == "" || epoch <= 0 {
		owner, epoch = task.OwnerWorkerID, task.Epoch
	}
	task.State = StateStopped
	task.DesiredRun = TaskDesiredStop
	// STOPPED 是“无执行归属”的稳定终态，清空运行时 ownership 字段。
	task.OwnerWorkerID = ""
	task.Epoch = 0
	task.RunID = ""
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_STOPPED", "task stopped", "")
	// P2-6a: fence with the pre-clear (owner, epoch) so a stale run-exit
	// STOPPED cannot land after another worker claimed the lease (#360).
	err := s.persistTaskFencedLocked(task, owner, epoch)
	if errors.Is(err, healthstore.ErrFenced) {
		s.markHealthDirty(id, ReasonLeaseLost)
		return nil
	}
	return err
}

// finishStoredStopLocked writes STOPPED again when the store still has the
// Stop row. The upsert keeps a newer spec, so the first snapshot can land
// without changing that row. Caller holds s.mu. One stored Stop is enough;
// a newer Start is left alone.
func (s *Scheduler) finishStoredStopLocked(id string) error {
	if s.store == nil {
		return nil
	}
	store := s.store
	for range 2 {
		s.mu.Unlock()
		ctx, cancel := s.withReadTimeout(context.Background())
		fresh, err := store.GetTask(ctx, id)
		cancel()
		s.mu.Lock()
		if err != nil {
			return err
		}
		if fresh.State == StateStopped {
			return nil
		}
		if mem, ok := s.tasks[id]; ok && mem.DesiredRun == TaskDesiredRun && mem.SpecRevision > fresh.SpecRevision && mem.SpecRevision > mem.AppliedSpecRevision {
			return nil
		}
		if fresh.DesiredRun == TaskDesiredRun && fresh.State != StateStopping && fresh.State != StateStopped {
			return nil
		}
		if fresh.DesiredRun != TaskDesiredStop && fresh.State != StateStopping {
			return nil
		}
		fresh.State = StateStopped
		fresh.DesiredRun = TaskDesiredStop
		fresh.OwnerWorkerID = ""
		fresh.Epoch = 0
		fresh.RunID = ""
		fresh.UpdatedAt = time.Now()
		// Schema 3 reads this row without pending_dump_cleanup. Overlay keeps
		// this process's marker. An empty schema-4 column still drops it.
		fresh = s.overlayPendingDumpLocked(fresh)
		s.tasks[id] = fresh
		if err := s.persistTaskLocked(fresh); err != nil {
			return err
		}
	}
	return nil
}
