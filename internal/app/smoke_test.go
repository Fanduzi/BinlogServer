// Package app provides module-level functionality for app.
// input: runtime config, scheduler/runner/meta store dependencies, process context
// output: application lifecycle control including startup, role wiring, and shutdown
// pos: application composition layer that wires modules into runnable service modes
// note: if this file changes, update this header and module README.md.
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/config"
	"binlog_server/internal/replication"
	"binlog_server/internal/tasks"
)

type fakeAppMetaStore struct {
	regMu sync.Mutex

	acquireOK      bool
	acquireCalls   int
	renewOK        bool
	renewErr       error
	renewCalls     int
	releaseCalls   int
	heartbeats     []tasks.WorkerHeartbeat
	listTasks      []tasks.Task
	waitRenewCalls chan struct{}
}

func (s *fakeAppMetaStore) Close() error { return nil }

func (s *fakeAppMetaStore) UpsertTask(_ context.Context, task tasks.Task) error {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	replaced := false
	for i := range s.listTasks {
		if s.listTasks[i].ID == task.ID {
			s.listTasks[i] = task
			replaced = true
			break
		}
	}
	if !replaced {
		s.listTasks = append(s.listTasks, task)
	}
	return nil
}

func (s *fakeAppMetaStore) ListTasks(_ context.Context) ([]tasks.Task, error) {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	return append([]tasks.Task(nil), s.listTasks...), nil
}

func (s *fakeAppMetaStore) DeleteTask(_ context.Context, taskID string) error {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	filtered := s.listTasks[:0]
	for _, task := range s.listTasks {
		if task.ID != taskID {
			filtered = append(filtered, task)
		}
	}
	s.listTasks = filtered
	return nil
}

func (s *fakeAppMetaStore) LoadCheckpoint(_ context.Context, _ string) (binlog.Checkpoint, bool, error) {
	return binlog.Checkpoint{}, false, nil
}

func (s *fakeAppMetaStore) UpsertCheckpoint(_ context.Context, _ string, _ binlog.Checkpoint) error {
	return nil
}

func (s *fakeAppMetaStore) AppendEvent(_ context.Context, _ tasks.TaskEvent) error { return nil }

func (s *fakeAppMetaStore) ListEvents(_ context.Context, _ string, _ int) ([]tasks.TaskEvent, error) {
	return nil, nil
}

func (s *fakeAppMetaStore) UpsertBinlogFile(_ context.Context, _ tasks.BinlogFile) error { return nil }

func (s *fakeAppMetaStore) ListBinlogFiles(_ context.Context, _ string, _ int) ([]tasks.BinlogFile, error) {
	return nil, nil
}

func (s *fakeAppMetaStore) CountUploadFailures(_ context.Context) (int64, error) { return 0, nil }

func (s *fakeAppMetaStore) ListUploadFailureReasons(_ context.Context, _ string, _ int) ([]tasks.UploadFailureReason, error) {
	return nil, nil
}

func (s *fakeAppMetaStore) ListTaskRuns(_ context.Context, _ string, _ int) ([]tasks.TaskRun, error) {
	return nil, nil
}

func (s *fakeAppMetaStore) ListWorkerHeartbeats(_ context.Context, _ int) ([]tasks.WorkerHeartbeat, error) {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	return append([]tasks.WorkerHeartbeat(nil), s.heartbeats...), nil
}

func (s *fakeAppMetaStore) UpsertWorkerHeartbeat(_ context.Context, hb tasks.WorkerHeartbeat) error {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	s.heartbeats = append(s.heartbeats, hb)
	return nil
}

func (s *fakeAppMetaStore) AcquireWorkerRegistration(_ context.Context, _, _ string, _ time.Duration) (bool, error) {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	s.acquireCalls++
	return s.acquireOK, nil
}

func (s *fakeAppMetaStore) RenewWorkerRegistration(_ context.Context, _, _ string, _ time.Duration) (bool, error) {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	s.renewCalls++
	if s.waitRenewCalls != nil {
		select {
		case s.waitRenewCalls <- struct{}{}:
		default:
		}
	}
	return s.renewOK, s.renewErr
}

func (s *fakeAppMetaStore) ReleaseWorkerRegistration(_ context.Context, _, _ string) error {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	s.releaseCalls++
	return nil
}

