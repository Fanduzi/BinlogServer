// Package tasks provides module-level functionality for tasks.
// input: lease renew ticks and lease manager renew responses/errors
// output: lease renewal decisions delegated to centralized lifecycle transitions; a lost lease whose row has a new owner or newer epoch stops the dump and yields in memory without writing the row (#346); the renew loop start marks LEASE_ACQUIRED and a yield marks LEASE_LOST for the health evaluator; each successful renew confirms the lease clock (WithLeaseClock) from the time the renew was sent, and a lost renew, exceeded grace or yield marks that epoch lost so the runner's write fence refuses the next write (ADR 0009 P2-6b, #417)
// note P2-6c (#421): a renew error after the run's context ended returns without LEASE_DEGRADED (it would cover the state the run ended in)
// pos: scheduler cluster lease-maintenance loop separated from transition recipes
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"time"
)

func (s *Scheduler) renewLeaseLoop(ctx context.Context, id, workerID string, epoch int64) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("lease renew loop panic task=%s worker=%s epoch=%d panic=%v stack=%s", id, workerID, epoch, r, debug.Stack())
			s.mu.Lock()
			logTransitionPersistError(id, StateStopping, s.failSafeStopLocked(id, "TASK_LEASE_RENEW_PANIC", fmt.Sprintf("lease renew loop panic: %v", r)))
			s.mu.Unlock()
		}
	}()

	s.markHealthDirty(id, ReasonLeaseAcquired) // the holder evaluates its task (design §5.2)
	ticker := time.NewTicker(s.leaseRenewInterval)
	defer ticker.Stop()

	// Step 1: 正常续租时保持/恢复 RUNNING。
	var degradedSince time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		leaseCtx, cancel := s.withLeaseTimeout(ctx)
		renewSentAt := time.Now()
		ok, err := s.leaseManager.Renew(leaseCtx, id, workerID, epoch, renewSentAt, s.leaseTTL)
		cancel()
		if err == nil && ok {
			s.leaseClock.Confirm(id, epoch, renewSentAt, s.leaseTTL)
			if !degradedSince.IsZero() {
				// 从降级状态恢复后，清空降级计时并收敛回 RUNNING。
				degradedSince = time.Time{}
				s.mu.Lock()
				logTransitionPersistError(id, StateRunning, s.markLeaseRenewedLocked(id))
				s.mu.Unlock()
			}
			continue
		}

		if err == nil && !ok {
			s.leaseClock.Lose(id, epoch)
			// lease 被抢占/丢失：立刻 fail-safe stop，避免双写同一任务。
			// #346: when another owner or a newer epoch is on the row, stop the
			// dump on this tick and yield in memory only; writing STOPPING or
			// STOPPED there would stop the new owner's dump.
			// A lease that was only released or expired, with no new owner on
			// the row, keeps the old STOPPING -> STOPPED path.
			s.mu.Lock()
			if s.rowTakenOverLocked(id, workerID, epoch) {
				s.yieldLostLeaseLocked(id, epoch)
			} else {
				logTransitionPersistError(id, StateStopping, s.failSafeStopLocked(id, "TASK_LEASE_LOST", "lease lost"))
			}
			s.mu.Unlock()
			return
		}

		if ctx.Err() != nil {
			// The run is ending (cancelled): a renew error from that is not
			// a degraded lease, and writing LEASE_DEGRADED would cover the
			// state the run ended in (a FAILED row, #421).
			return
		}

		// Step 2: 续租报错进入 LEASE_DEGRADED，并开始 grace 计时。
		now := time.Now()
		s.mu.Lock()
		logTransitionPersistError(id, StateLeaseDegraded, s.markLeaseDegradedLocked(id, err.Error(), now))
		s.mu.Unlock()

		if degradedSince.IsZero() {
			degradedSince = now
		}
		// Step 3: 超过 grace 仍不可续租，触发 fail-safe 停止。
		if now.Sub(degradedSince) >= s.leaseGrace {
			// 超过 grace 仍无法续租，必须停止，优先保证文件语义与单执行安全。
			s.leaseClock.Lose(id, epoch)
			s.mu.Lock()
			logTransitionPersistError(id, StateStopping, s.failSafeStopLocked(id, "TASK_LEASE_GRACE_EXCEEDED", "lease renew grace exceeded"))
			s.mu.Unlock()
			return
		}
	}
}

// failSafeStopLocked 在持锁上下文内触发强制停止（用于 lease 异常场景）。

// yieldLostLeaseLocked stops this worker's run after its lease epoch was lost.
// Only memory changes; the task row belongs to the new owner. Caller holds s.mu.
func (s *Scheduler) yieldLostLeaseLocked(id string, epoch int64) {
	s.leaseClock.Lose(id, epoch)
	s.markHealthDirty(id, ReasonLeaseLost)
	task, ok := s.tasks[id]
	if !ok {
		return
	}
	if cancel, ok := s.cancels[id]; ok {
		cancel()
		delete(s.cancels, id)
	}
	if task.Epoch != epoch {
		return
	}
	task.State = StateStopped
	task.OwnerWorkerID = ""
	task.Epoch = 0
	task.RunID = ""
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_LEASE_LOST", "lease lost; dump stopped, row left to the new owner", "")
	s.flushPendingEventsLocked()
}

// rowTakenOverLocked reports that the stored task row names another owner or
// a newer epoch than owner/epoch: the epoch is the fencing token, and a
// worker that lost it must not write the row. A read error is not a takeover.
// Caller holds s.mu; it is released during the read.
func (s *Scheduler) rowTakenOverLocked(id, owner string, epoch int64) bool {
	if s.store == nil || owner == "" || epoch <= 0 {
		return false
	}
	store := s.store
	s.mu.Unlock()
	row, err := s.readStoredTask(store, id)
	s.mu.Lock()
	if err != nil {
		return false
	}
	return rowTakenOver(row, owner, epoch)
}

func rowTakenOver(row Task, owner string, epoch int64) bool {
	if row.OwnerWorkerID == "" || row.Epoch <= 0 {
		return false
	}
	return row.OwnerWorkerID != owner || row.Epoch > epoch
}

// confirmHeldLeaseLocked renews a lease this worker already holds before a
// run starts without Acquire, so the run's write fence starts fresh. A failed
// renew leaves the clock as it is: the run's first write is then refused as a
// lease handoff (ADR 0009 P2-6). Caller holds s.mu; only with a lease clock.
func (s *Scheduler) confirmHeldLeaseLocked(id, owner string, epoch int64) {
	if s.leaseClock == nil || s.leaseManager == nil || owner == "" || epoch <= 0 {
		return
	}
	ctx, cancel := s.withLeaseTimeout(context.Background())
	defer cancel()
	sentAt := time.Now()
	ok, err := s.leaseManager.Renew(ctx, id, owner, epoch, sentAt, s.leaseTTL)
	if err == nil && ok {
		s.leaseClock.Confirm(id, epoch, sentAt, s.leaseTTL)
	}
}
