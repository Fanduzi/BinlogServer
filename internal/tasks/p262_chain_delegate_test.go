// Package tasks provides module-level functionality for tasks.
// input: two in-process cluster schedulers with separate data directories, one MemoryLease, and a health store that checks healthstore.CheckFence
// output: acceptance for ADR 0009 P2-6.2 (design §30): a new lease rejects a delegated write; a node that is not the last owner is fenced; the last owner still opens and closes a chain finding after release; a restart on another node retires the old directory's finding with EVAL; a gone last owner makes health UNKNOWN while the chain finding stays open; standalone is unchanged
// pos: regression for non-shared chain delegation; the in-process stand-in for the §13 QA row (the three-node e2e stays workflow_dispatch)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
)

// p262Log records transitions on top of the in-memory cluster health store.
type p262Log struct {
	*leaseHealthStore
	mu sync.Mutex
	ts []health.Transition
}

func (s *p262Log) ApplyTransitions(ctx context.Context, taskID string, f health.Fence, ts []health.Transition, h health.TaskHealth) (healthstore.Applied, error) {
	out, err := s.leaseHealthStore.ApplyTransitions(ctx, taskID, f, ts, h)
	if err != nil {
		return out, err
	}
	s.mu.Lock()
	s.ts = append(s.ts, append([]health.Transition(nil), ts...)...)
	s.mu.Unlock()
	return out, nil
}

func (s *p262Log) closeReason(kind health.Kind) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.ts) - 1; i >= 0; i-- {
		if s.ts[i].Op == health.OpClose && s.ts[i].Kind == kind {
			return s.ts[i].Reason, true
		}
	}
	return "", false
}

// hbStore is a task store that also answers worker heartbeats.
type hbStore struct {
	*fakeStore
	beats []WorkerHeartbeat
	err   error
}

func (h *hbStore) ListWorkerHeartbeats(context.Context, int) ([]WorkerHeartbeat, error) {
	if h.err != nil {
		return nil, h.err
	}
	return append([]WorkerHeartbeat(nil), h.beats...), nil
}

type p262Env struct {
	t      *testing.T
	leases *MemoryLease
	hs     *leaseHealthStore
	log    *p262Log
	store  *fakeStore
	task   Task
}

func newP262(t *testing.T) *p262Env {
	t.Helper()
	leases := NewMemoryLease()
	hs := newLeaseHealthStore(leases)
	env := &p262Env{t: t, leases: leases, hs: hs, log: &p262Log{leaseHealthStore: hs}, store: newFakeStore()}
	env.task = Task{
		ID: "1", Name: "t", ClusterKey: "k", UpdatedAt: time.Now(),
		Source: SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: "mysql"},
		Start:  StartConfig{Mode: StartModeGTID, GTIDSet: gtidPITRUUID + ":1-36"},
	}
	env.own("n1", 1, StateRunning, TaskDesiredRun)
	return env
}

func (e *p262Env) own(owner string, epoch int64, state State, desired string) {
	e.t.Helper()
	e.task.OwnerWorkerID = owner
	e.task.Epoch = epoch
	e.task.State = state
	e.task.DesiredRun = desired
	e.task.UpdatedAt = time.Now()
	if err := e.store.UpsertTask(context.Background(), e.task); err != nil {
		e.t.Fatal(err)
	}
	if owner != "" && epoch > 0 {
		e.leases.mu.Lock()
		e.leases.rows[e.task.ID] = memoryLeaseRow{owner: owner, epoch: epoch, expire: time.Now().Add(time.Hour)}
		e.leases.mu.Unlock()
	}
}

func (e *p262Env) release(owner string, epoch int64) {
	e.t.Helper()
	ok, err := e.leases.Release(context.Background(), e.task.ID, owner, epoch)
	if err != nil || !ok {
		e.t.Fatalf("release ok=%v err=%v", ok, err)
	}
	e.own("", 0, StateStopped, TaskDesiredStop)
}

