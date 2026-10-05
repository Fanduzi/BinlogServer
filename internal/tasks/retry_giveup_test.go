// Package tasks provides module-level functionality for tasks.
// input: runner errors for a sealed local file, a lease epoch mismatch, and a retryable source failure
// output: proof that a sealed file fails and releases the lease, a lease mismatch hands the task off, and a retryable source error stays in RETRY_BACKOFF
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