func (s *fakeAppMetaStore) snapshot() (acquireCalls, renewCalls, releaseCalls int, heartbeats []tasks.WorkerHeartbeat) {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	return s.acquireCalls, s.renewCalls, s.releaseCalls, append([]tasks.WorkerHeartbeat(nil), s.heartbeats...)
}

// TestApp_StartAndServeHealth 验证相关行为。
func TestApp_StartAndServeHealth(t *testing.T) {
	cfg := config.Config{ListenAddr: "127.0.0.1:0"}
	a := New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	select {
	case <-a.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("app did not become ready in time")
	}

	resp, err := http.Get("http://" + a.Addr() + "/healthz")
	if err != nil {
		t.Fatalf("health request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("app returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("app did not shut down in time")
	}
}

// TestApp_ClusterControlPlaneRole 验证相关行为。
func TestApp_ClusterControlPlaneRole(t *testing.T) {
	cfg := config.Config{
		ListenAddr: "127.0.0.1:0",
		Mode:       "cluster",
		Cluster: config.ClusterConfig{
			Role: "control-plane",
		},
	}
	a := New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	waitReady(t, a)

	if a.Addr() == "" {
		t.Fatal("expected control-plane role to expose api listener")
	}
	assertHTTPStatus(t, "http://"+a.Addr()+"/healthz", http.StatusOK)

	createBody := `{
		"name":"cluster-a",
		"cluster_key":"cluster-a-key",
		"source":{"host":"127.0.0.1","port":3306,"user":"repl"}
	}`
	createResp := postJSON(t, "http://"+a.Addr()+"/api/tasks", createBody)
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected create status 201, got %d body=%s", createResp.StatusCode, string(createResp.Body))
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createResp.Body, &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	startResp := postJSON(t, "http://"+a.Addr()+"/api/tasks/"+created.ID+"/start", "")
	if startResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected start status 400 in control-plane role, got %d body=%s", startResp.StatusCode, string(startResp.Body))
	}

	cancel()
	waitRunExit(t, errCh)
}

// TestApp_ClusterWorkerRole 验证相关行为。
func TestApp_ClusterWorkerRole(t *testing.T) {
	cfg := config.Config{
		ListenAddr: "127.0.0.1:0",
		Mode:       "cluster",
		Cluster: config.ClusterConfig{
			Role: "worker",
		},
	}
	a := New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	waitReady(t, a)

	if a.Addr() != "" {
		t.Fatalf("expected worker role to skip control-plane listener, got addr=%s", a.Addr())
	}

	cancel()
	waitRunExit(t, errCh)
}

// TestApp_ClusterWorkerRoleWithHealthProbe 验证相关行为。
func TestApp_ClusterWorkerRoleWithHealthProbe(t *testing.T) {
	cfg := config.Config{
		ListenAddr: "127.0.0.1:0",
		Mode:       "cluster",
		Cluster: config.ClusterConfig{
			Role:                   "worker",
			WorkerHealthListenAddr: "127.0.0.1:0",
		},
	}
	a := New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	waitReady(t, a)

	if a.Addr() != "" {
		t.Fatalf("expected worker role to skip control-plane listener, got addr=%s", a.Addr())
	}
	if a.WorkerHealthAddr() == "" {
		t.Fatal("expected worker health probe listener to be exposed")
	}

	assertHTTPStatus(t, "http://"+a.WorkerHealthAddr()+"/healthz", http.StatusOK)
	assertHTTPStatus(t, "http://"+a.WorkerHealthAddr()+"/readyz", http.StatusOK)
	assertHTTPStatus(t, "http://"+a.WorkerHealthAddr()+"/api/tasks", http.StatusNotFound)

	cancel()
	waitRunExit(t, errCh)
}

