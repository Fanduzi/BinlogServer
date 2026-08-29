// Package tasks provides module-level functionality for tasks.
// input: lease renew ticks and lease manager renew responses/errors
// output: lease renewal decisions delegated to centralized lifecycle transitions
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
		ok, err := s.leaseManager.Renew(leaseCtx, id, workerID, epoch, time.Now(), s.leaseTTL)
		cancel()
		if err == nil && ok {
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
			// lease 被抢占/丢失：立刻 fail-safe stop，避免双写同一任务。
			s.mu.Lock()
			logTransitionPersistError(id, StateStopping, s.failSafeStopLocked(id, "TASK_LEASE_LOST", "lease lost"))
			s.mu.Unlock()
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
			s.mu.Lock()
			logTransitionPersistError(id, StateStopping, s.failSafeStopLocked(id, "TASK_LEASE_GRACE_EXCEEDED", "lease renew grace exceeded"))
			s.mu.Unlock()
			return
		}
	}
}

// failSafeStopLocked 在持锁上下文内触发强制停止（用于 lease 异常场景）。
