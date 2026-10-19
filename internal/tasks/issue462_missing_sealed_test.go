// Package tasks provides module-level functionality for tasks.
// input: a cluster holder whose data_dir has a sealed LOCAL_ONLY segment removed, a second node with that file restored, a standalone directory of the same shape, and a peer start that returns a permanent SEGMENT_NOT_ON_WORKER before any health sweep
// output: acceptance for ADR 0009 §31: the owning disk opens CRITICAL SEGMENT_MISSING and closes it on restore or OWNER_CHANGED; standalone does not use OWNER_CHANGED; a peer that cannot read the local tail writes CRITICAL TASK_FAILED subject SEGMENT_NOT_ON_WORKER while the previous chain_owner and its finding stay
// pos: regression for a missing sealed segment and an unreachable local tail (design 0009 §31, #462)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
)

func issue462Holder(t *testing.T, env *p262Env, data string, catalog *lfCatalog, id string) *Evaluator {
	t.Helper()
	s := NewScheduler(
		WithDataDir(data),
		WithStore(env.store),
		WithFileStore(catalog),
		WithClusterLeaseManager(env.leases),
		WithClusterWorkerID(id),
		WithFindingStore(env.hs),
	)
	s.mu.Lock()
	s.tasks[env.task.ID] = env.task
	s.mu.Unlock()
	return NewEvaluator(s, EvaluatorConfig{
		Mode: EvaluatorOn, Store: env.log, Cluster: true, NodeID: id, Instance: id + "-i",
		Sweep: time.Minute, Debounce: time.Millisecond,
	})
}

func issue462Sealed(t *testing.T, taskID, dir string) *lfCatalog {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &lfFixture{catalog: newLFCatalog(), dir: dir, task: Task{ID: taskID}}
	f.chain(t, fbA)
	lfSegment(t, filepath.Join(dir, "mysql-bin.000001"), fbA+":1", fbEvents(fbA, 2, 3)...)
	lfSegment(t, filepath.Join(dir, "mysql-bin.000002"), fbA+":1-3", fbEvents(fbA, 4, 5)...)
	lfSegment(t, filepath.Join(dir, "mysql-bin.000003.open.e0"), fbA+":1-5", fbEvents(fbA, 6)...)
	f.row(t, "mysql-bin.000001", 0, "SEALED", "LOCAL_ONLY", "")
	f.row(t, "mysql-bin.000002", 0, "SEALED", "LOCAL_ONLY", "")
	f.row(t, "mysql-bin.000003", 0, "OPEN", "LOCAL_ONLY", "")
	for k, row := range f.catalog.rows {
		if info, err := os.Stat(row.FilePath); err == nil {
			row.SizeBytes = info.Size()
			f.catalog.rows[k] = row
		}
	}
	return f.catalog
}

