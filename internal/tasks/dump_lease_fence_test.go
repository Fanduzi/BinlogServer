// Package tasks provides module-level functionality for tasks.
// input: a shared MemoryLease with a test clock, a leftover dump KILL slower than the lease TTL, and a lease taken over while a dump runs
// output: proof that Start does not open a dump unless it holds an unexpired lease after the KILL, and that a worker that loses its lease stops its dump without writing the new owner's row
// pos: regression for issue 346 (two dumps open at once in the control-loop e2e)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"sync"
	"testing"
	"time"
)

type leaseTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *leaseTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *leaseTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// countingRunner counts the dumps it opens and how many are open at once.
type countingRunner struct {
	mu      sync.Mutex
	opened  int
	open    int
	started chan struct{}
	stopped chan struct{}
}

func (r *countingRunner) Run(ctx context.Context, task Task) error {
	r.mu.Lock()
	r.opened++
	r.open++
	r.mu.Unlock()
	select {
	case r.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	r.mu.Lock()
	r.open--
	r.mu.Unlock()
	select {
	case r.stopped <- struct{}{}:
	default:
	}
	return context.Canceled
}

func (r *countingRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.opened
}

func ownedIdleTask(t *testing.T, s *Scheduler, owner string, epoch int64, state State) Task {
	t.Helper()
	task := s.mustTask(t)
	s.mu.Lock()
	item := s.tasks[task.ID]
	item.State = state
	item.DesiredRun = TaskDesiredRun
	item.OwnerWorkerID = owner
	item.Epoch = epoch
	s.tasks[task.ID] = item
	if err := s.persistTaskLocked(item); err != nil {
		s.mu.Unlock()
		t.Fatalf("persist: %v", err)
	}
	s.mu.Unlock()
	return item
}

// TestStartTask_LeaseExpiredDuringKillDoesNotOpenDump is #346. Worker B owns
// the task at epoch 1 and must KILL its leftover dump first. The KILL takes
// longer than the lease TTL; worker A claims the expired lease at epoch 2
// meanwhile. B must not open a dump after the KILL.
func TestStartTask_LeaseExpiredDuringKillDoesNotOpenDump(t *testing.T) {
	for _, acquire := range []bool{false, true} {
		name := "resume-held"
		if acquire {
			name = "reclaim"
		}
		t.Run(name, func(t *testing.T) {
			const ttl = 10 * time.Second
			clock := &leaseTestClock{now: time.Now()}
			leases := NewMemoryLease()
			leases.now = clock.Now
			store := newPendingColumnStore()
			runner := &countingRunner{started: make(chan struct{}, 4), stopped: make(chan struct{}, 4)}
			s := NewScheduler(
				WithStore(store),
				WithClusterLeaseManager(leases),
				WithClusterWorkerID("worker-b"),
				WithDumpFenceRequired(),
				WithClusterLease(ttl, time.Hour, time.Hour),
			)
			s.SetRunner(runner)
			task := ownedIdleTask(t, s, "worker-b", 1, StateRunning)
			if epoch, ok, err := leases.Acquire(context.Background(), task.ID, "worker-b", ttl); err != nil || !ok || epoch != 1 {
				t.Fatalf("seed lease epoch=%d ok=%v err=%v", epoch, ok, err)
			}
			store.pending[task.ID] = EncodeDumpCleanup(DumpCleanup{ConnectionID: 193, Host: "10.0.0.8", Port: 3306})

			s.dumpKiller = func(SourceConfig, uint32) error {
				// The KILL outlives the lease, and worker A claims it meanwhile.
				clock.Advance(ttl + time.Second)
				epoch, ok, err := leases.Acquire(context.Background(), task.ID, "worker-a", ttl)
				if err != nil || !ok || epoch != 2 {
					t.Errorf("worker-a takeover epoch=%d ok=%v err=%v", epoch, ok, err)
				}
				return nil
			}
			_ = s.startTask(task.ID, taskStart{acquireLease: acquire})
			select {
			case <-runner.started:
				t.Fatal("worker-b opened a dump after its lease expired during the KILL (#346 double dump)")
			case <-time.After(100 * time.Millisecond):
			}
			held, err := leases.Verify(context.Background(), task.ID, "worker-a", 2)
			if err != nil || !held {
				t.Fatalf("worker-a lost the lease held=%v err=%v", held, err)
			}
		})
	}
}

// TestStartTask_SlowKillWithinLeaseRenewsBeforeDump: a KILL that uses most of
// the TTL leaves a lease that would expire right after the dump opens. Start
// must renew it before the dump, so another worker cannot claim it.
func TestStartTask_SlowKillWithinLeaseRenewsBeforeDump(t *testing.T) {
	const ttl = 10 * time.Second
	clock := &leaseTestClock{now: time.Now()}
	leases := NewMemoryLease()
	leases.now = clock.Now
	store := newPendingColumnStore()
	runner := &countingRunner{started: make(chan struct{}, 4), stopped: make(chan struct{}, 4)}
	s := NewScheduler(
		WithStore(store),
		WithClusterLeaseManager(leases),
		WithClusterWorkerID("worker-b"),
		WithDumpFenceRequired(),
		WithClusterLease(ttl, time.Hour, time.Hour),
	)
	s.SetRunner(runner)
	task := ownedIdleTask(t, s, "worker-b", 1, StateRunning)
	if _, ok, err := leases.Acquire(context.Background(), task.ID, "worker-b", ttl); err != nil || !ok {
		t.Fatalf("seed lease ok=%v err=%v", ok, err)
	}
	store.pending[task.ID] = EncodeDumpCleanup(DumpCleanup{ConnectionID: 193, Host: "10.0.0.8", Port: 3306})
	s.dumpKiller = func(SourceConfig, uint32) error {
		clock.Advance(ttl - time.Second)
		return nil
	}
	if err := s.startTask(task.ID, taskStart{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("dump did not open while the lease was still held")
	}
	clock.Advance(2 * time.Second)
	if _, ok, _ := leases.Acquire(context.Background(), task.ID, "worker-a", ttl); ok {
		t.Fatal("worker-a claimed the lease of a dump that just opened: the KILL time was not counted against the lease")
	}
	_ = s.StopTask(task.ID)
}

// TestRenewLeaseLoop_LostLeaseStopsDumpWithoutWritingRow: worker A has taken
// the lease at epoch 2 and written its row. Worker B's renew fails; B must
// stop its dump promptly and must not write STOPPING/STOPPED over A's row.
func TestRenewLeaseLoop_LostLeaseStopsDumpWithoutWritingRow(t *testing.T) {
	leases := NewMemoryLease()
	store := &lockedFakeStore{fakeStore: newFakeStore()}
	runner := &countingRunner{started: make(chan struct{}, 4), stopped: make(chan struct{}, 4)}
	s := NewScheduler(
		WithStore(store),
		WithClusterLeaseManager(leases),
		WithClusterWorkerID("worker-b"),
		WithClusterLease(time.Minute, 10*time.Millisecond, time.Minute),
	)
	s.SetRunner(runner)
	task := ownedIdleTask(t, s, "", 0, StateStopped)
	if err := s.startTask(task.ID, taskStart{acquireLease: true}); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("dump did not start")
	}
	waitTaskState(t, s, task.ID, 2*time.Second, StateRunning)

	// Worker A takes over at epoch 2 and publishes its row.
	leases.mu.Lock()
	leases.rows[task.ID] = memoryLeaseRow{owner: "worker-a", epoch: 2, expire: time.Now().Add(time.Hour)}
	leases.mu.Unlock()
	row, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	row.State = StateRunning
	row.DesiredRun = TaskDesiredRun
	row.OwnerWorkerID = "worker-a"
	row.Epoch = 2
	row.LastError = ""
	if err := store.UpsertTask(context.Background(), row); err != nil {
		t.Fatalf("worker-a row: %v", err)
	}

	select {
	case <-runner.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("worker-b kept its dump open after losing the lease")
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		_, open := s.runs[task.ID]
		s.mu.Unlock()
		if !open {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	got, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask after loss: %v", err)
	}
	if got.State != StateRunning || got.OwnerWorkerID != "worker-a" || got.Epoch != 2 {
		t.Fatalf("stale worker-b wrote the shared row: state=%s owner=%q epoch=%d last_error=%q", got.State, got.OwnerWorkerID, got.Epoch, got.LastError)
	}
}

// lockedFakeStore lets one worker write the shared row while another reads it.
// Every TaskStore method takes mu: an embedded list that skips the lock races
// the map under -race (claim loop vs markRunnerReadyLocked).
type lockedFakeStore struct {
	*fakeStore
	mu sync.Mutex
}

func (s *lockedFakeStore) UpsertTask(ctx context.Context, task Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeStore.UpsertTask(ctx, task)
}

func (s *lockedFakeStore) GetTask(ctx context.Context, taskID string) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeStore.GetTask(ctx, taskID)
}

func (s *lockedFakeStore) ListTasks(ctx context.Context) ([]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeStore.ListTasks(ctx)
}

func (s *lockedFakeStore) ListTasksPage(ctx context.Context, filter TaskListFilter) ([]Task, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeStore.ListTasksPage(ctx, filter)
}

func (s *lockedFakeStore) ListStartingUnownedTasks(ctx context.Context) ([]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeStore.ListStartingUnownedTasks(ctx)
}

func (s *lockedFakeStore) DeleteTask(ctx context.Context, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeStore.DeleteTask(ctx, taskID)
}
