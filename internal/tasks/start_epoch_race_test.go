// Package tasks provides module-level functionality for tasks.
// input: a task store whose second STARTING upsert waits, plus GetTask and syncTasksFromStore in that window
// output: proof that the runner starts with the owner and epoch StartTask acquired, reaches RUNNING, and renews that epoch
// pos: regression for a store read during StartTask persist replacing the in-memory task with STOPPED epoch 0
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"sync"
	"testing"
	"time"
)

// holdSecondStartStore publishes every upsert except the second STARTING row.
// That is the window where GetTask used to copy STOPPED epoch 0 back into memory.
type holdSecondStartStore struct {
	schedulerTestStore
	mu           sync.Mutex
	startUpserts int
	entered      chan struct{}
	release      chan struct{}
}

func (s *holdSecondStartStore) UpsertTask(ctx context.Context, task Task) error {
	if task.State == StateStarting && task.Epoch > 0 {
		s.mu.Lock()
		s.startUpserts++
		n := s.startUpserts
		s.mu.Unlock()
		if n == 2 {
			close(s.entered)
			<-s.release
		}
	}
	return s.schedulerTestStore.UpsertTask(ctx, task)
}

type epochRecordingRunner struct {
	mu      sync.Mutex
	got     []Task
	started chan struct{}
}

func (r *epochRecordingRunner) Run(ctx context.Context, task Task) error {
	r.mu.Lock()
	r.got = append(r.got, task)
	r.mu.Unlock()
	select {
	case r.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return context.Canceled
}

func (r *epochRecordingRunner) tasks() []Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Task, len(r.got))
	copy(out, r.got)
	return out
}

type epochRecordingLease struct {
	inner       *fakeLeaseManager
	mu          sync.Mutex
	renewEpochs []int64
}

func (l *epochRecordingLease) Acquire(ctx context.Context, taskID, workerID string, ttl time.Duration) (int64, bool, error) {
	return l.inner.Acquire(ctx, taskID, workerID, ttl)
}

func (l *epochRecordingLease) Renew(ctx context.Context, taskID, workerID string, epoch int64, now time.Time, ttl time.Duration) (bool, error) {
	l.mu.Lock()
	l.renewEpochs = append(l.renewEpochs, epoch)
	l.mu.Unlock()
	return l.inner.Renew(ctx, taskID, workerID, epoch, now, ttl)
}

func (l *epochRecordingLease) Release(ctx context.Context, taskID, workerID string, epoch int64) (bool, error) {
	return l.inner.Release(ctx, taskID, workerID, epoch)
}

func (l *epochRecordingLease) Verify(ctx context.Context, taskID, workerID string, epoch int64) (bool, error) {
	return l.inner.Verify(ctx, taskID, workerID, epoch)
}

func (l *epochRecordingLease) renewCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.renewEpochs)
}

func (l *epochRecordingLease) sawEpoch(epoch int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, got := range l.renewEpochs {
		if got == epoch {
			return true
		}
	}
	return false
}

// TestStartTask_StaleStoreReadKeepsLeaseEpoch is the #220 race.
// GetTask and the store sync run while the STARTING row is not visible yet.
// On the old scheduler the runner was the STOPPED epoch-0 copy.
func TestStartTask_StaleStoreReadKeepsLeaseEpoch(t *testing.T) {
	const (
		worker = "worker-a"
		epoch  = int64(11)
	)
	store := &holdSecondStartStore{
		schedulerTestStore: schedulerTestStore{tasks: make(map[string]Task)},
		entered:            make(chan struct{}),
		release:            make(chan struct{}),
	}
	lease := &epochRecordingLease{inner: &fakeLeaseManager{acquireEpoch: epoch, acquireOK: true}}
	runner := &epochRecordingRunner{started: make(chan struct{}, 4)}
	s := NewScheduler(
		WithStore(store),
		WithRunner(runner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID(worker),
		WithClusterLease(time.Minute, 15*time.Millisecond, time.Minute),
	)
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret"}); err != nil {
		t.Fatalf("ConfigureSource: %v", err)
	}
	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("first StartTask: %v", err)
	}
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first run did not start")
	}
	if err := s.StopTask(task.ID); err != nil {
		t.Fatalf("StopTask: %v", err)
	}
	waitTaskState(t, s, task.ID, 2*time.Second, StateStopped)

	startErr := make(chan error, 1)
	go func() {
		startErr <- s.StartTask(task.ID)
	}()
	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("second STARTING upsert did not reach the store")
	}

	if _, err := s.GetTask(task.ID); err != nil {
		t.Fatalf("GetTask during start: %v", err)
	}
	if err := s.syncTasksFromStore(); err != nil {
		t.Fatalf("syncTasksFromStore during start: %v", err)
	}
	beforeRenew := lease.renewCount()
	close(store.release)

	select {
	case err := <-startErr:
		if err != nil {
			t.Fatalf("second StartTask: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second StartTask did not return")
	}
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("second run did not start")
	}

	got := runner.tasks()
	if len(got) < 2 {
		t.Fatalf("runs=%d, want a second start", len(got))
	}
	started := got[len(got)-1]
	if started.OwnerWorkerID != worker || started.Epoch != epoch {
		t.Fatalf("runner task owner=%q epoch=%d, want %s epoch %d", started.OwnerWorkerID, started.Epoch, worker, epoch)
	}
	waitTaskState(t, s, task.ID, 2*time.Second, StateRunning)
	deadline := time.Now().Add(2 * time.Second)
	for lease.renewCount() <= beforeRenew || !lease.sawEpoch(epoch) {
		if time.Now().After(deadline) {
			t.Fatal("lease renewal did not run for the acquired epoch")
		}
		time.Sleep(5 * time.Millisecond)
	}
	row, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.Epoch != epoch || row.OwnerWorkerID != worker || row.State != StateRunning {
		t.Fatalf("task after start = state %s owner %q epoch %d", row.State, row.OwnerWorkerID, row.Epoch)
	}
	if err := s.StopTask(task.ID); err != nil {
		t.Fatalf("final StopTask: %v", err)
	}
}