// TestApp_ClusterAllInOneRole 验证相关行为。
func TestApp_ClusterAllInOneRole(t *testing.T) {
	cfg := config.Config{
		ListenAddr: "127.0.0.1:0",
		Mode:       "cluster",
		Cluster: config.ClusterConfig{
			Role: "all-in-one",
		},
	}
	a := New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	waitReady(t, a)

	if a.Addr() == "" {
		t.Fatal("expected all-in-one role to expose api listener")
	}
	assertHTTPStatus(t, "http://"+a.Addr()+"/healthz", http.StatusOK)

	createBody := `{
		"name":"cluster-a",
		"cluster_key":"cluster-a-key",
		"source":{"host":"127.0.0.1","port":3306,"user":"repl"}
	}`
	createResp := postJSON(t, "http://"+a.Addr()+"/api/tasks", createBody)
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected create status 201, got %d body=%s", createResp.StatusCode, string(createResp.Body))
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createResp.Body, &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	startResp := postJSON(t, "http://"+a.Addr()+"/api/tasks/"+created.ID+"/start", "")
	if startResp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected start status 204 in all-in-one role, got %d body=%s", startResp.StatusCode, string(startResp.Body))
	}

	cancel()
	waitRunExit(t, errCh)
}

type appLeaseManager struct {
	acquireOK    bool
	acquireEpoch int64
}

// Acquire 实现对应功能逻辑。
func (m *appLeaseManager) Acquire(_ context.Context, _ string, _ string, _ time.Duration) (int64, bool, error) {
	return m.acquireEpoch, m.acquireOK, nil
}

// Renew 实现对应功能逻辑。
func (m *appLeaseManager) Renew(_ context.Context, _ string, _ string, _ int64, _ time.Time, _ time.Duration) (bool, error) {
	return true, nil
}

// Release 实现对应功能逻辑。
func (m *appLeaseManager) Release(_ context.Context, _ string, _ string, _ int64) (bool, error) {
	return true, nil
}

type appLeaseVerifier struct{}

// VerifyLease 实现对应功能逻辑。
func (v *appLeaseVerifier) VerifyLease(_ context.Context, _ tasks.Task) (bool, error) {
	return true, nil
}

type appRunLeaseManager struct {
	acquireOK    bool
	acquireEpoch int64
}

func (m *appRunLeaseManager) Acquire(_ context.Context, _ string, _ string, _ time.Duration) (int64, bool, error) {
	return m.acquireEpoch, m.acquireOK, nil
}

func (m *appRunLeaseManager) Renew(_ context.Context, _ string, _ string, _ int64, _ time.Time, _ time.Duration) (bool, error) {
	return true, nil
}

func (m *appRunLeaseManager) Release(_ context.Context, _ string, _ string, _ int64) (bool, error) {
	return true, nil
}

type appFakeRunner struct {
	started chan tasks.Task
}

// Run 实现对应功能逻辑。
func (r *appFakeRunner) Run(ctx context.Context, task tasks.Task) error {
	select {
	case r.started <- task:
	default:
	}
	<-ctx.Done()
	return context.Canceled
}

type appFakeStore struct {
	mu    sync.Mutex
	tasks map[string]tasks.Task
}

// newAppFakeStore 实现对应功能逻辑。
func newAppFakeStore() *appFakeStore {
	return &appFakeStore{tasks: make(map[string]tasks.Task)}
}

// UpsertTask 实现对应功能逻辑。
func (s *appFakeStore) UpsertTask(_ context.Context, task tasks.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[task.ID] = task
	return nil
}

// ListTasks 实现对应功能逻辑。
func (s *appFakeStore) ListTasks(_ context.Context) ([]tasks.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]tasks.Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		out = append(out, task)
	}
	return out, nil
}

// DeleteTask 实现对应功能逻辑。
func (s *appFakeStore) DeleteTask(_ context.Context, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, taskID)
	return nil
}

// TestApp_ClusterRuntimeOptionsWireLeaseAndVerifier 验证相关行为。
func TestApp_ClusterRuntimeOptionsWireLeaseAndVerifier(t *testing.T) {
	cfg := config.Config{
		Mode: "cluster",
		Cluster: config.ClusterConfig{
			Role:                  "all-in-one",
			WorkerID:              "worker-a",
			LeaseTTLSec:           12,
			LeaseRenewIntervalSec: 4,
			LeaseGraceSec:         20,
		},
	}
	leaseManager := &appLeaseManager{acquireOK: false, acquireEpoch: 7}
	leaseVerifier := &appLeaseVerifier{}

	opts, runnerOpts := applyClusterRuntimeOptions(cfg, "worker-a", leaseManager, leaseVerifier, nil, nil)

	s := tasks.NewScheduler(append(opts, tasks.WithRunner(&appFakeRunner{started: make(chan tasks.Task, 1)}))...)
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := s.ConfigureSource(task.ID, tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}
	err = s.StartTask(task.ID)
	if !errors.Is(err, tasks.ErrLeaseNotAcquired) {
		t.Fatalf("expected ErrLeaseNotAcquired with wired lease manager, got %v", err)
	}

	runner := replication.NewMySQLRunner(t.TempDir(), runnerOpts...)
	field := reflect.ValueOf(runner).Elem().FieldByName("leaseVerifier")
	if !field.IsValid() || field.IsNil() {
		t.Fatal("expected runner lease verifier to be wired in cluster mode")
	}
}

