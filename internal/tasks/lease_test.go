// Package tasks provides module-level functionality for tasks.
// input: task commands/events, runner callbacks, store/lease/uploader dependencies
// output: task state transitions, scheduling decisions, and execution coordination
// pos: core domain orchestration layer governing backup task lifecycle and policies
// note: if this file changes, update this header and module README.md.
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

	renewCalls    int
	renewFn       func(call int) (bool, error)
	releaseCalls  int
	releaseCtxErr error
	releaseFn     func(taskID, workerID string, epoch int64) (bool, error)
}

// Acquire 实现对应功能逻辑。
func (f *fakeLeaseManager) Acquire(_ context.Context, _ string, _ string, _ time.Duration) (int64, bool, error) {
	return f.acquireEpoch, f.acquireOK, f.acquireErr
}

// Renew 实现对应功能逻辑。
func (f *fakeLeaseManager) Renew(_ context.Context, _ string, _ string, _ int64, _ time.Time, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewCalls++
	if f.renewFn == nil {
		return true, nil
	}
	return f.renewFn(f.renewCalls)
}

// Release 实现对应功能逻辑。
func (f *fakeLeaseManager) Release(ctx context.Context, taskID string, workerID string, epoch int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCalls++
	f.releaseCtxErr = ctx.Err()
	if f.releaseFn != nil {
		return f.releaseFn(taskID, workerID, epoch)
	}
	return true, nil
}

// TestScheduler_ClusterStartRequiresLease 验证相关行为。
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

	task, err := s.CreateTask("cluster-a", "cluster-a-key")
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

// TestScheduler_LeaseLostTransitionsToStoppingStopped 验证相关行为。
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

// TestScheduler_LeaseRenewFailureEntersDegradedThenStop 验证相关行为。
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

// TestScheduler_DeleteTaskReleasesLease 验证相关行为。
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

// TestScheduler_StopTaskReleasesLeaseWithIndependentContext 验证相关行为。
func TestScheduler_StopTaskReleasesLeaseWithIndependentContext(t *testing.T) {
	lease := &fakeLeaseManager{
		acquireEpoch: 17,
		acquireOK:    true,
	}
	runner := &fakeRunner{started: make(chan Task, 1)}
	s := NewScheduler(
		WithRunner(runner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(200*time.Millisecond, 10*time.Millisecond, 50*time.Millisecond),
	)

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
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner was not invoked")
	}

	if err := s.StopTask(task.ID); err != nil {
		t.Fatalf("StopTask returned error: %v", err)
	}
	waitTaskState(t, s, task.ID, 2*time.Second, StateStopped)

	deadline := time.Now().Add(2 * time.Second)
	for {
		lease.mu.Lock()
		releaseCalls := lease.releaseCalls
		releaseCtxErr := lease.releaseCtxErr
		lease.mu.Unlock()
		if releaseCalls > 0 {
			if releaseCtxErr != nil {
				t.Fatalf("expected release context not canceled, got %v", releaseCtxErr)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expected lease release on stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitTaskState 实现对应功能逻辑。
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
