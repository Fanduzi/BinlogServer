// Package tasks provides module-level functionality for tasks.
// input: desired_run, observed state, spec revisions, lease hold, and a local dump
// output: converge decisions for the desired/observed matrix, a spec bump that restarts one dump, an idempotent second pass, a legacy desired_run reconcile that keeps an active task running, and SetRunner binding the stored source for a dump-thread KILL
// pos: control-loop and upgrade-reconcile tests
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"testing"
	"time"
)

func TestDecideConvergeMatrix(t *testing.T) {
	cases := []struct {
		name string
		in   convergeInput
		want convergeAction
	}{
		{
			name: "running on the current spec",
			in: convergeInput{
				Desired: TaskDesiredRun, State: StateRunning, SpecRevision: 3,
				HasLocalRun: true, LocalAppliedSpec: 3, LeaseHeld: true,
			},
			want: convergeNone,
		},
		{
			name: "same observation is idempotent",
			in: convergeInput{
				Desired: TaskDesiredRun, State: StateRunning, SpecRevision: 3,
				HasLocalRun: true, LocalAppliedSpec: 3, LeaseHeld: true,
			},
			want: convergeNone,
		},
		{
			name: "spec bump restarts the open dump",
			in: convergeInput{
				Desired: TaskDesiredRun, State: StateRunning, SpecRevision: 4,
				HasLocalRun: true, LocalAppliedSpec: 3, LeaseHeld: true,
			},
			want: convergeRestart,
		},
		{
			name: "applied catches up",
			in: convergeInput{
				Desired: TaskDesiredRun, State: StateRunning, SpecRevision: 4,
				HasLocalRun: true, LocalAppliedSpec: 4, LeaseHeld: true,
			},
			want: convergeNone,
		},
		{
			name: "failed at this spec does not start",
			in: convergeInput{
				Desired: TaskDesiredRun, State: StateFailed,
				SpecRevision: 5, FailedSpecRevision: 5, LeaseMissingOrExpired: true,
			},
			want: convergeNone,
		},
		{
			name: "failed at an older spec starts",
			in: convergeInput{
				Desired: TaskDesiredRun, State: StateFailed,
				SpecRevision: 6, FailedSpecRevision: 5, LeaseMissingOrExpired: true,
			},
			want: convergeAcquireStart,
		},
		{
			name: "stop while another worker holds the lease",
			in: convergeInput{
				Desired: TaskDesiredStop, State: StateRunning,
				LeaseHeldByOther: true,
			},
			want: convergeNone,
		},
		{
			name: "stop with a local dump",
			in: convergeInput{
				Desired: TaskDesiredStop, State: StateRunning, HasLocalRun: true, LeaseHeld: true,
			},
			want: convergeStopLocal,
		},
		{
			name: "stop with no local dump finishes",
			in: convergeInput{
				Desired: TaskDesiredStop, State: StateStopping, LeaseHeld: true,
			},
			want: convergeFinishStop,
		},
		{
			name: "already stopped",
			in: convergeInput{
				Desired: TaskDesiredStop, State: StateStopped,
			},
			want: convergeNone,
		},
		{
			name: "upgrade hazard stops a running row whose desired is STOP",
			in: convergeInput{
				Desired: TaskDesiredStop, State: StateRunning, LeaseHeld: true,
			},
			want: convergeFinishStop,
		},
		{
			name: "reconciled running row starts on the held lease",
			in: convergeInput{
				Desired: TaskDesiredRun, State: StateRunning, LeaseHeld: true,
			},
			want: convergeStartHeld,
		},
		{
			name: "run desired with no lease acquires",
			in: convergeInput{
				Desired: TaskDesiredRun, State: StateCreated, LeaseMissingOrExpired: true,
			},
			want: convergeAcquireStart,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideConverge(tc.in); got != tc.want {
				t.Fatalf("decideConverge=%d, want %d", got, tc.want)
			}
			if again := decideConverge(tc.in); again != tc.want {
				t.Fatalf("second decideConverge=%d, want %d", again, tc.want)
			}
		})
	}
}