// TestApp_ResumeClusterWorkerTasksStartsRecoveredActiveTasks 验证相关行为。
func TestApp_ResumeClusterWorkerTasksStartsRecoveredActiveTasks(t *testing.T) {
	store := newAppFakeStore()
	store.tasks["1"] = tasks.Task{
		ID:            "1",
		Name:          "active-task",
		State:         tasks.StateRunning,
		OwnerWorkerID: "worker-a",
		Epoch:         9,
		Source: tasks.SourceConfig{
			Host: "127.0.0.1",
			Port: 3306,
			User: "repl",
		},
		Start: tasks.StartConfig{Mode: tasks.StartModeLatest},
	}
	store.tasks["2"] = tasks.Task{
		ID:    "2",
		Name:  "created-task",
		State: tasks.StateCreated,
		Source: tasks.SourceConfig{
			Host: "127.0.0.1",
			Port: 3307,
			User: "repl",
		},
		Start: tasks.StartConfig{Mode: tasks.StartModeLatest},
	}

	runner := &appFakeRunner{started: make(chan tasks.Task, 4)}
	s := tasks.NewScheduler(
		tasks.WithStore(store),
		tasks.WithRunner(runner),
		tasks.WithClusterLeaseManager(&appLeaseManager{acquireOK: true, acquireEpoch: 9}),
		tasks.WithClusterWorkerID("worker-a"),
	)
	if err := s.Restore(context.Background()); err != nil {
		t.Fatalf("Restore returned error: %v", err)
	}

	stats := resumeClusterWorkerTasks(s)
	if stats.Considered != 1 || stats.Resumed != 1 || stats.StopErrors != 0 || stats.StartErrors != 0 {
		t.Fatalf("unexpected resume stats: %+v", stats)
	}

	select {
	case started := <-runner.started:
		if started.ID != "1" {
			t.Fatalf("expected recovered active task 1 to start, got %s", started.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected recovered active task to start worker loop")
	}

	select {
	case started := <-runner.started:
		t.Fatalf("expected created task not auto-started, got task=%s", started.ID)
	case <-time.After(150 * time.Millisecond):
	}
}

type appHeartbeatCaptureSink struct {
	mu    sync.Mutex
	items []tasks.WorkerHeartbeat
	ch    chan struct{}
}

// UpsertWorkerHeartbeat 实现对应功能逻辑。
func (s *appHeartbeatCaptureSink) UpsertWorkerHeartbeat(_ context.Context, hb tasks.WorkerHeartbeat) error {
	s.mu.Lock()
	s.items = append(s.items, hb)
	s.mu.Unlock()
	select {
	case s.ch <- struct{}{}:
	default:
	}
	return nil
}

// latest 实现对应功能逻辑。
func (s *appHeartbeatCaptureSink) latest() (tasks.WorkerHeartbeat, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items) == 0 {
		return tasks.WorkerHeartbeat{}, false
	}
	return s.items[len(s.items)-1], true
}

type fakeWorkerRegistrationStore struct {
	renewOK bool
	calls   int
}

// AcquireWorkerRegistration 实现对应功能逻辑。
func (s *fakeWorkerRegistrationStore) AcquireWorkerRegistration(_ context.Context, _, _ string, _ time.Duration) (bool, error) {
	return true, nil
}

// RenewWorkerRegistration 实现对应功能逻辑。
func (s *fakeWorkerRegistrationStore) RenewWorkerRegistration(_ context.Context, _, _ string, _ time.Duration) (bool, error) {
	s.calls++
	return s.renewOK, nil
}

// ReleaseWorkerRegistration 实现对应功能逻辑。
func (s *fakeWorkerRegistrationStore) ReleaseWorkerRegistration(_ context.Context, _, _ string) error {
	return nil
}

// TestResolveClusterWorkerID_AutoGeneratedStable 验证相关行为。
func TestResolveClusterWorkerID_AutoGeneratedStable(t *testing.T) {
	cfg := config.Config{
		DataDir: t.TempDir(),
		Mode:    "cluster",
		Cluster: config.ClusterConfig{
			Role: "worker",
		},
	}

	id1, err := resolveClusterWorkerID(cfg)
	if err != nil {
		t.Fatalf("resolveClusterWorkerID returned error: %v", err)
	}
	if strings.TrimSpace(id1) == "" {
		t.Fatal("expected auto generated worker_id not empty")
	}
	if id1 == "worker-default" {
		t.Fatalf("expected auto generated worker_id not fallback default, got %q", id1)
	}
	if len(id1) > 128 {
		t.Fatalf("expected worker_id length <= 128, got %d", len(id1))
	}

	fileData, err := os.ReadFile(filepath.Join(cfg.DataDir, workerIDFileName))
	if err != nil {
		t.Fatalf("read worker id file failed: %v", err)
	}
	if strings.TrimSpace(string(fileData)) != id1 {
		t.Fatalf("worker id file mismatch: file=%q id=%q", strings.TrimSpace(string(fileData)), id1)
	}

	id2, err := resolveClusterWorkerID(cfg)
	if err != nil {
		t.Fatalf("resolveClusterWorkerID second call returned error: %v", err)
	}
	if id2 != id1 {
		t.Fatalf("expected stable worker_id, got id1=%q id2=%q", id1, id2)
	}
}

// TestResolveClusterWorkerID_ConfigValueTakesPriority 验证相关行为。
func TestResolveClusterWorkerID_ConfigValueTakesPriority(t *testing.T) {
	cfg := config.Config{
		DataDir: t.TempDir(),
		Mode:    "cluster",
		Cluster: config.ClusterConfig{
			Role:     "worker",
			WorkerID: "  worker-manual  ",
		},
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, workerIDFileName), []byte("from-file"), 0o644); err != nil {
		t.Fatalf("prepare worker id file failed: %v", err)
	}

	id, err := resolveClusterWorkerID(cfg)
	if err != nil {
		t.Fatalf("resolveClusterWorkerID returned error: %v", err)
	}
	if id != "worker-manual" {
		t.Fatalf("expected config worker_id takes priority, got %q", id)
	}
}

