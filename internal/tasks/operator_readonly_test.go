// Package tasks provides module-level functionality for tasks.
// input: tasks in RUNNING, RETRY_BACKOFF and STOPPED whose operator action (Start, Stop, Delete, Update, Configure*, Create) hits a metadata store that refuses writes, then recovers; and tasks left in STARTING or STOPPING with nothing driving them
// output: proof that a refused write leaves memory, the store row and the dump untouched, that the action works after recovery, and that the stalled-transition watchdog reconciles and raises TASK_TRANSITION_STALLED
// pos: regression for the tip 53a9 dogfood P1 (Start in RETRY_BACKOFF on read-only meta left a STARTING zombie), the same class as #336
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

var errReadOnly = errors.New("Error 1290 (HY000): The MySQL server is running with the --super-read-only option")

type roStore struct{ *gatedStore }

func (g roStore) DeleteTask(ctx context.Context, id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return g.err
	}
	return g.fakeStore.DeleteTask(ctx, id)
}

// flakyRunner fails with SOURCE_UNREACHABLE until ok is set, then runs live.
type flakyRunner struct {
	ok    atomic.Bool
	calls atomic.Int32
	liveRunner
}

func (r *flakyRunner) Run(ctx context.Context, task Task) error {
	r.calls.Add(1)
	if !r.ok.Load() {
		return NewRetryableSourceError(CodeSourceUnreachable, "source down")
	}
	return r.liveRunner.Run(ctx, task)
}

type roFixture struct {
	t      *testing.T
	store  roStore
	runner *flakyRunner
	s      *Scheduler
	id     string
}

func newROFixture(t *testing.T, retry time.Duration) *roFixture {
	t.Helper()
	store := roStore{&gatedStore{fakeStore: newFakeStore()}}
	runner := &flakyRunner{liveRunner: liveRunner{started: make(chan struct{}, 8)}}
	runner.ok.Store(true)
	s := NewScheduler(WithStore(store), WithRunner(runner), WithRetryBackoff(retry, retry))
	task, err := s.CreateTask("ro", "ro-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatal(err)
	}
	return &roFixture{t: t, store: store, runner: runner, s: s, id: task.ID}
}

func (f *roFixture) mem() Task {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	return f.s.tasks[f.id]
}

func (f *roFixture) running() {
	f.t.Helper()
	if err := f.s.StartTask(f.id); err != nil {
		f.t.Fatal(err)
	}
	<-f.runner.started
	waitFor(f.t, func() bool { return f.mem().State == StateRunning })
}

func (f *roFixture) liveRun() bool {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	done, ok := f.s.runs[f.id]
	return ok && !isClosed(done)
}

// refused checks the invariant after a refused write: an error, memory and
// the store row as they were, and no TASK_PERSIST_FAILED left open for it.
func (f *roFixture) refused(op string, err error, before, row Task) {
	f.t.Helper()
	if err == nil {
		f.t.Fatalf("%s on read-only meta returned nil", op)
	}
	after := f.mem()
	if after.State != before.State || after.SpecRevision != before.SpecRevision || after.DesiredRun != before.DesiredRun ||
		after.Source != before.Source || after.Start != before.Start || after.Storage != before.Storage || after.Name != before.Name || after.ClusterKey != before.ClusterKey {
		f.t.Fatalf("%s refused but memory changed:\n before %+v\n after  %+v", op, before, after)
	}
	if got := f.store.row(f.id); got.State != row.State || got.SpecRevision != row.SpecRevision {
		f.t.Fatalf("%s refused but store row changed: %s/%d -> %s/%d", op, row.State, row.SpecRevision, got.State, got.SpecRevision)
	}
	if _, open := openFinding(f.s, f.id, FindingTaskPersistFailed); open {
		f.t.Fatalf("%s refused: TASK_PERSIST_FAILED left open for a row that is unchanged", op)
	}
}

// The tip 53a9 P1: Start while RETRY_BACKOFF on read-only meta.
func TestReadOnlyMeta_StartFromRetryBackoff(t *testing.T) {
	f := newROFixture(t, 300*time.Millisecond)
	f.runner.ok.Store(false)
	if err := f.s.StartTask(f.id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return f.mem().State == StateRetryBackoff })
	before, row := f.mem(), f.store.row(f.id)
	f.store.setErr(errReadOnly)
	err := f.s.StartTask(f.id)
	f.refused("Start", err, before, row)
	if !f.liveRun() {
		t.Fatal("refused Start cancelled the retry loop: nothing drives the task")
	}
	f.runner.ok.Store(true)
	f.store.setErr(nil)
	// The retry loop that was left alone takes it to RUNNING.
	waitFor(t, func() bool { return f.mem().State == StateRunning && f.runner.live.Load() == 1 })
}

