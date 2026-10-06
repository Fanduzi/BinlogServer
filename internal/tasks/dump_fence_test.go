// Package tasks provides module-level functionality for tasks.
// input: a held Binlog Dump connection id, an expired lease, and a KILL that may not reach the source
// output: coverage that a foreign worker fences that connection instead of writing STOPPED, that Start does not open a dump until KILL is confirmed, and that the holder keeps its lease until Close returns
// pos: unit coverage for issue 255
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestApplyDumpCleanup_HeldBecomesPendingOnce(t *testing.T) {
	held := DumpCleanup{ConnectionID: 993, Host: "10.0.0.8", Port: 3306, Held: true, Epoch: 4}
	refused := errors.New("dial tcp: i/o timeout")
	got, event := ApplyDumpCleanup(held, DumpCleanup{ConnectionID: 993, Host: "10.0.0.8", Port: 3306}, refused)
	if event != dumpCleanupPending || got.Held || got.ConnectionID != 993 || got.Epoch != 4 {
		t.Fatalf("fence %+v event %q", got, event)
	}
	again, event := ApplyDumpCleanup(got, DumpCleanup{ConnectionID: 993, Host: "10.0.0.8", Port: 3306}, refused)
	if event != "" || again.Held || again.ConnectionID != 993 {
		t.Fatalf("repeat %+v event %q", again, event)
	}
	cleared, event := ApplyDumpCleanup(held, DumpCleanup{ConnectionID: 993}, nil)
	if event != "" || cleared.ConnectionID != 0 {
		t.Fatalf("quiet close %+v event %q", cleared, event)
	}
	round := DecodeDumpCleanup(EncodeDumpCleanup(held))
	if !round.Held || round.Epoch != 4 || round.ConnectionID != 993 || round.Warning != "" {
		t.Fatalf("round trip %+v", round)
	}
}

func TestScheduler_ForeignExpiredHeldDumpDoesNotWriteStopped(t *testing.T) {
	store := newPendingColumnStore()
	holder := NewScheduler(WithStore(store), WithClusterWorkerID("worker-a"))
	task := holder.mustTask(t)
	holder.mu.Lock()
	item := holder.tasks[task.ID]
	item.State = StateStopping
	item.DesiredRun = TaskDesiredStop
	item.OwnerWorkerID = "worker-a"
	item.Epoch = 4
	holder.tasks[task.ID] = item
	if err := holder.persistTaskLocked(item); err != nil {
		holder.mu.Unlock()
		t.Fatalf("persist: %v", err)
	}
	holder.mu.Unlock()
	holder.noteDumpHeld(task.ID, item.Source, 993, 4)

	worker := NewScheduler(
		WithStore(store),
		WithClusterLeaseManager(&fakeLeaseManager{}),
		WithClusterWorkerID("worker-b"),
		WithDumpFenceRequired(),
	)
	stored, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.PendingDumpCleanup == nil || !stored.PendingDumpCleanup.Held {
		t.Fatalf("stored held %+v raw %s", stored.PendingDumpCleanup, store.pending[task.ID])
	}
	if err := worker.settleForeignStop(stored); err != nil {
		t.Fatalf("settle: %v", err)
	}
	got, err := worker.GetTask(task.ID)
	if err != nil {
		t.Fatalf("worker GetTask: %v", err)
	}
	if got.State != StateStopping {
		t.Fatalf("state %s, want STOPPING", got.State)
	}
	if got.PendingDumpCleanup == nil || got.PendingDumpCleanup.ConnectionID != 993 || got.PendingDumpCleanup.Held {
		t.Fatalf("pending %+v", got.PendingDumpCleanup)
	}
	if !strings.Contains(got.PendingDumpCleanup.Warning, "may still be open") {
		t.Fatalf("warning %q", got.PendingDumpCleanup.Warning)
	}
	assertDumpCleanupEvents(t, worker, task.ID, 1, 0)

	killed := 0
	worker.dumpKiller = func(source SourceConfig, id uint32) error {
		killed++
		if id != 993 || source.Password != "secret" {
			t.Fatalf("kill %+v id %d", source, id)
		}
		return nil
	}
	if worker.retryPendingDumpCleanups() {
		t.Fatal("confirmed KILL should clear")
	}
	if killed != 1 {
		t.Fatalf("kills %d", killed)
	}
	got, err = worker.GetTask(task.ID)
	if err != nil {
		t.Fatalf("after kill: %v", err)
	}
	if got.State != StateStopped || got.PendingDumpCleanup != nil {
		t.Fatalf("after kill state %s pending %+v", got.State, got.PendingDumpCleanup)
	}
}