// TestApp_ClusterWorkerIDUsedConsistentlyBySchedulerAndHeartbeat 验证相关行为。
func TestApp_ClusterWorkerIDUsedConsistentlyBySchedulerAndHeartbeat(t *testing.T) {
	workerID := "worker-consistent-a"
	cfg := config.Config{
		Mode: "cluster",
		Cluster: config.ClusterConfig{
			Role:                  "all-in-one",
			LeaseTTLSec:           12,
			LeaseRenewIntervalSec: 4,
			LeaseGraceSec:         20,
		},
	}
	leaseManager := &appLeaseManager{acquireOK: true, acquireEpoch: 7}
	opts, _ := applyClusterRuntimeOptions(cfg, workerID, leaseManager, nil, nil, nil)

	runner := &appFakeRunner{started: make(chan tasks.Task, 1)}
	s := tasks.NewScheduler(append(opts, tasks.WithRunner(runner))...)
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := s.ConfigureSource(task.ID, tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}
	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask returned error: %v", err)
	}

	startedTask, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if startedTask.OwnerWorkerID != workerID {
		t.Fatalf("expected scheduler owner worker_id=%q, got %q", workerID, startedTask.OwnerWorkerID)
	}

	sink := &appHeartbeatCaptureSink{ch: make(chan struct{}, 1)}
	hbCtx, hbCancel := context.WithCancel(context.Background())
	defer hbCancel()
	go startWorkerHeartbeatLoop(hbCtx, sink, workerID, "host-a", "v1", 10*time.Millisecond)

	select {
	case <-sink.ch:
	case <-time.After(2 * time.Second):
		t.Fatal("expected heartbeat report")
	}
	hb, ok := sink.latest()
	if !ok {
		t.Fatal("expected heartbeat item")
	}
	if hb.WorkerID != startedTask.OwnerWorkerID {
		t.Fatalf("expected heartbeat worker_id equals scheduler owner_worker_id, hb=%q task=%q", hb.WorkerID, startedTask.OwnerWorkerID)
	}
}