func TestReadOnlyMeta_StartFromStopped(t *testing.T) {
	f := newROFixture(t, time.Hour)
	before, row := f.mem(), f.store.row(f.id)
	f.store.setErr(errReadOnly)
	f.refused("Start", f.s.StartTask(f.id), before, row)
	if f.liveRun() || f.runner.calls.Load() != 0 {
		t.Fatal("refused Start opened a dump")
	}
	f.store.setErr(nil)
	f.running()
}

func TestReadOnlyMeta_Stop(t *testing.T) {
	f := newROFixture(t, time.Hour)
	f.running()
	before, row := f.mem(), f.store.row(f.id)
	f.store.setErr(errReadOnly)
	f.refused("Stop", f.s.StopTask(f.id), before, row)
	if f.runner.live.Load() != 1 {
		t.Fatal("refused Stop stopped the dump")
	}
	f.store.setErr(nil)
	if err := f.s.StopTask(f.id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return f.mem().State == StateStopped && f.runner.live.Load() == 0 })
}

func TestReadOnlyMeta_Delete(t *testing.T) {
	f := newROFixture(t, time.Hour)
	f.running()
	before, row := f.mem(), f.store.row(f.id)
	f.store.setErr(errReadOnly)
	f.refused("Delete", f.s.DeleteTask(f.id), before, row)
	if f.runner.live.Load() != 1 || !f.liveRun() {
		t.Fatal("refused Delete stopped the dump")
	}
	f.store.setErr(nil)
	if err := f.s.DeleteTask(f.id); err != nil {
		t.Fatal(err)
	}
}

func TestReadOnlyMeta_UpdateAndConfigure(t *testing.T) {
	f := newROFixture(t, time.Hour)
	f.running()
	name := "renamed"
	ops := []struct {
		name string
		do   func() error
	}{
		{"Update", func() error {
			_, err := f.s.UpdateTask(f.id, TaskPatch{ClusterKey: "ro-key", Name: &name, Source: &SourceConfig{Host: "127.0.0.2", Port: 3306, User: "repl"}})
			return err
		}},
		{"ConfigureSource", func() error {
			return f.s.ConfigureSource(f.id, SourceConfig{Host: "127.0.0.3", Port: 3306, User: "repl"})
		}},
		{"ConfigureStart", func() error {
			return f.s.ConfigureStart(f.id, StartConfig{Mode: StartModeLatest})
		}},
		{"ConfigureStorage", func() error { return f.s.ConfigureStorage(f.id, Storage{RetentionDays: 3}) }},
		{"ConfigureName", func() error { return f.s.ConfigureName(f.id, "other") }},
		{"ConfigureClusterKey", func() error { return f.s.ConfigureClusterKey(f.id, "other-key") }},
	}
	for _, op := range ops {
		before, row := f.mem(), f.store.row(f.id)
		f.store.setErr(errReadOnly)
		f.refused(op.name, op.do(), before, row)
		f.store.setErr(nil)
		if f.runner.live.Load() != 1 {
			t.Fatalf("refused %s restarted or stopped the dump", op.name)
		}
	}
}

func TestReadOnlyMeta_Create(t *testing.T) {
	f := newROFixture(t, time.Hour)
	f.store.setErr(errReadOnly)
	if _, err := f.s.CreateTask("new", "new-key"); err == nil {
		t.Fatal("Create on read-only meta returned nil")
	}
	f.s.mu.Lock()
	n := len(f.s.tasks)
	f.s.mu.Unlock()
	if n != 1 {
		t.Fatalf("refused Create left %d tasks in memory", n)
	}
}

// Nothing may sit in STARTING or STOPPING without a run: the watchdog takes
// it on and raises a finding.
func TestStalledTransitionWatchdog(t *testing.T) {
	for _, state := range []State{StateStarting, StateStopping} {
		f := newROFixture(t, time.Hour)
		f.s.stalledAfter = 0
		f.s.mu.Lock()
		task := f.s.tasks[f.id]
		task.State = state
		task.DesiredRun = TaskDesiredRun
		if state == StateStopping {
			task.DesiredRun = TaskDesiredStop
		}
		task.SpecRevision++
		task.UpdatedAt = time.Now().Add(-time.Hour)
		f.s.tasks[f.id] = task
		_ = f.s.persistTaskLocked(task)
		f.s.mu.Unlock()

		f.s.reconcileStalledTransitions()
		if _, open := openFinding(f.s, f.id, FindingTransitionStalled); !open {
			t.Fatalf("%s with no run: no TASK_TRANSITION_STALLED", state)
		}
		want := StateRunning
		if state == StateStopping {
			want = StateStopped
		}
		waitFor(t, func() bool { return f.mem().State == want })
		f.s.reconcileStalledTransitions()
		if _, open := openFinding(f.s, f.id, FindingTransitionStalled); open {
			t.Fatalf("%s reconciled to %s but the finding is still open", state, want)
		}
	}
}
