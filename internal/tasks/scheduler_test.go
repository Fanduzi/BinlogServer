// Package tasks provides module-level functionality for tasks.
// input: task commands/events, runner callbacks, store/lease/uploader dependencies
// output: task state transitions, primary-key GetTask/GetCheckpoint refresh, STARTING-unowned claim, and execution coordination
// pos: core domain orchestration layer governing backup task lifecycle and policies
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/binlog"
)

func TestScheduler_CreateTaskFromSpecDoesNotPersistOnInvalidStart(t *testing.T) {
	s := NewScheduler()
	source := SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret"}
	start := StartConfig{Mode: StartModeGTID}
	_, err := s.CreateTaskFromSpec("repro-400-create", "repro-400-create", &source, &start, nil)
	if !errors.Is(err, ErrGTIDSetRequired) {
		t.Fatalf("expected ErrGTIDSetRequired, got %v", err)
	}
	if got := len(s.ListTasks()); got != 0 {
		t.Fatalf("expected no persisted task, got %d", got)
	}
}

func TestScheduler_StartTaskFromFailed(t *testing.T) {
	s := NewScheduler(WithRunner(&fakeRunner{started: make(chan Task, 1)}))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret"}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}
	s.mu.Lock()
	if err := s.markFailedLocked(task.ID, "SOURCE_ACCESS_DENIED: denied"); err != nil {
		s.mu.Unlock()
		t.Fatalf("markFailedLocked: %v", err)
	}
	s.mu.Unlock()
	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask from FAILED: %v", err)
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State != StateStarting && got.State != StateRunning {
		t.Fatalf("expected STARTING/RUNNING after restart, got %s", got.State)
	}
	if got.LastError != "" {
		t.Fatalf("expected last_error cleared, got %q", got.LastError)
	}
}

// TestScheduler_StartTaskTransitionsToRunning 验证相关行为。
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

// TestScheduler_CreateTaskRequiresClusterKey 验证相关行为。
func TestScheduler_CreateTaskRequiresClusterKey(t *testing.T) {
	s := NewScheduler()
	if _, err := s.CreateTask("cluster-a", ""); err != ErrClusterKeyRequired {
		t.Fatalf("expected ErrClusterKeyRequired, got %v", err)
	}
}

// TestScheduler_CreateTaskRejectsInvalidName 验证相关行为。
func TestScheduler_CreateTaskRejectsInvalidName(t *testing.T) {
	s := NewScheduler()
	longName := strings.Repeat("a", 256)
	if _, err := s.CreateTask(longName, "cluster-a-key"); err == nil {
		t.Fatal("expected invalid name error, got nil")
	}
}

// TestScheduler_ConfigureNameRejectsInvalidName 验证相关行为。
func TestScheduler_ConfigureNameRejectsInvalidName(t *testing.T) {
	s := NewScheduler()
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := s.ConfigureName(task.ID, strings.Repeat("b", 256)); err == nil {
		t.Fatal("expected invalid name error, got nil")
	}
}

// TestScheduler_CreateTaskRejectsDuplicateClusterKey 验证相关行为。
func TestScheduler_CreateTaskRejectsDuplicateClusterKey(t *testing.T) {
	s := NewScheduler()
	if _, err := s.CreateTask("cluster-a", "dup-key"); err != nil {
		t.Fatalf("first CreateTask returned error: %v", err)
	}
	if _, err := s.CreateTask("cluster-b", "dup-key"); err != ErrClusterKeyExists {
		t.Fatalf("expected ErrClusterKeyExists, got %v", err)
	}
}

// TestScheduler_CreateTaskRejectsInvalidClusterKey 验证相关行为。
func TestScheduler_CreateTaskRejectsInvalidClusterKey(t *testing.T) {
	s := NewScheduler()
	cases := []string{
		"../x",
		"a/b",
		`a\b`,
		"a b",
		"a@b",
	}
	for _, clusterKey := range cases {
		_, err := s.CreateTask("cluster-a", clusterKey)
		if !errors.Is(err, ErrInvalidClusterKey) {
			t.Fatalf("cluster_key=%q expected ErrInvalidClusterKey, got %v", clusterKey, err)
		}
	}
}

// TestScheduler_ConfigureClusterKeyRejectsInvalidClusterKey 验证相关行为。
func TestScheduler_ConfigureClusterKeyRejectsInvalidClusterKey(t *testing.T) {
	s := NewScheduler()
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}

	cases := []string{
		"../x",
		"a/b",
		`a\b`,
		"a b",
		"a@b",
	}
	for _, clusterKey := range cases {
		err := s.ConfigureClusterKey(task.ID, clusterKey)
		if !errors.Is(err, ErrInvalidClusterKey) {
			t.Fatalf("cluster_key=%q expected ErrInvalidClusterKey, got %v", clusterKey, err)
		}
	}
}

