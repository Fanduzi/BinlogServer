// Package tasks provides module-level functionality for tasks.
// input: MemoryLease + fenced fake store where A holds epoch 2 while the task row still names B@1; TaskFacts inputs where the row disagrees with the lease
// output: P2-6a: failSafeStopLocked's STOPPED/STOPPING write is refused and the row keeps the newer owner's state (#355 run 3 / #360); TaskFacts is nil and task.state incomplete on row/lease mismatch so health is not HEALTHY
// pos: ADR 0009 P2-6a acceptance tests (no dockerd)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
)

// fencedMemoryStore gates UpsertTaskFenced on MemoryLease the way MySQLTaskStore
// gates on task_leases (same transaction semantics approximated under one lock).
type fencedMemoryStore struct {
	*lockedFakeStore
	leases *MemoryLease
}

func (s *fencedMemoryStore) UpsertTaskFenced(ctx context.Context, task Task, owner string, epoch int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leases.mu.Lock()
	row, ok := s.leases.rows[task.ID]
	s.leases.mu.Unlock()
	in := TaskRowFenceInput{LeaseFound: ok, LeaseOwner: row.owner, LeaseEpoch: row.epoch}
	if !ok {
		if stored, err := s.fakeStore.GetTask(ctx, task.ID); err == nil {
			in.RowFound, in.RowOwner, in.RowEpoch = true, stored.OwnerWorkerID, stored.Epoch
		}
	}
	if err := CheckTaskRowFence(in, owner, epoch); err != nil {
		return err
	}
	return s.fakeStore.UpsertTask(ctx, task)
}

// TestFailSafeStop_StaleOwnerRefusedWhenLeaseMoved is #355 run 3: A claimed
// epoch 2 on the lease table but has not yet written the task row. B still
// sees itself on the row and takes the old STOPPING path; the fenced write
// must refuse so the row is not overwritten with STOPPED.
func TestFailSafeStop_StaleOwnerRefusedWhenLeaseMoved(t *testing.T) {
	leases := NewMemoryLease()
	base := &lockedFakeStore{fakeStore: newFakeStore()}
	store := &fencedMemoryStore{lockedFakeStore: base, leases: leases}
	s := NewScheduler(
		WithStore(store),
		WithClusterLeaseManager(leases),
		WithClusterWorkerID("worker-b"),
		WithClusterLease(time.Minute, time.Hour, time.Minute),
	)
	task := ownedIdleTask(t, s, "worker-b", 1, StateRunning)
	leases.mu.Lock()
	leases.rows[task.ID] = memoryLeaseRow{owner: "worker-b", epoch: 1, expire: time.Now().Add(time.Hour)}
	leases.mu.Unlock()

	// A takes the lease at epoch 2; the task row still names B@1.
	leases.mu.Lock()
	leases.rows[task.ID] = memoryLeaseRow{owner: "worker-a", epoch: 2, expire: time.Now().Add(time.Hour)}
	leases.mu.Unlock()

	s.mu.Lock()
	err := s.failSafeStopLocked(task.ID, "TASK_LEASE_LOST", "lease lost")
	s.mu.Unlock()
	if err != nil && !errors.Is(err, healthstore.ErrFenced) {
		t.Fatalf("failSafeStopLocked: %v", err)
	}

	got, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State == StateStopped || got.State == StateStopping {
		t.Fatalf("stale owner wrote %s over the row (owner=%q epoch=%d)", got.State, got.OwnerWorkerID, got.Epoch)
	}
	if got.OwnerWorkerID != "worker-b" || got.Epoch != 1 {
		t.Fatalf("row changed unexpectedly: state=%s owner=%q epoch=%d", got.State, got.OwnerWorkerID, got.Epoch)
	}
}

// TestTaskFacts_RowLeaseMismatchIncomplete: when the task row disagrees with
// task_leases, TaskFacts is nil and task.state is incomplete (health UNKNOWN).
func TestTaskFacts_RowLeaseMismatchIncomplete(t *testing.T) {
	leases := NewMemoryLease()
	store := newFakeStore()
	s := NewScheduler(
		WithStore(store),
		WithClusterLeaseManager(leases),
		WithClusterWorkerID("worker-a"),
	)
	task := Task{
		ID: "42", Name: "t", State: StateStopped, DesiredRun: TaskDesiredStop,
		OwnerWorkerID: "", Epoch: 0, UpdatedAt: time.Now(),
	}
	s.mu.Lock()
	s.tasks[task.ID] = task
	s.mu.Unlock()
	_ = store.UpsertTask(context.Background(), task)

	// Lease still names the live holder; row was wrongly cleared to STOPPED.
	leases.mu.Lock()
	leases.rows[task.ID] = memoryLeaseRow{owner: "worker-a", epoch: 2, expire: time.Now().Add(time.Hour)}
	leases.mu.Unlock()

	e := &Evaluator{s: s, cfg: EvaluatorConfig{NodeID: "worker-a", Cluster: true, Now: time.Now}}
	facts := &health.Facts{TaskID: task.ID, Now: time.Now(), Complete: map[health.Scope]bool{}}
	e.signalFacts(context.Background(), task, false, facts)
	if facts.Task != nil {
		t.Fatalf("TaskFacts = %+v, want nil on row/lease mismatch", facts.Task)
	}
	if facts.Complete[health.ScopeTaskState] {
		t.Fatal("task.state must be incomplete on row/lease mismatch")
	}
	h := health.DefaultRegistry().HealthOf(nil, facts)
	if h.Status == health.StatusHealthy {
		t.Fatalf("health = %s, want not HEALTHY", h.Status)
	}
}

func TestTaskFacts_RowLeaseMatchComplete(t *testing.T) {
	leases := NewMemoryLease()
	store := newFakeStore()
	s := NewScheduler(
		WithStore(store),
		WithClusterLeaseManager(leases),
		WithClusterWorkerID("worker-a"),
	)
	task := Task{
		ID: "7", Name: "t", State: StateRunning, DesiredRun: TaskDesiredRun,
		OwnerWorkerID: "worker-a", Epoch: 3, UpdatedAt: time.Now(),
	}
	s.mu.Lock()
	s.tasks[task.ID] = task
	s.mu.Unlock()
	leases.mu.Lock()
	leases.rows[task.ID] = memoryLeaseRow{owner: "worker-a", epoch: 3, expire: time.Now().Add(time.Hour)}
	leases.mu.Unlock()

	e := &Evaluator{s: s, cfg: EvaluatorConfig{NodeID: "worker-a", Cluster: true, Now: time.Now}}
	facts := &health.Facts{TaskID: task.ID, Now: time.Now(), Complete: map[health.Scope]bool{}}
	e.signalFacts(context.Background(), task, false, facts)
	if facts.Task == nil {
		t.Fatal("TaskFacts nil on matching row/lease")
	}
	if !facts.Complete[health.ScopeTaskState] {
		t.Fatal("task.state should be complete when row matches lease")
	}
}

var (
	_ TaskStore       = (*fencedMemoryStore)(nil)
	_ FencedTaskStore = (*fencedMemoryStore)(nil)
)