func issue462Twice(t *testing.T, e *Evaluator, id string) health.TaskHealth {
	t.Helper()
	var h health.TaskHealth
	var err error
	for i := 0; i < 2; i++ {
		h, err = e.EvaluateNow(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func issue462Missing(open map[health.Key]health.Finding, id string) (health.Finding, bool) {
	f, ok := open[health.Key{TaskID: id, Kind: health.SegmentMissing, Subject: "mysql-bin.000002"}]
	return f, ok
}

func TestIssue462ClusterMissingSealedOpensAndRestores(t *testing.T) {
	env := newP262(t)
	data := t.TempDir()
	dir := filepath.Join(data, env.task.ID)
	catalog := issue462Sealed(t, env.task.ID, dir)
	if err := os.Remove(filepath.Join(dir, "mysql-bin.000002")); err != nil {
		t.Fatal(err)
	}
	e := issue462Holder(t, env, data, catalog, "n1")
	h := issue462Twice(t, e, env.task.ID)
	_, open, _ := env.hs.snapshot(env.task.ID)
	got, ok := issue462Missing(open, env.task.ID)
	if !ok || got.Severity != health.Critical {
		t.Fatalf("open %+v health %s, want CRITICAL SEGMENT_MISSING/mysql-bin.000002", open, h.Status)
	}
	if h.Status != health.StatusUnhealthy {
		t.Fatalf("health %s, want UNHEALTHY", h.Status)
	}
	owner, err := env.hs.ChainOwner(context.Background(), env.task.ID)
	if err != nil || owner != "n1" {
		t.Fatalf("chain owner %q (%v), want n1", owner, err)
	}

	lfSegment(t, filepath.Join(dir, "mysql-bin.000002"), fbA+":1-3", fbEvents(fbA, 4, 5)...)
	if _, err := e.EvaluateNow(context.Background(), env.task.ID); err != nil {
		t.Fatal(err)
	}
	_, open, _ = env.hs.snapshot(env.task.ID)
	if _, ok := issue462Missing(open, env.task.ID); ok {
		t.Fatalf("still open after restore: %+v", open)
	}
	if reason, ok := env.log.closeReason(health.SegmentMissing); !ok || reason != health.ReasonEval {
		t.Fatalf("close reason %q ok=%v, want EVAL", reason, ok)
	}
}

func TestIssue462OwnerChangedWhenPeerHasTheSegment(t *testing.T) {
	env := newP262(t)
	data1 := t.TempDir()
	dir1 := filepath.Join(data1, env.task.ID)
	catalog := issue462Sealed(t, env.task.ID, dir1)
	if err := os.Remove(filepath.Join(dir1, "mysql-bin.000002")); err != nil {
		t.Fatal(err)
	}
	e1 := issue462Holder(t, env, data1, catalog, "n1")
	issue462Twice(t, e1, env.task.ID)
	if _, open, _ := env.hs.snapshot(env.task.ID); !func() bool { _, ok := issue462Missing(open, env.task.ID); return ok }() {
		t.Fatal("SEGMENT_MISSING did not open on the owner")
	}

	data2 := t.TempDir()
	dir2 := filepath.Join(data2, env.task.ID)
	if err := os.CopyFS(dir2, os.DirFS(dir1)); err != nil {
		t.Fatal(err)
	}
	lfSegment(t, filepath.Join(dir2, "mysql-bin.000002"), fbA+":1-3", fbEvents(fbA, 4, 5)...)
	env.own("n2", 2, StateRunning, TaskDesiredRun)
	e2 := issue462Holder(t, env, data2, catalog, "n2")
	if _, err := e2.EvaluateNow(context.Background(), env.task.ID); err != nil {
		t.Fatal(err)
	}
	_, open, _ := env.hs.snapshot(env.task.ID)
	if _, ok := issue462Missing(open, env.task.ID); ok {
		t.Fatalf("missing finding still open on the peer directory: %+v", open)
	}
	if reason, ok := env.log.closeReason(health.SegmentMissing); !ok || reason != health.ReasonOwnerChanged {
		t.Fatalf("close reason %q ok=%v, want OWNER_CHANGED", reason, ok)
	}
}

func TestIssue462StandaloneMissingSealedUnchanged(t *testing.T) {
	f := sizedDir(t)
	if err := os.Remove(filepath.Join(f.dir, "mysql-bin.000002")); err != nil {
		t.Fatal(err)
	}
	base := healthstore.NewFileStore(filepath.Join(t.TempDir(), "h"))
	rec := &issue462Reasons{Store: base}
	e := newEval(t, f.s, rec, EvaluatorOn)
	sweepTwice(t, e)
	if _, ok := p251Open(t, base, f.task.ID)[string(health.SegmentMissing)+"/mysql-bin.000002"]; !ok {
		t.Fatalf("standalone SEGMENT_MISSING not open: %v", p251Open(t, base, f.task.ID))
	}
	lfSegment(t, filepath.Join(f.dir, "mysql-bin.000002"), fbA+":1-3", fbEvents(fbA, 4, 5)...)
	e.MarkDirty(f.task.ID, ReasonCatalog)
	sweepTwice(t, e)
	if open := p251Open(t, base, f.task.ID); len(open) != 0 {
		t.Fatalf("standalone still open after restore: %v", open)
	}
	for _, reason := range rec.reasons {
		if reason == health.ReasonOwnerChanged {
			t.Fatalf("standalone closed with %s", reason)
		}
	}
}

// issue462Reasons records close reasons in front of a health store.
type issue462Reasons struct {
	healthstore.Store
	reasons []string
}

func (r *issue462Reasons) ApplyTransitions(ctx context.Context, taskID string, f health.Fence, ts []health.Transition, h health.TaskHealth) (healthstore.Applied, error) {
	for _, tr := range ts {
		if tr.Op == health.OpClose {
			r.reasons = append(r.reasons, tr.Reason)
		}
	}
	return r.Store.ApplyTransitions(ctx, taskID, f, ts, h)
}

func TestIssue462PeerStartSurfacesUnreachableTail(t *testing.T) {
	env := newP262(t)
	data1 := t.TempDir()
	dir1 := filepath.Join(data1, env.task.ID)
	p262WriteChain(t, dir1, true)
	_, e1 := env.node("n1", data1, nil)
	if _, err := e1.EvaluateNow(context.Background(), env.task.ID); err != nil {
		t.Fatal(err)
	}
	if owner, err := env.hs.ChainOwner(context.Background(), env.task.ID); err != nil || owner != "n1" {
		t.Fatalf("chain owner %q (%v), want n1", owner, err)
	}
	env.release("n1", 1)

	data2 := t.TempDir()
	r := &matrixRunner{started: make(chan struct{}, 4), fail: NewPermanentError(CodeSegmentNotOnWorker, "/data/n1/1/mysql-bin.000003.open.e1 is not on this worker")}
	s2 := NewScheduler(
		WithDataDir(data2),
		WithStore(env.store),
		WithClusterLeaseManager(env.leases),
		WithClusterWorkerID("n2"),
		WithFindingStore(env.hs),
		WithRunner(r),
	)
	if _, err := s2.GetTask(env.task.ID); err != nil {
		t.Fatal(err)
	}
	NewEvaluator(s2, EvaluatorConfig{
		Mode: EvaluatorOn, Store: env.log, Cluster: true, NodeID: "n2", Instance: "n2-i",
		Sweep: time.Minute, Debounce: time.Millisecond,
	})
	startAndWait(t, s2, r, env.task.ID)
	waitRunClosed(t, s2, env.task.ID, "peer start")
	row, err := env.store.GetTask(context.Background(), env.task.ID)
	if err != nil || row.State != StateFailed {
		t.Fatalf("stored state %s (%v), want FAILED", row.State, err)
	}
	if row.DesiredRun != TaskDesiredStop {
		t.Fatalf("desired %s, want STOP", row.DesiredRun)
	}

	_, open, _ := env.hs.snapshot(env.task.ID)
	got, ok := open[health.Key{TaskID: env.task.ID, Kind: health.TaskFailed, Subject: "SEGMENT_NOT_ON_WORKER"}]
	if !ok || got.Severity != health.Critical {
		t.Fatalf("open %+v, want CRITICAL TASK_FAILED/SEGMENT_NOT_ON_WORKER without a sweep", open)
	}
	if owner, err := env.hs.ChainOwner(context.Background(), env.task.ID); err != nil || owner != "n1" {
		t.Fatalf("chain owner %q (%v), want the last directory owner n1", owner, err)
	}
	if open[p262LeftoverKey(env.task.ID)].Kind != health.SegmentEmptyLeftover {
		t.Fatalf("old directory finding %+v, want it still open", open[p262LeftoverKey(env.task.ID)])
	}
	if reason, ok := env.log.closeReason(health.SegmentEmptyLeftover); ok {
		t.Fatalf("leftover closed with %s; a failed start must not take the chain", reason)
	}
	summary := s2.HealthSummary(context.Background())
	if summary.Status != HealthDegraded {
		t.Fatalf("health %+v, want degraded", summary)
	}
}