// TestScheduler_ConfigureSourceRejectsInvalidHostAndFlavor 验证相关行为。
func TestScheduler_ConfigureSourceRejectsInvalidHostAndFlavor(t *testing.T) {
	s := NewScheduler()
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}

	if err := s.ConfigureSource(task.ID, SourceConfig{
		Host:   "bad host",
		Port:   3306,
		User:   "repl",
		Flavor: "mysql",
	}); !errors.Is(err, ErrInvalidSourceConfig) {
		t.Fatalf("expected ErrInvalidSourceConfig for invalid host, got %v", err)
	}

	if err := s.ConfigureSource(task.ID, SourceConfig{
		Host:   "127.0.0.1",
		Port:   3306,
		User:   "repl",
		Flavor: "mysql@8",
	}); !errors.Is(err, ErrInvalidSourceConfig) {
		t.Fatalf("expected ErrInvalidSourceConfig for invalid flavor, got %v", err)
	}
}

// TestScheduler_ConfigureStartRejectsInvalidMode 验证相关行为。
func TestScheduler_ConfigureStartRejectsInvalidMode(t *testing.T) {
	s := NewScheduler()
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}

	if err := s.ConfigureStart(task.ID, StartConfig{Mode: StartMode("BAD_MODE")}); err == nil {
		t.Fatal("expected invalid start mode error, got nil")
	}
}

// TestScheduler_ConfigureStorageRejectsInvalidRetentionDays 验证相关行为。
func TestScheduler_ConfigureStorageRejectsInvalidRetentionDays(t *testing.T) {
	s := NewScheduler()
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}

	if err := s.ConfigureStorage(task.ID, Storage{RetentionDays: 0}); err == nil {
		t.Fatal("expected invalid retention_days error for 0, got nil")
	}
	if err := s.ConfigureStorage(task.ID, Storage{RetentionDays: 3651}); err == nil {
		t.Fatal("expected invalid retention_days error for upper bound overflow, got nil")
	}
}

// TestScheduler_RetryableErrorTransitionsToBackoff 验证相关行为。
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

// TestScheduler_StopTaskTransitionsToStopped 验证相关行为。
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

// TestScheduler_StartTaskWithoutRunnerReturnsError 验证相关行为。
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

// TestScheduler_StartTaskWithoutRunnerInClusterDispatchMode 验证相关行为。
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

// TestScheduler_GetTaskRefreshesFromStore 验证相关行为。
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
	if got := store.ListTasksCalls(); got != 1 {
		t.Fatalf("expected CreateTask sync ListTasks once, got %d", got)
	}
	if got := store.GetTaskCalls(); got != 1 {
		t.Fatalf("expected GetTask to hit store.GetTask once, got %d", got)
	}
}

// TestScheduler_GetTaskUsesPrimaryKeyNotFullList 验证 GetTask 不走整表 ListTasks。
func TestScheduler_GetTaskUsesPrimaryKeyNotFullList(t *testing.T) {
	store := &schedulerTestStore{
		tasks: map[string]Task{
			"9": {ID: "9", Name: "remote", State: StateStopped},
		},
	}
	s := NewScheduler(WithStore(store))
	got, err := s.GetTask("9")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.Name != "remote" {
		t.Fatalf("unexpected task: %+v", got)
	}
	if got := store.ListTasksCalls(); got != 0 {
		t.Fatalf("GetTask must not call ListTasks, got %d", got)
	}
	if got := store.GetTaskCalls(); got != 1 {
		t.Fatalf("expected GetTask calls=1, got %d", got)
	}
}

type schedulerCheckpointReader struct {
	checkpoints map[string]binlog.Checkpoint
}

// LoadCheckpoint 实现对应功能逻辑。
func (r *schedulerCheckpointReader) LoadCheckpoint(_ context.Context, taskID string) (binlog.Checkpoint, bool, error) {
	cp, ok := r.checkpoints[taskID]
	return cp, ok, nil
}

// TestScheduler_GetCheckpointDoesNotRefreshTaskListForKnownTask 验证相关行为。
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
	if got := store.GetTaskCalls(); got != 0 {
		t.Fatalf("expected GetCheckpoint not to call GetTask for known task, got %d", got)
	}
}