func (e *p262Env) node(id, data string, store TaskStore) (*Scheduler, *Evaluator) {
	e.t.Helper()
	if store == nil {
		store = e.store
	}
	s := NewScheduler(WithDataDir(data), WithStore(store), WithClusterLeaseManager(e.leases), WithClusterWorkerID(id))
	s.mu.Lock()
	s.tasks[e.task.ID] = e.task
	s.mu.Unlock()
	ev := NewEvaluator(s, EvaluatorConfig{
		Store: e.log, Cluster: true, NodeID: id, Instance: id + "-i",
		Sweep: time.Minute, Debounce: time.Millisecond,
	})
	return s, ev
}

func p262WriteChain(t *testing.T, dir string, leftover bool) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000001"), 37, 38)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000002"), 39, 40)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000003"), 41, 42)
	if !leftover {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "mysql-bin.000002.open.e3"), []byte{0xfe, 'b', 'i', 'n'}, 0o644); err != nil {
		t.Fatal(err)
	}
}

func p262LeftoverKey(taskID string) health.Key {
	return health.Key{TaskID: taskID, Kind: health.SegmentEmptyLeftover, Subject: "mysql-bin.000002.open.e3"}
}

func TestP262NewLeaseRejectsDelegatedWrite(t *testing.T) {
	lease := &healthstore.Lease{Owner: "n2", Epoch: 3}
	stored := &healthstore.Stored{Mode: health.FenceCluster, Epoch: 2, Node: "n1"}
	err := healthstore.CheckFence(health.Fence{Mode: health.FenceCluster, TaskEpoch: 2, Node: "n1", Delegated: true}, lease, stored)
	if !errors.Is(err, healthstore.ErrFenced) {
		t.Fatalf("delegated write after a new lease: %v, want ErrFenced", err)
	}
}

func TestP262OtherNodeDelegatedWriteRejected(t *testing.T) {
	// Released lease, and a proxy write has already replaced fence_node.
	lease := &healthstore.Lease{Owner: "", Epoch: 2, Expired: true}
	stored := &healthstore.Stored{Mode: health.FenceCluster, Epoch: 2, Node: "proxy"}
	err := healthstore.CheckFence(health.Fence{Mode: health.FenceCluster, TaskEpoch: 2, Node: "n9", Delegated: true}, lease, stored)
	if !errors.Is(err, healthstore.ErrFenced) {
		t.Fatalf("delegated write from a node that is not the last owner: %v, want ErrFenced", err)
	}
}

func TestP262StoppedLastOwnerOpensAndClosesChain(t *testing.T) {
	env := newP262(t)
	data1 := t.TempDir()
	dir := filepath.Join(data1, env.task.ID)
	p262WriteChain(t, dir, true)
	_, e1 := env.node("n1", data1, nil)
	ctx := context.Background()
	h, err := e1.EvaluateNow(ctx, env.task.ID)
	if err != nil {
		t.Fatalf("holder: %v", err)
	}
	if h.Status != health.StatusHealthy {
		t.Fatalf("holder health %s, want HEALTHY (INFO leftover)", h.Status)
	}
	if _, open, _ := env.hs.snapshot(env.task.ID); open[p262LeftoverKey(env.task.ID)].Kind != health.SegmentEmptyLeftover {
		t.Fatalf("open %+v, want SEGMENT_EMPTY_LEFTOVER", open)
	}

	env.release("n1", 1)
	_, e2 := env.node("n2", t.TempDir(), nil)
	if err := e2.SweepNow(ctx); err != nil {
		t.Fatalf("proxy sweep: %v", err)
	}
	owner, err := env.hs.ChainOwner(ctx, env.task.ID)
	if err != nil || owner != "n1" {
		t.Fatalf("chain owner %q after proxy (%v), want n1", owner, err)
	}
	if err := os.Remove(filepath.Join(dir, "mysql-bin.000002.open.e3")); err != nil {
		t.Fatal(err)
	}
	h, err = e1.EvaluateNow(ctx, env.task.ID)
	if err != nil {
		t.Fatalf("last owner after release: %v", err)
	}
	if h.Status == health.StatusUnknown {
		t.Fatalf("health UNKNOWN after the leftover was removed: %+v", h)
	}
	if _, open, _ := env.hs.snapshot(env.task.ID); len(open) != 0 {
		t.Fatalf("leftover still open: %+v", open)
	}
}