func TestScheduler_RetryPendingDumpOnRunning(t *testing.T) {
	store := newPendingColumnStore()
	s := NewScheduler(WithStore(store), WithClusterWorkerID("worker-b"), WithDumpFenceRequired())
	task := s.mustTask(t)
	s.mu.Lock()
	item := s.tasks[task.ID]
	item.State = StateRunning
	item.DesiredRun = TaskDesiredRun
	item.OwnerWorkerID = "worker-b"
	item.Epoch = 2
	s.tasks[task.ID] = item
	if err := s.persistTaskLocked(item); err != nil {
		s.mu.Unlock()
		t.Fatalf("persist: %v", err)
	}
	s.mu.Unlock()
	store.pending[task.ID] = EncodeDumpCleanup(DumpCleanup{ConnectionID: 88, Host: "10.0.0.8", Port: 3306})

	var killed uint32
	s.dumpKiller = func(_ SourceConfig, id uint32) error {
		killed = id
		return nil
	}
	if s.retryPendingDumpCleanups() {
		t.Fatal("RUNNING leftover should clear")
	}
	if killed != 88 {
		t.Fatalf("killed %d", killed)
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State != StateRunning || got.PendingDumpCleanup != nil {
		t.Fatalf("state %s pending %+v", got.State, got.PendingDumpCleanup)
	}
}

func TestScheduler_HeldDumpIsNotKilledWhileLeaseHolds(t *testing.T) {
	store := newPendingColumnStore()
	leases := NewMemoryLease()
	s := NewScheduler(
		WithStore(store),
		WithClusterLeaseManager(leases),
		WithClusterWorkerID("worker-b"),
		WithDumpFenceRequired(),
		WithClusterLease(time.Hour, time.Hour, time.Hour),
	)
	task := s.mustTask(t)
	s.mu.Lock()
	item := s.tasks[task.ID]
	item.State = StateRunning
	item.DesiredRun = TaskDesiredRun
	item.OwnerWorkerID = "worker-a"
	item.Epoch = 3
	s.tasks[task.ID] = item
	if err := s.persistTaskLocked(item); err != nil {
		s.mu.Unlock()
		t.Fatalf("persist: %v", err)
	}
	s.mu.Unlock()
	if _, ok, err := leases.Acquire(context.Background(), task.ID, "worker-a", time.Hour); err != nil || !ok {
		t.Fatalf("acquire ok=%v err=%v", ok, err)
	}
	// Acquire on an empty table starts at epoch 1. Pin the row to the task epoch.
	leases.mu.Lock()
	row := leases.rows[task.ID]
	row.epoch = 3
	row.owner = "worker-a"
	leases.rows[task.ID] = row
	leases.mu.Unlock()
	store.pending[task.ID] = EncodeDumpCleanup(DumpCleanup{ConnectionID: 993, Host: "10.0.0.8", Port: 3306, Held: true, Epoch: 3})

	called := false
	s.dumpKiller = func(SourceConfig, uint32) error {
		called = true
		return nil
	}
	if s.retryPendingDumpCleanups() || called {
		t.Fatal("live held dump must not be killed")
	}
}

func TestScheduler_StartRefusesUntilPendingKillConfirmed(t *testing.T) {
	store := newPendingColumnStore()
	leases := NewMemoryLease()
	runner := &fenceGateRunner{started: make(chan struct{}, 1)}
	s := NewScheduler(
		WithStore(store),
		WithClusterLeaseManager(leases),
		WithClusterWorkerID("worker-b"),
		WithDumpFenceRequired(),
		WithClusterLease(time.Hour, time.Hour, time.Hour),
	)
	s.SetRunner(runner)
	task := s.mustTask(t)
	s.mu.Lock()
	item := s.tasks[task.ID]
	item.State = StateStopped
	item.DesiredRun = TaskDesiredRun
	item.OwnerWorkerID = ""
	item.Epoch = 0
	s.tasks[task.ID] = item
	if err := s.persistTaskLocked(item); err != nil {
		s.mu.Unlock()
		t.Fatalf("persist: %v", err)
	}
	s.mu.Unlock()
	store.pending[task.ID] = EncodeDumpCleanup(DumpCleanup{ConnectionID: 77, Host: "10.0.0.8", Port: 3306})

	s.dumpKiller = func(SourceConfig, uint32) error {
		return errors.New("dial tcp: i/o timeout")
	}
	if err := s.startTask(task.ID, taskStart{acquireLease: true}); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-runner.started:
		t.Fatal("dump opened before KILL was confirmed")
	case <-time.After(50 * time.Millisecond):
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State != StateRetryBackoff || got.PendingDumpCleanup == nil || got.PendingDumpCleanup.ConnectionID != 77 {
		t.Fatalf("state %s pending %+v err %s", got.State, got.PendingDumpCleanup, got.LastError)
	}
	if !strings.Contains(got.LastError, "may still be open") {
		t.Fatalf("last error %q", got.LastError)
	}

	s.dumpKiller = func(SourceConfig, uint32) error { return nil }
	if err := s.startTask(task.ID, taskStart{acquireLease: true}); err != nil {
		t.Fatalf("start after kill: %v", err)
	}
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("dump did not start after KILL")
	}
	got, err = s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask running: %v", err)
	}
	if got.PendingDumpCleanup != nil {
		t.Fatalf("pending still set %+v", got.PendingDumpCleanup)
	}
	s.StopTask(task.ID)
}