// TestScheduler_GetCheckpointLoadsMissingTaskByPrimaryKey 验证缺失任务按主键补齐，不加载整表。
func TestScheduler_GetCheckpointLoadsMissingTaskByPrimaryKey(t *testing.T) {
	store := &schedulerTestStore{
		tasks: map[string]Task{
			"9": {ID: "9", Name: "remote", State: StateStopped},
		},
	}
	reader := &schedulerCheckpointReader{
		checkpoints: map[string]binlog.Checkpoint{
			"9": {File: "mysql-bin.000009", Pos: 4},
		},
	}
	s := NewScheduler(WithStore(store), WithCheckpointReader(reader))
	cp, ok, err := s.GetCheckpoint(context.Background(), "9")
	if err != nil {
		t.Fatalf("GetCheckpoint returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected checkpoint exists")
	}
	if cp.File != "mysql-bin.000009" {
		t.Fatalf("unexpected checkpoint: %+v", cp)
	}
	if got := store.ListTasksCalls(); got != 0 {
		t.Fatalf("GetCheckpoint must not call ListTasks, got %d", got)
	}
	if got := store.GetTaskCalls(); got != 1 {
		t.Fatalf("expected GetTask calls=1, got %d", got)
	}
}

// TestScheduler_ClaimStartingTasksClaimsDispatchedTask 验证相关行为。
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
	if got := store.ListStartingUnownedCalls(); got != 1 {
		t.Fatalf("expected ListStartingUnownedTasks once, got %d", got)
	}
	if got := store.ListTasksCalls(); got != 1 {
		t.Fatalf("expected ListTasks only from CreateTask sync, got %d", got)
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

// TestScheduler_ClaimStartingTasksDoesNotListAllTasks 验证认领只查询 STARTING 空 owner，不走整表 ListTasks。
func TestScheduler_ClaimStartingTasksDoesNotListAllTasks(t *testing.T) {
	store := &schedulerTestStore{
		tasks: map[string]Task{
			"1": {
				ID:     "1",
				Name:   "dispatch",
				State:  StateStarting,
				Source: SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"},
			},
			"2": {
				ID:            "2",
				Name:          "owned",
				State:         StateStarting,
				OwnerWorkerID: "worker-b",
				Source:        SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"},
			},
			"3": {
				ID:     "3",
				Name:   "running",
				State:  StateRunning,
				Source: SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"},
			},
		},
	}
	lease := &fakeLeaseManager{
		acquireEpoch: 3,
		acquireOK:    true,
	}
	worker := NewScheduler(
		WithStore(store),
		WithRunner(&fakeRunner{started: make(chan Task, 1)}),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-a"),
	)

	claimed, err := worker.ClaimStartingTasks()
	if err != nil {
		t.Fatalf("ClaimStartingTasks returned error: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("expected claimed=1, got %d", claimed)
	}
	if got := store.ListTasksCalls(); got != 0 {
		t.Fatalf("ClaimStartingTasks must not call ListTasks, got %d", got)
	}
	if got := store.ListStartingUnownedCalls(); got != 1 {
		t.Fatalf("expected ListStartingUnownedTasks once, got %d", got)
	}
}

// TestScheduler_StartTaskRejectsNonDispatchStartingTask 验证相关行为。
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

// TestScheduler_UpdateTaskAppliesValidPatchAtomically 验证相关行为。
func TestScheduler_UpdateTaskAppliesValidPatchAtomically(t *testing.T) {
	store := &schedulerTestStore{
		tasks: make(map[string]Task),
	}
	s := NewScheduler(WithStore(store))

	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Flavor: "mysql", ServerID: 200001}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}

	nextName := "cluster-a-updated"
	nextSource := SourceConfig{Host: "127.0.0.1", Port: 3307, User: "repl2", Flavor: "mysql", ServerID: 200002}
	nextStart := StartConfig{Mode: StartModeFilePos, File: "mysql-bin.000001", Pos: 4}
	nextStorage := Storage{RetentionDays: 30}
	updated, err := s.UpdateTask(task.ID, TaskPatch{
		Name:       &nextName,
		ClusterKey: "cluster-a-key-updated",
		Source:     &nextSource,
		Start:      &nextStart,
		Storage:    &nextStorage,
	})
	if err != nil {
		t.Fatalf("UpdateTask returned error: %v", err)
	}

	if updated.Name != nextName {
		t.Fatalf("expected name %q, got %q", nextName, updated.Name)
	}
	if updated.ClusterKey != "cluster-a-key-updated" {
		t.Fatalf("expected cluster_key updated, got %q", updated.ClusterKey)
	}
	if updated.Source.Port != 3307 || updated.Start.Mode != StartModeFilePos || updated.Storage.RetentionDays != 30 {
		t.Fatalf("expected source/start/storage updated, got source=%+v start=%+v storage=%+v", updated.Source, updated.Start, updated.Storage)
	}
}

