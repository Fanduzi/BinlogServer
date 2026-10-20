// Package tasks provides module-level functionality for tasks.
// input: a cluster whose task row is FAILED with no owner (another epoch failed it and released the lease), task_health still HEALTHY from the last owner's epoch, a health store that checks healthstore.CheckFence against the in-memory task_leases, and evaluators on two nodes
// output: proof that the _health holder opens CRITICAL TASK_FAILED and the health turns UNHEALTHY, that only one node evaluates unowned tasks, that a proxy pass reads no local signals, and that a proxy write fenced by a live claim is retried once the lease is released; the in-memory store keeps chain_owner across proxy, delegated, and PreserveChainOwner writes (§30, §31) and, when Fence.FollowRow is set, records task_health.owner from the task row (§31.6)
// pos: regression for ADR 0009 P2-6c (design 0009 §24.2–§24.3, #421)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
)

// leaseHealthStore is a cluster health store: the fence is checked with
// healthstore.CheckFence against MemoryLease, as meta does under lock.
type leaseHealthStore struct {
	mu     sync.Mutex
	leases *MemoryLease
	open   map[string]map[health.Key]health.Finding
	health map[string]health.TaskHealth
	stored map[string]healthstore.Stored
	writes map[string]int
	// rowOwner / rowEpoch mirror task_health.owner and owner_epoch.
	// FollowRow copies the task row; otherwise the columns stay put.
	rowOwner map[string]string
	rowEpoch map[string]int64
}

func newLeaseHealthStore(leases *MemoryLease) *leaseHealthStore {
	return &leaseHealthStore{leases: leases, open: map[string]map[health.Key]health.Finding{}, health: map[string]health.TaskHealth{},
		stored: map[string]healthstore.Stored{}, writes: map[string]int{},
		rowOwner: map[string]string{}, rowEpoch: map[string]int64{}}
}

func (s *leaseHealthStore) ApplyTransitions(_ context.Context, taskID string, f health.Fence, ts []health.Transition, h health.TaskHealth) (healthstore.Applied, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leases.mu.Lock()
	row, ok := s.leases.rows[taskID]
	s.leases.mu.Unlock()
	var lease *healthstore.Lease
	if ok {
		lease = &healthstore.Lease{Owner: row.owner, Epoch: row.epoch, Expired: !row.expire.After(time.Now())}
	}
	var stored *healthstore.Stored
	if st, ok := s.stored[taskID]; ok {
		stored = &st
	}
	if err := healthstore.CheckFence(f, lease, stored); err != nil {
		return healthstore.Applied{}, err
	}
	m := s.open[taskID]
	if m == nil {
		m = map[health.Key]health.Finding{}
		s.open[taskID] = m
	}
	out := healthstore.Applied{Generations: make([]int64, len(ts))}
	for i, t := range ts {
		out.Generations[i] = int64(i + 1)
		if t.Op == health.OpClose {
			delete(m, t.Key)
			continue
		}
		m[t.Key] = health.Finding{TaskID: taskID, Kind: t.Kind, Subject: t.Subject, Severity: t.Instance.Severity, Detail: t.Instance.Detail}
	}
	s.health[taskID] = h
	prev := s.stored[taskID]
	next := healthstore.Stored{Mode: f.Mode, Epoch: f.TaskEpoch, Node: f.Node, Instance: f.Instance, ChainOwner: prev.ChainOwner}
	// Only a cluster holder records the chain owner (design §30). A
	// tail-failure write sets PreserveChainOwner and leaves it (§31).
	if f.Mode == health.FenceCluster && !f.Proxy && !f.Delegated && !f.PreserveChainOwner {
		next.ChainOwner = f.Node
	}
	s.stored[taskID] = next
	s.writes[taskID]++
	// task_health.owner follows the task row when the writer says so (§31.6).
	if f.FollowRow {
		s.rowOwner[taskID] = f.RowOwner
		s.rowEpoch[taskID] = f.RowEpoch
	}
	return out, nil
}

func (s *leaseHealthStore) rowOwnership(taskID string) (string, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rowOwner[taskID], s.rowEpoch[taskID]
}

func (s *leaseHealthStore) ChainOwner(_ context.Context, taskID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stored[taskID].ChainOwner, nil
}

func (s *leaseHealthStore) ListOpenFindingsForTask(_ context.Context, taskID string) ([]health.Finding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []health.Finding
	for _, f := range s.open[taskID] {
		out = append(out, f)
	}
	return out, nil
}

func (s *leaseHealthStore) ListHealth(context.Context) ([]health.TaskHealth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []health.TaskHealth
	for _, h := range s.health {
		out = append(out, h)
	}
	return out, nil
}

func (s *leaseHealthStore) snapshot(taskID string) (health.TaskHealth, map[health.Key]health.Finding, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := map[health.Key]health.Finding{}
	for k, v := range s.open[taskID] {
		m[k] = v
	}
	return s.health[taskID], m, s.writes[taskID]
}

type unownedFixture struct {
	leases *MemoryLease
	hs     *leaseHealthStore
	store  *fakeStore
	taskID string
}

const failedLastError = "SEGMENT_NOT_ON_WORKER: /data/n1/1/mbin.000001.open.e1 is not on this worker"