func TestP262OwnerChangedClosesOldDirectory(t *testing.T) {
	env := newP262(t)
	data1 := t.TempDir()
	p262WriteChain(t, filepath.Join(data1, env.task.ID), true)
	_, e1 := env.node("n1", data1, nil)
	ctx := context.Background()
	if _, err := e1.EvaluateNow(ctx, env.task.ID); err != nil {
		t.Fatalf("holder: %v", err)
	}
	env.release("n1", 1)
	epoch, ok, err := env.leases.Acquire(ctx, env.task.ID, "n2", time.Hour)
	if err != nil || !ok || epoch <= 1 {
		t.Fatalf("acquire epoch=%d ok=%v err=%v", epoch, ok, err)
	}
	env.own("n2", epoch, StateRunning, TaskDesiredRun)
	data2 := t.TempDir()
	p262WriteChain(t, filepath.Join(data2, env.task.ID), false)
	_, e2 := env.node("n2", data2, nil)
	if _, err := e2.EvaluateNow(ctx, env.task.ID); err != nil {
		t.Fatalf("new owner: %v", err)
	}
	reason, found := env.log.closeReason(health.SegmentEmptyLeftover)
	// PR-5: no OWNER_CHANGED close exists; the new owner's complete read
	// retires the old directory's finding with EVAL.
	if !found || reason != "EVAL" {
		t.Fatalf("close reason %q found=%v, want EVAL", reason, found)
	}
}

func TestP262NodeGoneChainUnknown(t *testing.T) {
	env := newP262(t)
	data1 := t.TempDir()
	p262WriteChain(t, filepath.Join(data1, env.task.ID), true)
	_, e1 := env.node("n1", data1, nil)
	ctx := context.Background()
	if _, err := e1.EvaluateNow(ctx, env.task.ID); err != nil {
		t.Fatalf("holder: %v", err)
	}
	env.release("n1", 1)
	hb := &hbStore{fakeStore: env.store, beats: []WorkerHeartbeat{{
		WorkerID: "n1", LastSeenAt: time.Now().Add(-time.Hour), Status: "up",
	}}}
	_, e2 := env.node("n2", t.TempDir(), hb)
	if err := e2.SweepNow(ctx); err != nil {
		t.Fatalf("proxy sweep: %v", err)
	}
	h, open, _ := env.hs.snapshot(env.task.ID)
	if h.Status != health.StatusUnknown || h.UnknownDetail != "node gone" {
		t.Fatalf("health %+v, want UNKNOWN (node gone)", h)
	}
	if len(h.Unknown) != 1 || h.Unknown[0] != health.ScopeChainLocal {
		t.Fatalf("unknown scopes %+v, want chain.local", h.Unknown)
	}
	if open[p262LeftoverKey(env.task.ID)].Kind != health.SegmentEmptyLeftover {
		t.Fatalf("chain finding %+v, want it still open", open)
	}
}

func TestP262MissingHeartbeatIsNodeGone(t *testing.T) {
	env := newP262(t)
	data1 := t.TempDir()
	p262WriteChain(t, filepath.Join(data1, env.task.ID), true)
	_, e1 := env.node("n1", data1, nil)
	ctx := context.Background()
	if _, err := e1.EvaluateNow(ctx, env.task.ID); err != nil {
		t.Fatalf("holder: %v", err)
	}
	env.release("n1", 1)
	hb := &hbStore{fakeStore: env.store}
	_, e2 := env.node("n2", t.TempDir(), hb)
	if err := e2.SweepNow(ctx); err != nil {
		t.Fatalf("proxy sweep: %v", err)
	}
	h, open, _ := env.hs.snapshot(env.task.ID)
	if h.Status != health.StatusUnknown || open[p262LeftoverKey(env.task.ID)].Kind != health.SegmentEmptyLeftover {
		t.Fatalf("health %+v open %+v, want UNKNOWN with the chain finding still open", h, open)
	}
}

