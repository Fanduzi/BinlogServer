// Package tasks provides module-level functionality for tasks.
// input: runner errors for a sealed local file, a lease epoch mismatch, a retryable source failure, and a non-allowlisted error during Stop
// output: proof that a sealed file and an unclassified runner error fail once and release the lease, a lease mismatch hands the task off, a retryable source error stays in RETRY_BACKOFF, Start after FAILED arms the task again, and a non-allowlisted error during Stop (in memory, or a newer stored Stop whose KILL could not reach the source) stays STOPPED with pending_dump_cleanup and does not write TASK_FAILED
// pos: scheduler retry-policy regression coverage for permanent local errors and source retry
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type scriptedRunner struct {
	mu    sync.Mutex
	calls int
	fn    func(ctx context.Context, task Task, call int) error
}

func (r *scriptedRunner) Run(ctx context.Context, task Task) error {
	r.mu.Lock()
	r.calls++
	call := r.calls
	fn := r.fn
	r.mu.Unlock()
	return fn(ctx, task, call)
}

func (r *scriptedRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func leaseReleaseCount(f *fakeLeaseManager) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.releaseCalls
}

func TestScheduler_SealedFileExistsFailsAndReleasesLease(t *testing.T) {
	runner := &scriptedRunner{fn: func(context.Context, Task, int) error {
		return fmt.Errorf("sealed file already exists: mysql-bin.000003")
	}}
	lease := &fakeLeaseManager{acquireEpoch: 3, acquireOK: true}
	s := NewScheduler(
		WithRunner(runner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(time.Hour, time.Hour, time.Hour),
		WithRetryBackoff(time.Millisecond, time.Millisecond),
	)
	task := mustStartSourcedTask(t, s)

	deadline := time.Now().Add(2 * time.Second)
	var got Task
	for {
		var err error
		got, err = s.GetTask(task.ID)
		if err != nil {
			t.Fatalf("GetTask returned error: %v", err)
		}
		if got.State == StateFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected FAILED, state=%s last_error=%q calls=%d release=%d", got.State, got.LastError, runner.callCount(), leaseReleaseCount(lease))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got.LastError != "SEALED_FILE_EXISTS: sealed file already exists: mysql-bin.000003" {
		t.Fatalf("last_error=%q", got.LastError)
	}
	if got.OwnerWorkerID != "" || got.Epoch != 0 {
		t.Fatalf("expected lease identity cleared, owner=%q epoch=%d", got.OwnerWorkerID, got.Epoch)
	}
	time.Sleep(40 * time.Millisecond)
	if calls := runner.callCount(); calls != 1 {
		t.Fatalf("expected no further source reconnect, calls=%d", calls)
	}
	if got := leaseReleaseCount(lease); got != 1 {
		t.Fatalf("expected one lease release, got %d", got)
	}
	assertEventTypes(t, s, task.ID, map[string]int{"TASK_RETRY_BACKOFF": 0, "TASK_FAILED": 1})
}

func TestScheduler_UnclassifiedRunnerErrorFailsOnce(t *testing.T) {
	runner := &scriptedRunner{fn: func(context.Context, Task, int) error {
		return errors.New("append failed")
	}}
	lease := &fakeLeaseManager{acquireEpoch: 6, acquireOK: true}
	s := NewScheduler(
		WithRunner(runner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(time.Hour, time.Hour, time.Hour),
		WithRetryBackoff(time.Millisecond, time.Millisecond),
	)
	task := mustStartSourcedTask(t, s)

	deadline := time.Now().Add(2 * time.Second)
	var got Task
	for {
		var err error
		got, err = s.GetTask(task.ID)
		if err != nil {
			t.Fatalf("GetTask returned error: %v", err)
		}
		if got.State == StateFailed {
			break
		}
		if got.State == StateRetryBackoff {
			t.Fatalf("unclassified error entered RETRY_BACKOFF: %s", got.LastError)
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected FAILED, state=%s last_error=%q", got.State, got.LastError)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got.LastError != "append failed" {
		t.Fatalf("last_error=%q", got.LastError)
	}
	if got.DesiredRun != TaskDesiredStop || got.FailedSpecRevision != got.SpecRevision || got.SpecRevision == 0 {
		t.Fatalf("desired=%s failed_spec=%d spec=%d", got.DesiredRun, got.FailedSpecRevision, got.SpecRevision)
	}
	if got.OwnerWorkerID != "" || got.Epoch != 0 {
		t.Fatalf("expected lease identity cleared, owner=%q epoch=%d", got.OwnerWorkerID, got.Epoch)
	}
	time.Sleep(40 * time.Millisecond)
	if calls := runner.callCount(); calls != 1 {
		t.Fatalf("expected one attempt, calls=%d", calls)
	}
	if got := leaseReleaseCount(lease); got != 1 {
		t.Fatalf("expected one lease release, got %d", got)
	}
	assertEventTypes(t, s, task.ID, map[string]int{"TASK_RETRY_BACKOFF": 0, "TASK_FAILED": 1})
}

func TestScheduler_StartAfterFailedArmsTask(t *testing.T) {
	runner := &scriptedRunner{fn: func(ctx context.Context, _ Task, call int) error {
		if call == 1 {
			return errors.New("append failed")
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	s := NewScheduler(WithRunner(runner), WithRetryBackoff(time.Millisecond, time.Millisecond))
	task := mustStartSourcedTask(t, s)

	deadline := time.Now().Add(2 * time.Second)
	var failed Task
	for {
		var err error
		failed, err = s.GetTask(task.ID)
		if err != nil {
			t.Fatalf("GetTask returned error: %v", err)
		}
		if failed.State == StateFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected FAILED, state=%s", failed.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("Start after FAILED: %v", err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for runner.callCount() < 2 {
		if time.Now().After(deadline) {
			got, _ := s.GetTask(task.ID)
			t.Fatalf("Start did not run the task again, calls=%d state=%s", runner.callCount(), got.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.State != StateRunning {
		t.Fatalf("expected RUNNING after Start, state=%s last_error=%q", got.State, got.LastError)
	}
	if got.DesiredRun != TaskDesiredRun || got.SpecRevision <= failed.SpecRevision {
		t.Fatalf("desired=%s spec=%d failed_at=%d", got.DesiredRun, got.SpecRevision, failed.SpecRevision)
	}
	if got.RetryAttempt != 0 || got.ConsecutiveSourceFailures != 0 {
		t.Fatalf("Start did not zero the budget, attempt=%d consecutive=%d", got.RetryAttempt, got.ConsecutiveSourceFailures)
	}
	if err := s.StopTask(task.ID); err != nil {
		t.Fatalf("StopTask returned error: %v", err)
	}
}

func TestRetryAllowed(t *testing.T) {
	purged := errors.New("ERROR 1236 (HY000): Could not find first log file name in binary log index file")
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"unreachable", NewRetryableSourceError(CodeSourceUnreachable, "dial tcp: connection refused"), true},
		{"deadlock", errors.New("Deadlock found when trying to get lock"), true},
		{"purge", errors.New("OBJECT_PURGE_FAILED: mysql-bin.000001"), true},
		{"append", errors.New("append failed"), false},
		{"sealed", NewPermanentError(CodeSealedFileExists, "sealed file already exists: x"), false},
		{"handoff", ErrLeaseHandoff, false},
		{"1236", purged, false},
		{"throttled", NewRetryableSourceError("SOURCE_THROTTLED", "source busy"), false},
	}
	for _, tc := range cases {
		if got := retryAllowed(tc.err); got != tc.want {
			t.Errorf("%s: retryAllowed=%v, want %v (%v)", tc.name, got, tc.want, tc.err)
		}
	}
}

func TestScheduler_RetryableSourceErrorStaysInRetryBackoff(t *testing.T) {
	runner := &scriptedRunner{fn: func(context.Context, Task, int) error {
		return NewRetryableSourceError(CodeSourceUnreachable, "dial tcp: connection refused")
	}}
	lease := &fakeLeaseManager{acquireEpoch: 4, acquireOK: true}
	s := NewScheduler(
		WithRunner(runner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(time.Hour, time.Hour, time.Hour),
		WithRetryBackoff(time.Hour, time.Hour),
	)
	task := mustStartSourcedTask(t, s)

	deadline := time.Now().Add(2 * time.Second)
	var got Task
	for {
		var err error
		got, err = s.GetTask(task.ID)
		if err != nil {
			t.Fatalf("GetTask returned error: %v", err)
		}
		if got.State == StateRetryBackoff {
			break
		}
		if got.State == StateFailed {
			t.Fatalf("retryable source error must not fail on the first attempt, last_error=%q", got.LastError)
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected RETRY_BACKOFF, state=%s last_error=%q", got.State, got.LastError)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got.LastError != "SOURCE_UNREACHABLE: dial tcp: connection refused" {
		t.Fatalf("last_error=%q", got.LastError)
	}
	if got.OwnerWorkerID != "worker-a" || got.Epoch != 4 {
		t.Fatalf("retry must keep the lease, owner=%q epoch=%d", got.OwnerWorkerID, got.Epoch)
	}
	if calls := runner.callCount(); calls != 1 {
		t.Fatalf("expected one attempt before the long backoff, calls=%d", calls)
	}
	if got := leaseReleaseCount(lease); got != 0 {
		t.Fatalf("retry must not release the lease, releases=%d", got)
	}
}

func TestScheduler_LeaseEpochMismatchHandsOffWithoutRetry(t *testing.T) {
	store := &schedulerTestStore{tasks: map[string]Task{}}
	var releasedWorker atomic.Value
	var releasedEpoch atomic.Int64
	lease := &fakeLeaseManager{
		acquireEpoch: 5,
		acquireOK:    true,
		releaseFn: func(_, workerID string, epoch int64) (bool, error) {
			releasedWorker.Store(workerID)
			releasedEpoch.Store(epoch)
			return true, nil
		},
	}
	runner := &scriptedRunner{fn: func(_ context.Context, task Task, _ int) error {
		next := task
		next.State = StateRunning
		next.OwnerWorkerID = "worker-b"
		next.Epoch = 99
		next.LastError = ""
		if err := store.UpsertTask(context.Background(), next); err != nil {
			return err
		}
		return errors.New("lease/epoch mismatch")
	}}
	s := NewScheduler(
		WithStore(store),
		WithRunner(runner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(time.Hour, time.Hour, time.Hour),
		WithRetryBackoff(time.Millisecond, time.Millisecond),
	)
	task := mustStartSourcedTask(t, s)

	deadline := time.Now().Add(2 * time.Second)
	for runner.callCount() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("runner was not called")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(80 * time.Millisecond)
	if calls := runner.callCount(); calls != 1 {
		got, _ := s.GetTask(task.ID)
		t.Fatalf("expected one attempt, calls=%d state=%s last_error=%q", calls, got.State, got.LastError)
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.State != StateRunning || got.OwnerWorkerID != "worker-b" || got.Epoch != 99 {
		t.Fatalf("handoff rewrote the new owner, state=%s owner=%q epoch=%d last_error=%q", got.State, got.OwnerWorkerID, got.Epoch, got.LastError)
	}
	if got.LastError != "" {
		t.Fatalf("handoff wrote last_error onto the new owner: %q", got.LastError)
	}
	if worker, _ := releasedWorker.Load().(string); worker != "worker-a" || releasedEpoch.Load() != 5 {
		t.Fatalf("expected release of this runner's lease, worker=%q epoch=%d releases=%d", worker, releasedEpoch.Load(), leaseReleaseCount(lease))
	}
	assertEventTypes(t, s, task.ID, map[string]int{"TASK_LEASE_YIELDED": 1, "TASK_FAILED": 0, "TASK_RETRY_BACKOFF": 0})
}

func assertEventTypes(t *testing.T, s *Scheduler, id string, want map[string]int) {
	t.Helper()
	events, err := s.ListEvents(id, 0)
	if err != nil {
		t.Fatalf("ListEvents returned error: %v", err)
	}
	got := map[string]int{}
	for _, event := range events {
		got[event.Type]++
	}
	for eventType, n := range want {
		if got[eventType] != n {
			t.Fatalf("event %s count=%d, want %d; events=%v", eventType, got[eventType], n, events)
		}
	}
}

// unreachableKillError is a source error that is not on the retry allowlist.
// A net.OpError would be SOURCE_UNREACHABLE. This string is what a KILL or a
// dump close can return when the address is simply unreachable.
const unreachableKillError = "dial tcp 10.0.0.1:3306: connect: network is unreachable"

// stopThenErrorRunner lets Stop cancel the run, reports the failed KILL, then
// returns an error the allowlist would otherwise fail on the first occurrence.
type stopThenErrorRunner struct {
	mu      sync.Mutex
	notify  func(taskID string, source SourceConfig, connectionID uint32, killErr error)
	entered chan struct{}
	once    sync.Once
}

func (r *stopThenErrorRunner) BindDumpCleanup(fn func(string, SourceConfig, uint32, error)) {
	r.mu.Lock()
	r.notify = fn
	r.mu.Unlock()
}

func (r *stopThenErrorRunner) Run(ctx context.Context, task Task) error {
	r.once.Do(func() { close(r.entered) })
	<-ctx.Done()
	r.mu.Lock()
	notify := r.notify
	r.mu.Unlock()
	if notify != nil {
		notify(task.ID, SourceConfig{Host: "10.0.0.1", Port: 3306, User: "repl", Password: "secret"}, 42, errors.New(unreachableKillError))
	}
	return errors.New(unreachableKillError)
}

func TestScheduler_UnclassifiedErrorDuringStopStaysStopped(t *testing.T) {
	if retryAllowed(errors.New(unreachableKillError)) {
		t.Fatal("test error must sit outside the retry allowlist")
	}
	runner := &stopThenErrorRunner{entered: make(chan struct{})}
	s := NewScheduler(WithRetryBackoff(time.Millisecond, time.Millisecond))
	s.SetRunner(runner)
	task := mustStartSourcedTask(t, s)
	select {
	case <-runner.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not start")
	}
	if err := s.StopTask(task.ID); err != nil {
		t.Fatalf("StopTask: %v", err)
	}
	waitTaskState(t, s, task.ID, 2*time.Second, StateStopped)
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State == StateFailed {
		t.Fatalf("stop became FAILED: %s", got.LastError)
	}
	if got.PendingDumpCleanup == nil || got.PendingDumpCleanup.ConnectionID != 42 {
		t.Fatalf("pending=%+v", got.PendingDumpCleanup)
	}
	if got.DesiredRun != TaskDesiredStop {
		t.Fatalf("desired=%s", got.DesiredRun)
	}
	assertEventTypes(t, s, task.ID, map[string]int{"TASK_FAILED": 0, "TASK_RETRY_BACKOFF": 0, "DUMP_CLEANUP_PENDING": 1})
}

func TestScheduler_StoredStopBeatsUnclassifiedError(t *testing.T) {
	if retryAllowed(errors.New(unreachableKillError)) {
		t.Fatal("test error must sit outside the retry allowlist")
	}
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	runner := &scriptedRunner{fn: func(context.Context, Task, int) error {
		once.Do(func() { close(entered) })
		<-release
		return errors.New(unreachableKillError)
	}}
	store := &schedulerTestStore{tasks: map[string]Task{}}
	s := NewScheduler(WithRunner(runner), WithStore(store), WithRetryBackoff(time.Hour, time.Hour))
	task := mustStartSourcedTask(t, s)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not start")
	}

	store.mu.Lock()
	row, ok := store.tasks[task.ID]
	if !ok {
		store.mu.Unlock()
		t.Fatal("task was not stored")
	}
	row.DesiredRun = TaskDesiredStop
	row.State = StateStopping
	row.SpecRevision++
	warned := DumpCleanup{ConnectionID: 77, Host: "10.0.0.1", Port: 3306}.warned()
	row.PendingDumpCleanup = &warned
	store.tasks[task.ID] = row
	store.mu.Unlock()
	close(release)

	waitTaskState(t, s, task.ID, 2*time.Second, StateStopped)
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State == StateFailed {
		t.Fatalf("stored stop became FAILED: %s", got.LastError)
	}
	if got.PendingDumpCleanup == nil || got.PendingDumpCleanup.ConnectionID != 77 {
		t.Fatalf("pending=%+v", got.PendingDumpCleanup)
	}
	if got.DesiredRun != TaskDesiredStop {
		t.Fatalf("desired=%s", got.DesiredRun)
	}
	assertEventTypes(t, s, task.ID, map[string]int{"TASK_FAILED": 0, "TASK_RETRY_BACKOFF": 0})
}

func mustStartSourcedTask(t *testing.T, s *Scheduler) Task {
	t.Helper()
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}
	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask returned error: %v", err)
	}
	return task
}
