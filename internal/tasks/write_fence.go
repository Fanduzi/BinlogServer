// Package tasks provides module-level functionality for tasks.
// input: lease acquire/renew/lose events from the scheduler, and the (task, owner, epoch) identity a run writes under
// output: LeaseClock, the worker's monotonic view of whether its lease epoch is still fresh (a write boundary refuses once ttl*4/5 has passed since the last confirmed acquire or renew, or once the scheduler marked the epoch lost); Lost reports that exact epoch so a resume does not open a dump whose writes stay fenced (§29); FenceIdentity carried on the run context so metadata stores can check the epoch against task_leases in the same transaction as the write; ErrWriteFenced (ADR 0009 P2-6, #417)
// pos: shared fencing token for every side effect a non-owner could produce (segment bytes, seal, checkpoint, catalog rows, uploads)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrWriteFenced is returned by a write boundary when the writer's lease
// epoch no longer owns the task. It is a handoff, never a task failure.
var ErrWriteFenced = errors.New("WRITE_FENCED: lease epoch no longer owns the task")

// FenceIdentity is the fencing token a run writes under.
type FenceIdentity struct {
	TaskID string
	Owner  string
	Epoch  int64
}

type fenceIdentityKey struct{}

// WithFenceIdentity attaches id to ctx. Metadata stores that see it check
// (owner, epoch) against task_leases in the same transaction as the write.
func WithFenceIdentity(ctx context.Context, id FenceIdentity) context.Context {
	if ctx == nil || id.TaskID == "" || id.Owner == "" || id.Epoch <= 0 {
		return ctx
	}
	return context.WithValue(ctx, fenceIdentityKey{}, id)
}

// FenceIdentityFrom returns the identity attached by WithFenceIdentity.
func FenceIdentityFrom(ctx context.Context) (FenceIdentity, bool) {
	if ctx == nil {
		return FenceIdentity{}, false
	}
	id, ok := ctx.Value(fenceIdentityKey{}).(FenceIdentity)
	return id, ok
}

// WriteFence is the cheap local check a write boundary asks before touching
// the disk. LeaseClock implements it.
type WriteFence interface {
	Fresh(taskID string, epoch int64) bool
}

type leaseClockEntry struct {
	epoch       int64
	confirmedAt time.Time
	ttl         time.Duration
	lost        bool
}

// LeaseClock records, per task, when this worker last confirmed its lease.
// Times come from time.Now and are compared with their monotonic reading,
// which keeps running while the process is SIGSTOPped or the VM is paused,
// so a thawed owner sees its lease as stale before its first write.
type LeaseClock struct {
	mu      sync.Mutex
	entries map[string]leaseClockEntry
	now     func() time.Time
}

// NewLeaseClock returns an empty clock.
func NewLeaseClock() *LeaseClock {
	return &LeaseClock{entries: map[string]leaseClockEntry{}, now: time.Now}
}

// freshFraction is how much of the ttl a confirmation is trusted for. The
// rest absorbs clock rate differences between this host and the database.
const freshNum, freshDen = 4, 5

// Confirm records that the lease (taskID, epoch) was held from before at for
// ttl. Pass the time taken before the Acquire/Renew call was sent.
func (c *LeaseClock) Confirm(taskID string, epoch int64, at time.Time, ttl time.Duration) {
	if c == nil || taskID == "" || epoch <= 0 || ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, ok := c.entries[taskID]
	if ok && cur.epoch > epoch {
		return
	}
	if ok && cur.epoch == epoch && (cur.lost || at.Before(cur.confirmedAt)) {
		return
	}
	c.entries[taskID] = leaseClockEntry{epoch: epoch, confirmedAt: at, ttl: ttl}
}

// Lost reports that this process marked (taskID, epoch) lost. A new epoch
// is not lost. Confirm of a lost epoch does not revive it (§29).
func (c *LeaseClock) Lost(taskID string, epoch int64) bool {
	if c == nil || taskID == "" || epoch <= 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, ok := c.entries[taskID]
	return ok && cur.lost && cur.epoch == epoch
}

// Lose marks (taskID, epoch) as no longer held. A later Confirm of the same
// epoch does not revive it; only a new epoch does.
func (c *LeaseClock) Lose(taskID string, epoch int64) {
	if c == nil || taskID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, ok := c.entries[taskID]
	if ok && cur.epoch > epoch {
		return
	}
	c.entries[taskID] = leaseClockEntry{epoch: epoch, confirmedAt: cur.confirmedAt, ttl: cur.ttl, lost: true}
}

// Fresh reports whether (taskID, epoch) is still inside its trusted window.
// An unknown task is not fresh.
func (c *LeaseClock) Fresh(taskID string, epoch int64) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, ok := c.entries[taskID]
	if !ok || cur.lost || cur.epoch != epoch {
		return false
	}
	return c.now().Sub(cur.confirmedAt) < cur.ttl*freshNum/freshDen
}
