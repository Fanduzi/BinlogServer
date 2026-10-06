// Package tasks provides module-level functionality for tasks.
// input: in-process Acquire/Renew/Release calls with worker id, epoch, and TTL
// output: exclusive task ownership without a lease table; same-owner reclaim keeps the epoch; a different worker taking an expired lease increases it; Release frees the row immediately; Verify answers seal-time ownership
// pos: in-memory LeaseManager adapter for standalone and tests (the ownership door)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"sync"
	"time"
)

type memoryLeaseRow struct {
	owner  string
	epoch  int64
	expire time.Time
}

// MemoryLease is an in-process LeaseManager. Standalone has one process, so
// this is always this process. Tests share one table across fake workers.
// Semantics match task_leases: Acquire takes over only when the row is
// missing, empty, or expired. The same worker keeps the epoch, even when
// the lease has expired. A different worker taking an expired lease, or any
// worker taking a released row, increases the epoch. Release expires the
// row immediately and clears the owner.
type MemoryLease struct {
	mu   sync.Mutex
	rows map[string]memoryLeaseRow
	now  func() time.Time
}

// NewMemoryLease constructs an empty in-process lease table.
func NewMemoryLease() *MemoryLease {
	return &MemoryLease{
		rows: make(map[string]memoryLeaseRow),
		now:  time.Now,
	}
}

// Acquire grants the task to workerID when the lease is free or expired.
// The same workerID reclaims an expired lease without increasing the epoch.
func (m *MemoryLease) Acquire(_ context.Context, taskID, workerID string, ttl time.Duration) (int64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	row, ok := m.rows[taskID]
	if !ok {
		m.rows[taskID] = memoryLeaseRow{owner: workerID, epoch: 1, expire: now.Add(ttl)}
		return 1, true, nil
	}
	if row.expire.After(now) && row.owner != "" {
		if row.owner == workerID {
			return row.epoch, true, nil
		}
		return row.epoch, false, nil
	}
	epoch := row.epoch
	if row.owner != workerID {
		epoch = row.epoch + 1
	}
	m.rows[taskID] = memoryLeaseRow{owner: workerID, epoch: epoch, expire: now.Add(ttl)}
	return epoch, true, nil
}

// Renew extends the lease when workerID still holds the given epoch.
func (m *MemoryLease) Renew(_ context.Context, taskID, workerID string, epoch int64, now time.Time, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[taskID]
	if !ok || row.owner != workerID || row.epoch != epoch {
		return false, nil
	}
	if now.IsZero() {
		now = m.now()
	}
	row.expire = now.Add(ttl)
	m.rows[taskID] = row
	return true, nil
}

// Release expires the lease immediately when workerID still holds the epoch.
func (m *MemoryLease) Release(_ context.Context, taskID, workerID string, epoch int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[taskID]
	if !ok || row.owner != workerID || row.epoch != epoch {
		return false, nil
	}
	row.owner = ""
	row.expire = m.now()
	m.rows[taskID] = row
	return true, nil
}

// Verify reports whether workerID still holds an unexpired lease at epoch.
func (m *MemoryLease) Verify(_ context.Context, taskID, workerID string, epoch int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[taskID]
	if !ok || row.owner != workerID || row.epoch != epoch {
		return false, nil
	}
	return row.expire.After(m.now()), nil
}
