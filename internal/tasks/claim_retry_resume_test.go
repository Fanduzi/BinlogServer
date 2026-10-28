// Package tasks provides module-level functionality for tasks.
// input: claim-loop ticks, recorded runner errors, an unreachable or disconnecting source, and restart cycles
// output: proof that a same-owner restart keeps the source-failure budget and epoch, a held lease is resumed not re-acquired, restarts continue the persisted budget, the claim loop starts an idle owned RETRY_BACKOFF, an open-dump disconnect counts toward the budget, and a consecutive-unreachable streak skips only this start
// pos: scheduler claim/retry regression coverage for restart and resume, split from claim_retry_test.go
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSameOwnerRestartKeepsUnreachableBudget(t *testing.T) {
	store := &schedulerTestStore{tasks: map[string]Task{}}
	leases := NewMemoryLease()
	runner := &gateRunner{limit: 0, release: make(chan struct{})}
	s := NewScheduler(
		WithStore(store),
		WithRunner(runner),
		WithClusterLeaseManager(leases),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(time.Hour, time.Hour, time.Hour),
		WithRetryBackoff(time.Hour, time.Hour),
	)
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource: %v", err)
	}
	epoch, acquired, err := leases.Acquire(context.Background(), task.ID, "worker-a", time.Hour)
	if err != nil || !acquired || epoch == 0 {
		t.Fatalf("Acquire epoch=%d acquired=%v err=%v", epoch, acquired, err)
	}
	const kept = "SOURCE_UNREACHABLE: dial tcp: connection refused"
	s.mu.Lock()
	current := s.tasks[task.ID]
	current.State = StateRetryBackoff
	current.DesiredRun = TaskDesiredRun
	current.OwnerWorkerID = "worker-a"
	current.Epoch = epoch
	current.RetryAttempt = 7
	current.ConsecutiveSourceFailures = 7
	current.LastError = kept
	s.tasks[task.ID] = current
	s.mu.Unlock()
	if err := store.UpsertTask(context.Background(), current); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}

	if _, err := s.ClaimRunnableTasks(); err != nil {
		t.Fatalf("ClaimRunnableTasks: %v", err)
	}
	waitGateBlocked(t, runner)
	got, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State != StateStarting {
		t.Fatalf("state=%s, want STARTING", got.State)
	}
	if got.Epoch != epoch {
		t.Fatalf("same-owner restart bumped epoch %d -> %d", epoch, got.Epoch)
	}
	if got.RetryAttempt != 7 || got.ConsecutiveSourceFailures != 7 {
		t.Fatalf("budget reset attempt=%d consecutive=%d", got.RetryAttempt, got.ConsecutiveSourceFailures)
	}
	if got.LastError != kept {
		t.Fatalf("last_error=%q, want it kept across the restart", got.LastError)
	}
	cancelRun(t, s, task.ID)
}