// TestStartWorkerRegistrationRenewLoop_TriggersOwnershipLostCallback 验证相关行为。
func TestStartWorkerRegistrationRenewLoop_TriggersOwnershipLostCallback(t *testing.T) {
	store := &fakeWorkerRegistrationStore{renewOK: false}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lostCh := make(chan struct{}, 1)
	go startWorkerRegistrationRenewLoop(
		ctx,
		store,
		"worker-a",
		"session-a",
		10*time.Millisecond,
		30*time.Second,
		func() {
			select {
			case lostCh <- struct{}{}:
			default:
			}
		},
	)

	select {
	case <-lostCh:
	case <-time.After(2 * time.Second):
		t.Fatal("expected ownership lost callback triggered when renew returns false")
	}
	if store.calls == 0 {
		t.Fatal("expected renew called at least once")
	}
}

// TestApp_RunRejectsDuplicateWorkerRegistrationOwnership 验证已有 active session 持有时 duplicate worker_id 启动被拒绝。
func TestApp_RunRejectsDuplicateWorkerRegistrationOwnership(t *testing.T) {
	store := &fakeAppMetaStore{
		acquireOK: false,
		renewOK:   true,
	}
	restoreMetaStoreFactory := newAppMetaStoreForRun
	newAppMetaStoreForRun = func(_ config.Config) (appMetaStore, error) {
		return store, nil
	}
	defer func() { newAppMetaStoreForRun = restoreMetaStoreFactory }()

	cfg := config.Config{
		DataDir:    t.TempDir(),
		MetaDSN:    "fake-meta",
		Mode:       "cluster",
		ListenAddr: "127.0.0.1:0",
		Cluster: config.ClusterConfig{
			Role:                  "worker",
			WorkerID:              "worker-dup-a",
			LeaseTTLSec:           1,
			LeaseRenewIntervalSec: 1,
			LeaseGraceSec:         2,
		},
	}

	err := New(cfg).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "worker_id is already in use") {
		t.Fatalf("expected duplicate worker_id rejection, got %v", err)
	}
	acquireCalls, renewCalls, releaseCalls, _ := store.snapshot()
	if acquireCalls != 1 || renewCalls != 0 || releaseCalls != 0 {
		t.Fatalf("unexpected registration calls acquire=%d renew=%d release=%d", acquireCalls, renewCalls, releaseCalls)
	}
}

// TestApp_RunWorkerRoleReturnsOwnershipLostAndKeepsControlPlaneOff 验证 cluster+worker 失租后退出健康 worker posture 且不启 control-plane。
func TestApp_RunWorkerRoleReturnsOwnershipLostAndKeepsControlPlaneOff(t *testing.T) {
	store := &fakeAppMetaStore{
		acquireOK:      true,
		renewOK:        false,
		waitRenewCalls: make(chan struct{}, 1),
	}
	restoreMetaStoreFactory := newAppMetaStoreForRun
	newAppMetaStoreForRun = func(_ config.Config) (appMetaStore, error) {
		return store, nil
	}
	defer func() { newAppMetaStoreForRun = restoreMetaStoreFactory }()

	cfg := config.Config{
		DataDir:    t.TempDir(),
		MetaDSN:    "fake-meta",
		Mode:       "cluster",
		ListenAddr: "127.0.0.1:0",
		Cluster: config.ClusterConfig{
			Role:                   "worker",
			WorkerID:               "worker-owned-a",
			WorkerHealthListenAddr: "127.0.0.1:0",
			LeaseTTLSec:            1,
			LeaseRenewIntervalSec:  1,
			LeaseGraceSec:          2,
		},
	}
	a := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- a.Run(ctx) }()

	waitReady(t, a)
	if a.Addr() != "" {
		t.Fatalf("expected worker role to keep control-plane off, got addr=%q", a.Addr())
	}
	if a.WorkerHealthAddr() == "" {
		t.Fatal("expected worker health probe to be exposed before ownership loss")
	}

	waitForSignal(t, store.waitRenewCalls, 2*time.Second, "renew call")
	err := waitErr(t, errCh, 3*time.Second)
	if !errors.Is(err, errWorkerRegistrationOwnershipLost) {
		t.Fatalf("expected errWorkerRegistrationOwnershipLost, got %v", err)
	}
}

