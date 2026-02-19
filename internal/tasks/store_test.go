package tasks

import (
	"context"
	"errors"
	"testing"
)

type fakeStore struct {
	tasks     map[string]Task
	upsertErr error
	listErr   error
	deleteErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{tasks: make(map[string]Task)}
}

func (f *fakeStore) UpsertTask(_ context.Context, task Task) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.tasks[task.ID] = task
	return nil
}

func (f *fakeStore) ListTasks(_ context.Context) ([]Task, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]Task, 0, len(f.tasks))
	for _, t := range f.tasks {
		out = append(out, t)
	}
	return out, nil
}

func (f *fakeStore) DeleteTask(_ context.Context, taskID string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.tasks, taskID)
	return nil
}

func TestScheduler_PersistsCreatedTask(t *testing.T) {
	store := newFakeStore()
	s := NewScheduler(WithStore(store))

	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}

	persisted, ok := store.tasks[task.ID]
	if !ok {
		t.Fatalf("task %s was not persisted", task.ID)
	}
	if persisted.Name != "cluster-a" {
		t.Fatalf("unexpected persisted task name: %s", persisted.Name)
	}
}

func TestScheduler_RestoreLoadsTasks(t *testing.T) {
	store := newFakeStore()
	store.tasks["7"] = Task{
		ID:    "7",
		Name:  "restored-task",
		State: StateStopped,
		Start: StartConfig{Mode: StartModeLatest},
		Source: SourceConfig{
			Host: "127.0.0.1",
			Port: 3306,
			User: "repl",
		},
	}

	s := NewScheduler(WithStore(store))
	if err := s.Restore(context.Background()); err != nil {
		t.Fatalf("Restore returned error: %v", err)
	}

	got, err := s.GetTask("7")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.Name != "restored-task" {
		t.Fatalf("unexpected restored task name: %s", got.Name)
	}

	newTask, err := s.CreateTask("next", "next-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if newTask.ID != "8" {
		t.Fatalf("expected next id 8, got %s", newTask.ID)
	}
}

func TestScheduler_CreateTaskReturnsErrorWhenStoreFails(t *testing.T) {
	store := newFakeStore()
	store.upsertErr = errors.New("store unavailable")
	s := NewScheduler(WithStore(store))

	_, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err == nil {
		t.Fatal("expected error when store upsert fails")
	}
}
