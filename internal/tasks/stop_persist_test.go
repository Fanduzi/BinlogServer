// Package tasks provides module-level functionality for tasks.
// input: a running task whose Stop hits a metadata store that rejects writes, then recovers
// output: proof that the running dump and the task state never disagree: no RUNNING row without a dump, and Start works after recovery
// pos: regression test for the #336 QA P1 (zombie RUNNING after Stop on a read-only meta)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type gatedStore struct {
	*fakeStore
	mu  sync.Mutex
	err error
}

func (g *gatedStore) setErr(err error) {
	g.mu.Lock()
	g.err = err
	g.mu.Unlock()
}

func (g *gatedStore) UpsertTask(ctx context.Context, task Task) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return g.err
	}
	return g.fakeStore.UpsertTask(ctx, task)
}

func (g *gatedStore) GetTask(ctx context.Context, id string) (Task, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fakeStore.GetTask(ctx, id)
}

func (g *gatedStore) ListTasks(ctx context.Context) ([]Task, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fakeStore.ListTasks(ctx)
}

func (g *gatedStore) row(id string) Task {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fakeStore.tasks[id]
}

type liveRunner struct {
	live    atomic.Int32
	started chan struct{}
}

func (r *liveRunner) Run(ctx context.Context, _ Task) error {
	r.live.Add(1)
	defer r.live.Add(-1)
	select {
	case r.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return context.Canceled
}

func TestStopOnReadOnlyMetaNeverLeavesZombieRunning(t *testing.T) {
	store := &gatedStore{fakeStore: newFakeStore()}
	runner := &liveRunner{started: make(chan struct{}, 4)}
	s := NewScheduler(WithStore(store), WithRunner(runner))
	task, err := s.CreateTask("zombie", "zombie-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatal(err)
	}
	if err := s.StartTask(task.ID); err != nil {
		t.Fatal(err)
	}
	<-runner.started
	waitFor(t, func() bool { got, _ := s.GetTask(task.ID); return got.State == StateRunning })

	store.setErr(errors.New("Error 1290: --super-read-only"))
	stopErr := s.StopTask(task.ID)
	time.Sleep(100 * time.Millisecond)

	// Invariant while the store rejects writes: a RUNNING task has a live dump.
	got, _ := s.GetTask(task.ID)
	if got.State == StateRunning && runner.live.Load() == 0 {
		t.Fatalf("zombie: task RUNNING, no dump (stop err=%v)", stopErr)
	}

	store.setErr(nil)
	s.republishUnpublished()
	if stopErr != nil {
		// The Stop was refused, so the dump must still run and the operator can retry it.
		if runner.live.Load() == 0 {
			t.Fatal("Stop returned an error but stopped the dump")
		}
		if err := s.StopTask(task.ID); err != nil {
			t.Fatalf("retry Stop after recovery: %v", err)
		}
	}
	waitFor(t, func() bool {
		got, _ := s.GetTask(task.ID)
		return got.State == StateStopped && store.row(task.ID).State == StateStopped && runner.live.Load() == 0
	})
	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("Start after recovery: %v", err)
	}
	<-runner.started
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in 3s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type stoppedRejectStore struct{ *gatedStore }

func (g stoppedRejectStore) UpsertTask(ctx context.Context, task Task) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil && task.State == StateStopped {
		return g.err
	}
	return g.fakeStore.UpsertTask(ctx, task)
}

// The dump stopped but the STOPPED row was refused: a store read in between
// must not copy the old row back over memory, and the retry must land STOPPED.
func TestStoppedWriteRefusedIsNotUndoneByStoreReads(t *testing.T) {
	inner := &gatedStore{fakeStore: newFakeStore()}
	store := stoppedRejectStore{inner}
	runner := &liveRunner{started: make(chan struct{}, 4)}
	s := NewScheduler(WithStore(store), WithRunner(runner))
	task, _ := s.CreateTask("z2", "z2-key")
	_ = s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"})
	if err := s.StartTask(task.ID); err != nil {
		t.Fatal(err)
	}
	<-runner.started
	inner.setErr(errors.New("Error 1290"))
	if err := s.StopTask(task.ID); err != nil {
		t.Fatalf("STOPPING write was allowed, Stop must succeed: %v", err)
	}
	waitFor(t, func() bool { return runner.live.Load() == 0 })
	time.Sleep(50 * time.Millisecond)
	_ = s.syncTasksFromStore()
	got, _ := s.GetTask(task.ID)
	if got.State == StateRunning || got.State == StateStarting {
		t.Fatalf("store read undid the stop: %s with no dump", got.State)
	}
	inner.setErr(nil)
	s.republishUnpublished()
	waitFor(t, func() bool { return inner.row(task.ID).State == StateStopped })
	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("Start after recovery: %v", err)
	}
	<-runner.started
}

type absentFindingStore struct{ *memFindingStore }

func (*absentFindingStore) ListOpenFindings(context.Context, int) ([]Finding, error) {
	return nil, ErrFindingStoreAbsent
}

// #336 QA P3: on schema 6 the store is inactive from startup, not after the first failed write.
func TestFindingStoreInactiveFromStartupOnSchema6(t *testing.T) {
	s := NewScheduler(WithFindingStore(&absentFindingStore{newMemFindingStore()}))
	if err := s.LoadOpenFindings(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.FindingStats().StoreActive {
		t.Fatal("finding_store_active is 1 on schema 6 after startup")
	}
}

// S15 reaches every list path (API pages too), not only ListTasks.
func TestDiskListFailureFromAnyListPathIsFinding(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(WithDataDir(dir + "/missing-parent/x\x00"))
	_, _ = s.listMemoryAndDiskTasks()
	if _, ok := openFinding(s, ClusterFindingTask, FindingDiskListFailed); !ok {
		t.Fatalf("no DISK_LIST_FAILED from listMemoryAndDiskTasks: %+v", s.FindingStats().Open)
	}
}