// TestApp_RunAllInOneRoleReturnsOwnershipLostButKeepsRoleSemantics 验证 cluster+all-in-one 保持既有语义但不忽略 registration ownership loss。
func TestApp_RunAllInOneRoleReturnsOwnershipLostButKeepsRoleSemantics(t *testing.T) {
	store := &fakeAppMetaStore{
		acquireOK:      true,
		renewOK:        false,
		waitRenewCalls: make(chan struct{}, 1),
	}
	restoreMetaStoreFactory := newAppMetaStoreForRun
	newAppMetaStoreForRun = func(_ config.Config) (appMetaStore, error) {
		return store, nil
	}
	defer func() { newAppMetaStoreForRun = restoreMetaStoreFactory }()

	cfg := config.Config{
		DataDir:    t.TempDir(),
		MetaDSN:    "fake-meta",
		Mode:       "cluster",
		ListenAddr: "127.0.0.1:0",
		Cluster: config.ClusterConfig{
			Role:                  "all-in-one",
			WorkerID:              "worker-all-in-one-a",
			LeaseTTLSec:           1,
			LeaseRenewIntervalSec: 1,
			LeaseGraceSec:         2,
		},
	}
	a := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- a.Run(ctx) }()

	waitReady(t, a)
	if a.Addr() == "" {
		t.Fatal("expected all-in-one role to still expose control-plane listener")
	}
	assertHTTPStatus(t, "http://"+a.Addr()+"/healthz", http.StatusOK)

	waitForSignal(t, store.waitRenewCalls, 2*time.Second, "renew call")
	err := waitErr(t, errCh, 3*time.Second)
	if !errors.Is(err, errWorkerRegistrationOwnershipLost) {
		t.Fatalf("expected errWorkerRegistrationOwnershipLost, got %v", err)
	}
}

// TestApp_RunWorkerIdentityStaysCoherentForActiveSession 验证 task/heartbeat worker identity 与 active session 保持一致。
func TestApp_RunWorkerIdentityStaysCoherentForActiveSession(t *testing.T) {
	store := &fakeAppMetaStore{
		acquireOK: true,
		renewOK:   true,
	}
	restoreMetaStoreFactory := newAppMetaStoreForRun
	newAppMetaStoreForRun = func(_ config.Config) (appMetaStore, error) {
		return store, nil
	}
	defer func() { newAppMetaStoreForRun = restoreMetaStoreFactory }()

	restoreNewRunner := newRunnerForRun
	started := make(chan tasks.Task, 1)
	newRunnerForRun = func(_ config.Config, opts ...replication.RunnerOption) tasks.Runner {
		return &appFakeRunner{started: started}
	}
	defer func() { newRunnerForRun = restoreNewRunner }()
	restoreLeaseRuntime := newClusterLeaseRuntimeForRun
	newClusterLeaseRuntimeForRun = func(_ appMetaStore) (tasks.LeaseManager, replication.LeaseVerifier) {
		return &appRunLeaseManager{acquireOK: true, acquireEpoch: 7}, &appLeaseVerifier{}
	}
	defer func() { newClusterLeaseRuntimeForRun = restoreLeaseRuntime }()

	cfg := config.Config{
		DataDir:    t.TempDir(),
		MetaDSN:    "fake-meta",
		Mode:       "cluster",
		ListenAddr: "127.0.0.1:0",
		Cluster: config.ClusterConfig{
			Role:                  "all-in-one",
			WorkerID:              "worker-session-coherent-a",
			LeaseTTLSec:           5,
			LeaseRenewIntervalSec: 1,
			LeaseGraceSec:         2,
		},
	}
	a := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- a.Run(ctx) }()
	waitReady(t, a)

	taskBody := `{"name":"cluster-a","cluster_key":"cluster-a-key","source":{"host":"127.0.0.1","port":3306,"user":"repl"}}`
	createResp := postJSON(t, "http://"+a.Addr()+"/api/tasks", taskBody)
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected create status 201, got %d body=%s", createResp.StatusCode, string(createResp.Body))
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createResp.Body, &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	startResp := postJSON(t, "http://"+a.Addr()+"/api/tasks/"+created.ID+"/start", "")
	if startResp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected start status 204, got %d body=%s", startResp.StatusCode, string(startResp.Body))
	}

	var startedTask tasks.Task
	select {
	case startedTask = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("expected runner start")
	}
	if startedTask.OwnerWorkerID != cfg.Cluster.WorkerID {
		t.Fatalf("expected started task owner=%q, got %q", cfg.Cluster.WorkerID, startedTask.OwnerWorkerID)
	}

	waitForHeartbeatWorkerID(t, store, cfg.Cluster.WorkerID, 2*time.Second)

	cancel()
	err := waitErr(t, errCh, 3*time.Second)
	if err != nil {
		t.Fatalf("expected clean shutdown, got %v", err)
	}
}