func TestScheduler_Schema3ClusterRefusesForeignDump(t *testing.T) {
	store := newColumnlessStore()
	s := NewScheduler(
		WithStore(store),
		WithClusterLeaseManager(&fakeLeaseManager{acquireEpoch: 8, acquireOK: true}),
		WithClusterWorkerID("worker-b"),
		WithDumpFenceRequired(),
	)
	task := s.mustTask(t)
	s.mu.Lock()
	item := s.tasks[task.ID]
	item.State = StateStopping
	item.DesiredRun = TaskDesiredStop
	item.OwnerWorkerID = "worker-a"
	item.Epoch = 3
	s.tasks[task.ID] = item
	if err := s.persistTaskLocked(item); err != nil {
		s.mu.Unlock()
		t.Fatalf("persist: %v", err)
	}
	s.mu.Unlock()

	stored, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if err := s.settleForeignStop(stored); err != nil {
		t.Fatalf("settle: %v", err)
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("after settle: %v", err)
	}
	if got.State != StateStopping || !strings.Contains(got.LastError, "000004") {
		t.Fatalf("state %s err %q", got.State, got.LastError)
	}

	runner := &fenceGateRunner{started: make(chan struct{}, 1)}
	s.SetRunner(runner)
	s.mu.Lock()
	item = s.tasks[task.ID]
	item.State = StateRunning
	item.DesiredRun = TaskDesiredRun
	item.OwnerWorkerID = "worker-a"
	item.Epoch = 3
	s.tasks[task.ID] = item
	s.mu.Unlock()
	if err := s.startTask(task.ID, taskStart{acquireLease: true}); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-runner.started:
		t.Fatal("schema 3 cluster opened a dump another worker may still hold")
	case <-time.After(50 * time.Millisecond):
	}
	got, err = s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("after start: %v", err)
	}
	if got.State != StateRetryBackoff || !strings.Contains(got.LastError, "000004") {
		t.Fatalf("state %s err %q", got.State, got.LastError)
	}
}

func TestScheduler_Schema3ClusterLeavesLiveOwnerAlone(t *testing.T) {
	store := newColumnlessStore()
	leases := NewMemoryLease()
	runner := &fenceGateRunner{started: make(chan struct{}, 1)}
	s := NewScheduler(
		WithStore(store),
		WithClusterLeaseManager(leases),
		WithClusterWorkerID("worker-b"),
		WithDumpFenceRequired(),
		WithClusterLease(time.Hour, time.Hour, time.Hour),
	)
	s.SetRunner(runner)
	task := s.mustTask(t)
	if _, ok, err := leases.Acquire(context.Background(), task.ID, "worker-a", time.Hour); err != nil || !ok {
		t.Fatalf("acquire ok=%v err=%v", ok, err)
	}
	s.mu.Lock()
	item := s.tasks[task.ID]
	item.State = StateRunning
	item.DesiredRun = TaskDesiredRun
	item.OwnerWorkerID = "worker-a"
	item.Epoch = 1
	s.tasks[task.ID] = item
	if err := s.persistTaskLocked(item); err != nil {
		s.mu.Unlock()
		t.Fatalf("persist: %v", err)
	}
	s.mu.Unlock()
	err := s.startTask(task.ID, taskStart{acquireLease: true})
	if !errors.Is(err, ErrLeaseNotAcquired) {
		t.Fatalf("start err %v", err)
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State != StateRunning || got.OwnerWorkerID != "worker-a" {
		t.Fatalf("state %s owner %s", got.State, got.OwnerWorkerID)
	}
}

func TestScheduler_RenewContinuesUntilCloseReturns(t *testing.T) {
	store := newPendingColumnStore()
	leases := NewMemoryLease()
	runner := &slowCloseRunner{
		started: make(chan struct{}, 1),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	ttl := 300 * time.Millisecond
	holder := NewScheduler(
		WithStore(store),
		WithClusterLeaseManager(leases),
		WithClusterWorkerID("worker-a"),
		WithDumpFenceRequired(),
		WithClusterLease(ttl, 40*time.Millisecond, time.Second),
	)
	holder.SetRunner(runner)
	task := holder.mustTask(t)
	if err := holder.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("dump did not start")
	}
	waitTaskState(t, holder, task.ID, 2*time.Second, StateRunning)
	running, err := holder.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !strings.Contains(store.pending[task.ID], `"held":true`) {
		t.Fatalf("held marker %s", store.pending[task.ID])
	}

	if err := holder.StopTask(task.ID); err != nil {
		t.Fatalf("StopTask: %v", err)
	}
	select {
	case <-runner.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("close did not start")
	}
	time.Sleep(ttl + 200*time.Millisecond)

	other := NewScheduler(
		WithStore(store),
		WithClusterLeaseManager(leases),
		WithClusterWorkerID("worker-b"),
		WithDumpFenceRequired(),
		WithClusterLease(ttl, 40*time.Millisecond, time.Second),
	)
	stored, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("stored: %v", err)
	}
	if err := other.settleForeignStop(stored); err != nil {
		t.Fatalf("settle: %v", err)
	}
	ok, err := leases.Verify(context.Background(), task.ID, "worker-a", running.Epoch)
	if err != nil || !ok {
		t.Fatalf("lease during close ok=%v err=%v", ok, err)
	}
	if !strings.Contains(store.pending[task.ID], `"held":true`) {
		t.Fatalf("held marker changed during close: %s", store.pending[task.ID])
	}
	got, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("state read: %v", err)
	}
	if got.State == StateStopped {
		t.Fatal("foreign worker wrote STOPPED before Close returned")
	}

	close(runner.release)
	waitTaskState(t, holder, task.ID, 2*time.Second, StateStopped)
}

