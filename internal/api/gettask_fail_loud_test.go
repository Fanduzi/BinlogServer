// Package api provides module-level functionality for api.
// input: HTTP GET /api/tasks/{id}, scheduler with TaskStore, in-memory ownership copy
// output: store-not-found is 404, other store errors are 5xx, no-store still reads memory
// pos: control-plane HTTP seam tests for fail-loud task reads
// note: if this file changes, update this header and module README.md.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"binlog_server/internal/tasks"
)

type getTaskFailLoudStore struct {
	tasks  map[string]tasks.Task
	getErr error
}

func newGetTaskFailLoudStore(task tasks.Task) *getTaskFailLoudStore {
	return &getTaskFailLoudStore{
		tasks: map[string]tasks.Task{task.ID: task},
	}
}

func (s *getTaskFailLoudStore) snapshot() []tasks.Task {
	out := make([]tasks.Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		out = append(out, task)
	}
	return out
}

func (s *getTaskFailLoudStore) UpsertTask(_ context.Context, task tasks.Task) error {
	s.tasks[task.ID] = task
	return nil
}

func (s *getTaskFailLoudStore) GetTask(_ context.Context, taskID string) (tasks.Task, error) {
	if s.getErr != nil {
		return tasks.Task{}, s.getErr
	}
	task, ok := s.tasks[taskID]
	if !ok {
		return tasks.Task{}, tasks.ErrTaskNotFound
	}
	return task, nil
}

func (s *getTaskFailLoudStore) ListTasks(_ context.Context) ([]tasks.Task, error) {
	return s.snapshot(), nil
}

func (s *getTaskFailLoudStore) ListTasksPage(_ context.Context, filter tasks.TaskListFilter) ([]tasks.Task, int, error) {
	page, total := tasks.PageTasks(s.snapshot(), filter)
	return page, total, nil
}

func (s *getTaskFailLoudStore) ListStartingUnownedTasks(_ context.Context) ([]tasks.Task, error) {
	return tasks.StartingUnownedTasks(s.snapshot()), nil
}

func (s *getTaskFailLoudStore) DeleteTask(_ context.Context, taskID string) error {
	delete(s.tasks, taskID)
	return nil
}

func staleOwnershipTask() tasks.Task {
	return tasks.Task{
		ID:            "1",
		Name:          "stale-copy",
		ClusterKey:    "stale-copy-key",
		State:         tasks.StateRunning,
		OwnerWorkerID: "stale-worker",
		Epoch:         7,
		Source:        tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"},
	}
}

func restoreSchedulerWithStore(t *testing.T, store tasks.TaskStore) *tasks.Scheduler {
	t.Helper()
	scheduler := tasks.NewScheduler(tasks.WithStore(store))
	if err := scheduler.Restore(context.Background()); err != nil {
		t.Fatalf("Restore returned error: %v", err)
	}
	return scheduler
}

func getTaskByID(handler http.Handler, id string) *httptest.ResponseRecorder {
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/tasks/"+id, nil))
	return resp
}

func TestTaskAPI_GetTaskStoreErrorDoesNotReturnStaleCopy(t *testing.T) {
	store := newGetTaskFailLoudStore(staleOwnershipTask())
	handler := NewServer(restoreSchedulerWithStore(t, store))
	store.getErr = errors.New("store unavailable")

	resp := getTaskByID(handler, "1")
	if resp.Code == http.StatusOK {
		var task tasks.Task
		if err := json.Unmarshal(resp.Body.Bytes(), &task); err == nil &&
			task.OwnerWorkerID == "stale-worker" && task.Epoch == 7 {
			t.Fatalf("store error returned 200 stale ownership copy: %+v", task)
		}
		t.Fatalf("store error returned 200, want failure; body=%s", resp.Body.String())
	}
	if resp.Code < 500 {
		t.Fatalf("store error returned %d body=%s, want 5xx", resp.Code, resp.Body.String())
	}
}

func TestTaskAPI_GetTaskStoreNotFoundDoesNotReturnStaleCopy(t *testing.T) {
	store := newGetTaskFailLoudStore(staleOwnershipTask())
	handler := NewServer(restoreSchedulerWithStore(t, store))
	delete(store.tasks, "1")

	resp := getTaskByID(handler, "1")
	if resp.Code == http.StatusOK {
		var task tasks.Task
		if err := json.Unmarshal(resp.Body.Bytes(), &task); err == nil && task.ID == "1" {
			t.Fatalf("store not-found returned 200 stale task: %+v", task)
		}
		t.Fatalf("store not-found returned 200, want 404; body=%s", resp.Body.String())
	}
	if resp.Code != http.StatusNotFound {
		t.Fatalf("store not-found returned %d body=%s, want 404", resp.Code, resp.Body.String())
	}
}

func TestTaskAPI_GetTaskWithoutStoreReadsMemory(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)
	task, err := scheduler.CreateTask("memory-only", "memory-only-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}

	resp := getTaskByID(handler, task.ID)
	if resp.Code != http.StatusOK {
		t.Fatalf("no-store get returned %d body=%s, want 200", resp.Code, resp.Body.String())
	}
	var got tasks.Task
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get task failed: %v", err)
	}
	if got.ID != task.ID || got.Name != "memory-only" || got.ClusterKey != "memory-only-key" {
		t.Fatalf("no-store get returned %+v, want id=%s name=memory-only", got, task.ID)
	}
}
