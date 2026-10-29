// Package tasks provides module-level functionality for tasks.
// input: a worker whose tail yield wrote task_node_refusal, then an operator Start recorded on another node
// output: proof the stored refusal row — not the yielding process's cached copy — gates the reclaim, so an operator Start anywhere unblocks the worker
// pos: operator-recovery regression for the foreign-tail takeover path (ADR 0011: copy the tail, then Start)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/ownership"
)

// metaLease wraps MemoryLease so leaseStoredInMeta reports true: the named
// type stands in for the SQL lease manager cluster workers run.
type metaLease struct{ *MemoryLease }

// stubClusterGate turns on CommitYield: commitYieldLocked skips stores
// without a gate. It answers per_node for every registration.
type stubClusterGate struct{ mode string }

func (g *stubClusterGate) RegisterClusterStorage(_ context.Context, mode string) (string, error) {
	if g.mode == "" {
		g.mode = mode
	}
	return g.mode, nil
}

func (g *stubClusterGate) CountOlderProtocols(context.Context, string, int64) (int, error) {
	return 0, nil
}

// refusalMetaStore adds the task_node_refusal surface a meta store exposes:
// CommitYield writes the row a yield carries, RefusalOpen reads it, and
// DeleteTaskRefusals is what an operator Start runs — on any node.
type refusalMetaStore struct {
	*resumeTable
	leases   *MemoryLease
	mu       sync.Mutex
	refusals map[string]map[string]ownership.Refusal
}

func newRefusalStore(inner *resumeTable, leases *MemoryLease) *refusalMetaStore {
	return &refusalMetaStore{resumeTable: inner, leases: leases, refusals: map[string]map[string]ownership.Refusal{}}
}

func (s *refusalMetaStore) CommitYield(ctx context.Context, row YieldCommit) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, err := s.resumeTable.GetTask(ctx, row.TaskID)
	if err != nil {
		return 0, err
	}
	if task.RowGeneration != row.ExpectGeneration {
		return 0, nil
	}
	task.State = State(row.State)
	task.DesiredRun = row.Desired
	task.OwnerWorkerID = row.Owner
	task.Epoch = row.Epoch
	task.RunID = row.RunID
	task.LastError = row.LastError
	task.FailedReason = row.FailedReason
	task.FailedAt = row.FailedAt
	task.TailUnreadableSince = row.TailSince
	task.PendingDelete = row.PendingDelete
	task.UpdatedAt = row.UpdatedAt
	task.RowGeneration++
	if err := s.resumeTable.UpsertTask(ctx, task); err != nil {
		return 0, err
	}
	if row.RefusalNode != "" {
		m := s.refusals[row.TaskID]
		if m == nil {
			m = map[string]ownership.Refusal{}
			s.refusals[row.TaskID] = m
		}
		m[row.RefusalNode] = ownership.Refusal{Reason: row.RefusalReason, Until: row.RefusalUntil, OpenEnded: row.RefusalOpen}
	}
	if row.Release {
		s.leases.mu.Lock()
		if l, ok := s.leases.rows[row.TaskID]; ok && l.epoch == row.ReleaseEpoch {
			l.owner = ""
			l.expire = time.Now()
			s.leases.rows[row.TaskID] = l
		}
		s.leases.mu.Unlock()
	}
	return 1, nil
}

func (s *refusalMetaStore) RefusalOpen(_ context.Context, taskID, nodeID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.refusals[taskID][nodeID]
	if !ok {
		return false, nil
	}
	return r.OpenEnded || r.Until.After(time.Now()), nil
}

func (s *refusalMetaStore) DeleteTaskRefusals(_ context.Context, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.refusals, taskID)
	return nil
}

func (s *refusalMetaStore) TaskRowGeneration(ctx context.Context, taskID string) (int64, error) {
	task, err := s.resumeTable.GetTask(ctx, taskID)
	if err != nil {
		return 0, err
	}
	return task.RowGeneration, nil
}

