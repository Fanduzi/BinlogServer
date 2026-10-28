// Package tasks provides module-level functionality for tasks.
// input: locked task snapshots plus runner and lease lifecycle signals
// output: private state/event/persistence transitions plus best-effort persistence failure logs; STOPPED keeps the latest stored source config, the newer Stop spec when a stale snapshot would leave the row STOPPING, and, when pending_dump_cleanup is not a column, this process's dump marker; RUNNING clears the retry budget; a runner error goes through ownership.Step so FAILED keeps desired RUN and only the terminal allowlist reaches FAILED, including a used SOURCE_UNREACHABLE budget, and that FAILED row stores epoch 0; a fail-safe stop leaves FAILED unchanged; an expired STOPPING lease is finalized by StopExpired (pending_delete deletes) without a synthesized live lease; STOPPING/STOPPED from the holder's own exit still use StopConfirmed and write through persistTaskFencedLocked (#360 / P2-6a)
// note P2-6c (#421): markStoppedLocked and finishStoredStopLocked never write STOPPED over a stored FAILED row, STOPPED is fenced with this run's (owner, epoch), not the row read back, and markLeaseDegradedLocked leaves FAILED alone
// pos: centralized lifecycle transition recipes shared by scheduler orchestration loops
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"log"
	"time"

	"binlog_server/internal/healthstore"
	"binlog_server/internal/ownership"
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
	if _, applied := s.applyHolderEventLocked(id, ownership.EventDumpReady, "", time.Now()); !applied {
		return nil
	}
	task = s.tasks[id]
	task.LastError = ""
	task.RetryAttempt = 0
	task.ConsecutiveSourceFailures = 0
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_RUNNING", "runner is running", "")
	return s.persistTaskLocked(task)
}

