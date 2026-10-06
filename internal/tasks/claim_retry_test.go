// Package tasks provides module-level functionality for tasks.
// input: claim-loop ticks, recorded runner errors, and an unreachable or mid-dump source
// output: proof that an owned RETRY_BACKOFF run keeps its backoff, failure budget, and lease epoch, that the persisted columns survive restart, that operator Start resets them, and that a same-owner reclaim does not bump the epoch
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
	store := &schedulerTestStore{tasks: map[string]Task{}}
	s := NewScheduler(
		WithStore(store),
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
	if err := store.UpsertTask(context.Background(), current); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}

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

func TestClaimUsesPersistedBudgetInsteadOfEvents(t *testing.T) {
	events := oldestFirstUnreachableEvents(1)
	runner := &unreachableRunner{}
	store := &schedulerTestStore{tasks: map[string]Task{}}
	s := NewScheduler(
		WithStore(store),
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
	current.DesiredRun = TaskDesiredRun
	current.RetryAttempt = 9
	current.ConsecutiveSourceFailures = 9
	s.tasks[task.ID] = current
	s.mu.Unlock()
	if err := store.UpsertTask(context.Background(), current); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}

	if _, err := s.ClaimRunnableTasks(); err != nil {
		t.Fatalf("ClaimRunnableTasks: %v", err)
	}
	waitTaskState(t, s, task.ID, 2*time.Second, StateFailed)
	if calls := runner.callCount(); calls != 1 {
		t.Fatalf("calls=%d, want the one failure that exhausts the stored budget", calls)
	}
	got, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.DesiredRun != TaskDesiredStop || got.FailedSpecRevision != got.SpecRevision {
		t.Fatalf("FAILED row desired=%s failed_spec=%d spec=%d", got.DesiredRun, got.FailedSpecRevision, got.SpecRevision)
	}
	if got.ConsecutiveSourceFailures != 10 {
		t.Fatalf("consecutive=%d, want 10", got.ConsecutiveSourceFailures)
	}
}

func TestOperatorStartResetsPersistedBudget(t *testing.T) {
	runner := &unreachableRunner{}
	store := &schedulerTestStore{tasks: map[string]Task{}}
	s := NewScheduler(
		WithStore(store),
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
	current.DesiredRun = TaskDesiredRun
	current.RetryAttempt = 9
	current.ConsecutiveSourceFailures = 9
	current.SpecRevision = 2
	s.tasks[task.ID] = current
	s.mu.Unlock()

	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	waitTaskState(t, s, task.ID, 2*time.Second, StateRetryBackoff)
	got, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State == StateFailed {
		t.Fatal("operator Start kept the old budget")
	}
	if got.ConsecutiveSourceFailures != 1 || got.RetryAttempt != 1 {
		t.Fatalf("after one fresh failure consecutive=%d attempt=%d", got.ConsecutiveSourceFailures, got.RetryAttempt)
	}
	if got.DesiredRun != TaskDesiredRun {
		t.Fatalf("desired_run=%s", got.DesiredRun)
	}
}

func TestPersistedRetryAttemptContinuesBackoff(t *testing.T) {
	runner := &unreachableRunner{}
	store := &schedulerTestStore{tasks: map[string]Task{}}
	s := NewScheduler(
		WithStore(store),
		WithRunner(runner),
		WithClusterLeaseManager(NewMemoryLease()),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(time.Hour, time.Hour, time.Hour),
		WithRetryBackoff(time.Hour, 32*time.Hour),
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
	current.RetryAttempt = 4
	current.ConsecutiveSourceFailures = 4
	s.tasks[task.ID] = current
	s.mu.Unlock()
	if err := store.UpsertTask(context.Background(), current); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}

	if _, err := s.ClaimRunnableTasks(); err != nil {
		t.Fatalf("ClaimRunnableTasks: %v", err)
	}
	waitTaskState(t, s, task.ID, 2*time.Second, StateRetryBackoff)
	got, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.RetryAttempt != 5 || got.ConsecutiveSourceFailures != 5 {
		t.Fatalf("attempt=%d consecutive=%d, want 5 and 5", got.RetryAttempt, got.ConsecutiveSourceFailures)
	}
	// attempt 5 is base * 2^4. A restart that forgot the column would sleep base (1h).
	if s.retryDelay(int(got.RetryAttempt)) != 16*time.Hour {
		t.Fatalf("delay from stored attempt = %s, want 16h", s.retryDelay(int(got.RetryAttempt)))
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
	if got.ConsecutiveSourceFailures != 10 || got.DesiredRun != TaskDesiredStop {
		t.Fatalf("FAILED consecutive=%d desired=%s", got.ConsecutiveSourceFailures, got.DesiredRun)
	}
	if got.Epoch != 0 || got.OwnerWorkerID != "" {
		t.Fatalf("lease not released owner=%q epoch=%d", got.OwnerWorkerID, got.Epoch)
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