// TestScheduler_UpdateTaskRejectsInvalidPatchWithoutMutation 验证相关行为。
func TestScheduler_UpdateTaskRejectsInvalidPatchWithoutMutation(t *testing.T) {
	store := &schedulerTestStore{
		tasks: make(map[string]Task),
	}
	s := NewScheduler(WithStore(store))

	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Flavor: "mysql", ServerID: 200001}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}
	original, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}

	nextName := "cluster-b"
	_, err = s.UpdateTask(task.ID, TaskPatch{
		Name:       &nextName,
		ClusterKey: "cluster-b-key",
		Start:      &StartConfig{Mode: StartMode("BAD_MODE")},
	})
	if err == nil {
		t.Fatal("expected invalid patch error, got nil")
	}

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.Name != original.Name || got.ClusterKey != original.ClusterKey || got.Source != original.Source || got.Start != original.Start || got.Storage != original.Storage {
		t.Fatalf("task mutated after invalid patch, got=%+v want=%+v", got, original)
	}
}

// TestScheduler_UpdateTaskStoreFailureHasNoMutation 验证相关行为。
func TestScheduler_UpdateTaskStoreFailureHasNoMutation(t *testing.T) {
	store := &schedulerTestStore{
		tasks: make(map[string]Task),
	}
	s := NewScheduler(WithStore(store))

	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Flavor: "mysql", ServerID: 200001}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}
	original, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}

	nextName := "cluster-b"
	nextSource := SourceConfig{Host: "127.0.0.1", Port: 3307, User: "repl2", Flavor: "mysql", ServerID: 200002}
	nextStorage := Storage{RetentionDays: 30}
	store.SetUpsertErr(errors.New("store unavailable"))
	_, err = s.UpdateTask(task.ID, TaskPatch{
		Name:       &nextName,
		ClusterKey: "cluster-b-key",
		Source:     &nextSource,
		Storage:    &nextStorage,
	})
	if err == nil {
		t.Fatal("expected store error, got nil")
	}

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.Name != original.Name || got.ClusterKey != original.ClusterKey || got.Source != original.Source || got.Start != original.Start || got.Storage != original.Storage {
		t.Fatalf("task mutated after store failure, got=%+v want=%+v", got, original)
	}
}

type schedulerTestStore struct {
	mu                      sync.Mutex
	tasks                   map[string]Task
	listTasksCall           int
	getTaskCall             int
	listStartingUnownedCall int
	upsertErr               error
}

// UpsertTask 实现对应功能逻辑。
func (s *schedulerTestStore) UpsertTask(_ context.Context, task Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.upsertErr != nil {
		return s.upsertErr
	}
	s.tasks[task.ID] = task
	return nil
}

func (s *schedulerTestStore) snapshotLocked() []Task {
	out := make([]Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		out = append(out, task)
	}
	return out
}

// GetTask 实现对应功能逻辑。
func (s *schedulerTestStore) GetTask(_ context.Context, taskID string) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getTaskCall++
	task, ok := s.tasks[taskID]
	if !ok {
		return Task{}, ErrTaskNotFound
	}
	return task, nil
}

// ListTasks 实现对应功能逻辑。
func (s *schedulerTestStore) ListTasks(_ context.Context) ([]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listTasksCall++
	return s.snapshotLocked(), nil
}

// ListTasksPage 实现对应功能逻辑。
func (s *schedulerTestStore) ListTasksPage(_ context.Context, filter TaskListFilter) ([]Task, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	page, total := PageTasks(s.snapshotLocked(), filter)
	return page, total, nil
}

// ListStartingUnownedTasks 实现对应功能逻辑。
func (s *schedulerTestStore) ListStartingUnownedTasks(_ context.Context) ([]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listStartingUnownedCall++
	return StartingUnownedTasks(s.snapshotLocked()), nil
}

// DeleteTask 实现对应功能逻辑。
func (s *schedulerTestStore) DeleteTask(_ context.Context, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, taskID)
	return nil
}

// ListTasksCalls 实现对应功能逻辑。
func (s *schedulerTestStore) ListTasksCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listTasksCall
}

// GetTaskCalls 实现对应功能逻辑。
func (s *schedulerTestStore) GetTaskCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getTaskCall
}

// ListStartingUnownedCalls 实现对应功能逻辑。
func (s *schedulerTestStore) ListStartingUnownedCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listStartingUnownedCall
}

// SetUpsertErr 实现对应功能逻辑。
func (s *schedulerTestStore) SetUpsertErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upsertErr = err
}