// newUnownedFixture: the #421 state after n2 failed the task at epoch 2.
func newUnownedFixture(t *testing.T) *unownedFixture {
	t.Helper()
	leases := NewMemoryLease()
	f := &unownedFixture{leases: leases, hs: newLeaseHealthStore(leases), store: newFakeStore(), taskID: "1"}
	task := Task{ID: f.taskID, Name: "t", ClusterKey: "k", State: StateFailed, DesiredRun: TaskDesiredStop, LastError: failedLastError,
		SpecRevision: 2, FailedSpecRevision: 2, UpdatedAt: time.Now()}
	if err := f.store.UpsertTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	leases.rows[f.taskID] = memoryLeaseRow{owner: "", epoch: 2, expire: time.Now().Add(-time.Second)}
	// n1 wrote HEALTHY at epoch 1 before it was frozen.
	f.hs.health[f.taskID] = health.TaskHealth{TaskID: f.taskID, Status: health.StatusHealthy}
	f.hs.stored[f.taskID] = healthstore.Stored{Mode: health.FenceCluster, Epoch: 1, Node: "n1"}
	return f
}

func (f *unownedFixture) node(t *testing.T, id string) (*Scheduler, *Evaluator) {
	t.Helper()
	s := NewScheduler(WithStore(f.store), WithClusterLeaseManager(f.leases), WithClusterWorkerID(id))
	task, err := f.store.GetTask(context.Background(), f.taskID)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.tasks[f.taskID] = task
	s.mu.Unlock()
	e := NewEvaluator(s, EvaluatorConfig{Mode: EvaluatorOn, Store: f.hs, Cluster: true, NodeID: id, Instance: id + "-i", Sweep: time.Minute})
	return s, e
}

func TestUnownedFailedOpensTaskFailed(t *testing.T) {
	f := newUnownedFixture(t)
	_, e1 := f.node(t, "n1")
	_, e2 := f.node(t, "n2")
	ctx := context.Background()

	if err := e1.SweepNow(ctx); err != nil {
		t.Fatalf("n1 sweep: %v", err)
	}
	if err := e2.SweepNow(ctx); err != nil {
		t.Fatalf("n2 sweep: %v", err)
	}
	h, open, writes := f.hs.snapshot(f.taskID)
	var failed *health.Finding
	for _, o := range open {
		if o.Kind == health.TaskFailed {
			o := o
			failed = &o
		}
	}
	if failed == nil || failed.Severity != health.Critical {
		t.Fatalf("open findings %+v, want a CRITICAL TASK_FAILED", open)
	}
	if h.Status != health.StatusUnhealthy {
		t.Fatalf("health %s, want UNHEALTHY", h.Status)
	}
	if writes != 1 {
		t.Fatalf("%d health writes, want 1 (only the _health holder evaluates unowned tasks)", writes)
	}
	if !e1.healthHeld.Load() || e2.healthHeld.Load() {
		t.Fatalf("_health held: n1=%v n2=%v, want n1 only", e1.healthHeld.Load(), e2.healthHeld.Load())
	}
}

// A proxy pass must not use signals this node kept from when it held the
// task: they describe a run that is gone.
func TestUnownedProxyIgnoresLocalSignals(t *testing.T) {
	f := newUnownedFixture(t)
	_, e1 := f.node(t, "n1")
	e1.Signal(f.taskID, FindingTaskPersistFailed, "", false, "persist failed at epoch 1")
	if err := e1.SweepNow(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	_, open, _ := f.hs.snapshot(f.taskID)
	for k := range open {
		if k.Kind == health.TaskPersistFailed {
			t.Fatalf("proxy pass opened %s from a local signal", k.Kind)
		}
	}
}

// The lease is held for a moment while the row has no owner yet (a claim
// in flight): the proxy write is refused, and not latched, so the next
// pass after the release writes.
func TestUnownedProxyFencedWhileClaimedThenRetried(t *testing.T) {
	f := newUnownedFixture(t)
	f.leases.rows[f.taskID] = memoryLeaseRow{owner: "n3", epoch: 2, expire: time.Now().Add(time.Hour)}
	_, e1 := f.node(t, "n1")
	ctx := context.Background()
	_ = e1.SweepNow(ctx)
	if _, _, writes := f.hs.snapshot(f.taskID); writes != 0 {
		t.Fatalf("proxy wrote while n3 held the lease")
	}
	f.leases.mu.Lock()
	f.leases.rows[f.taskID] = memoryLeaseRow{owner: "", epoch: 2, expire: time.Now().Add(-time.Second)}
	f.leases.mu.Unlock()
	if err := e1.SweepNow(ctx); err != nil {
		t.Fatalf("sweep after release: %v", err)
	}
	if h, _, _ := f.hs.snapshot(f.taskID); h.Status != health.StatusUnhealthy {
		t.Fatalf("health %s after release, want UNHEALTHY", h.Status)
	}
}

// An owned task is still evaluated only by its holder: the _health holder
// does not proxy it.
func TestOwnedTaskNotProxied(t *testing.T) {
	f := newUnownedFixture(t)
	ctx := context.Background()
	task, _ := f.store.GetTask(ctx, f.taskID)
	task.State, task.DesiredRun, task.OwnerWorkerID, task.Epoch, task.LastError = StateRunning, TaskDesiredRun, "n2", 3, ""
	_ = f.store.UpsertTask(ctx, task)
	f.leases.rows[f.taskID] = memoryLeaseRow{owner: "n2", epoch: 3, expire: time.Now().Add(time.Hour)}
	_, e1 := f.node(t, "n1")
	_ = e1.SweepNow(ctx)
	if _, _, writes := f.hs.snapshot(f.taskID); writes != 0 {
		t.Fatalf("the _health holder evaluated a task n2 owns")
	}
}