type fakeResumeScheduler struct {
	items      []tasks.Task
	stopErr    map[string]error
	startErr   map[string]error
	stopCalls  []string
	startCalls []string
}

// ListTasks 实现对应功能逻辑。
func (f *fakeResumeScheduler) ListTasks() []tasks.Task {
	return append([]tasks.Task(nil), f.items...)
}

// StopTask 实现对应功能逻辑。
func (f *fakeResumeScheduler) StopTask(id string) error {
	f.stopCalls = append(f.stopCalls, id)
	if err, ok := f.stopErr[id]; ok {
		return err
	}
	return nil
}

// StartTask 实现对应功能逻辑。
func (f *fakeResumeScheduler) StartTask(id string) error {
	f.startCalls = append(f.startCalls, id)
	if err, ok := f.startErr[id]; ok {
		return err
	}
	return nil
}

// TestApp_ResumeClusterWorkerTasksLogsAndCountsErrors 验证相关行为。
func TestApp_ResumeClusterWorkerTasksLogsAndCountsErrors(t *testing.T) {
	resumer := &fakeResumeScheduler{
		items: []tasks.Task{
			{ID: "1", State: tasks.StateRunning},
			{ID: "2", State: tasks.StateLeaseDegraded},
		},
		stopErr: map[string]error{
			"1": errors.New("stop failed"),
		},
		startErr: map[string]error{
			"2": errors.New("start failed"),
		},
	}

	var buf bytes.Buffer
	origWriter := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(origWriter)

	stats := resumeClusterWorkerTasks(resumer)
	if stats.Considered != 2 || stats.Resumed != 0 || stats.StopErrors != 1 || stats.StartErrors != 1 {
		t.Fatalf("unexpected resume stats: %+v", stats)
	}
	if !strings.Contains(buf.String(), "task=1") || !strings.Contains(buf.String(), "task=2") {
		t.Fatalf("expected error logs contain task ids, got logs=%q", buf.String())
	}
}

// waitReady 实现对应功能逻辑。
func waitReady(t *testing.T, a *App) {
	t.Helper()
	select {
	case <-a.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("app did not become ready in time")
	}
}

// waitRunExit 实现对应功能逻辑。
func waitRunExit(t *testing.T, errCh <-chan error) {
	t.Helper()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("app returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("app did not shut down in time")
	}
}

func waitErr(t *testing.T, errCh <-chan error, timeout time.Duration) error {
	t.Helper()
	select {
	case err := <-errCh:
		return err
	case <-time.After(timeout):
		t.Fatal("app did not shut down in time")
		return nil
	}
}

func waitForSignal(t *testing.T, ch <-chan struct{}, timeout time.Duration, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(timeout):
		t.Fatalf("expected %s in time", label)
	}
}

func waitForHeartbeatWorkerID(t *testing.T, store *fakeAppMetaStore, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		_, _, _, heartbeats := store.snapshot()
		for _, hb := range heartbeats {
			if hb.WorkerID == want && hb.Status == "ONLINE" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected heartbeat worker_id=%q", want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// assertHTTPStatus 实现对应功能逻辑。
func assertHTTPStatus(t *testing.T, url string, want int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("request %s failed: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected %d, got %d body=%s", want, resp.StatusCode, string(body))
	}
}

type httpResult struct {
	StatusCode int
	Body       []byte
}

// postJSON 实现对应功能逻辑。
func postJSON(t *testing.T, url string, body string) httpResult {
	t.Helper()
	reqBody := bytes.NewBufferString(body)
	resp, err := http.Post(url, "application/json", reqBody)
	if err != nil {
		t.Fatalf("post %s failed: %v", url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return httpResult{
		StatusCode: resp.StatusCode,
		Body:       data,
	}
}