func (s *refusalMetaStore) openFor(taskID, node string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.refusals[taskID][node]
	return ok && (r.OpenEnded || r.Until.After(time.Now()))
}

// refusalWorld is two workers and a control-plane scheduler on one lease
// table, one task table, and one refusal table — the shape the rolling
// upgrade runs: n1 owns, n2 takes over, cp records the operator Start.
type refusalWorld struct {
	leases *MemoryLease
	store  *refusalMetaStore
	inner  *resumeTable
	hs     *leaseHealthStore
	log    *dumpLog
	nodes  map[string]*Scheduler
	run    map[string]*nodeRunner
}

func newRefusalWorld(t *testing.T) *refusalWorld {
	t.Helper()
	leases := NewMemoryLease()
	inner := &resumeTable{fencedMemoryStore: &fencedMemoryStore{lockedFakeStore: &lockedFakeStore{fakeStore: newFakeStore()}, leases: leases}}
	store := newRefusalStore(inner, leases)
	w := &refusalWorld{
		leases: leases, store: store, inner: inner,
		hs: newLeaseHealthStore(leases), log: newDumpLog(),
		nodes: map[string]*Scheduler{}, run: map[string]*nodeRunner{},
	}
	timeouts := InternalCallTimeouts{Read: 40 * time.Millisecond, Write: 40 * time.Millisecond, Lease: 25 * time.Millisecond, Upload: 40 * time.Millisecond}
	gate := &stubClusterGate{}
	for _, id := range []string{"n1", "n2", "cp"} {
		var lease LeaseManager = leases
		if id == "n2" {
			lease = &metaLease{MemoryLease: leases}
		}
		s := NewScheduler(
			WithStore(store),
			WithFindingStore(w.hs),
			WithClusterGate(gate),
			WithClusterLeaseManager(lease),
			WithClusterWorkerID(id),
			WithClusterLease(time.Minute, time.Hour, time.Minute),
			WithLeaseClock(NewLeaseClock()),
			WithInternalCallTimeouts(timeouts),
		)
		if id != "cp" {
			r := &nodeRunner{node: id, log: w.log}
			s.SetRunner(r)
			w.run[id] = r
		}
		w.nodes[id] = s
	}
	return w
}

func (w *refusalWorld) taskRow(t *testing.T, id string) Task {
	t.Helper()
	task, err := w.inner.GetTask(context.Background(), id)
	if err != nil {
		t.Fatalf("row: %v", err)
	}
	return task
}

