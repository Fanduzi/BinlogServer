// Package tasks provides module-level functionality for tasks.
// input: task commands/events, runner callbacks, store/lease/uploader dependencies
// output: task state transitions, scheduling decisions, cluster lease, and expired-lease takeover coverage
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
	acquireCalls int

	renewCalls    int
	renewFn       func(call int) (bool, error)
	releaseCalls  int
	releaseCtxErr error
	releaseFn     func(taskID, workerID string, epoch int64) (bool, error)
}

// Acquire 实现对应功能逻辑。
func (f *fakeLeaseManager) Acquire(_ context.Context, _ string, _ string, _ time.Duration) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquireCalls++
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

// TestScheduler_LeaseRenewFailureWithinGraceDoesNotStopImmediately 验证 grace 内续租失败只进入降级，不会立即停机。
func TestScheduler_LeaseRenewFailureWithinGraceDoesNotStopImmediately(t *testing.T) {
	lease := &fakeLeaseManager{
		acquireEpoch: 12,
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
		WithClusterLease(200*time.Millisecond, 10*time.Millisecond, 200*time.Millisecond),
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
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.State == StateStopping || got.State == StateStopped {
		t.Fatalf("expected task to remain active within grace, got %s", got.State)
	}

	if err := s.StopTask(task.ID); err != nil {
		t.Fatalf("StopTask returned error: %v", err)
	}
	waitTaskState(t, s, task.ID, 2*time.Second, StateStopped)
}

// TestScheduler_LeaseRenewRecoveryWithinGraceKeepsRunning 验证 grace 内恢复后任务继续运行且不误停机。
func TestScheduler_LeaseRenewRecoveryWithinGraceKeepsRunning(t *testing.T) {
	lease := &fakeLeaseManager{
		acquireEpoch: 13,
		acquireOK:    true,
		renewFn: func(call int) (bool, error) {
			if call == 1 {
				return false, errors.New("meta db unavailable")
			}
			return true, nil
		},
	}
	runner := &fakeRunner{started: make(chan Task, 1)}
	s := NewScheduler(
		WithRunner(runner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(200*time.Millisecond, 10*time.Millisecond, 80*time.Millisecond),
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

	waitLeaseRenewCalls(t, lease, 3, 2*time.Second)
	waitTaskState(t, s, task.ID, 2*time.Second, StateRunning)

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.State != StateRunning {
		t.Fatalf("expected recovered task to keep running, got %s", got.State)
	}

	if err := s.StopTask(task.ID); err != nil {
		t.Fatalf("StopTask returned error: %v", err)
	}
	waitTaskState(t, s, task.ID, 2*time.Second, StateStopped)
}

// TestScheduler_OwnershipLossLeavesHealthyRunningPosture 验证 ownership-loss 后旧 owner 退出健康运行态。
func TestScheduler_OwnershipLossLeavesHealthyRunningPosture(t *testing.T) {
	lease := &fakeLeaseManager{
		acquireEpoch: 18,
		acquireOK:    true,
		renewFn: func(_ int) (bool, error) {
			return false, nil
		},
	}
	runner := &delayedStopRunner{
		started: make(chan Task, 1),
		delay:   100 * time.Millisecond,
	}
	s := NewScheduler(
		WithRunner(runner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(200*time.Millisecond, 10*time.Millisecond, 80*time.Millisecond),
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

	waitTaskState(t, s, task.ID, 2*time.Second, StateStopping)
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.State == StateRunning {
		t.Fatalf("expected ownership-loss to exit healthy running posture, got %s", got.State)
	}
	if got.OwnerWorkerID == "" || got.Epoch == 0 || got.RunID == "" {
		t.Fatalf("expected runtime ownership retained until final convergence, got owner=%q epoch=%d run_id=%q", got.OwnerWorkerID, got.Epoch, got.RunID)
	}
}

// TestScheduler_OwnershipLossSuppressesProgressAfterRenewLoss 验证 renew 报告 ownership-loss 后旧 owner 不再更新 progress。
func TestScheduler_OwnershipLossSuppressesProgressAfterRenewLoss(t *testing.T) {
	lease := &fakeLeaseManager{
		acquireEpoch: 19,
		acquireOK:    true,
		renewFn: func(_ int) (bool, error) {
			return false, nil
		},
	}
	runner := &delayedStopRunner{
		started: make(chan Task, 1),
		delay:   100 * time.Millisecond,
	}
	s := NewScheduler(
		WithRunner(runner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(200*time.Millisecond, 10*time.Millisecond, 80*time.Millisecond),
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

	s.ReportReplicationProgress(task.ID, time.Unix(100, 0), "mysql-bin.000001", 123, false)
	before, ok, err := s.GetReplicationProgress(task.ID)
	if err != nil {
		t.Fatalf("GetReplicationProgress returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected initial replication progress to exist")
	}

	waitTaskState(t, s, task.ID, 2*time.Second, StateStopping)

	s.ReportReplicationProgress(task.ID, time.Unix(200, 0), "mysql-bin.000002", 456, false)
	after, ok, err := s.GetReplicationProgress(task.ID)
	if err != nil {
		t.Fatalf("GetReplicationProgress returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected replication progress to remain readable")
	}
	if after.LastEventAt != before.LastEventAt || after.LastEventFile != before.LastEventFile || after.LastEventPos != before.LastEventPos {
		t.Fatalf("expected progress to stop changing after ownership loss, before=%+v after=%+v", before, after)
	}
}

// TestScheduler_OwnershipLossStopPathIsIdempotent 验证 ownership-loss stop 路径在重复信号下保持幂等。
func TestScheduler_OwnershipLossStopPathIsIdempotent(t *testing.T) {
	lease := &fakeLeaseManager{
		acquireEpoch: 20,
		acquireOK:    true,
	}
	runner := &delayedStopRunner{
		started: make(chan Task, 1),
		delay:   50 * time.Millisecond,
	}
	s := NewScheduler(
		WithRunner(runner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(200*time.Millisecond, 10*time.Millisecond, 80*time.Millisecond),
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

	s.mu.Lock()
	s.failSafeStopLocked(task.ID, "TASK_LEASE_LOST", "lease lost")
	s.failSafeStopLocked(task.ID, "TASK_LEASE_LOST", "lease lost")
	s.mu.Unlock()

	waitTaskState(t, s, task.ID, 2*time.Second, StateStopped)

	events, err := s.ListEvents(task.ID, 20)
	if err != nil {
		t.Fatalf("ListEvents returned error: %v", err)
	}
	leaseLostEvents := 0
	for _, event := range events {
		if event.Type == "TASK_LEASE_LOST" {
			leaseLostEvents++
		}
	}
	if leaseLostEvents != 1 {
		t.Fatalf("expected exactly one ownership-loss stop event, got %d", leaseLostEvents)
	}
}

// TestScheduler_OwnershipLossClearsRuntimeOwnershipOnStop 验证 ownership-loss 最终收敛后清空 runtime ownership 字段。
func TestScheduler_OwnershipLossClearsRuntimeOwnershipOnStop(t *testing.T) {
	lease := &fakeLeaseManager{
		acquireEpoch: 21,
		acquireOK:    true,
		renewFn: func(_ int) (bool, error) {
			return false, nil
		},
	}
	runner := &delayedStopRunner{
		started: make(chan Task, 1),
		delay:   50 * time.Millisecond,
	}
	s := NewScheduler(
		WithRunner(runner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(200*time.Millisecond, 10*time.Millisecond, 80*time.Millisecond),
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
		t.Fatalf("expected ownership cleared after final stop, got owner=%q epoch=%d run_id=%q", got.OwnerWorkerID, got.Epoch, got.RunID)
	}
}

// TestScheduler_FailSafeStopIsIdempotentAcrossLeaseLossSignals 验证重复 lease-loss 信号下 fail-safe stop 保持幂等。
func TestScheduler_FailSafeStopIsIdempotentAcrossLeaseLossSignals(t *testing.T) {
	lease := &fakeLeaseManager{
		acquireEpoch: 14,
		acquireOK:    true,
	}
	runner := &delayedStopRunner{
		started: make(chan Task, 1),
		delay:   50 * time.Millisecond,
	}
	s := NewScheduler(
		WithRunner(runner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(200*time.Millisecond, 10*time.Millisecond, 80*time.Millisecond),
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

	s.mu.Lock()
	s.failSafeStopLocked(task.ID, "TASK_LEASE_LOST", "lease lost")
	s.failSafeStopLocked(task.ID, "TASK_LEASE_GRACE_EXCEEDED", "lease renew grace exceeded")
	s.mu.Unlock()

	waitTaskState(t, s, task.ID, 2*time.Second, StateStopped)

	events, err := s.ListEvents(task.ID, 20)
	if err != nil {
		t.Fatalf("ListEvents returned error: %v", err)
	}
	leaseStopEvents := 0
	for _, event := range events {
		if event.Type == "TASK_LEASE_LOST" || event.Type == "TASK_LEASE_GRACE_EXCEEDED" {
			leaseStopEvents++
		}
	}
	if leaseStopEvents != 1 {
		t.Fatalf("expected exactly one fail-safe stop event, got %d", leaseStopEvents)
	}
}

// TestScheduler_FailSafeStopSuppressesReplicationProgress 验证 stop 触发后不再接受正常 progress 上报。
func TestScheduler_FailSafeStopSuppressesReplicationProgress(t *testing.T) {
	lease := &fakeLeaseManager{
		acquireEpoch: 16,
		acquireOK:    true,
	}
	runner := &delayedStopRunner{
		started: make(chan Task, 1),
		delay:   100 * time.Millisecond,
	}
	s := NewScheduler(
		WithRunner(runner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(200*time.Millisecond, 10*time.Millisecond, 80*time.Millisecond),
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

	s.ReportReplicationProgress(task.ID, time.Unix(100, 0), "mysql-bin.000001", 123, false)
	before, ok, err := s.GetReplicationProgress(task.ID)
	if err != nil {
		t.Fatalf("GetReplicationProgress returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected initial replication progress to exist")
	}

	s.mu.Lock()
	s.failSafeStopLocked(task.ID, "TASK_LEASE_LOST", "lease lost")
	s.mu.Unlock()

	waitTaskState(t, s, task.ID, 2*time.Second, StateStopping)

	s.ReportReplicationProgress(task.ID, time.Unix(200, 0), "mysql-bin.000002", 456, false)
	after, ok, err := s.GetReplicationProgress(task.ID)
	if err != nil {
		t.Fatalf("GetReplicationProgress returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected replication progress to remain readable")
	}
	if after.LastEventAt != before.LastEventAt || after.LastEventFile != before.LastEventFile || after.LastEventPos != before.LastEventPos {
		t.Fatalf("expected progress to stop changing after fail-safe stop, before=%+v after=%+v", before, after)
	}

	waitTaskState(t, s, task.ID, 2*time.Second, StateStopped)
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

func waitLeaseRenewCalls(t *testing.T, lease *fakeLeaseManager, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		lease.mu.Lock()
		got := lease.renewCalls
		lease.mu.Unlock()
		if got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected at least %d renew calls, got %d", want, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type expiredLeaseTestStore struct {
	mu       sync.Mutex
	tasks    map[string]Task
	expired  []Task
	upserted []Task
}

func (s *expiredLeaseTestStore) UpsertTask(_ context.Context, task Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tasks == nil {
		s.tasks = make(map[string]Task)
	}
	s.tasks[task.ID] = task
	s.upserted = append(s.upserted, task)
	return nil
}

func (s *expiredLeaseTestStore) ListTasks(_ context.Context) ([]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		out = append(out, task)
	}
	return out, nil
}

func (s *expiredLeaseTestStore) GetTask(_ context.Context, taskID string) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[taskID]
	if !ok {
		return Task{}, ErrTaskNotFound
	}
	return task, nil
}

func (s *expiredLeaseTestStore) ListTasksPage(_ context.Context, filter TaskListFilter) ([]Task, int, error) {
	list, err := s.ListTasks(context.Background())
	if err != nil {
		return nil, 0, err
	}
	page, total := PageTasks(list, filter)
	return page, total, nil
}

func (s *expiredLeaseTestStore) ListStartingUnownedTasks(_ context.Context) ([]Task, error) {
	list, err := s.ListTasks(context.Background())
	if err != nil {
		return nil, err
	}
	return StartingUnownedTasks(list), nil
}

func (s *expiredLeaseTestStore) DeleteTask(_ context.Context, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, taskID)
	return nil
}

func (s *expiredLeaseTestStore) ListTasksWithExpiredLease(_ context.Context) ([]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Task, len(s.expired))
	copy(out, s.expired)
	return out, nil
}

func (s *expiredLeaseTestStore) persistedStates() []State {
	s.mu.Lock()
	defer s.mu.Unlock()
	states := make([]State, 0, len(s.upserted))
	for _, task := range s.upserted {
		states = append(states, task.State)
	}
	return states
}

func newExpiredOwnedTask(id, owner string, state State) Task {
	return Task{
		ID:            id,
		Name:          "cluster-a",
		ClusterKey:    "cluster-a-key",
		State:         state,
		OwnerWorkerID: owner,
		Epoch:         7,
		RunID:         "run-old",
		Source:        SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"},
		Start:         StartConfig{Mode: StartModeLatest},
	}
}

func assertNoStopPersisted(t *testing.T, store *expiredLeaseTestStore) {
	t.Helper()
	for _, state := range store.persistedStates() {
		if state == StateStopping || state == StateStopped {
			t.Fatalf("takeover persisted %s", state)
		}
	}
}

func waitRunnerStarted(t *testing.T, runner *fakeRunner) {
	t.Helper()
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("expected worker runner to start claimed task")
	}
}

func TestScheduler_ClaimExpiredTasksStartsExpiredRunningTask(t *testing.T) {
	store := &expiredLeaseTestStore{tasks: make(map[string]Task)}
	lease := &fakeLeaseManager{acquireEpoch: 22, acquireOK: true}
	runner := &fakeRunner{started: make(chan Task, 1)}
	worker := NewScheduler(
		WithStore(store),
		WithRunner(runner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-b"),
	)

	task := newExpiredOwnedTask("1", "worker-dead", StateRunning)
	store.tasks[task.ID] = task
	store.expired = []Task{task}

	claimed, err := worker.ClaimExpiredTasks()
	if err != nil {
		t.Fatalf("ClaimExpiredTasks returned error: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("expected claimed=1, got %d", claimed)
	}
	lease.mu.Lock()
	acquireCalls := lease.acquireCalls
	lease.mu.Unlock()
	if acquireCalls == 0 {
		t.Fatal("expected lease acquire on expired takeover")
	}

	waitRunnerStarted(t, runner)
	waitTaskState(t, worker, task.ID, 2*time.Second, StateRunning)
	assertNoStopPersisted(t, store)

	got, err := worker.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.OwnerWorkerID != "worker-b" {
		t.Fatalf("expected owner worker-b after takeover, got %q", got.OwnerWorkerID)
	}
	if got.Epoch != 22 {
		t.Fatalf("expected epoch 22 after takeover, got %d", got.Epoch)
	}

	events, err := worker.ListEvents(task.ID, 20)
	if err != nil {
		t.Fatalf("ListEvents returned error: %v", err)
	}
	sawTakeover := false
	for _, event := range events {
		if event.Type == "TASK_STOPPING" || event.Type == "TASK_STOPPED" {
			t.Fatalf("takeover emitted stop event %s", event.Type)
		}
		if event.Type == "TASK_LEASE_TAKEOVER" {
			sawTakeover = true
		}
	}
	if !sawTakeover {
		t.Fatal("expected TASK_LEASE_TAKEOVER event")
	}

	if err := worker.StopTask(task.ID); err != nil {
		t.Fatalf("StopTask returned error: %v", err)
	}
	waitTaskState(t, worker, task.ID, 2*time.Second, StateStopped)
}

func TestScheduler_ClaimExpiredTasksDoesNotPersistStoppedWhenAcquireFails(t *testing.T) {
	store := &expiredLeaseTestStore{tasks: make(map[string]Task)}
	lease := &fakeLeaseManager{acquireEpoch: 28, acquireOK: false}
	runner := &fakeRunner{started: make(chan Task, 1)}
	worker := NewScheduler(
		WithStore(store),
		WithRunner(runner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-b"),
	)

	task := newExpiredOwnedTask("1", "worker-live", StateRunning)
	store.tasks[task.ID] = task
	store.expired = []Task{task}

	claimed, err := worker.ClaimExpiredTasks()
	if err != nil {
		t.Fatalf("ClaimExpiredTasks returned error: %v", err)
	}
	if claimed != 0 {
		t.Fatalf("expected claimed=0 when acquire fails, got %d", claimed)
	}
	got, err := worker.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.State != StateRunning || got.OwnerWorkerID != "worker-live" {
		t.Fatalf("expected original running owner retained, got state=%s owner=%q", got.State, got.OwnerWorkerID)
	}
	assertNoStopPersisted(t, store)
	select {
	case <-runner.started:
		t.Fatal("did not expect runner to start when acquire failed")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestScheduler_ClaimExpiredTasksIgnoresValidLease(t *testing.T) {
	store := &expiredLeaseTestStore{tasks: make(map[string]Task)}
	lease := &fakeLeaseManager{acquireEpoch: 23, acquireOK: true}
	runner := &fakeRunner{started: make(chan Task, 1)}
	worker := NewScheduler(
		WithStore(store),
		WithRunner(runner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-b"),
	)

	task := newExpiredOwnedTask("1", "worker-live", StateRunning)
	store.tasks[task.ID] = task
	store.expired = nil

	claimed, err := worker.ClaimExpiredTasks()
	if err != nil {
		t.Fatalf("ClaimExpiredTasks returned error: %v", err)
	}
	if claimed != 0 {
		t.Fatalf("expected claimed=0 for valid lease, got %d", claimed)
	}
	select {
	case <-runner.started:
		t.Fatal("did not expect runner to start for unexpired lease")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestScheduler_StartTaskDoesNotStealLiveLease(t *testing.T) {
	store := &expiredLeaseTestStore{tasks: make(map[string]Task)}
	lease := &fakeLeaseManager{acquireEpoch: 24, acquireOK: false}
	runner := &fakeRunner{started: make(chan Task, 1)}
	s := NewScheduler(
		WithStore(store),
		WithRunner(runner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-b"),
	)

	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}

	s.mu.Lock()
	current := s.tasks[task.ID]
	current.State = StateRunning
	current.OwnerWorkerID = "worker-live"
	current.Epoch = 7
	current.RunID = "run-live"
	s.tasks[task.ID] = current
	s.mu.Unlock()
	if err := store.UpsertTask(context.Background(), current); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}

	err = s.StartTask(task.ID)
	if !errors.Is(err, ErrLeaseNotAcquired) {
		t.Fatalf("expected ErrLeaseNotAcquired, got %v", err)
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.State != StateRunning {
		t.Fatalf("expected state %s after failed takeover, got %s", StateRunning, got.State)
	}
	if got.OwnerWorkerID != "worker-live" {
		t.Fatalf("expected original owner retained, got %q", got.OwnerWorkerID)
	}
	assertNoStopPersisted(t, store)
	select {
	case <-runner.started:
		t.Fatal("did not expect runner to start when lease is still valid")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestScheduler_ClaimStartingTasksStillClaimsUnownedStarting(t *testing.T) {
	store := &expiredLeaseTestStore{tasks: make(map[string]Task)}
	lease := &fakeLeaseManager{acquireEpoch: 25, acquireOK: true}
	control := NewScheduler(
		WithStore(store),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("control-plane-a"),
	)
	workerRunner := &fakeRunner{started: make(chan Task, 1)}
	worker := NewScheduler(
		WithStore(store),
		WithRunner(workerRunner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-a"),
	)

	task, err := control.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := control.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}
	if err := control.StartTask(task.ID); err != nil {
		t.Fatalf("control StartTask returned error: %v", err)
	}

	running := newExpiredOwnedTask("99", "worker-dead", StateRunning)
	store.mu.Lock()
	store.tasks[running.ID] = running
	store.expired = []Task{running}
	store.mu.Unlock()

	claimed, err := worker.ClaimStartingTasks()
	if err != nil {
		t.Fatalf("ClaimStartingTasks returned error: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("expected claimed=1 starting task, got %d", claimed)
	}
	waitRunnerStarted(t, workerRunner)
	waitTaskState(t, worker, task.ID, 2*time.Second, StateRunning)

	if err := worker.StopTask(task.ID); err != nil {
		t.Fatalf("StopTask returned error: %v", err)
	}
	waitTaskState(t, worker, task.ID, 2*time.Second, StateStopped)
}

func TestScheduler_ClaimExpiredTasksSkipsLocalLiveRun(t *testing.T) {
	store := &expiredLeaseTestStore{tasks: make(map[string]Task)}
	lease := &fakeLeaseManager{acquireEpoch: 26, acquireOK: true}
	runner := &fakeRunner{started: make(chan Task, 1)}
	worker := NewScheduler(
		WithStore(store),
		WithRunner(runner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-a"),
	)

	task, err := worker.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := worker.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}
	if err := worker.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask returned error: %v", err)
	}
	waitRunnerStarted(t, runner)
	waitTaskState(t, worker, task.ID, 2*time.Second, StateRunning)

	got, err := worker.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	store.mu.Lock()
	store.expired = []Task{got}
	store.mu.Unlock()

	claimed, err := worker.ClaimExpiredTasks()
	if err != nil {
		t.Fatalf("ClaimExpiredTasks returned error: %v", err)
	}
	if claimed != 0 {
		t.Fatalf("expected claimed=0 when local run is live, got %d", claimed)
	}

	if err := worker.StopTask(task.ID); err != nil {
		t.Fatalf("StopTask returned error: %v", err)
	}
	waitTaskState(t, worker, task.ID, 2*time.Second, StateStopped)
}

func TestScheduler_ClaimExpiredTasksStartsExpiredLeaseDegradedAndRetryBackoff(t *testing.T) {
	for _, state := range []State{StateLeaseDegraded, StateRetryBackoff} {
		t.Run(string(state), func(t *testing.T) {
			store := &expiredLeaseTestStore{tasks: make(map[string]Task)}
			lease := &fakeLeaseManager{acquireEpoch: 27, acquireOK: true}
			runner := &fakeRunner{started: make(chan Task, 1)}
			worker := NewScheduler(
				WithStore(store),
				WithRunner(runner),
				WithClusterLeaseManager(lease),
				WithClusterWorkerID("worker-b"),
			)
			task := newExpiredOwnedTask("1", "worker-dead", state)
			store.tasks[task.ID] = task
			store.expired = []Task{task}

			claimed, err := worker.ClaimExpiredTasks()
			if err != nil {
				t.Fatalf("ClaimExpiredTasks returned error: %v", err)
			}
			if claimed != 1 {
				t.Fatalf("expected claimed=1, got %d", claimed)
			}
			waitRunnerStarted(t, runner)
			waitTaskState(t, worker, task.ID, 2*time.Second, StateRunning)
			assertNoStopPersisted(t, store)
			if err := worker.StopTask(task.ID); err != nil {
				t.Fatalf("StopTask returned error: %v", err)
			}
			waitTaskState(t, worker, task.ID, 2*time.Second, StateStopped)
		})
	}
}
