package tasks

import (
	"context"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/binlog"
)

func TestScheduler_StartTaskTransitionsToRunning(t *testing.T) {
	s := NewScheduler(WithRunner(&fakeRunner{started: make(chan Task, 1)}))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
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

func TestScheduler_CreateTaskRequiresClusterKey(t *testing.T) {
	s := NewScheduler()
	if _, err := s.CreateTask("cluster-a", ""); err != ErrClusterKeyRequired {
		t.Fatalf("expected ErrClusterKeyRequired, got %v", err)
	}
}

func TestScheduler_CreateTaskRejectsDuplicateClusterKey(t *testing.T) {
	s := NewScheduler()
	if _, err := s.CreateTask("cluster-a", "dup-key"); err != nil {
		t.Fatalf("first CreateTask returned error: %v", err)
	}
	if _, err := s.CreateTask("cluster-b", "dup-key"); err != ErrClusterKeyExists {
		t.Fatalf("expected ErrClusterKeyExists, got %v", err)
	}
}

func TestScheduler_RetryableErrorTransitionsToBackoff(t *testing.T) {
	s := NewScheduler(WithRunner(&fakeRunner{started: make(chan Task, 1)}))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
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
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
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
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
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
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
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

	task, err := s.CreateTask("cluster-a", "cluster-a-key")
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

type schedulerCheckpointReader struct {
	checkpoints map[string]binlog.Checkpoint
}

func (r *schedulerCheckpointReader) LoadCheckpoint(_ context.Context, taskID string) (binlog.Checkpoint, bool, error) {
	cp, ok := r.checkpoints[taskID]
	return cp, ok, nil
}

func TestScheduler_GetCheckpointDoesNotRefreshTaskListForKnownTask(t *testing.T) {
	store := &schedulerTestStore{
		tasks: make(map[string]Task),
	}
	reader := &schedulerCheckpointReader{
		checkpoints: map[string]binlog.Checkpoint{},
	}
	s := NewScheduler(WithStore(store), WithCheckpointReader(reader))

	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	reader.checkpoints[task.ID] = binlog.Checkpoint{
		File:      "mysql-bin.000001",
		Pos:       4,
		UpdatedAt: time.Now(),
	}

	_, ok, err := s.GetCheckpoint(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetCheckpoint returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected checkpoint exists")
	}
	if got := store.ListTasksCalls(); got != 1 {
		t.Fatalf("expected store ListTasks called once by CreateTask sync, got %d", got)
	}

	_, _, err = s.GetCheckpoint(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetCheckpoint returned error: %v", err)
	}
	if got := store.ListTasksCalls(); got != 1 {
		t.Fatalf("expected GetCheckpoint not to trigger extra ListTasks for known task, got %d", got)
	}
}

func TestScheduler_ClaimStartingTasksClaimsDispatchedTask(t *testing.T) {
	store := &schedulerTestStore{
		tasks: make(map[string]Task),
	}
	lease := &fakeLeaseManager{
		acquireEpoch: 9,
		acquireOK:    true,
	}

	control := NewScheduler(
		WithStore(store),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("control-plane-a"),
	)
	workerRunner := &fakeRunner{started: make(chan Task, 1)}
	worker := NewScheduler(
		WithStore(store),
		WithRunner(workerRunner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-a"),
	)

	task, err := control.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := control.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}
	if err := control.StartTask(task.ID); err != nil {
		t.Fatalf("control StartTask returned error: %v", err)
	}

	claimed, err := worker.ClaimStartingTasks()
	if err != nil {
		t.Fatalf("ClaimStartingTasks returned error: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("expected claimed=1, got %d", claimed)
	}

	select {
	case <-workerRunner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("expected worker runner to start claimed task")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := worker.GetTask(task.ID)
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

func TestScheduler_StartTaskRejectsNonDispatchStartingTask(t *testing.T) {
	lease := &fakeLeaseManager{
		acquireEpoch: 11,
		acquireOK:    true,
	}
	s := NewScheduler(
		WithRunner(&fakeRunner{started: make(chan Task, 1)}),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-a"),
	)

	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}

	// 非 dispatch STARTING（owner/epoch/run_id 不为空）不应被 Claim/Start 放行。
	s.mu.Lock()
	nonDispatch := s.tasks[task.ID]
	nonDispatch.State = StateStarting
	nonDispatch.OwnerWorkerID = "worker-old"
	nonDispatch.Epoch = 7
	nonDispatch.RunID = "run-old"
	s.tasks[task.ID] = nonDispatch
	s.mu.Unlock()

	err = s.StartTask(task.ID)
	if err == nil {
		t.Fatal("expected cannot start from state STARTING, got nil")
	}
	if err.Error() != "cannot start from state STARTING" {
		t.Fatalf("expected cannot-start error, got %v", err)
	}
}

type schedulerTestStore struct {
	mu            sync.Mutex
	tasks         map[string]Task
	listTasksCall int
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
	s.listTasksCall++
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

func (s *schedulerTestStore) ListTasksCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listTasksCall
}
