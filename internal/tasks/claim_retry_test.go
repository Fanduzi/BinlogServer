// Package tasks provides module-level functionality for tasks.
// input: claim-loop ticks, recorded runner errors, and an unreachable or mid-dump source
// output: proof that an owned RETRY_BACKOFF run keeps its backoff, failure budget, and lease epoch, that the persisted columns survive restart, that operator Start resets them, and that a same-owner reclaim does not bump the epoch
// pos: scheduler claim/retry regression coverage for source-unreachable give-up
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"strings"
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
		// The tenth failure is FAILED. A yield would release the lease and
		// the claim loop's next Acquire would bump the epoch.
		if got.State == StateFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected FAILED, state=%s epoch=%d maxEpoch=%d calls=%d last_error=%q", got.State, got.Epoch, maxEpoch, runner.callCount(), got.LastError)
		}
		time.Sleep(time.Millisecond)
	}
	if maxEpoch != started.Epoch || got.Epoch != 0 {
		t.Fatalf("lease epoch climbed or was kept, started=%d max=%d now=%d", started.Epoch, maxEpoch, got.Epoch)
	}
	if got.DesiredRun != TaskDesiredRun {
		t.Fatalf("desired=%s, want RUN", got.DesiredRun)
	}
	if !strings.HasPrefix(got.LastError, "SOURCE_UNREACHABLE:") {
		t.Fatalf("last_error=%q", got.LastError)
	}
	if got.OwnerWorkerID != "" {
		t.Fatalf("FAILED should clear the owner, owner=%q epoch=%d", got.OwnerWorkerID, got.Epoch)
	}
	if calls := runner.callCount(); calls != maxConsecutiveRetryableSourceFailures {
		t.Fatalf("calls=%d, want %d", calls, maxConsecutiveRetryableSourceFailures)
	}
	if _, err := s.ClaimRunnableTasks(); err != nil {
		t.Fatalf("ClaimRunnableTasks: %v", err)
	}
	after, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if after.Epoch != got.Epoch || after.State != StateFailed {
		t.Fatalf("claim restarted a FAILED give-up, state=%s epoch %d -> %d", after.State, got.Epoch, after.Epoch)
	}
	if calls := runner.callCount(); calls != maxConsecutiveRetryableSourceFailures {
		t.Fatalf("refusal let another run start, calls=%d", calls)
	}
	releaseDeadline := time.Now().Add(time.Second)
	for {
		held, err := leases.Verify(context.Background(), task.ID, "worker-a", started.Epoch)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if !held {
			break
		}
		if time.Now().After(releaseDeadline) {
			t.Fatal("lease still held after the yield")
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
	if got.DesiredRun != TaskDesiredRun || !strings.HasPrefix(got.LastError, "SOURCE_UNREACHABLE:") {
		t.Fatalf("state=%s desired=%s last_error=%q", got.State, got.DesiredRun, got.LastError)
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
	if got.DesiredRun != TaskDesiredRun || !strings.HasPrefix(got.LastError, "SOURCE_UNREACHABLE:") {
		t.Fatalf("state=%s desired=%s last_error=%q", got.State, got.DesiredRun, got.LastError)
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
	if got.DesiredRun != TaskDesiredRun {
		t.Fatalf("yield row desired=%s, want RUN", got.DesiredRun)
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
