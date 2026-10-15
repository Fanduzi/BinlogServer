// Package tasks provides module-level functionality for tasks.
// input: a scheduler on MemoryLease (the standalone door) with a runner that fails every run with a non-retryable error, and a lease Release the test can hold
// output: the regression test for the FAILED-then-GET race: a GET between a run writing FAILED and that run's exit keeps the task FAILED with its error, and the lease is released once (never a silent STOPPED)
// pos: scheduler run exit and the GET remote-stop check (scheduler_lifecycle.go noteRemoteStopLocked, runTask exit)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"sync"
	"testing"
)

// holdReleaseLease is MemoryLease whose first Release waits for the test.
type holdReleaseLease struct {
	*MemoryLease
	once     sync.Once
	entered  chan struct{}
	proceed  chan struct{}
	mu       sync.Mutex
	failures []int64 // epochs whose Release found no lease
}

func (l *holdReleaseLease) Release(ctx context.Context, taskID, workerID string, epoch int64) (bool, error) {
	l.once.Do(func() {
		close(l.entered)
		<-l.proceed
	})
	ok, err := l.MemoryLease.Release(ctx, taskID, workerID, epoch)
	if !ok {
		l.mu.Lock()
		l.failures = append(l.failures, epoch)
		l.mu.Unlock()
	}
	return ok, err
}

// epochRunner fails every run with a non-retryable error and records
// whether the lease was held at the run's epoch.
type epochRunner struct {
	lease *holdReleaseLease
	mu    sync.Mutex
	calls int
	held  []bool
}

func (r *epochRunner) Run(ctx context.Context, task Task) error {
	ok, _ := r.lease.MemoryLease.Verify(ctx, task.ID, task.OwnerWorkerID, task.Epoch)
	r.mu.Lock()
	r.calls++
	r.held = append(r.held, ok)
	r.mu.Unlock()
	return NewPermanentError(CodeSourceAccessDenied, "denied")
}

func (r *epochRunner) snapshot() (int, []bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, append([]bool(nil), r.held...)
}

// The source-switchover e2e race (#384 CI, 3 of 12): a GET that lands
// after a run wrote FAILED and before that run's exit ran read the stored
// FAILED row (desired STOP) as a remote Stop. It rewrote the in-memory
// FAILED as STOPPING, the run's exit wrote STOPPED and released the lease
// FAILED had already released ("lease release failed ... released=false").
// The task ended STOPPED with no error: a silent stop (#326).
func TestGetBetweenFailedAndRunExitKeepsFailed(t *testing.T) {
	lease := &holdReleaseLease{MemoryLease: NewMemoryLease(), entered: make(chan struct{}), proceed: make(chan struct{})}
	runner := &epochRunner{lease: lease}
	s := NewScheduler(WithStore(newFakeStore()), WithRunner(runner), WithClusterLeaseManager(lease), WithClusterWorkerID("standalone"))
	task, err := s.CreateTask("a", "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatal(err)
	}
	if err := s.StartTask(task.ID); err != nil {
		t.Fatal(err)
	}
	<-lease.entered // the run wrote FAILED and is releasing its lease; its exit has not run
	if got, err := s.GetTask(task.ID); err != nil || got.State != StateFailed {
		t.Fatalf("GET while the run exits: state %s err %v, want FAILED", got.State, err)
	}
	close(lease.proceed)
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, open := s.runs[task.ID]
		return !open
	})
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateFailed || got.LastError == "" {
		evs, _ := s.ListEvents(task.ID, 50)
		var types []string
		for _, e := range evs {
			types = append(types, e.Type)
		}
		t.Fatalf("after the run exited: state %s last_error %q (events %v), want FAILED with the error", got.State, got.LastError, types)
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if len(lease.failures) != 0 {
		t.Fatalf("lease release failed for epochs %v", lease.failures)
	}
	if calls, held := runner.snapshot(); calls != 1 || !held[0] {
		t.Fatalf("runs %d held %v", calls, held)
	}
}