func TestHeldLeaseResumeDoesNotAcquire(t *testing.T) {
	leases := NewMemoryLease()
	counter := &countingLease{inner: leases}
	runner := &unreachableRunner{}
	s := NewScheduler(
		WithRunner(runner),
		WithClusterLeaseManager(counter),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(time.Hour, time.Hour, time.Hour),
		WithRetryBackoff(time.Hour, time.Hour),
	)
	task := mustStartSourcedTask(t, s)
	waitTaskState(t, s, task.ID, 2*time.Second, StateRetryBackoff)
	if counter.acquires != 1 {
		t.Fatalf("acquires after start=%d, want 1", counter.acquires)
	}
	before, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	cancelRun(t, s, task.ID)
	claimed, err := s.ClaimRunnableTasks()
	if err != nil {
		t.Fatalf("ClaimRunnableTasks: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("claimed=%d, want the idle owned task resumed", claimed)
	}
	if counter.acquires != 1 {
		t.Fatalf("held-lease resume acquired again, acquires=%d", counter.acquires)
	}
	waitTaskState(t, s, task.ID, 2*time.Second, StateRetryBackoff)
	after, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if after.Epoch != before.Epoch {
		t.Fatalf("epoch %d -> %d", before.Epoch, after.Epoch)
	}
	if after.ConsecutiveSourceFailures != before.ConsecutiveSourceFailures+1 {
		t.Fatalf("consecutive %d -> %d", before.ConsecutiveSourceFailures, after.ConsecutiveSourceFailures)
	}
}

func TestSameOwnerExpiredReclaimKeepsEpoch(t *testing.T) {
	leases := NewMemoryLease()
	runner := &unreachableRunner{}
	s := NewScheduler(
		WithRunner(runner),
		WithClusterLeaseManager(leases),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(40*time.Millisecond, time.Hour, time.Hour),
		WithRetryBackoff(time.Hour, time.Hour),
	)
	task := mustStartSourcedTask(t, s)
	waitTaskState(t, s, task.ID, 2*time.Second, StateRetryBackoff)
	before, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	cancelRun(t, s, task.ID)
	time.Sleep(60 * time.Millisecond)
	if _, err := s.ClaimRunnableTasks(); err != nil {
		t.Fatalf("ClaimRunnableTasks: %v", err)
	}
	waitTaskState(t, s, task.ID, 2*time.Second, StateRetryBackoff)
	after, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if after.Epoch != before.Epoch || after.Epoch == 0 {
		t.Fatalf("same-owner expired reclaim epoch %d -> %d", before.Epoch, after.Epoch)
	}
}

func TestTwoRestartsContinuePersistedSourceBudget(t *testing.T) {
	store := &schedulerTestStore{tasks: map[string]Task{}}
	leases := NewMemoryLease()
	var runner *gateRunner
	newSched := func(limit int) *Scheduler {
		runner = &gateRunner{limit: limit, release: make(chan struct{})}
		return NewScheduler(
			WithStore(store),
			WithRunner(runner),
			WithClusterLeaseManager(leases),
			WithClusterWorkerID("worker-a"),
			WithClusterLease(time.Hour, time.Hour, time.Hour),
			WithRetryBackoff(time.Millisecond, time.Millisecond),
		)
	}
	s := newSched(3)
	task := mustStartSourcedTask(t, s)
	waitGateBlocked(t, runner)
	cancelRun(t, s, task.ID)
	assertBudget(t, store, task.ID, 3, 3, 1)

	s = newSched(3)
	if err := s.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := s.ClaimRunnableTasks(); err != nil {
		t.Fatalf("ClaimRunnableTasks: %v", err)
	}
	waitGateBlocked(t, runner)
	cancelRun(t, s, task.ID)
	assertBudget(t, store, task.ID, 6, 6, 1)

	s = newSched(100)
	if err := s.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := s.ClaimRunnableTasks(); err != nil {
		t.Fatalf("ClaimRunnableTasks: %v", err)
	}
	waitTaskState(t, s, task.ID, 2*time.Second, StateFailed)
	got, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.ConsecutiveSourceFailures != 10 || got.DesiredRun != TaskDesiredRun {
		t.Fatalf("yield consecutive=%d desired=%s", got.ConsecutiveSourceFailures, got.DesiredRun)
	}
	if got.Epoch != 0 || got.OwnerWorkerID != "" {
		t.Fatalf("FAILED should release the lease, owner=%q epoch=%d", got.OwnerWorkerID, got.Epoch)
	}
	if !strings.HasPrefix(got.LastError, "SOURCE_UNREACHABLE:") {
		t.Fatalf("last_error=%q", got.LastError)
	}
}

func TestClaimRunnableTasksStartsIdleOwnedRetryBackoff(t *testing.T) {
	runner := &fakeRunner{started: make(chan Task, 1)}
	s := NewScheduler(
		WithRunner(runner),
		WithClusterLeaseManager(NewMemoryLease()),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(time.Hour, time.Hour, time.Hour),
	)
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource: %v", err)
	}
	s.mu.Lock()
	current := s.tasks[task.ID]
	current.State = StateRetryBackoff
	current.OwnerWorkerID = "worker-a"
	current.Epoch = 4
	s.tasks[task.ID] = current
	s.mu.Unlock()

	claimed, err := s.ClaimRunnableTasks()
	if err != nil {
		t.Fatalf("ClaimRunnableTasks: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("expected claimed=1, got %d", claimed)
	}
	waitRunnerStarted(t, runner)
}

func TestOpenDumpDisconnectCountsTowardSourceBudget(t *testing.T) {
	runner := &dumpDeathRunner{}
	s := NewScheduler(WithRunner(runner), WithRetryBackoff(time.Millisecond, time.Millisecond))
	task := mustStartSourcedTask(t, s)
	waitTaskState(t, s, task.ID, 2*time.Second, StateFailed)
	if calls := runner.callCount(); calls != 14 {
		got, _ := s.GetTask(task.ID)
		t.Fatalf("calls=%d, want 14 (4 failures, one ready disconnect, then 9 more), state=%s last_error=%q", calls, got.State, got.LastError)
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !strings.HasPrefix(got.LastError, "SOURCE_UNREACHABLE:") {
		t.Fatalf("last_error=%q", got.LastError)
	}
}

func TestConsecutiveUnreachableStreakSkipsThisStartOnly(t *testing.T) {
	newestFirst := []TaskEvent{
		{Type: "TASK_STARTED"},
		{Type: "TASK_RETRY_BACKOFF", Detail: "SOURCE_UNREACHABLE: now"},
		{Type: "TASK_RUNNER_ERROR", Detail: "SOURCE_UNREACHABLE: now"},
		{Type: "TASK_STARTED"},
		{Type: "TASK_RUNNER_ERROR", Detail: "SOURCE_UNREACHABLE: before operator start"},
	}
	if got := consecutiveUnreachableStreak(newestFirst, true); got != 1 {
		t.Fatalf("newest-first streak=%d, want 1", got)
	}
	broken := []TaskEvent{
		{Type: "TASK_RUNNER_ERROR", Detail: "SOURCE_UNREACHABLE: latest"},
		{Type: "TASK_RUNNER_ERROR", Detail: "dial timeout"},
		{Type: "TASK_RUNNER_ERROR", Detail: "SOURCE_UNREACHABLE: older"},
	}
	if got := consecutiveUnreachableStreak(broken, true); got != 1 {
		t.Fatalf("non-unreachable break streak=%d, want 1", got)
	}
}

type fixedEventStore struct {
	events []TaskEvent
}

func (f *fixedEventStore) AppendEvent(context.Context, TaskEvent) error { return nil }

func (f *fixedEventStore) ListEvents(context.Context, string, int) ([]TaskEvent, error) {
	out := append([]TaskEvent(nil), f.events...)
	return out, nil
}

type dumpDeathRunner struct {
	mu    sync.Mutex
	calls int
}

func (r *dumpDeathRunner) Run(context.Context, Task) error {
	return NewRetryableSourceError(CodeSourceUnreachable, "RunWithNotify required")
}

func (r *dumpDeathRunner) RunWithNotify(ctx context.Context, _ Task, onReady func()) error {
	r.mu.Lock()
	r.calls++
	call := r.calls
	r.mu.Unlock()
	if call == 5 {
		onReady()
		return NewRetryableSourceError(CodeSourceUnreachable, "read tcp: connection reset")
	}
	if call >= 14 {
		return NewRetryableSourceError(CodeSourceUnreachable, "dial tcp: connection refused")
	}
	return NewRetryableSourceError(CodeSourceUnreachable, "dial tcp: connection refused")
}

func (r *dumpDeathRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type countingLease struct {
	inner    *MemoryLease
	mu       sync.Mutex
	acquires int
}

func (c *countingLease) Acquire(ctx context.Context, taskID, workerID string, ttl time.Duration) (int64, bool, error) {
	c.mu.Lock()
	c.acquires++
	c.mu.Unlock()
	return c.inner.Acquire(ctx, taskID, workerID, ttl)
}

func (c *countingLease) Renew(ctx context.Context, taskID, workerID string, epoch int64, now time.Time, ttl time.Duration) (bool, error) {
	return c.inner.Renew(ctx, taskID, workerID, epoch, now, ttl)
}

func (c *countingLease) Release(ctx context.Context, taskID, workerID string, epoch int64) (bool, error) {
	return c.inner.Release(ctx, taskID, workerID, epoch)
}

func (c *countingLease) Verify(ctx context.Context, taskID, workerID string, epoch int64) (bool, error) {
	return c.inner.Verify(ctx, taskID, workerID, epoch)
}

type gateRunner struct {
	mu      sync.Mutex
	calls   int
	limit   int
	release chan struct{}
}

func (r *gateRunner) Run(ctx context.Context, _ Task) error {
	r.mu.Lock()
	r.calls++
	call := r.calls
	limit := r.limit
	r.mu.Unlock()
	if call > limit {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.release:
		}
	}
	return NewRetryableSourceError(CodeSourceUnreachable, "dial tcp: connection refused")
}

func (r *gateRunner) RunWithNotify(ctx context.Context, task Task, _ func()) error {
	return r.Run(ctx, task)
}

func waitGateBlocked(t *testing.T, r *gateRunner) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		calls := r.calls
		limit := r.limit
		r.mu.Unlock()
		if calls > limit {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("runner calls=%d, want more than %d", calls, limit)
		}
		time.Sleep(time.Millisecond)
	}
}

func cancelRun(t *testing.T, s *Scheduler, id string) {
	t.Helper()
	s.mu.Lock()
	cancel, ok := s.cancels[id]
	s.mu.Unlock()
	if !ok {
		t.Fatalf("task %s has no cancel", id)
	}
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		_, running := s.runs[id]
		s.mu.Unlock()
		if !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s run still active", id)
		}
		time.Sleep(time.Millisecond)
	}
}

func assertBudget(t *testing.T, store *schedulerTestStore, id string, attempt, consecutive int, epoch int64) {
	t.Helper()
	got, err := store.GetTask(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.RetryAttempt != int64(attempt) || got.ConsecutiveSourceFailures != int64(consecutive) || got.Epoch != epoch {
		t.Fatalf("attempt=%d consecutive=%d epoch=%d, want %d %d %d (state=%s)", got.RetryAttempt, got.ConsecutiveSourceFailures, got.Epoch, attempt, consecutive, epoch, got.State)
	}
}
