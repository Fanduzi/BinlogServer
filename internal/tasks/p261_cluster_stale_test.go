// Package tasks provides module-level functionality for tasks.
// input: three in-process cluster schedulers sharing one MemoryLease and one health store, with an injected clock
// output: proof that only the _health holder opens WARNING HEALTH_STALE on _cluster (subject = task id, never fence, epoch 0), that a task inside 5 sweeps does not open it, and that a failed ListHealth neither opens nor closes
// pos: regression for ADR 0009 P2-6.1 (design 0009 §28); the in-process stand-in for the 3-node QA row
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
)

// recordingHealthStore records the fence of each health write and can fail ListHealth.
type recordingHealthStore struct {
	*leaseHealthStore
	mu      sync.Mutex
	fences  map[string][]health.Fence
	listErr error
}

func (s *recordingHealthStore) ApplyTransitions(ctx context.Context, taskID string, f health.Fence, ts []health.Transition, h health.TaskHealth) (healthstore.Applied, error) {
	s.mu.Lock()
	if s.fences == nil {
		s.fences = map[string][]health.Fence{}
	}
	s.fences[taskID] = append(s.fences[taskID], f)
	s.mu.Unlock()
	return s.leaseHealthStore.ApplyTransitions(ctx, taskID, f, ts, h)
}

func (s *recordingHealthStore) ListHealth(ctx context.Context) ([]health.TaskHealth, error) {
	s.mu.Lock()
	err := s.listErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.leaseHealthStore.ListHealth(ctx)
}

func (s *recordingHealthStore) fencesOf(taskID string) []health.Fence {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]health.Fence(nil), s.fences[taskID]...)
}

// staleCluster is three schedulers on one lease table. Sweep is one minute,
// so the default stale threshold is five minutes without a new config field.
type staleCluster struct {
	t      *testing.T
	start  time.Time
	clock  time.Time
	leases *MemoryLease
	hs     *leaseHealthStore
	rec    *recordingHealthStore
	store  *fakeStore
}

func newStaleCluster(t *testing.T) *staleCluster {
	t.Helper()
	start := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	hs := newLeaseHealthStore(NewMemoryLease())
	return &staleCluster{
		t: t, start: start, clock: start, leases: hs.leases, hs: hs,
		rec:   &recordingHealthStore{leaseHealthStore: hs},
		store: newFakeStore(),
	}
}

func (c *staleCluster) now() time.Time { return c.clock }

func (c *staleCluster) advance(d time.Duration) { c.clock = c.clock.Add(d) }

func (c *staleCluster) putOwned(id, owner string, epoch int64, evaluated time.Time, hasRow bool) Task {
	c.t.Helper()
	task := Task{ID: id, Name: id, State: StateRunning, DesiredRun: TaskDesiredRun, OwnerWorkerID: owner, Epoch: epoch}
	if err := c.store.UpsertTask(context.Background(), task); err != nil {
		c.t.Fatal(err)
	}
	c.leases.mu.Lock()
	c.leases.rows[id] = memoryLeaseRow{owner: owner, epoch: epoch, expire: time.Now().Add(time.Hour)}
	c.leases.mu.Unlock()
	if hasRow {
		c.hs.mu.Lock()
		c.hs.health[id] = health.TaskHealth{TaskID: id, Status: health.StatusHealthy, EvaluatedAt: evaluated}
		c.hs.mu.Unlock()
	}
	return task
}

func (c *staleCluster) node(id string, mem []Task, cp bool, mode EvaluatorMode) (*Scheduler, *Evaluator) {
	c.t.Helper()
	s := NewScheduler(WithStore(c.store), WithClusterLeaseManager(c.leases), WithClusterWorkerID(id), WithFindingStore(c.hs))
	s.mu.Lock()
	for _, task := range mem {
		s.tasks[task.ID] = task
	}
	s.mu.Unlock()
	e := NewEvaluator(s, EvaluatorConfig{
		Mode: mode, Store: c.rec, Cluster: true, NodeID: id, Instance: id + "-i",
		Sweep: time.Minute, Now: c.now, ControlPlaneOnly: cp,
	})
	return s, e
}

func staleOpen(t *testing.T, hs *leaseHealthStore) []health.Finding {
	t.Helper()
	_, open, _ := hs.snapshot(ClusterFindingTask)
	var out []health.Finding
	for _, f := range open {
		if f.Kind == health.HealthStale {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subject < out[j].Subject })
	return out
}

