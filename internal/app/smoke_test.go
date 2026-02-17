package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/config"
	"binlog_server/internal/replication"
	"binlog_server/internal/tasks"
)

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

func (m *appLeaseManager) Acquire(_ context.Context, _ string, _ string, _ time.Time, _ time.Duration) (int64, bool, error) {
	return m.acquireEpoch, m.acquireOK, nil
}

func (m *appLeaseManager) Renew(_ context.Context, _ string, _ string, _ int64, _ time.Time, _ time.Duration) (bool, error) {
	return true, nil
}

func (m *appLeaseManager) Release(_ context.Context, _ string, _ string, _ int64) (bool, error) {
	return true, nil
}

type appLeaseVerifier struct{}

func (v *appLeaseVerifier) VerifyLease(_ context.Context, _ tasks.Task) (bool, error) {
	return true, nil
}

type appFakeRunner struct {
	started chan tasks.Task
}

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

func newAppFakeStore() *appFakeStore {
	return &appFakeStore{tasks: make(map[string]tasks.Task)}
}

func (s *appFakeStore) UpsertTask(_ context.Context, task tasks.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[task.ID] = task
	return nil
}

func (s *appFakeStore) ListTasks(_ context.Context) ([]tasks.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]tasks.Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		out = append(out, task)
	}
	return out, nil
}

func (s *appFakeStore) DeleteTask(_ context.Context, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, taskID)
	return nil
}

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

	opts, runnerOpts := applyClusterRuntimeOptions(cfg, leaseManager, leaseVerifier, nil, nil)

	s := tasks.NewScheduler(append(opts, tasks.WithRunner(&appFakeRunner{started: make(chan tasks.Task, 1)}))...)
	task, err := s.CreateTask("cluster-a")
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

type fakeResumeScheduler struct {
	items      []tasks.Task
	stopErr    map[string]error
	startErr   map[string]error
	stopCalls  []string
	startCalls []string
}

func (f *fakeResumeScheduler) ListTasks() []tasks.Task {
	return append([]tasks.Task(nil), f.items...)
}

func (f *fakeResumeScheduler) StopTask(id string) error {
	f.stopCalls = append(f.stopCalls, id)
	if err, ok := f.stopErr[id]; ok {
		return err
	}
	return nil
}

func (f *fakeResumeScheduler) StartTask(id string) error {
	f.startCalls = append(f.startCalls, id)
	if err, ok := f.startErr[id]; ok {
		return err
	}
	return nil
}

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

func waitReady(t *testing.T, a *App) {
	t.Helper()
	select {
	case <-a.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("app did not become ready in time")
	}
}

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