func TestP262HeartbeatErrorDoesNotDeclareGone(t *testing.T) {
	env := newP262(t)
	data1 := t.TempDir()
	p262WriteChain(t, filepath.Join(data1, env.task.ID), true)
	_, e1 := env.node("n1", data1, nil)
	ctx := context.Background()
	if _, err := e1.EvaluateNow(ctx, env.task.ID); err != nil {
		t.Fatalf("holder: %v", err)
	}
	env.release("n1", 1)
	hb := &hbStore{fakeStore: env.store, err: errors.New("heartbeat list failed")}
	_, e2 := env.node("n2", t.TempDir(), hb)
	if err := e2.SweepNow(ctx); err != nil {
		t.Fatalf("proxy sweep: %v", err)
	}
	h, open, _ := env.hs.snapshot(env.task.ID)
	if h.Status == health.StatusUnknown {
		t.Fatalf("health %+v, a heartbeat read error must not declare the node gone", h)
	}
	if open[p262LeftoverKey(env.task.ID)].Kind != health.SegmentEmptyLeftover {
		t.Fatalf("chain finding %+v, want it still open", open)
	}
}

func TestP262SameNodeNewEpochIsEval(t *testing.T) {
	env := newP262(t)
	data := t.TempDir()
	dir := filepath.Join(data, env.task.ID)
	p262WriteChain(t, dir, true)
	_, e1 := env.node("n1", data, nil)
	ctx := context.Background()
	if _, err := e1.EvaluateNow(ctx, env.task.ID); err != nil {
		t.Fatalf("holder: %v", err)
	}
	env.release("n1", 1)
	epoch, ok, err := env.leases.Acquire(ctx, env.task.ID, "n1", time.Hour)
	if err != nil || !ok || epoch <= 1 {
		t.Fatalf("acquire epoch=%d ok=%v err=%v", epoch, ok, err)
	}
	env.own("n1", epoch, StateRunning, TaskDesiredRun)
	if err := os.Remove(filepath.Join(dir, "mysql-bin.000002.open.e3")); err != nil {
		t.Fatal(err)
	}
	_, again := env.node("n1", data, nil)
	h, err := again.EvaluateNow(ctx, env.task.ID)
	if err != nil {
		t.Fatalf("same node, new epoch: %v", err)
	}
	reason, found := env.log.closeReason(health.SegmentEmptyLeftover)
	if !found || reason != health.ReasonEval {
		t.Fatalf("close reason %q found=%v, want EVAL", reason, found)
	}
	if h.Status == health.StatusUnknown {
		t.Fatalf("health %+v", h)
	}
}

func TestP262LastOwnerDelegatedWriteAllowed(t *testing.T) {
	lease := &healthstore.Lease{Owner: "", Epoch: 2, Expired: true}
	stored := &healthstore.Stored{Mode: health.FenceCluster, Epoch: 2, Node: "proxy", ChainOwner: "n1"}
	if err := healthstore.CheckFence(health.Fence{Mode: health.FenceCluster, TaskEpoch: 2, Node: "n1", Delegated: true}, lease, stored); err != nil {
		t.Fatalf("last owner delegated write: %v", err)
	}
	if err := healthstore.CheckFence(health.Fence{Mode: health.FenceCluster, TaskEpoch: 2, Node: "n9", Delegated: true}, lease, stored); !errors.Is(err, healthstore.ErrFenced) {
		t.Fatalf("other node: %v, want ErrFenced", err)
	}
}

func TestP262StandaloneChainUnchanged(t *testing.T) {
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, dir := evalTask(t, s, data)
	p262WriteChain(t, dir, true)
	store := healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))
	e := newEval(t, s, store)
	ctx := context.Background()
	h, err := e.EvaluateNow(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if h.Status == health.StatusUnknown {
		t.Fatalf("standalone health %+v", h)
	}
	if err := os.Remove(filepath.Join(dir, "mysql-bin.000002.open.e3")); err != nil {
		t.Fatal(err)
	}
	if h, err = e.EvaluateNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if h.Status == health.StatusUnknown {
		t.Fatalf("standalone health after close %+v", h)
	}
	rows, err := store.ListTransitions(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Reason != "EVAL" {
			t.Fatalf("standalone transition %+v", row)
		}
	}
	if open := openOf(t, store, task.ID); len(open) != 0 {
		t.Fatalf("still open %+v", open)
	}
}
