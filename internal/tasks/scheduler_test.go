package tasks

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestScheduler_StartTaskTransitionsToRunning(t *testing.T) {
	s := NewScheduler(WithRunner(&fakeRunner{started: make(chan Task, 1)}))
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

	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := s.GetTask(task.ID)
		if err != nil {
			t.Fatalf("GetTask returned error: %v", err)
		}
		if got.State == StateRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected state %s, got %s", StateRunning, got.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestScheduler_RetryableErrorTransitionsToBackoff(t *testing.T) {
	s := NewScheduler(WithRunner(&fakeRunner{started: make(chan Task, 1)}))
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

	if err := s.MarkRetryableError(task.ID, "network timeout"); err != nil {
		t.Fatalf("MarkRetryableError returned error: %v", err)
	}

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.State != StateRetryBackoff {
		t.Fatalf("expected state %s, got %s", StateRetryBackoff, got.State)
	}
	if got.LastError != "network timeout" {
		t.Fatalf("expected last error to be recorded, got %q", got.LastError)
	}
}

func TestScheduler_StopTaskTransitionsToStopped(t *testing.T) {
	s := NewScheduler(WithRunner(&fakeRunner{started: make(chan Task, 1)}))
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

	if err := s.StopTask(task.ID); err != nil {
		t.Fatalf("StopTask returned error: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := s.GetTask(task.ID)
		if err != nil {
			t.Fatalf("GetTask returned error: %v", err)
		}
		if got.State == StateStopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected state %s, got %s", StateStopped, got.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestScheduler_StartTaskWithoutRunnerReturnsError(t *testing.T) {
	s := NewScheduler()
	task, err := s.CreateTask("cluster-a")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}

	err = s.StartTask(task.ID)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err != ErrRunnerNotConfigured {
		t.Fatalf("expected ErrRunnerNotConfigured, got %v", err)
	}
}

func TestScheduler_StartTaskWithoutRunnerInClusterDispatchMode(t *testing.T) {
	lease := &fakeLeaseManager{
		acquireEpoch: 7,
		acquireOK:    true,
	}
	s := NewScheduler(
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("control-plane-a"),
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

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.State != StateStarting {
		t.Fatalf("expected state %s, got %s", StateStarting, got.State)
	}
	if got.OwnerWorkerID != "" || got.Epoch != 0 || got.RunID != "" {
		t.Fatalf("expected no ownership in dispatch mode, got owner=%q epoch=%d run_id=%q", got.OwnerWorkerID, got.Epoch, got.RunID)
	}
}

func TestScheduler_GetTaskRefreshesFromStore(t *testing.T) {
	store := &schedulerTestStore{
		tasks: make(map[string]Task),
	}
	s := NewScheduler(WithStore(store))

	task, err := s.CreateTask("cluster-a")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}

	remote := task
	remote.State = StateRunning
	remote.UpdatedAt = time.Now()
	if err := store.UpsertTask(context.Background(), remote); err != nil {
		t.Fatalf("store.UpsertTask returned error: %v", err)
	}

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.State != StateRunning {
		t.Fatalf("expected state %s from store, got %s", StateRunning, got.State)
	}
}

type schedulerTestStore struct {
	mu    sync.Mutex
	tasks map[string]Task
}

func (s *schedulerTestStore) UpsertTask(_ context.Context, task Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[task.ID] = task
	return nil
}

func (s *schedulerTestStore) ListTasks(_ context.Context) ([]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		out = append(out, task)
	}
	return out, nil
}

func (s *schedulerTestStore) DeleteTask(_ context.Context, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, taskID)
	return nil
}