func TestClusterHealthStaleOnlyHolderWrites(t *testing.T) {
	c := newStaleCluster(t)
	task := c.putOwned("7", "n2", 4, c.start.Add(-6*time.Minute), true)
	s1, e1 := c.node("n1", []Task{task}, false, EvaluatorOn)
	_, e2 := c.node("n2", []Task{task}, false, EvaluatorOn)
	_, e3 := c.node("n3", []Task{task}, false, EvaluatorOn)
	ctx := context.Background()
	if err := e1.SweepNow(ctx); err != nil {
		t.Fatalf("n1 sweep: %v", err)
	}
	got := staleOpen(t, c.hs)
	if len(got) != 1 || got[0].Subject != "7" || got[0].Severity != health.Warning {
		t.Fatalf("open %+v, want one WARNING HEALTH_STALE subject 7", got)
	}
	h, _, writes := c.hs.snapshot(ClusterFindingTask)
	if h.Status != health.StatusDegraded {
		t.Fatalf("_cluster health %s, want DEGRADED", h.Status)
	}
	if writes != 1 {
		t.Fatalf("%d _cluster writes, want 1", writes)
	}
	fences := c.rec.fencesOf(ClusterFindingTask)
	if len(fences) != 1 {
		t.Fatalf("fences %+v, want 1", fences)
	}
	f := fences[0]
	ep := e1.healthEpoch.Load()
	if f.Mode != health.FenceNeverLeased || f.TaskEpoch != 0 || f.Proxy || f.Delegated || f.Node != "n1" || ep == 0 || f.TaskEpoch == ep {
		t.Fatalf("fence %+v _health epoch %d, want never epoch 0 by n1 (not the _health epoch)", f, ep)
	}
	detail := got[0].Detail
	if err := e1.SweepNow(ctx); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	again := staleOpen(t, c.hs)
	if len(again) != 1 || again[0].Detail != detail {
		t.Fatalf("detail changed %q -> %+v", detail, again)
	}
	if err := e3.SweepNow(ctx); err != nil {
		t.Fatalf("n3 sweep: %v", err)
	}
	if _, _, writes = c.hs.snapshot(ClusterFindingTask); writes != 2 {
		// n1 wrote twice (open, then the same finding); n3 must not add one.
		// The second n1 sweep still writes the _cluster health row.
		t.Fatalf("%d _cluster writes after n3, want 2 (n1 only)", writes)
	}
	if e3.healthHeld.Load() || !e1.healthHeld.Load() || e2.healthHeld.Load() {
		t.Fatalf("held n1=%v n2=%v n3=%v, want n1 only", e1.healthHeld.Load(), e2.healthHeld.Load(), e3.healthHeld.Load())
	}
	sum := s1.HealthSummary(ctx)
	if sum.Status != HealthOK {
		t.Fatalf("summary %+v, want ok (WARNING does not degrade /api/health)", sum)
	}
	listed, err := s1.OpenFindings(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, f := range listed {
		if f.TaskID == ClusterFindingTask && f.Kind == FindingKind(health.HealthStale) && f.Subject == "7" && f.Open() {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("OpenFindings %+v, want HEALTH_STALE/7 on _cluster", listed)
	}
}

func TestClusterHealthStaleNotBeforeFiveSweeps(t *testing.T) {
	c := newStaleCluster(t)
	var mem []Task
	for i := 0; i < 120; i++ {
		mem = append(mem, c.putOwned(fmt.Sprintf("t%03d", i), "n2", 1, c.start.Add(-4*time.Minute), true))
	}
	mem = append(mem, c.putOwned("late", "n2", 1, c.start.Add(-5*time.Minute), true))
	_, e1 := c.node("n1", mem, false, EvaluatorOn)
	_, _ = c.node("n2", mem, false, EvaluatorOn)
	_, _ = c.node("n3", mem, false, EvaluatorOn)
	if err := e1.SweepNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := staleOpen(t, c.hs)
	if len(got) != 1 || got[0].Subject != "late" {
		t.Fatalf("open %+v, want only late", got)
	}
}

func TestClusterHealthStaleWaitsForKnownSince(t *testing.T) {
	c := newStaleCluster(t)
	task := c.putOwned("9", "n2", 2, time.Time{}, false)
	_, e1 := c.node("n1", []Task{task}, false, EvaluatorOn)
	_, _ = c.node("n2", []Task{task}, false, EvaluatorOn)
	_, _ = c.node("n3", []Task{task}, false, EvaluatorOn)
	ctx := context.Background()
	if err := e1.SweepNow(ctx); err != nil {
		t.Fatal(err)
	}
	if got := staleOpen(t, c.hs); len(got) != 0 {
		t.Fatalf("first sight opened %+v", got)
	}
	c.advance(5 * time.Minute)
	if err := e1.SweepNow(ctx); err != nil {
		t.Fatal(err)
	}
	got := staleOpen(t, c.hs)
	if len(got) != 1 || got[0].Subject != "9" || got[0].Severity != health.Warning {
		t.Fatalf("after stale_after %+v, want one WARNING for 9", got)
	}
}

func TestClusterHealthStaleClosesWhenEvaluated(t *testing.T) {
	c := newStaleCluster(t)
	task := c.putOwned("7", "n2", 4, c.start.Add(-6*time.Minute), true)
	_, e1 := c.node("n1", []Task{task}, false, EvaluatorOn)
	_, _ = c.node("n2", []Task{task}, false, EvaluatorOn)
	_, _ = c.node("n3", []Task{task}, false, EvaluatorOn)
	ctx := context.Background()
	if err := e1.SweepNow(ctx); err != nil {
		t.Fatal(err)
	}
	if got := staleOpen(t, c.hs); len(got) != 1 {
		t.Fatalf("setup %+v, want one open", got)
	}
	c.hs.mu.Lock()
	row := c.hs.health["7"]
	row.EvaluatedAt = c.now()
	c.hs.health["7"] = row
	c.hs.mu.Unlock()
	if err := e1.SweepNow(ctx); err != nil {
		t.Fatal(err)
	}
	if got := staleOpen(t, c.hs); len(got) != 0 {
		t.Fatalf("still open after a fresh evaluated_at: %+v", got)
	}
}

func TestClusterHealthStaleClosesWhenTaskGone(t *testing.T) {
	c := newStaleCluster(t)
	task := c.putOwned("7", "n2", 4, c.start.Add(-6*time.Minute), true)
	s1, e1 := c.node("n1", []Task{task}, false, EvaluatorOn)
	_, _ = c.node("n2", []Task{task}, false, EvaluatorOn)
	_, _ = c.node("n3", []Task{task}, false, EvaluatorOn)
	ctx := context.Background()
	if err := e1.SweepNow(ctx); err != nil {
		t.Fatal(err)
	}
	if got := staleOpen(t, c.hs); len(got) != 1 {
		t.Fatalf("setup %+v, want one open", got)
	}
	s1.mu.Lock()
	delete(s1.tasks, "7")
	s1.mu.Unlock()
	delete(c.store.tasks, "7")
	if err := e1.SweepNow(ctx); err != nil {
		t.Fatal(err)
	}
	if got := staleOpen(t, c.hs); len(got) != 0 {
		t.Fatalf("still open after the task left the list: %+v", got)
	}
}

func TestClusterHealthStaleListFailureDoesNotOpenOrClose(t *testing.T) {
	c := newStaleCluster(t)
	task := c.putOwned("7", "n2", 4, c.start.Add(-6*time.Minute), true)
	_, e1 := c.node("n1", []Task{task}, false, EvaluatorOn)
	_, _ = c.node("n2", []Task{task}, false, EvaluatorOn)
	_, _ = c.node("n3", []Task{task}, false, EvaluatorOn)
	ctx := context.Background()
	c.rec.mu.Lock()
	c.rec.listErr = errors.New("list health down")
	c.rec.mu.Unlock()
	if err := e1.SweepNow(ctx); err == nil {
		t.Fatal("ListHealth failure returned nil")
	}
	if got := staleOpen(t, c.hs); len(got) != 0 {
		t.Fatalf("opened on a failed list: %+v", got)
	}
	c.rec.mu.Lock()
	c.rec.listErr = nil
	c.rec.mu.Unlock()
	if err := e1.SweepNow(ctx); err != nil {
		t.Fatal(err)
	}
	if got := staleOpen(t, c.hs); len(got) != 1 {
		t.Fatalf("setup %+v, want one open", got)
	}
	c.rec.mu.Lock()
	c.rec.listErr = errors.New("list health down")
	c.rec.mu.Unlock()
	if err := e1.SweepNow(ctx); err == nil {
		t.Fatal("ListHealth failure returned nil")
	}
	if got := staleOpen(t, c.hs); len(got) != 1 {
		t.Fatalf("closed or dropped on a failed list: %+v", got)
	}
}

func TestClusterHealthStaleFromControlPlane(t *testing.T) {
	c := newStaleCluster(t)
	task := c.putOwned("7", "n2", 4, c.start.Add(-6*time.Minute), true)
	_, eCP := c.node("cp", nil, true, EvaluatorOn)
	_, _ = c.node("n2", []Task{task}, false, EvaluatorOn)
	_, _ = c.node("n3", []Task{task}, false, EvaluatorOn)
	if err := eCP.SweepNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := staleOpen(t, c.hs)
	if len(got) != 1 || got[0].Subject != "7" {
		t.Fatalf("control plane open %+v, want HEALTH_STALE/7", got)
	}
	if !eCP.healthHeld.Load() {
		t.Fatal("control plane did not hold _health")
	}
}

func TestClusterHealthStaleLeavesMetaUnreachableOpen(t *testing.T) {
	c := newStaleCluster(t)
	task := c.putOwned("7", "n2", 4, c.start.Add(-6*time.Minute), true)
	key := health.Key{TaskID: ClusterFindingTask, Kind: health.MetaUnreachable, Subject: "n9"}
	c.hs.mu.Lock()
	if c.hs.open[ClusterFindingTask] == nil {
		c.hs.open[ClusterFindingTask] = map[health.Key]health.Finding{}
	}
	c.hs.open[ClusterFindingTask][key] = health.Finding{TaskID: ClusterFindingTask, Kind: health.MetaUnreachable, Subject: "n9", Severity: health.Critical, Detail: "meta away"}
	c.hs.mu.Unlock()
	_, e1 := c.node("n1", []Task{task}, false, EvaluatorOn)
	_, _ = c.node("n2", []Task{task}, false, EvaluatorOn)
	_, _ = c.node("n3", []Task{task}, false, EvaluatorOn)
	if err := e1.SweepNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, open, _ := c.hs.snapshot(ClusterFindingTask)
	if _, ok := open[key]; !ok {
		t.Fatalf("META_UNREACHABLE closed by the stale pass: %+v", open)
	}
	if got := staleOpen(t, c.hs); len(got) != 1 || got[0].Subject != "7" {
		t.Fatalf("stale %+v, want HEALTH_STALE/7 beside the critical finding", got)
	}
}

func TestClusterHealthStaleShadowWritesNothing(t *testing.T) {
	c := newStaleCluster(t)
	task := c.putOwned("7", "n2", 4, c.start.Add(-6*time.Minute), true)
	_, e1 := c.node("n1", []Task{task}, false, EvaluatorShadow)
	_, _ = c.node("n2", []Task{task}, false, EvaluatorOn)
	_, _ = c.node("n3", []Task{task}, false, EvaluatorOn)
	if err := e1.SweepNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, writes := c.hs.snapshot(ClusterFindingTask); writes != 0 {
		t.Fatalf("shadow wrote _cluster %d times", writes)
	}
	if got := staleOpen(t, c.hs); len(got) != 0 {
		t.Fatalf("shadow opened %+v", got)
	}
}

func TestStandaloneClusterPathDoesNotOpenHealthStale(t *testing.T) {
	dir := t.TempDir()
	store := healthstore.NewFileStore(filepath.Join(dir, ".health.json"))
	s := NewScheduler()
	start := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	e := NewEvaluator(s, EvaluatorConfig{
		Mode: EvaluatorOn, Store: store, Cluster: false, NodeID: "local", Instance: "i",
		Sweep: time.Minute, Now: func() time.Time { return start },
	})
	e.Signal(ClusterFindingTask, FindingDiskListFailed, "data", false, "list failed")
	if err := e.SweepNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	open, err := store.ListOpenFindingsForTask(context.Background(), ClusterFindingTask)
	if err != nil {
		t.Fatal(err)
	}
	var signal, stale int
	for _, f := range open {
		switch f.Kind {
		case health.DiskListFailed:
			signal++
		case health.HealthStale:
			stale++
		}
	}
	if signal != 1 || stale != 0 {
		t.Fatalf("standalone _cluster open %+v, want the signal and no HEALTH_STALE", open)
	}
}