func TestScheduler_LeaseLossFencesHeldDump(t *testing.T) {
	store := newPendingColumnStore()
	leases := NewMemoryLease()
	runner := &slowCloseRunner{
		started: make(chan struct{}, 1),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	holder := NewScheduler(
		WithStore(store),
		WithClusterLeaseManager(leases),
		WithClusterWorkerID("worker-a"),
		WithDumpFenceRequired(),
		WithClusterLease(time.Hour, 30*time.Millisecond, time.Hour),
	)
	holder.SetRunner(runner)
	task := holder.mustTask(t)
	if err := holder.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("dump did not start")
	}
	waitTaskState(t, holder, task.ID, 2*time.Second, StateRunning)
	running, err := holder.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if _, err := leases.Release(context.Background(), task.ID, "worker-a", running.Epoch); err != nil {
		t.Fatalf("release: %v", err)
	}
	select {
	case <-runner.entered:
	case <-time.After(time.Second):
		t.Fatal("lease loss did not cancel the dump")
	}

	other := NewScheduler(
		WithStore(store),
		WithClusterLeaseManager(leases),
		WithClusterWorkerID("worker-b"),
		WithDumpFenceRequired(),
		WithClusterLease(time.Hour, time.Hour, time.Hour),
	)
	stored, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("stored: %v", err)
	}
	if err := other.settleForeignStop(stored); err != nil {
		t.Fatalf("settle: %v", err)
	}
	got, err := other.GetTask(task.ID)
	if err != nil {
		t.Fatalf("other GetTask: %v", err)
	}
	if got.State == StateStopped {
		t.Fatal("foreign worker wrote STOPPED for a dump it did not close")
	}
	if got.PendingDumpCleanup == nil || got.PendingDumpCleanup.Held || got.PendingDumpCleanup.ConnectionID != 993 {
		t.Fatalf("fenced pending %+v raw %s", got.PendingDumpCleanup, store.pending[task.ID])
	}
	close(runner.release)
	waitTaskState(t, holder, task.ID, 2*time.Second, StateStopped)
}

type fenceGateRunner struct {
	started chan struct{}
}

func (r *fenceGateRunner) Run(ctx context.Context, _ Task) error {
	select {
	case r.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return context.Canceled
}

func (r *fenceGateRunner) KillDumpThread(SourceConfig, uint32) error { return nil }

type slowCloseRunner struct {
	started chan struct{}
	entered chan struct{}
	release chan struct{}
	held    func(taskID string, source SourceConfig, connectionID uint32, epoch int64)
}

func (r *slowCloseRunner) BindDumpHeld(fn func(taskID string, source SourceConfig, connectionID uint32, epoch int64)) {
	r.held = fn
}

func (r *slowCloseRunner) Run(ctx context.Context, task Task) error {
	if r.held != nil {
		r.held(task.ID, task.Source, 993, task.Epoch)
	}
	select {
	case r.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	select {
	case <-r.entered:
	default:
		close(r.entered)
	}
	<-r.release
	return context.Canceled
}