func (w *refusalWorld) waitRowState(t *testing.T, id string, want State) Task {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		task := w.taskRow(t, id)
		if task.State == want {
			return task
		}
		if time.Now().After(deadline) {
			t.Fatalf("row state %s, want %s", task.State, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestOperatorStartOnAnotherNodeUnblocksTailYield reproduces the rolling
// upgrade operator path: n2's takeover failed on the foreign tail and wrote
// an open-ended refusal; the operator Start is recorded by the control
// plane, which deletes the stored row but cannot touch n2's cached copy.
// The reclaim must follow the durable record, or the task deadlocks in
// STARTING with no log — the exact CI failure.
func TestOperatorStartOnAnotherNodeUnblocksTailYield(t *testing.T) {
	w := newRefusalWorld(t)
	n1, n2, cp := w.nodes["n1"], w.nodes["n2"], w.nodes["cp"]

	task := ownedIdleTask(t, n1, "", 0, StateStopped)
	id := task.ID
	if err := n1.startTask(id, taskStart{acquireLease: true}); err != nil {
		t.Fatalf("n1 start: %v", err)
	}
	w.waitRowState(t, id, StateRunning)
	expireHeldLeaseFor(w.leases, id)

	// n2 takes over; the foreign open segment is a permanent tail yield.
	w.run["n2"].fail = NewPermanentError(CodeSegmentNotOnWorker, "SEGMENT_NOT_ON_WORKER: foreign tail")
	if _, err := n2.ClaimExpiredTasks(); err != nil {
		t.Fatalf("n2 claim expired: %v", err)
	}
	w.waitRowState(t, id, StateLeaseDegraded)
	if !w.store.openFor(id, "n2") {
		t.Fatal("tail yield did not write the n2 refusal row")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		n2.mu.Lock()
		open := n2.refusals[id]["n2"].OpenEnded
		n2.mu.Unlock()
		if open || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	n2.mu.Lock()
	cached := n2.refusals[id]["n2"].OpenEnded
	n2.mu.Unlock()
	if !cached {
		t.Fatal("n2 did not cache its own refusal")
	}

	// The operator copies the tail onto n2 and asks the control plane to
	// Start. cp clears the durable record; n2's cache is untouched.
	cp.mu.Lock()
	cp.tasks[id] = w.taskRow(t, id)
	cp.mu.Unlock()
	w.run["n2"].fail = nil
	if err := cp.StartTask(id); err != nil {
		t.Fatalf("cp start: %v", err)
	}
	if w.store.openFor(id, "n2") {
		t.Fatal("operator Start left the refusal row")
	}
	if got := w.taskRow(t, id); got.State != StateStarting || got.OwnerWorkerID != "" {
		t.Fatalf("after start: %s owner=%q", got.State, got.OwnerWorkerID)
	}

	// n2's next sweep must converge: the stale cache must not outlive the
	// deleted row.
	before := w.log.count()
	deadline = time.Now().Add(3 * time.Second)
	for w.log.count() == before && time.Now().Before(deadline) {
		_, _ = n2.ClaimRunnableTasks()
		time.Sleep(10 * time.Millisecond)
	}
	if w.log.count() == before {
		got := w.taskRow(t, id)
		t.Fatalf("operator start never converged on n2: row %s owner=%q — the cached refusal outlived the deleted row", got.State, got.OwnerWorkerID)
	}
	waitTaskRowOwner(t, w, id, "n2")
}

// TestOpenRefusalStillBlocksReclaim is the safety half: while the stored row
// stands, the node that cannot read the tail stays out.
func TestOpenRefusalStillBlocksReclaim(t *testing.T) {
	w := newRefusalWorld(t)
	n1, n2 := w.nodes["n1"], w.nodes["n2"]

	task := ownedIdleTask(t, n1, "", 0, StateStopped)
	id := task.ID
	if err := n1.startTask(id, taskStart{acquireLease: true}); err != nil {
		t.Fatalf("n1 start: %v", err)
	}
	w.waitRowState(t, id, StateRunning)
	expireHeldLeaseFor(w.leases, id)

	w.run["n2"].fail = NewPermanentError(CodeSegmentNotOnWorker, "SEGMENT_NOT_ON_WORKER: foreign tail")
	if _, err := n2.ClaimExpiredTasks(); err != nil {
		t.Fatalf("n2 claim expired: %v", err)
	}
	w.waitRowState(t, id, StateLeaseDegraded)
	if !w.store.openFor(id, "n2") {
		t.Fatal("tail yield did not write the n2 refusal row")
	}

	before := w.log.count()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		_, _ = n2.ClaimRunnableTasks()
		time.Sleep(10 * time.Millisecond)
	}
	if w.log.count() != before {
		t.Fatal("n2 reclaimed while its open-ended refusal row still stands")
	}
	if got := w.taskRow(t, id); got.State != StateLeaseDegraded {
		t.Fatalf("row moved without operator intervention: %s", got.State)
	}
}

func expireHeldLeaseFor(leases *MemoryLease, id string) {
	leases.mu.Lock()
	row := leases.rows[id]
	row.expire = time.Now().Add(-time.Minute)
	leases.rows[id] = row
	leases.mu.Unlock()
}

func waitTaskRowOwner(t *testing.T, w *refusalWorld, id, owner string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		task := w.taskRow(t, id)
		if task.State == StateRunning && task.OwnerWorkerID == owner {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("row %s owner=%q, want RUNNING on %s", task.State, task.OwnerWorkerID, owner)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