func TestLegacyDesiredFromState(t *testing.T) {
	active := []State{StateRunning, StateStarting, StateRetryBackoff, StateLeaseDegraded, StateRebuildingFile}
	for _, state := range active {
		if got := legacyDesiredFromState(state); got != TaskDesiredRun {
			t.Fatalf("state %s desired=%s, want RUN", state, got)
		}
	}
	for _, state := range []State{StateCreated, StateStopping, StateStopped, StateFailed} {
		if got := legacyDesiredFromState(state); got != TaskDesiredStop {
			t.Fatalf("state %s desired=%s, want STOP", state, got)
		}
	}
	running := Task{State: StateRunning}
	if got := effectiveDesired(running); got != TaskDesiredRun {
		t.Fatalf("empty desired on RUNNING = %s", got)
	}
	legacyStop := Task{State: StateRunning, DesiredRun: TaskDesiredStop}
	if got := effectiveDesired(legacyStop); got != TaskDesiredRun {
		t.Fatalf("spec 0 RUNNING with desired STOP = %s, want RUN", got)
	}
	legacyRun := Task{State: StateStopped, DesiredRun: TaskDesiredRun}
	if got := effectiveDesired(legacyRun); got != TaskDesiredStop {
		t.Fatalf("spec 0 STOPPED with desired RUN = %s, want STOP", got)
	}
	operatorStop := Task{State: StateRunning, DesiredRun: TaskDesiredStop, SpecRevision: 2, AppliedSpecRevision: 1}
	if got := effectiveDesired(operatorStop); got != TaskDesiredStop {
		t.Fatalf("operator STOP = %s", got)
	}
}

type legacyReconcileStore struct {
	schedulerTestStore
}

func (s *legacyReconcileStore) ReconcileLegacyDesiredRun(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for id, task := range s.tasks {
		if task.SpecRevision != 0 || task.AppliedSpecRevision != 0 {
			continue
		}
		want := legacyDesiredFromState(task.State)
		if task.DesiredRun == want {
			continue
		}
		task.DesiredRun = want
		s.tasks[id] = task
		n++
	}
	return n, nil
}

