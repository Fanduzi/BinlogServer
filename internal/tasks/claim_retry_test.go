// Package tasks provides module-level functionality for tasks.
// input: claim-loop ticks, recorded runner errors, and an unreachable or mid-dump source
// output: proof that an owned RETRY_BACKOFF run keeps its backoff, failure budget, and lease epoch, and that a same-owner re-claim and expired-lease takeover continue that budget from an oldest-first event slice
// pos: scheduler claim/retry regression coverage for source-unreachable give-up
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClaimLoopDoesNotRestartOwnedRetryBackoff(t *testing.T) {
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
	time.Sleep(60 * time.Millisecond)

	before, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	callsBefore := runner.callCount()
	claimed, err := s.ClaimRunnableTasks()
	if err != nil {
		t.Fatalf("ClaimRunnableTasks: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	after, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if claimed != 0 {
		t.Fatalf("claim restarted an owned retry, claimed=%d", claimed)
	}
	if calls := runner.callCount(); calls != callsBefore {
		t.Fatalf("backoff restarted early, calls %d -> %d", callsBefore, calls)
	}
	if after.Epoch != before.Epoch || after.Epoch == 0 {
		t.Fatalf("lease epoch changed on retry, before=%d after=%d", before.Epoch, after.Epoch)
	}
	if after.State != StateRetryBackoff || !strings.HasPrefix(after.LastError, "SOURCE_UNREACHABLE:") {
		t.Fatalf("state=%s last_error=%q", after.State, after.LastError)
	}
}

func TestClaimLoopUnreachableReachesFailedWithoutEpochClimb(t *testing.T) {
	leases := NewMemoryLease()
	runner := &unreachableRunner{}
	s := NewScheduler(
		WithRunner(runner),
		WithClusterLeaseManager(leases),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(time.Hour, time.Millisecond, time.Hour),
		WithRetryBackoff(time.Millisecond, time.Millisecond),
	)
	task := mustStartSourcedTask(t, s)
	started, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = s.ClaimRunnableTasks()
				time.Sleep(time.Millisecond)
			}
		}
	}()

	maxEpoch := started.Epoch
	deadline := time.Now().Add(3 * time.Second)
	var got Task
	for {
		var err error
		got, err = s.GetTask(task.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got.Epoch > maxEpoch {
			maxEpoch = got.Epoch
		}
		if got.State == StateFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected FAILED, state=%s epoch=%d maxEpoch=%d calls=%d last_error=%q", got.State, got.Epoch, maxEpoch, runner.callCount(), got.LastError)
		}
		time.Sleep(time.Millisecond)
	}
	if maxEpoch != 1 {
		t.Fatalf("lease epoch climbed to %d", maxEpoch)
	}
	if !strings.HasPrefix(got.LastError, "SOURCE_UNREACHABLE:") {
		t.Fatalf("last_error=%q", got.LastError)
	}
	if got.OwnerWorkerID != "" || got.Epoch != 0 {
		t.Fatalf("FAILED should release the lease, owner=%q epoch=%d", got.OwnerWorkerID, got.Epoch)
	}
	if calls := runner.callCount(); calls != maxConsecutiveRetryableSourceFailures {
		t.Fatalf("calls=%d, want %d", calls, maxConsecutiveRetryableSourceFailures)
	}
	releaseDeadline := time.Now().Add(time.Second)
	for {
		held, err := leases.Verify(context.Background(), task.ID, "worker-a", 1)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if !held {
			break
		}
		if time.Now().After(releaseDeadline) {
			t.Fatal("lease still held after FAILED")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestClaimCarriesRecordedSourceFailures(t *testing.T) {
	const already = 9
	// Oldest first, matching MySQL ListEvents. A newest-first walk of this slice is 0.
	events := oldestFirstUnreachableEvents(already)
	if got := consecutiveUnreachableStreak(events, true); got != 0 {
		t.Fatalf("newest-first walk of oldest-first events = %d, want 0", got)
	}
	if got := consecutiveUnreachableStreak(events, false); got != already {
		t.Fatalf("oldest-first streak = %d, want %d", got, already)
	}

	runner := &unreachableRunner{}
	s := NewScheduler(
		WithRunner(runner),
		WithEventStore(&fixedEventStore{events: events}),
		WithClusterLeaseManager(NewMemoryLease()),
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
	s.mu.Lock()
	current := s.tasks[task.ID]
	current.State = StateRetryBackoff
	current.OwnerWorkerID = "worker-a"
	current.LastError = "SOURCE_UNREACHABLE: dial tcp: connection refused"
	s.tasks[task.ID] = current
	s.mu.Unlock()

	claimed, err := s.ClaimRunnableTasks()
	if err != nil {
		t.Fatalf("ClaimRunnableTasks: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("expected the idle owned retry to be claimed, claimed=%d", claimed)
	}
	waitTaskState(t, s, task.ID, 2*time.Second, StateFailed)
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !strings.HasPrefix(got.LastError, "SOURCE_UNREACHABLE:") {
		t.Fatalf("last_error=%q", got.LastError)
	}
	if calls := runner.callCount(); calls != 1 {
		t.Fatalf("calls=%d, want the one failure that exhausts the carried budget", calls)
	}
}

func TestClaimExpiredTasksCarriesOldestFirstSourceStreak(t *testing.T) {
	const already = 9
	events := oldestFirstUnreachableEvents(already)
	store := &expiredLeaseTestStore{tasks: map[string]Task{}}
	task := newExpiredOwnedTask("1", "worker-dead", StateRetryBackoff)
	task.LastError = "SOURCE_UNREACHABLE: dial tcp: connection refused"
	store.expired = []Task{task}
	store.tasks[task.ID] = task

	runner := &unreachableRunner{}
	s := NewScheduler(
		WithStore(store),
		WithRunner(runner),
		WithEventStore(&fixedEventStore{events: events}),
		WithClusterLeaseManager(NewMemoryLease()),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(time.Hour, time.Hour, time.Hour),
		WithRetryBackoff(time.Hour, time.Hour),
	)
	claimed, err := s.ClaimExpiredTasks()
	if err != nil {
		t.Fatalf("ClaimExpiredTasks: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("expected the expired retry to be claimed, claimed=%d", claimed)
	}
	waitTaskState(t, s, task.ID, 2*time.Second, StateFailed)
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !strings.HasPrefix(got.LastError, "SOURCE_UNREACHABLE:") {
		t.Fatalf("last_error=%q", got.LastError)
	}
	if calls := runner.callCount(); calls != 1 {
		t.Fatalf("calls=%d, want the one failure that exhausts the carried budget", calls)
	}
}

// oldestFirstUnreachableEvents is a MySQL ListEvents slice: oldest first.
// TASK_CREATED sits before the first TASK_STARTED, then N SOURCE_UNREACHABLE
// runner errors, then the TASK_STARTED this claim already flushed.
func oldestFirstUnreachableEvents(n int) []TaskEvent {
	events := []TaskEvent{
		{Type: "TASK_CREATED"},
		{Type: "TASK_STARTED"},
	}
	for i := 0; i < n; i++ {
		events = append(events, TaskEvent{
			Type:   "TASK_RUNNER_ERROR",
			Detail: "SOURCE_UNREACHABLE: dial tcp: connection refused",
		})
	}
	events = append(events, TaskEvent{Type: "TASK_STARTED"})
	return events
}

func TestOperatorStartResetsSourceFailureBudget(t *testing.T) {
	runner := &unreachableRunner{}
	s := NewScheduler(
		WithRunner(runner),
		WithClusterLeaseManager(NewMemoryLease()),
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
	s.mu.Lock()
	current := s.tasks[task.ID]
	current.State = StateRetryBackoff
	current.OwnerWorkerID = "worker-a"
	for i := 0; i < 9; i++ {
		s.appendEventLocked(task.ID, "TASK_RUNNER_ERROR", "runner error", "SOURCE_UNREACHABLE: dial tcp: connection refused")
		s.appendEventLocked(task.ID, "TASK_RETRY_BACKOFF", "task entered retry backoff", "SOURCE_UNREACHABLE: dial tcp: connection refused")
	}
	s.tasks[task.ID] = current
	s.mu.Unlock()

	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	waitTaskState(t, s, task.ID, 2*time.Second, StateRetryBackoff)
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State == StateFailed {
		t.Fatalf("operator Start reset the budget, last_error=%q", got.LastError)
	}
	if calls := runner.callCount(); calls != 1 {
		t.Fatalf("calls=%d, want one fresh attempt", calls)
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
