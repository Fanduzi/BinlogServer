package tasks

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeLeaseManager struct {
	mu sync.Mutex

	acquireEpoch int64
	acquireOK    bool
	acquireErr   error

	renewCalls   int
	renewFn      func(call int) (bool, error)
	releaseCalls int
	releaseFn    func(taskID, workerID string, epoch int64) (bool, error)
}

func (f *fakeLeaseManager) Acquire(_ context.Context, _ string, _ string, _ time.Time, _ time.Duration) (int64, bool, error) {
	return f.acquireEpoch, f.acquireOK, f.acquireErr
}

func (f *fakeLeaseManager) Renew(_ context.Context, _ string, _ string, _ int64, _ time.Time, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewCalls++
	if f.renewFn == nil {
		return true, nil
	}
	return f.renewFn(f.renewCalls)
}

func (f *fakeLeaseManager) Release(_ context.Context, taskID string, workerID string, epoch int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCalls++
	if f.releaseFn != nil {
		return f.releaseFn(taskID, workerID, epoch)
	}
	return true, nil
}

func TestScheduler_ClusterStartRequiresLease(t *testing.T) {
	lease := &fakeLeaseManager{
		acquireEpoch: 7,
		acquireOK:    false,
	}
	s := NewScheduler(
		WithRunner(&fakeRunner{started: make(chan Task, 1)}),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(200*time.Millisecond, 10*time.Millisecond, 50*time.Millisecond),
	)

	task, err := s.CreateTask("cluster-a")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}

	err = s.StartTask(task.ID)
	if !errors.Is(err, ErrLeaseNotAcquired) {
		t.Fatalf("expected ErrLeaseNotAcquired, got %v", err)
	}

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.State != StateCreated {
		t.Fatalf("expected state %s, got %s", StateCreated, got.State)
	}
}

func TestScheduler_LeaseLostTransitionsToStoppingStopped(t *testing.T) {
	lease := &fakeLeaseManager{
		acquireEpoch: 9,
		acquireOK:    true,
		renewFn: func(_ int) (bool, error) {
			return false, nil
		},
	}
	runner := &fakeRunner{started: make(chan Task, 1)}
	s := NewScheduler(
		WithRunner(runner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(200*time.Millisecond, 10*time.Millisecond, 30*time.Millisecond),
	)

	task, err := s.CreateTask("cluster-a")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}
	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask returned error: %v", err)
	}

	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner was not invoked")
	}

	waitTaskState(t, s, task.ID, 2*time.Second, StateStopped)
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.OwnerWorkerID != "" || got.Epoch != 0 || got.RunID != "" {
		t.Fatalf("expected lease identity cleared after stop, got owner=%q epoch=%d run_id=%q", got.OwnerWorkerID, got.Epoch, got.RunID)
	}
}

func TestScheduler_LeaseRenewFailureEntersDegradedThenStop(t *testing.T) {
	lease := &fakeLeaseManager{
		acquireEpoch: 11,
		acquireOK:    true,
		renewFn: func(_ int) (bool, error) {
			return false, errors.New("meta db unavailable")
		},
	}
	runner := &fakeRunner{started: make(chan Task, 1)}
	s := NewScheduler(
		WithRunner(runner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(200*time.Millisecond, 10*time.Millisecond, 40*time.Millisecond),
	)

	task, err := s.CreateTask("cluster-a")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}
	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask returned error: %v", err)
	}

	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner was not invoked")
	}

	waitTaskState(t, s, task.ID, 2*time.Second, StateLeaseDegraded)
	waitTaskState(t, s, task.ID, 2*time.Second, StateStopped)
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.OwnerWorkerID != "" || got.Epoch != 0 || got.RunID != "" {
		t.Fatalf("expected lease identity cleared after stop, got owner=%q epoch=%d run_id=%q", got.OwnerWorkerID, got.Epoch, got.RunID)
	}
}

func TestScheduler_DeleteTaskReleasesLease(t *testing.T) {
	lease := &fakeLeaseManager{
		acquireEpoch: 15,
		acquireOK:    true,
	}
	runner := &fakeRunner{started: make(chan Task, 1)}
	s := NewScheduler(
		WithRunner(runner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(200*time.Millisecond, 10*time.Millisecond, 50*time.Millisecond),
	)

	task, err := s.CreateTask("cluster-a")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}
	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask returned error: %v", err)
	}
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner was not invoked")
	}

	if err := s.DeleteTask(task.ID); err != nil {
		t.Fatalf("DeleteTask returned error: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		lease.mu.Lock()
		releaseCalls := lease.releaseCalls
		lease.mu.Unlock()
		if releaseCalls > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expected lease release on delete")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitTaskState(t *testing.T, s *Scheduler, taskID string, timeout time.Duration, want State) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got, err := s.GetTask(taskID)
		if err != nil {
			t.Fatalf("GetTask returned error: %v", err)
		}
		if got.State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected state %s, got %s", want, got.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