func TestReconcileLegacyDesiredKeepsRunningTask(t *testing.T) {
	store := &legacyReconcileStore{schedulerTestStore: schedulerTestStore{tasks: map[string]Task{}}}
	runner := &fakeRunner{started: make(chan Task, 1)}
	s := NewScheduler(WithStore(store), WithRunner(runner))
	source := SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: "mysql"}
	created, err := s.CreateTaskFromSpec("legacy", "legacy-key", &source, nil, nil)
	if err != nil {
		t.Fatalf("CreateTaskFromSpec: %v", err)
	}
	s.mu.Lock()
	row := s.tasks[created.ID]
	row.State = StateRunning
	row.DesiredRun = TaskDesiredStop
	row.SpecRevision = 0
	row.AppliedSpecRevision = 0
	s.tasks[created.ID] = row
	s.mu.Unlock()
	if err := store.UpsertTask(context.Background(), row); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}

	n, err := s.ReconcileLegacyDesiredRun(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 1 {
		t.Fatalf("reconciled %d rows, want 1", n)
	}
	got, err := store.GetTask(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.DesiredRun != TaskDesiredRun || got.SpecRevision != 0 || got.State != StateRunning {
		t.Fatalf("after reconcile desired=%s spec=%d state=%s", got.DesiredRun, got.SpecRevision, got.State)
	}
	again, err := s.ReconcileLegacyDesiredRun(context.Background())
	if err != nil || again != 0 {
		t.Fatalf("second reconcile n=%d err=%v", again, err)
	}
	if err := s.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	claimed, err := s.ClaimRunnableTasks()
	if err != nil {
		t.Fatalf("ClaimRunnableTasks: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("claimed=%d, want 1", claimed)
	}
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("reconciled RUNNING task did not start")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err = s.GetTask(created.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got.State == StateStopped {
			t.Fatal("upgrade reconcile stopped a RUNNING task")
		}
		if got.State == StateRunning && got.DesiredRun == TaskDesiredRun {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state=%s desired=%s", got.State, got.DesiredRun)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReconcileSkipsOperatorStop(t *testing.T) {
	store := &legacyReconcileStore{schedulerTestStore: schedulerTestStore{tasks: map[string]Task{}}}
	s := NewScheduler(WithStore(store))
	source := SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: "mysql"}
	created, err := s.CreateTaskFromSpec("stopped-on-purpose", "stopped-key", &source, nil, nil)
	if err != nil {
		t.Fatalf("CreateTaskFromSpec: %v", err)
	}
	row, err := store.GetTask(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	row.State = StateRunning
	row.DesiredRun = TaskDesiredStop
	row.SpecRevision = 2
	row.AppliedSpecRevision = 1
	if err := store.UpsertTask(context.Background(), row); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	n, err := s.ReconcileLegacyDesiredRun(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("reconcile n=%d err=%v", n, err)
	}
	got, err := store.GetTask(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.DesiredRun != TaskDesiredStop || got.SpecRevision != 2 {
		t.Fatalf("operator stop rewritten: desired=%s spec=%d", got.DesiredRun, got.SpecRevision)
	}
}

type holdRunner struct {
	started  chan Task
	release  chan struct{}
	returned chan struct{}
}

func (r *holdRunner) Run(ctx context.Context, task Task) error {
	select {
	case r.started <- task:
	default:
	}
	<-ctx.Done()
	<-r.release
	close(r.returned)
	return context.Canceled
}

func TestStoppedIsWrittenAfterRunReturns(t *testing.T) {
	runner := &holdRunner{
		started:  make(chan Task, 1),
		release:  make(chan struct{}),
		returned: make(chan struct{}),
	}
	s := NewScheduler(WithRunner(runner))
	source := SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: "mysql"}
	task, err := s.CreateTaskFromSpec("close-first", "close-first", &source, nil, nil)
	if err != nil {
		t.Fatalf("CreateTaskFromSpec: %v", err)
	}
	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not start")
	}
	if err := s.StopTask(task.ID); err != nil {
		t.Fatalf("StopTask: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		got, err := s.GetTask(task.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got.State == StateStopped {
			t.Fatal("STOPPED before Run returned")
		}
		if got.State == StateStopping {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state=%s, want STOPPING while Run is blocked", got.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(runner.release)
	select {
	case <-runner.returned:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		got, err := s.GetTask(task.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got.State == StateStopped && got.OwnerWorkerID == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("state=%s owner=%q, want STOPPED and empty owner", got.State, got.OwnerWorkerID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSpecBumpRestartsOneDump(t *testing.T) {
	runner := &heldConfigRunner{}
	s := NewScheduler(WithRunner(runner))
	source := SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "old-secret", Flavor: "mysql"}
	task, err := s.CreateTaskFromSpec("spec", "spec-key", &source, nil, nil)
	if err != nil {
		t.Fatalf("CreateTaskFromSpec: %v", err)
	}
	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	first := runner.waitCall(t, 1)
	if first.Source.Password != "old-secret" {
		t.Fatalf("first password=%q", first.Source.Password)
	}
	s.mu.Lock()
	epoch := s.tasks[task.ID].Epoch
	s.mu.Unlock()
	next := SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "new-secret", Flavor: "mysql"}
	updated, err := s.UpdateTask(task.ID, TaskPatch{ClusterKey: "spec-key", Source: &next})
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if updated.SpecRevision <= first.SpecRevision {
		t.Fatalf("spec=%d, opened=%d", updated.SpecRevision, first.SpecRevision)
	}
	second := runner.waitCall(t, 2)
	if second.Source.Password != "new-secret" {
		t.Fatalf("second password=%q", second.Source.Password)
	}
	if _, err := s.convergeTask(task.ID); err != nil {
		t.Fatalf("converge: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	runner.mu.Lock()
	calls := len(runner.calls)
	runner.mu.Unlock()
	if calls != 2 {
		t.Fatalf("calls=%d, want 2 after an idempotent converge", calls)
	}
	s.mu.Lock()
	gotEpoch := s.tasks[task.ID].Epoch
	s.mu.Unlock()
	if gotEpoch != epoch {
		t.Fatalf("epoch changed %d -> %d", epoch, gotEpoch)
	}
}

type bindSourceRunner struct {
	lookup func(string) (SourceConfig, bool)
}

func (r *bindSourceRunner) Run(context.Context, Task) error { return nil }

func (r *bindSourceRunner) BindDumpSource(fn func(string) (SourceConfig, bool)) {
	r.lookup = fn
}

func TestSetRunnerBindsStoredDumpSource(t *testing.T) {
	s := NewScheduler()
	s.mu.Lock()
	s.tasks["9"] = Task{
		ID: "9",
		Source: SourceConfig{
			Host: "127.0.0.1", Port: 3306, User: "repl", Password: "from-memory", Flavor: "mysql",
		},
	}
	s.mu.Unlock()
	runner := &bindSourceRunner{}
	s.SetRunner(runner)
	if runner.lookup == nil {
		t.Fatal("dump source lookup was not bound")
	}
	got, ok := runner.lookup("9")
	if !ok || got.Password != "from-memory" {
		t.Fatalf("source password=%q ok=%v", got.Password, ok)
	}
}
