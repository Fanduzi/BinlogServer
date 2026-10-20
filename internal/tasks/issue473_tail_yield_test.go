// Package tasks provides module-level functionality for tasks.
// input: a sealed segment whose bytes no longer match .seal-sha256 while size and mtime stay put, a new owner whose chain lock is already held, a peer start that returns permanent SEGMENT_NOT_ON_WORKER while chain_owner is another node, a yielded row whose chain owner heartbeat is older than node_gone_after, and a holder write followed by a proxy write
// output: acceptance for ADR 0009 §31.6: an unhashed SEGMENT_UNREADABLE stays open when chain facts return before AwaitRehash is filled; a peer yields instead of FAILED and desired STOP; the yielded row becomes FAILED with desired RUN only after the chain owner is gone; task_health.owner follows the task row
// pos: regression for #462 claim (c) and #473 (design 0009 §31.6)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/health"
)

// expiredAll lists every stored task. ClaimExpiredTasks is what decides
// which of them to start; the lease clock lives in MemoryLease.
type expiredAll struct{ *fakeStore }

func (e *expiredAll) ListTasksWithExpiredLease(ctx context.Context) ([]Task, error) {
	return e.ListTasks(ctx)
}

func issue473FlipChecksum(t *testing.T, path string) {
	t.Helper()
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(orig) < 4 {
		t.Fatalf("%s is %d bytes", path, len(orig))
	}
	raw := append([]byte(nil), orig...)
	for i := len(raw) - 4; i < len(raw); i++ {
		raw[i] ^= 0xff
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
}

func TestIssue462UnreadableSurvivesEarlyChainReturn(t *testing.T) {
	env := newP262(t)
	data1 := t.TempDir()
	dir1 := filepath.Join(data1, env.task.ID)
	p262WriteChain(t, dir1, false)
	const name = "mysql-bin.000002"
	target := filepath.Join(dir1, name)
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := binlog.RecordSealSum(dir1, name, info.Size(), fileSHA(t, target)); err != nil {
		t.Fatal(err)
	}
	issue473FlipChecksum(t, target)

	s1, e1 := env.node("n1", data1, nil)
	r := NewRehasher(e1, 64<<20)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		if _, err := r.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e1.EvaluateNow(ctx, env.task.ID); err != nil {
		t.Fatal(err)
	}
	key := health.Key{TaskID: env.task.ID, Kind: health.SegmentUnreadable, Subject: name}
	if _, open, _ := env.hs.snapshot(env.task.ID); open[key].Kind != health.SegmentUnreadable {
		t.Fatalf("open %+v, want SEGMENT_UNREADABLE/%s before takeover", open, name)
	}
	if owner, err := env.hs.ChainOwner(ctx, env.task.ID); err != nil || owner != "n1" {
		t.Fatalf("chain owner %q (%v), want n1", owner, err)
	}
	_ = s1

	data2 := t.TempDir()
	dir2 := filepath.Join(data2, env.task.ID)
	if err := os.CopyFS(dir2, os.DirFS(dir1)); err != nil {
		t.Fatal(err)
	}
	env.own("n2", 2, StateRunning, TaskDesiredRun)
	s2 := NewScheduler(
		WithDataDir(data2),
		WithStore(env.store),
		WithClusterLeaseManager(env.leases),
		WithClusterWorkerID("n2"),
	)
	s2.mu.Lock()
	s2.tasks[env.task.ID] = env.task
	s2.mu.Unlock()
	e2 := NewEvaluator(s2, EvaluatorConfig{
		Mode: EvaluatorOn, Store: env.log, Cluster: true, NodeID: "n2", Instance: "n2-i",
		Sweep: time.Minute, Debounce: time.Millisecond, ChainLockWait: 30 * time.Millisecond,
	})
	release := s2.HoldChain(env.task.ID)
	if _, err := e2.EvaluateNow(ctx, env.task.ID); err != nil {
		release()
		t.Fatal(err)
	}
	release()
	if _, open, _ := env.hs.snapshot(env.task.ID); open[key].Kind != health.SegmentUnreadable {
		t.Fatalf("open %+v after the chain lock was missed, want SEGMENT_UNREADABLE still open", open)
	}
	if reason, ok := env.log.closeReason(health.SegmentUnreadable); ok {
		t.Fatalf("closed with %s while chain facts returned before the segment was judged", reason)
	}

	if _, err := e2.EvaluateNow(ctx, env.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, open, _ := env.hs.snapshot(env.task.ID); open[key].Kind != health.SegmentUnreadable {
		t.Fatalf("open %+v before rehash on the new owner, want SEGMENT_UNREADABLE still open", open)
	}
	if reason, ok := env.log.closeReason(health.SegmentUnreadable); ok && reason == health.ReasonEval {
		t.Fatalf("EVAL close before the new owner rehashed %s", name)
	}
}

func TestIssue473PeerYieldsUnreachableTail(t *testing.T) {
	env := newP262(t)
	listed := &expiredAll{fakeStore: env.store}
	data1 := t.TempDir()
	p262WriteChain(t, filepath.Join(data1, env.task.ID), true)
	_, e1 := env.node("n1", data1, listed)
	ctx := context.Background()
	if _, err := e1.EvaluateNow(ctx, env.task.ID); err != nil {
		t.Fatal(err)
	}
	env.release("n1", 1)

	data2 := t.TempDir()
	r := &matrixRunner{started: make(chan struct{}, 4), fail: NewPermanentError(CodeSegmentNotOnWorker, "/data/n1/1/mysql-bin.000003.open.e1 is not on this worker")}
	s2 := NewScheduler(
		WithDataDir(data2),
		WithStore(listed),
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
	row, err := env.store.GetTask(ctx, env.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State == StateFailed || row.DesiredRun != TaskDesiredRun {
		t.Fatalf("state %s desired %s, want a yield that keeps desired RUN and does not write FAILED", row.State, row.DesiredRun)
	}
	if row.OwnerWorkerID != "" || row.Epoch != 0 {
		t.Fatalf("row owner %q epoch %d, want unowned", row.OwnerWorkerID, row.Epoch)
	}
	_, open, _ := env.hs.snapshot(env.task.ID)
	got, ok := open[health.Key{TaskID: env.task.ID, Kind: health.TaskFailed, Subject: "SEGMENT_NOT_ON_WORKER"}]
	if !ok || got.Severity != health.Critical {
		t.Fatalf("open %+v, want CRITICAL TASK_FAILED/SEGMENT_NOT_ON_WORKER without a sweep", open)
	}
	if owner, err := env.hs.ChainOwner(ctx, env.task.ID); err != nil || owner != "n1" {
		t.Fatalf("chain owner %q (%v), want n1", owner, err)
	}
	claimed, err := s2.ClaimExpiredTasks()
	if err != nil || claimed != 0 {
		t.Fatalf("claimed %d err %v, want 0 while this node is backing off", claimed, err)
	}
}

func TestIssue473NodeGoneFailsYieldedTail(t *testing.T) {
	env := newP262(t)
	data1 := t.TempDir()
	p262WriteChain(t, filepath.Join(data1, env.task.ID), false)
	_, e1 := env.node("n1", data1, nil)
	ctx := context.Background()
	if _, err := e1.EvaluateNow(ctx, env.task.ID); err != nil {
		t.Fatal(err)
	}
	env.task.State = StateLeaseDegraded
	env.task.DesiredRun = TaskDesiredRun
	env.task.OwnerWorkerID = ""
	env.task.Epoch = 0
	env.task.LastError = "SEGMENT_NOT_ON_WORKER: /data/n1/1/mysql-bin.000003.open.e1 is not on this worker"
	env.task.UpdatedAt = time.Now()
	if err := env.store.UpsertTask(ctx, env.task); err != nil {
		t.Fatal(err)
	}
	env.leases.mu.Lock()
	env.leases.rows[env.task.ID] = memoryLeaseRow{owner: "", epoch: 2, expire: time.Now().Add(-time.Second)}
	env.leases.mu.Unlock()
	hb := &hbStore{fakeStore: env.store, beats: []WorkerHeartbeat{{
		WorkerID: "n1", LastSeenAt: time.Now().Add(-time.Hour), Status: "up",
	}}}
	_, e2 := env.node("n2", t.TempDir(), hb)
	if err := e2.SweepNow(ctx); err != nil {
		t.Fatal(err)
	}
	row, err := env.store.GetTask(ctx, env.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != StateFailed || row.DesiredRun != TaskDesiredRun {
		t.Fatalf("state %s desired %s, want FAILED with desired RUN after the chain owner is gone", row.State, row.DesiredRun)
	}
}

func TestIssue473HealthOwnerFollowsRow(t *testing.T) {
	env := newP262(t)
	data1 := t.TempDir()
	p262WriteChain(t, filepath.Join(data1, env.task.ID), false)
	_, e1 := env.node("n1", data1, nil)
	ctx := context.Background()
	if _, err := e1.EvaluateNow(ctx, env.task.ID); err != nil {
		t.Fatal(err)
	}
	owner, epoch := env.hs.rowOwnership(env.task.ID)
	if owner != "n1" || epoch != 1 {
		t.Fatalf("health owner %q epoch %d, want n1/1", owner, epoch)
	}
	env.release("n1", 1)
	_, e2 := env.node("n2", t.TempDir(), nil)
	if err := e2.SweepNow(ctx); err != nil {
		t.Fatal(err)
	}
	owner, epoch = env.hs.rowOwnership(env.task.ID)
	if owner != "" || epoch != 0 {
		t.Fatalf("health owner %q epoch %d after the row has no owner, want empty/0", owner, epoch)
	}
}