func (s *Scheduler) markLeaseDegradedLocked(id, message string, now time.Time) error {
	task, ok := s.tasks[id]
	// FAILED (this run's, or another epoch's adopted from the row) is not
	// degraded: a renew error on the way out must not rewrite it (#421).
	if !ok || task.State == StateStopping || task.State == StateStopped || task.State == StateFailed {
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

// markRestoredFailedLocked keeps a standalone restore that cannot find a safe
// position FAILED. Routing it through EpochLost would leave desired RUN and
// LEASE_DEGRADED, and the next process would start at LATEST and skip
// transactions. This is not a runner event. Desired stays RUN.
func (s *Scheduler) markRestoredFailedLocked(id, message string) error {
	task, ok := s.tasks[id]
	if !ok {
		return nil
	}
	task.State = StateFailed
	task.DesiredRun = TaskDesiredRun
	task.LastError = message
	if task.SpecRevision == 0 {
		task.SpecRevision = 1
	}
	task.FailedSpecRevision = task.SpecRevision
	task.OwnerWorkerID = ""
	task.RunID = ""
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_FAILED", "task failed", message)
	return s.persistTaskLocked(task)
}

// markFailedLocked asks ownership.Step what this runner error writes.
// SOURCE_AUTH and LOG_BIN_OFF become FAILED with desired RUN after the
// terminal retry window. SOURCE_SWITCHOVER becomes FAILED on the first
// occurrence. SOURCE_UNREACHABLE becomes FAILED when the caller has already
// used the budget of 10. Desired stays RUN, the claim set does not include
// the row, and the stored epoch is 0 so a later claim cannot treat the
// released lease as a new acquire. SEGMENT_NOT_ON_WORKER yields the tail.
// Every other message is EpochLost: LEASE_DEGRADED and desired RUN.
// Desired STOP is not written for held tasks. An ownerless task whose
// start attempt hits a terminal error (no lease to fence) is written
// FAILED with desired STOP directly — a rejected Step is honoured, not
// forced.
func (s *Scheduler) markFailedLocked(id, message string) error {
	task, ok := s.tasks[id]
	if !ok {
		return nil
	}
	kind, cause := failureKind(message)
	if task.State == StateFailed && kind == ownership.EventTerminalError {
		task.LastError = message
		task.FailedReason = string(cause)
		if task.FailedReason == string(ownership.CauseSourceAuth) {
			task.FailedReason = ownership.ReasonSourceAuth
		}
		if task.FailedReason == string(ownership.CauseLogBinOff) {
			task.FailedReason = ownership.ReasonLogBinOff
		}
		if task.FailedReason == string(ownership.CauseSourceSwitchover) {
			task.FailedReason = ownership.ReasonSourceSwitchover
		}
		if task.FailedReason == string(ownership.CauseSourceUnreachable) {
			task.FailedReason = ownership.ReasonSourceUnreachable
		}
		task.DesiredRun = TaskDesiredRun
		task.FailedSpecRevision = task.SpecRevision
		s.tasks[id] = task
		return s.persistTaskLocked(task)
	}
	now := time.Now()
	res, applied := s.applyHolderEventLocked(id, kind, cause, now)
	if !applied && kind == ownership.EventTerminalError && res.Decision == ownership.DecisionRejected {
		res, applied = s.applyHolderEventLocked(id, ownership.EventEpochLost, ownership.CauseUnclassified, now)
	}
	if !applied {
		// A terminal error on an ownerless task has no held lease to fence:
		// the start attempt itself is the failed action (a leftover-KILL TLS
		// verify failure, an auth refusal before any run exists). Write
		// FAILED directly, desired STOP, so the operator sees the terminal
		// outcome. An owned task takes the Step path above; a rejected or
		// discarded write there stays untouched — the fence stands.
		if kind == ownership.EventTerminalError && task.OwnerWorkerID == "" && task.Epoch == 0 {
			task.State = StateFailed
			task.DesiredRun = TaskDesiredStop
			task.LastError = message
			task.FailedSpecRevision = task.SpecRevision
			task.RunID = ""
			task.UpdatedAt = now
			s.tasks[id] = task
			s.appendEventLocked(id, "TASK_FAILED", "task failed", message)
			return s.persistTaskLocked(task)
		}
		return nil
	}
	current := s.tasks[id]
	if kind == ownership.EventTerminalError && current.State != StateFailed &&
		task.OwnerWorkerID == "" && task.Epoch == 0 {
		// Step answered a terminal error on an ownerless row with
		// finishStop: nothing fences it, and the operator asked to start —
		// FAILED, not a silent stop, is the truthful outcome. The shared
		// tail below writes it.
		current.State = StateFailed
		current.DesiredRun = TaskDesiredStop
	}
	current.LastError = message
	if current.State == StateFailed {
		// The lease release above kept the epoch so the lease row matches.
		// The task row must not: an empty owner with the old epoch is what
		// the next Acquire bumps (F9), and the claim loop would start again.
		current.OwnerWorkerID = ""
		current.Epoch = 0
		current.RunID = ""
		if current.FailedSpecRevision == 0 {
			current.FailedSpecRevision = current.SpecRevision
		}
	}
	s.tasks[id] = current
	eventType := "TASK_FAILED"
	eventMessage := "task failed"
	if current.State != StateFailed {
		eventType = "TASK_LEASE_DEGRADED"
		eventMessage = "runner yielded"
	}
	s.appendEventLocked(id, eventType, eventMessage, message)
	if cancel, ok := s.cancels[id]; ok {
		cancel()
		delete(s.cancels, id)
	}
	s.clearTerminalWindow(id)
	return s.persistTaskLocked(current)
}

// markStoppedLocked 将任务收敛到最终 STOPPED 并清理运行时 ownership 字段。
// 有 store 时用最新行上的配置（含密码），只改状态和归属，避免这次收尾把控制面刚写的配置盖回去。
// 正在落库的 Stop 可能已经把 spec_revision 加一。读到的旧行 spec 更小，不能拿它写 STOPPED：
// 元数据库在 spec 更小时会留下那次 Stop 的 STOPPING。
func (s *Scheduler) markStoppedLocked(id string) error {
	return s.finishStopWriteLocked(id, false)
}

// markStopExpiredLocked finalizes STOPPING after the lease is already gone.
// The dead holder does not send StopConfirmed. Caller holds s.mu.
func (s *Scheduler) markStopExpiredLocked(id string) error {
	return s.finishStopWriteLocked(id, true)
}

func (s *Scheduler) finishStopWriteLocked(id string, expired bool) error {
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
	if expired {
		stopping := task.State == StateStopping || task.DesiredRun == TaskDesiredStop || task.PendingDelete
		if !stopping {
			return nil
		}
		applied, err := s.writeStopExpiredLocked(id, task)
		if err != nil || !applied {
			return err
		}
		if _, ok := s.tasks[id]; !ok {
			return nil
		}
	} else if err := s.writeStoppedLocked(id, task, fenceOwner, fenceEpoch); err != nil {
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

// writeStopExpiredLocked applies StopExpired with the lease already expired.
// A synthesized live lease would make InStopSet reject the event. The task
// row still drops its owner so the claim query can see it. Caller holds s.mu.
func (s *Scheduler) writeStopExpiredLocked(id string, task Task) (bool, error) {
	now := time.Now()
	st := s.ownershipState(task, now)
	st.Desired = ownership.DesiredStop
	st.Phase = ownership.PhaseStopping
	st.StoppedByOperator = true
	if st.Lease.Owner != "" && !st.Lease.ExpireAt.Before(now) {
		st.Lease.ExpireAt = now.Add(-time.Nanosecond)
	}
	res := ownership.Step(st, ownership.Event{Kind: ownership.EventStopExpired, Node: s.holderNode()}, now)
	if res.Decision != ownership.DecisionApplied && res.Decision != ownership.DecisionIdempotent {
		log.Printf("stop expired not applied task=%s decision=%s detail=%s", id, res.Decision, res.Detail)
		return false, nil
	}
	if res.State.Deleted {
		s.noteStepIntentsLocked(id, res)
		if err := s.commitDeleteLocked(id); err != nil {
			return false, err
		}
		return true, nil
	}
	owner, epoch := task.OwnerWorkerID, task.Epoch
	task.State = StateStopped
	task.DesiredRun = TaskDesiredStop
	task.OwnerWorkerID = ""
	task.Epoch = 0
	task.RunID = ""
	task.PendingDelete = false
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_STOPPED", "expired stop finalized", "")
	err := s.persistTaskFencedLocked(task, owner, epoch)
	if errors.Is(err, healthstore.ErrFenced) {
		s.markHealthDirty(id, ReasonLeaseLost)
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// writeStoppedLocked writes STOPPED fenced by the run's (owner, epoch); an
// empty identity falls back to the unfenced write (standalone).
func (s *Scheduler) writeStoppedLocked(id string, task Task, owner string, epoch int64) error {
	if owner == "" || epoch <= 0 {
		owner, epoch = task.OwnerWorkerID, task.Epoch
	}
	now := time.Now()
	st := s.ownershipState(task, now)
	fenceNode := owner
	if fenceNode == "" {
		fenceNode = task.OwnerWorkerID
	}
	if fenceNode == "" {
		fenceNode = s.holderNode()
	}
	leaseEpoch := epoch
	if leaseEpoch <= 0 {
		leaseEpoch = task.Epoch
	}
	st.Lease.Owner = fenceNode
	if leaseEpoch > 0 {
		st.Lease.Epoch = leaseEpoch
	} else if st.Lease.Epoch <= 0 {
		st.Lease.Epoch = 1
	}
	st.Lease.Present = true
	st.Lease.ExpireAt = now.Add(time.Minute)
	if task.DesiredRun == TaskDesiredStop || task.State == StateStopping || task.State == StateStopped {
		st.Desired = ownership.DesiredStop
		if task.State != StateStopped {
			st.Phase = ownership.PhaseStopping
		}
	}
	ev := ownership.Held(st, ownership.EventStopConfirmed, fenceNode)
	res := ownership.Step(st, ev, now)
	if res.Decision == ownership.DecisionApplied && res.State.Deleted {
		s.noteStepIntentsLocked(id, res)
		return s.commitDeleteLocked(id)
	}
	if res.Decision != ownership.DecisionApplied && res.Decision != ownership.DecisionIdempotent {
		return nil
	}
	task.State = StateStopped
	task.DesiredRun = TaskDesiredStop
	// The task row drops its owner so the claim query can see it. The lease
	// table keeps the epoch (Release does not zero it).
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
		if fresh.State == StateStopped || fresh.State == StateFailed {
			// FAILED is not a Stop to finish (§24.4, #421).
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
