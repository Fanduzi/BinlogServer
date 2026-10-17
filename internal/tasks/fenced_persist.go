// Package tasks provides module-level functionality for tasks.
// input: a STOPPING/STOPPED task snapshot plus the writer’s (owner, epoch) lease identity
// output: persistTaskFencedLocked writes through FencedTaskStore.UpsertTaskFenced when the store supports it, so a stale lease-loss path cannot overwrite a newer holder’s row (ADR 0009 P2-6a / #360); otherwise falls back to UpsertTask. CheckTaskRowFence is the one task-row fence rule every FencedTaskStore applies: a held lease must be the writer's, a released lease must be released at the writer's epoch, and with no lease row the task row must not name another owner or a newer epoch (P2-6c / #421)
// pos: write fence for failSafeStopLocked and writeStoppedLocked
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"

	"binlog_server/internal/healthstore"
)

// FencedTaskStore is a TaskStore that can upsert under a task_leases fence.
// MySQLTaskStore implements it; test fakes may too.
type FencedTaskStore interface {
	TaskStore
	UpsertTaskFenced(ctx context.Context, task Task, owner string, epoch int64) error
}

// TaskRowFenceInput is what a FencedTaskStore read under lock: the
// task_leases row and, when there is none, the stored task row.
type TaskRowFenceInput struct {
	LeaseFound bool
	LeaseOwner string
	LeaseEpoch int64
	// RowFound / RowOwner / RowEpoch are the tasks row; read only when
	// LeaseFound is false.
	RowFound bool
	RowOwner string
	RowEpoch int64
}

// CheckTaskRowFence is the task-row fence (design 0009 §8.4, §24.4) for a
// writer (owner, epoch). A held lease must be exactly the writer's. A
// released lease (owner "") lets the writer converge only when it was
// released at the writer's epoch: a newer released epoch means another
// worker ran the task after it and ended it (FAILED, #421). With no lease
// row the task row decides: it must not name another owner or a newer
// epoch. Every refusal is healthstore.ErrFenced.
func CheckTaskRowFence(in TaskRowFenceInput, owner string, epoch int64) error {
	switch {
	case in.LeaseFound && in.LeaseOwner != "":
		if in.LeaseOwner == owner && in.LeaseEpoch == epoch {
			return nil
		}
	case in.LeaseFound:
		if in.LeaseEpoch == epoch {
			return nil
		}
	case !in.RowFound || in.RowEpoch <= 0 || in.RowOwner == "":
		return nil
	case in.RowOwner == owner && in.RowEpoch == epoch:
		return nil
	}
	return healthstore.ErrFenced
}

// LeaseView reads the lease row as stored (including released/expired).
// Used by TaskFacts to cross-check the task row against task_leases (#360).
type LeaseView interface {
	ViewLease(ctx context.Context, taskID string) (owner string, epoch int64, present bool, err error)
}

// persistTaskFencedLocked is persistTaskLocked gated by (owner, epoch) on
// stores that implement FencedTaskStore. Caller holds s.mu.
func (s *Scheduler) persistTaskFencedLocked(task Task, owner string, epoch int64) error {
	if s.store == nil || owner == "" || epoch <= 0 {
		return s.persistTaskLocked(task)
	}
	fs, ok := s.store.(FencedTaskStore)
	if !ok {
		return s.persistTaskLocked(task)
	}
	current := s.persisted[task.ID]
	current.gen++
	current.task = task
	s.persisted[task.ID] = current
	gen := current.gen
	snapshot := task
	fenceOwner, fenceEpoch := owner, epoch
	for {
		batch := s.detachPendingEventsLocked()
		s.mu.Unlock()
		s.writeEventBatch(batch)
		ctx, cancel := s.withWriteTimeout(context.Background())
		err := fs.UpsertTaskFenced(ctx, snapshot, fenceOwner, fenceEpoch)
		cancel()
		s.mu.Lock()
		if err != nil {
			s.notePersistFailureLocked(task.ID, snapshot.State, err)
			return err
		}
		s.notePersistedLocked(task.ID)
		latest := s.persisted[task.ID]
		if latest.gen == gen {
			latest.published = gen
			s.persisted[task.ID] = latest
			s.restorePublishedTaskLocked(snapshot)
			return nil
		}
		gen = latest.gen
		snapshot = latest.task
	}
}
