// Package api provides module-level functionality for api.
// input: HTTP requests, router params, scheduler/task service interfaces
// output: REST API responses and status codes for task/cluster operations
// pos: external control-plane API layer bridging clients and domain services
// note: if this file changes, update this header and module README.md.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type fakeAPIRunner struct{}

// Run 实现对应功能逻辑。
func (r *fakeAPIRunner) Run(_ context.Context, _ tasks.Task) error {
	return nil
}

type fakeAPILeaseManager struct {
	epoch int64
}

// Acquire 实现对应功能逻辑。
func (m *fakeAPILeaseManager) Acquire(_ context.Context, _ string, _ string, _ time.Duration) (int64, bool, error) {
	return m.epoch, true, nil
}

// Renew 实现对应功能逻辑。
func (m *fakeAPILeaseManager) Renew(_ context.Context, _ string, _ string, _ int64, _ time.Time, _ time.Duration) (bool, error) {
	return true, nil
}

// Release 实现对应功能逻辑。
func (m *fakeAPILeaseManager) Release(_ context.Context, _ string, _ string, _ int64) (bool, error) {
	return true, nil
}

type fakeAPIRunHistoryStore struct {
	tasks     map[string]tasks.Task
	runs      map[string][]tasks.TaskRun
	workers   []tasks.WorkerHeartbeat
	lastLimit int
}

// newFakeAPIRunHistoryStore 实现对应功能逻辑。
func newFakeAPIRunHistoryStore() *fakeAPIRunHistoryStore {
	return &fakeAPIRunHistoryStore{
		tasks: make(map[string]tasks.Task),
		runs:  make(map[string][]tasks.TaskRun),
	}
}

// UpsertTask 实现对应功能逻辑。
func (s *fakeAPIRunHistoryStore) UpsertTask(_ context.Context, task tasks.Task) error {
	s.tasks[task.ID] = task
	return nil
}

// ListTasks 实现对应功能逻辑。
func (s *fakeAPIRunHistoryStore) ListTasks(_ context.Context) ([]tasks.Task, error) {
	out := make([]tasks.Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		out = append(out, task)
	}
	return out, nil
}

// DeleteTask 实现对应功能逻辑。
func (s *fakeAPIRunHistoryStore) DeleteTask(_ context.Context, taskID string) error {
	delete(s.tasks, taskID)
	delete(s.runs, taskID)
	return nil
}

// ListTaskRuns 实现对应功能逻辑。
func (s *fakeAPIRunHistoryStore) ListTaskRuns(_ context.Context, taskID string, limit int) ([]tasks.TaskRun, error) {
	s.lastLimit = limit
	rows := s.runs[taskID]
	if limit <= 0 || limit >= len(rows) {
		out := make([]tasks.TaskRun, len(rows))
		copy(out, rows)
		return out, nil
	}
	out := make([]tasks.TaskRun, limit)
	copy(out, rows[:limit])
	return out, nil
}

// UpsertWorkerHeartbeat 实现对应功能逻辑。
func (s *fakeAPIRunHistoryStore) UpsertWorkerHeartbeat(_ context.Context, hb tasks.WorkerHeartbeat) error {
	for i := range s.workers {
		if s.workers[i].WorkerID == hb.WorkerID {
			s.workers[i] = hb
			return nil
		}
	}
	s.workers = append(s.workers, hb)
	return nil
}

// ListWorkerHeartbeats 实现对应功能逻辑。
func (s *fakeAPIRunHistoryStore) ListWorkerHeartbeats(_ context.Context, _ int) ([]tasks.WorkerHeartbeat, error) {
	out := make([]tasks.WorkerHeartbeat, len(s.workers))
	copy(out, s.workers)
	return out, nil
}

// TestTaskAPI_CreateListStartStop 验证相关行为。
func TestTaskAPI_CreateListStartStop(t *testing.T) {
	scheduler := tasks.NewScheduler(tasks.WithRunner(&fakeAPIRunner{}))
	handler := NewServer(scheduler)

	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{
		"name":"cluster-a",
		"cluster_key":"cluster-a-key",
		"source":{"host":"127.0.0.1","port":3306,"user":"repl","flavor":"mysql","server_id":200001}
	}`))
	createReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", createResp.Code, createResp.Body.String())
	}

	var created tasks.Task
	if err := json.Unmarshal(createResp.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected created task id")
	}

	listResp := httptest.NewRecorder()
	listReq := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	handler.ServeHTTP(listResp, listReq)
	if listResp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", listResp.Code)
	}

	startResp := httptest.NewRecorder()
	startReq := httptest.NewRequest(http.MethodPost, "/api/tasks/"+created.ID+"/start", nil)
	handler.ServeHTTP(startResp, startReq)
	if startResp.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d body=%s", startResp.Code, startResp.Body.String())
	}

	stopResp := httptest.NewRecorder()
	stopReq := httptest.NewRequest(http.MethodPost, "/api/tasks/"+created.ID+"/stop", nil)
	handler.ServeHTTP(stopResp, stopReq)
	if stopResp.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d body=%s", stopResp.Code, stopResp.Body.String())
	}

	finalListResp := httptest.NewRecorder()
	finalListReq := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	deadline := time.Now().Add(2 * time.Second)
	for {
		finalListResp = httptest.NewRecorder()
		handler.ServeHTTP(finalListResp, finalListReq)
		if finalListResp.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", finalListResp.Code)
		}

		var list []tasks.Task
		if err := json.Unmarshal(finalListResp.Body.Bytes(), &list); err != nil {
			t.Fatalf("decode list response: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("expected one task, got %d", len(list))
		}
		if list[0].State == tasks.StateStopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected final state %s, got %s", tasks.StateStopped, list[0].State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTaskAPI_CreateWithSourceAndStart 验证相关行为。
func TestTaskAPI_CreateWithSourceAndStart(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	reqBody := `{
		"name":"cluster-a",
		"cluster_key":"cluster-a-key",
		"source":{"host":"127.0.0.1","port":3306,"user":"repl","password":"secret","flavor":"mysql","server_id":200001},
		"start":{"mode":"FILE_POS","file":"mysql-bin.000001","pos":4},
		"storage":{"dir":"./data","retention_days":15}
	}`
	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(reqBody))
	createReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", createResp.Code, createResp.Body.String())
	}

	var created tasks.Task
	if err := json.Unmarshal(createResp.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.Source.Host != "127.0.0.1" || created.Source.User != "repl" {
		t.Fatalf("source not persisted: %+v", created.Source)
	}
	if created.Source.Password != "" {
		t.Fatalf("expected password hidden in response, got %q", created.Source.Password)
	}
	if created.Start.Mode != tasks.StartModeFilePos || created.Start.File != "mysql-bin.000001" || created.Start.Pos != 4 {
		t.Fatalf("start not persisted: %+v", created.Start)
	}
	if created.Storage.RetentionDays != 15 {
		t.Fatalf("storage not persisted: %+v", created.Storage)
	}

	stored, err := scheduler.GetTask(created.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if stored.Source.Password != "secret" {
		t.Fatalf("expected password stored internally, got %q", stored.Source.Password)
	}
}

// TestTaskAPI_CreateTaskRequiresClusterKey 验证相关行为。
func TestTaskAPI_CreateTaskRequiresClusterKey(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{
		"name":"cluster-a",
		"source":{"host":"127.0.0.1","port":3306,"user":"repl","flavor":"mysql","server_id":200001}
	}`))
	createReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(createResp, createReq)

	if createResp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when cluster_key missing, got %d body=%s", createResp.Code, createResp.Body.String())
	}
}

// TestTaskAPI_UpdateTaskRequiresClusterKey 验证相关行为。
func TestTaskAPI_UpdateTaskRequiresClusterKey(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{
		"name":"cluster-a",
		"cluster_key":"cluster-a-key",
		"source":{"host":"127.0.0.1","port":3306,"user":"repl","flavor":"mysql","server_id":200001}
	}`))
	createReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected create 201, got %d body=%s", createResp.Code, createResp.Body.String())
	}

	updateResp := httptest.NewRecorder()
	updateReq := httptest.NewRequest(http.MethodPut, "/api/tasks/1", bytes.NewBufferString(`{"name":"cluster-b"}`))
	updateReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(updateResp, updateReq)
	if updateResp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when cluster_key missing in update, got %d body=%s", updateResp.Code, updateResp.Body.String())
	}
}

// TestTaskAPI_ClusterKeyMustBeUnique 验证相关行为。
func TestTaskAPI_ClusterKeyMustBeUnique(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	first := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{
		"name":"cluster-a",
		"cluster_key":"dup-key",
		"source":{"host":"127.0.0.1","port":3306,"user":"repl","flavor":"mysql","server_id":200001}
	}`))
	firstReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(first, firstReq)
	if first.Code != http.StatusCreated {
		t.Fatalf("expected first create 201, got %d body=%s", first.Code, first.Body.String())
	}

	second := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{
		"name":"cluster-b",
		"cluster_key":"dup-key",
		"source":{"host":"127.0.0.1","port":3307,"user":"repl","flavor":"mysql","server_id":200002}
	}`))
	secondReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(second, secondReq)
	if second.Code != http.StatusBadRequest {
		t.Fatalf("expected duplicate cluster_key rejected with 400, got %d body=%s", second.Code, second.Body.String())
	}
}

// TestTaskAPI_CreateRejectsInvalidInput 验证相关行为。
func TestTaskAPI_CreateRejectsInvalidInput(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	cases := []string{
		`{"name":"` + strings.Repeat("a", 256) + `","cluster_key":"cluster-a-key"}`,
		`{"name":"cluster-a","cluster_key":"../bad"}`,
		`{"name":"cluster-a","cluster_key":"cluster-a-key","source":{"host":"bad host","port":3306,"user":"repl","flavor":"mysql","server_id":200001}}`,
		`{"name":"cluster-a","cluster_key":"cluster-a-key","start":{"mode":"BAD_MODE"}}`,
		`{"name":"cluster-a","cluster_key":"cluster-a-key","storage":{"retention_days":0}}`,
	}
	for _, body := range cases {
		resp := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for invalid create payload, got %d body=%s", resp.Code, resp.Body.String())
		}
	}
}

// TestTaskAPI_UpdateRejectsInvalidInput 验证相关行为。
func TestTaskAPI_UpdateRejectsInvalidInput(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{
		"name":"cluster-a",
		"cluster_key":"cluster-a-key",
		"source":{"host":"127.0.0.1","port":3306,"user":"repl","flavor":"mysql","server_id":200001}
	}`))
	createReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", createResp.Code, createResp.Body.String())
	}

	cases := []string{
		`{"name":"` + strings.Repeat("b", 256) + `","cluster_key":"cluster-a-key"}`,
		`{"name":"cluster-b","cluster_key":"../bad"}`,
		`{"name":"cluster-b","cluster_key":"cluster-a-key","source":{"host":"bad host","port":3306,"user":"repl","flavor":"mysql"}}`,
		`{"name":"cluster-b","cluster_key":"cluster-a-key","start":{"mode":"BAD_MODE"}}`,
		`{"name":"cluster-b","cluster_key":"cluster-a-key","storage":{"retention_days":3651}}`,
	}
	for _, body := range cases {
		resp := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/tasks/1", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for invalid update payload, got %d body=%s", resp.Code, resp.Body.String())
		}
	}
}

// TestTaskAPI_UpdateInvalidStartHasNoSideEffects 验证相关行为。
func TestTaskAPI_UpdateInvalidStartHasNoSideEffects(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{
		"name":"cluster-a",
		"cluster_key":"cluster-a-key",
		"source":{"host":"127.0.0.1","port":3306,"user":"repl","flavor":"mysql","server_id":200001}
	}`))
	createReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected create 201, got %d body=%s", createResp.Code, createResp.Body.String())
	}
	var created tasks.Task
	if err := json.Unmarshal(createResp.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create task failed: %v", err)
	}

	updateResp := httptest.NewRecorder()
	updateReq := httptest.NewRequest(http.MethodPut, "/api/tasks/1", bytes.NewBufferString(`{
		"name":"cluster-b",
		"cluster_key":"cluster-b-key",
		"source":{"host":"127.0.0.2","port":3307,"user":"other","flavor":"mysql","server_id":200002},
		"start":{"mode":"BAD_MODE"}
	}`))
	updateReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(updateResp, updateReq)
	if updateResp.Code != http.StatusBadRequest {
		t.Fatalf("expected update 400, got %d body=%s", updateResp.Code, updateResp.Body.String())
	}

	getResp := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/api/tasks/1", nil)
	handler.ServeHTTP(getResp, getReq)
	if getResp.Code != http.StatusOK {
		t.Fatalf("expected get 200, got %d body=%s", getResp.Code, getResp.Body.String())
	}

	var task tasks.Task
	if err := json.Unmarshal(getResp.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode get task failed: %v", err)
	}
	if task.ClusterKey != "cluster-a-key" {
		t.Fatalf("cluster_key changed after failed update: got %q", task.ClusterKey)
	}
	if task.Name != "cluster-a" {
		t.Fatalf("name changed after failed update: got %q", task.Name)
	}
	if task.Source != created.Source {
		t.Fatalf("source changed after failed update: got=%+v want=%+v", task.Source, created.Source)
	}
	if task.Start != created.Start {
		t.Fatalf("start changed after failed update: got=%+v want=%+v", task.Start, created.Start)
	}
	if task.Storage != created.Storage {
		t.Fatalf("storage changed after failed update: got=%+v want=%+v", task.Storage, created.Storage)
	}
}

// TestTaskAPI_UpdateTaskStoreFailureReturnsInternalServerError 验证相关行为。
func TestTaskAPI_UpdateTaskStoreFailureReturnsInternalServerError(t *testing.T) {
	store := newFailingUpdateStore()
	scheduler := tasks.NewScheduler(tasks.WithStore(store))
	handler := NewServer(scheduler)

	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{
		"name":"cluster-a",
		"cluster_key":"cluster-a-key",
		"source":{"host":"127.0.0.1","port":3306,"user":"repl","flavor":"mysql","server_id":200001}
	}`))
	createReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected create 201, got %d body=%s", createResp.Code, createResp.Body.String())
	}

	store.failUpsert = true
	updateResp := httptest.NewRecorder()
	updateReq := httptest.NewRequest(http.MethodPut, "/api/tasks/1", bytes.NewBufferString(`{
		"name":"cluster-b",
		"cluster_key":"cluster-b-key",
		"source":{"host":"127.0.0.1","port":3307,"user":"repl2","flavor":"mysql","server_id":200002}
	}`))
	updateReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(updateResp, updateReq)
	if updateResp.Code != http.StatusInternalServerError {
		t.Fatalf("expected update 500 on store failure, got %d body=%s", updateResp.Code, updateResp.Body.String())
	}
}

type fakeCheckpointReader struct {
	checkpoints map[string]binlog.Checkpoint
}

type failingUpdateStore struct {
	tasks      map[string]tasks.Task
	failUpsert bool
}

// newFailingUpdateStore 实现对应功能逻辑。
func newFailingUpdateStore() *failingUpdateStore {
	return &failingUpdateStore{tasks: make(map[string]tasks.Task)}
}

// UpsertTask 实现对应功能逻辑。
func (s *failingUpdateStore) UpsertTask(_ context.Context, task tasks.Task) error {
	if s.failUpsert {
		return errors.New("store unavailable")
	}
	s.tasks[task.ID] = task
	return nil
}

// ListTasks 实现对应功能逻辑。
func (s *failingUpdateStore) ListTasks(_ context.Context) ([]tasks.Task, error) {
	out := make([]tasks.Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		out = append(out, task)
	}
	return out, nil
}

// DeleteTask 实现对应功能逻辑。
func (s *failingUpdateStore) DeleteTask(_ context.Context, taskID string) error {
	delete(s.tasks, taskID)
	return nil
}

// LoadCheckpoint 实现对应功能逻辑。
func (f *fakeCheckpointReader) LoadCheckpoint(_ context.Context, taskID string) (binlog.Checkpoint, bool, error) {
	cp, ok := f.checkpoints[taskID]
	return cp, ok, nil
}

type fakeFileStore struct {
	files map[string][]tasks.BinlogFile
}

// newFakeFileStore 实现对应功能逻辑。
func newFakeFileStore() *fakeFileStore {
	return &fakeFileStore{files: make(map[string][]tasks.BinlogFile)}
}

// UpsertBinlogFile 实现对应功能逻辑。
func (f *fakeFileStore) UpsertBinlogFile(_ context.Context, meta tasks.BinlogFile) error {
	items := f.files[meta.TaskID]
	for i := range items {
		if items[i].FileName == meta.FileName {
			items[i] = meta
			f.files[meta.TaskID] = items
			return nil
		}
	}
	f.files[meta.TaskID] = append(items, meta)
	return nil
}

// ListBinlogFiles 实现对应功能逻辑。
func (f *fakeFileStore) ListBinlogFiles(_ context.Context, taskID string, limit int) ([]tasks.BinlogFile, error) {
	items := f.files[taskID]
	if limit <= 0 || limit >= len(items) {
		out := make([]tasks.BinlogFile, len(items))
		copy(out, items)
		return out, nil
	}
	out := make([]tasks.BinlogFile, limit)
	copy(out, items[len(items)-limit:])
	return out, nil
}

type fakeRetryUploader struct {
	errByObject map[string]error
}

// UploadFile 实现对应功能逻辑。
func (u *fakeRetryUploader) UploadFile(_ context.Context, _ string, localPath, objectKey string) error {
	if _, err := os.Stat(localPath); err != nil {
		return err
	}
	if u.errByObject != nil {
		if err := u.errByObject[objectKey]; err != nil {
			return err
		}
	}
	return nil
}

// TestTaskAPI_GetCheckpoint 验证相关行为。
func TestTaskAPI_GetCheckpoint(t *testing.T) {
	reader := &fakeCheckpointReader{
		checkpoints: map[string]binlog.Checkpoint{
			"1": {
				File: "mysql-bin.000123",
				Pos:  456,
			},
		},
	}
	scheduler := tasks.NewScheduler(tasks.WithCheckpointReader(reader))
	handler := NewServer(scheduler)

	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{"name":"cluster-a","cluster_key":"cluster-a-key"}`))
	createReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", createResp.Code)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/1/checkpoint", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	var cp binlog.Checkpoint
	if err := json.Unmarshal(resp.Body.Bytes(), &cp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if cp.File != "mysql-bin.000123" || cp.Pos != 456 {
		t.Fatalf("unexpected checkpoint: %+v", cp)
	}

	var raw map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw response: %v", err)
	}
	if _, ok := raw["file"]; !ok {
		t.Fatalf("expected lowercase key 'file', body=%s", resp.Body.String())
	}
	if _, ok := raw["pos"]; !ok {
		t.Fatalf("expected lowercase key 'pos', body=%s", resp.Body.String())
	}
}

// TestTaskAPI_UpdateAndDeleteTask 验证相关行为。
func TestTaskAPI_UpdateAndDeleteTask(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{"name":"cluster-a","cluster_key":"cluster-a-key"}`))
	createReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", createResp.Code)
	}

	updateBody := `{
		"name":"cluster-b",
		"cluster_key":"cluster-a-key",
		"source":{"host":"10.0.0.1","port":3306,"user":"repl","password":"secret","flavor":"mysql","server_id":300001},
		"start":{"mode":"GTID","gtid_set":"24BC785E-9A61-11E1-8A5D-080027635EF5:1-10"},
		"storage":{"retention_days":30}
	}`
	updateResp := httptest.NewRecorder()
	updateReq := httptest.NewRequest(http.MethodPut, "/api/tasks/1", bytes.NewBufferString(updateBody))
	updateReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(updateResp, updateReq)
	if updateResp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", updateResp.Code, updateResp.Body.String())
	}

	var updated tasks.Task
	if err := json.Unmarshal(updateResp.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	if updated.Name != "cluster-b" {
		t.Fatalf("expected name cluster-b, got %s", updated.Name)
	}
	if updated.Start.Mode != tasks.StartModeGTID || updated.Start.GTIDSet == "" {
		t.Fatalf("start not updated: %+v", updated.Start)
	}
	if updated.Storage.RetentionDays != 30 {
		t.Fatalf("storage not updated: %+v", updated.Storage)
	}
	if updated.Source.Password != "" {
		t.Fatalf("expected password hidden in response, got %q", updated.Source.Password)
	}

	updateNoPassword := `{
		"cluster_key":"cluster-a-key",
		"source":{"host":"10.0.0.1","port":3306,"user":"repl","flavor":"mysql","server_id":300001}
	}`
	updateNoPwdResp := httptest.NewRecorder()
	updateNoPwdReq := httptest.NewRequest(http.MethodPut, "/api/tasks/1", bytes.NewBufferString(updateNoPassword))
	updateNoPwdReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(updateNoPwdResp, updateNoPwdReq)
	if updateNoPwdResp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", updateNoPwdResp.Code, updateNoPwdResp.Body.String())
	}

	internalTask, err := scheduler.GetTask("1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if internalTask.Source.Password != "secret" {
		t.Fatalf("expected password preserved when omitted, got %q", internalTask.Source.Password)
	}

	getResp := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/api/tasks/1", nil)
	handler.ServeHTTP(getResp, getReq)
	if getResp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", getResp.Code)
	}

	deleteResp := httptest.NewRecorder()
	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/tasks/1", nil)
	handler.ServeHTTP(deleteResp, deleteReq)
	if deleteResp.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", deleteResp.Code)
	}

	getAfterDeleteResp := httptest.NewRecorder()
	getAfterDeleteReq := httptest.NewRequest(http.MethodGet, "/api/tasks/1", nil)
	handler.ServeHTTP(getAfterDeleteResp, getAfterDeleteReq)
	if getAfterDeleteResp.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", getAfterDeleteResp.Code)
	}
}

// TestAPI_MetricsEndpointContainsCoreMetrics 验证相关行为。
func TestAPI_MetricsEndpointContainsCoreMetrics(t *testing.T) {
	store := newFakeAPIRunHistoryStore()
	checkpointReader := &fakeCheckpointReader{
		checkpoints: map[string]binlog.Checkpoint{},
	}
	fileStore := newFakeFileStore()
	scheduler := tasks.NewScheduler(
		tasks.WithStore(store),
		tasks.WithCheckpointReader(checkpointReader),
		tasks.WithFileStore(fileStore),
	)
	handler := NewServer(scheduler)

	task, err := scheduler.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	scheduler.ReportReplicationProgress(task.ID, time.Now().Add(-3*time.Second), "mysql-bin.000001", 123)
	checkpointReader.checkpoints[task.ID] = binlog.Checkpoint{
		File:      "mysql-bin.000001",
		Pos:       123,
		UpdatedAt: time.Now().Add(-5 * time.Second),
	}
	if err := fileStore.UpsertBinlogFile(context.Background(), tasks.BinlogFile{
		TaskID:      task.ID,
		FileName:    "mysql-bin.000001",
		UploadState: "UPLOAD_FAILED",
	}); err != nil {
		t.Fatalf("UpsertBinlogFile returned error: %v", err)
	}
	store.workers = []tasks.WorkerHeartbeat{
		{
			WorkerID:   "worker-a",
			LastSeenAt: time.Now(),
			Status:     "ONLINE",
		},
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	body := resp.Body.String()
	required := []string{
		"binlog_server_task_state_count",
		"binlog_server_replication_lag_seconds",
		"binlog_server_checkpoint_age_seconds",
		"binlog_server_worker_online",
		"binlog_server_upload_failures_total",
		"binlog_server_upload_retry_total",
		"binlog_server_upload_retry_last_ts",
	}
	for _, name := range required {
		if !strings.Contains(body, name) {
			t.Fatalf("expected metrics output contains %s, body=%s", name, body)
		}
	}

	if !strings.Contains(body, `binlog_server_task_state_count{state="`+string(task.State)+`"}`) {
		t.Fatalf("expected task_state metric with state label, body=%s", body)
	}
	if !strings.Contains(body, `binlog_server_replication_lag_seconds{task_id="`+task.ID+`"}`) {
		t.Fatalf("expected replication lag metric with task_id label, body=%s", body)
	}
	if !strings.Contains(body, `binlog_server_checkpoint_age_seconds{task_id="`+task.ID+`"}`) {
		t.Fatalf("expected checkpoint age metric with task_id label, body=%s", body)
	}
	if !strings.Contains(body, `binlog_server_worker_online{worker_id="worker-a"} 1`) {
		t.Fatalf("expected worker_online metric with worker_id label, body=%s", body)
	}
	if !strings.Contains(body, `# TYPE binlog_server_upload_retry_total counter`) {
		t.Fatalf("expected upload_retry_total type counter, body=%s", body)
	}
}

// TestAPI_MetricsEndpointContainsCoreMetricsWithoutReplicationProgress 验证未上报复制进度时也暴露核心指标名。
func TestAPI_MetricsEndpointContainsCoreMetricsWithoutReplicationProgress(t *testing.T) {
	store := newFakeAPIRunHistoryStore()
	scheduler := tasks.NewScheduler(
		tasks.WithStore(store),
	)
	handler := NewServer(scheduler)

	if _, err := scheduler.CreateTask("cluster-a", "cluster-a-key"); err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	body := resp.Body.String()
	required := []string{
		"binlog_server_task_state_count",
		"binlog_server_replication_lag_seconds",
		"binlog_server_checkpoint_age_seconds",
		"binlog_server_worker_online",
		"binlog_server_upload_failures_total",
	}
	for _, name := range required {
		if !strings.Contains(body, name) {
			t.Fatalf("expected metrics output contains %s, body=%s", name, body)
		}
	}
}

// TestAPI_MetricsEndpointCoreMetricsExistOnEmptySystem 验证空系统下核心指标名仍可见。
func TestAPI_MetricsEndpointCoreMetricsExistOnEmptySystem(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	body := resp.Body.String()
	required := []string{
		"binlog_server_replication_lag_seconds",
		"binlog_server_checkpoint_age_seconds",
		"binlog_server_worker_online",
		"binlog_server_upload_failures_total",
	}
	for _, name := range required {
		if !strings.Contains(body, name) {
			t.Fatalf("expected metrics output contains %s, body=%s", name, body)
		}
	}
}

// TestAPI_MetricsUploadFailuresTotalCountsAllRecords 验证相关行为。
func TestAPI_MetricsUploadFailuresTotalCountsAllRecords(t *testing.T) {
	store := newFakeAPIRunHistoryStore()
	fileStore := newFakeFileStore()
	scheduler := tasks.NewScheduler(
		tasks.WithStore(store),
		tasks.WithFileStore(fileStore),
	)
	handler := NewServer(scheduler)

	task, err := scheduler.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}

	for i := 0; i < 201; i++ {
		if err := fileStore.UpsertBinlogFile(context.Background(), tasks.BinlogFile{
			TaskID:      task.ID,
			FileName:    fmt.Sprintf("mysql-bin.%06d", i),
			UploadState: "UPLOAD_FAILED",
		}); err != nil {
			t.Fatalf("UpsertBinlogFile returned error: %v", err)
		}
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	got, ok, err := readPromMetricValue(resp.Body.String(), "binlog_server_upload_failures_total")
	if err != nil {
		t.Fatalf("readPromMetricValue returned error: %v", err)
	}
	if !ok {
		t.Fatalf("missing metric binlog_server_upload_failures_total, body=%s", resp.Body.String())
	}
	if got != 201 {
		t.Fatalf("expected binlog_server_upload_failures_total=201, got %v", got)
	}
}

// TestAPI_TracingDisabledHasZeroIngressSpans 验证默认关闭时不产生入站 span。
func TestAPI_TracingDisabledHasZeroIngressSpans(t *testing.T) {
	prevProvider := otel.GetTracerProvider()
	defer otel.SetTracerProvider(prevProvider)

	spanRecorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider()
	tracerProvider.RegisterSpanProcessor(spanRecorder)
	otel.SetTracerProvider(tracerProvider)

	handler := NewServer(tasks.NewScheduler())
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}
	if got := len(spanRecorder.Ended()); got != 0 {
		t.Fatalf("expected no spans when tracing disabled, got %d", got)
	}
}

// TestAPI_TracingEnabledCreatesIngressSpan 验证启用后会生成 HTTP 入站 span。
func TestAPI_TracingEnabledCreatesIngressSpan(t *testing.T) {
	spanRecorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider()
	tracerProvider.RegisterSpanProcessor(spanRecorder)

	handler := NewServer(
		tasks.NewScheduler(),
		WithTracing(TracingConfig{
			Enabled:        true,
			ServiceName:    "binlog-server",
			TracerProvider: tracerProvider,
		}),
	)
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	ended := spanRecorder.Ended()
	if len(ended) == 0 {
		t.Fatal("expected at least one span when tracing enabled")
	}
	if ended[0].Name() != "GET /api/tasks" {
		t.Fatalf("unexpected span name: %s", ended[0].Name())
	}
}

// BenchmarkAPIHealthzTracingDisabled 评估 tracing 关闭时的请求开销基线。
func BenchmarkAPIHealthzTracingDisabled(b *testing.B) {
	handler := NewServer(tasks.NewScheduler())

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			b.Fatalf("expected 200, got %d", resp.Code)
		}
	}
}

// BenchmarkAPIHealthzTracingEnabled 评估 tracing 开启时的请求开销。
func BenchmarkAPIHealthzTracingEnabled(b *testing.B) {
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	handler := NewServer(
		tasks.NewScheduler(),
		WithTracing(TracingConfig{
			Enabled:        true,
			ServiceName:    "binlog-server",
			TracerProvider: tracerProvider,
		}),
	)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			b.Fatalf("expected 200, got %d", resp.Code)
		}
	}
}

// TestAPI_AuthMiddlewareProtectsMetricsAndAPIRoutes 验证 Bearer 鉴权中间件对 /metrics 与 /api/* 生效。
func TestAPI_AuthMiddlewareProtectsMetricsAndAPIRoutes(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler, WithAuth(AuthConfig{
		Enabled:        true,
		Mode:           AuthModeBearer,
		BearerToken:    "secret-token",
		ProtectAPI:     true,
		ProtectMetrics: true,
	}))

	healthResp := httptest.NewRecorder()
	healthReq := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	handler.ServeHTTP(healthResp, healthReq)
	if healthResp.Code != http.StatusOK {
		t.Fatalf("expected /healthz 200 without auth, got %d", healthResp.Code)
	}

	metricsNoAuthResp := httptest.NewRecorder()
	metricsNoAuthReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	handler.ServeHTTP(metricsNoAuthResp, metricsNoAuthReq)
	if metricsNoAuthResp.Code != http.StatusUnauthorized {
		t.Fatalf("expected /metrics 401 without auth, got %d body=%s", metricsNoAuthResp.Code, metricsNoAuthResp.Body.String())
	}

	metricsForbiddenResp := httptest.NewRecorder()
	metricsForbiddenReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsForbiddenReq.Header.Set("Authorization", "Bearer wrong-token")
	handler.ServeHTTP(metricsForbiddenResp, metricsForbiddenReq)
	if metricsForbiddenResp.Code != http.StatusForbidden {
		t.Fatalf("expected /metrics 403 with bad token, got %d body=%s", metricsForbiddenResp.Code, metricsForbiddenResp.Body.String())
	}

	metricsOKResp := httptest.NewRecorder()
	metricsOKReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsOKReq.Header.Set("Authorization", "Bearer secret-token")
	handler.ServeHTTP(metricsOKResp, metricsOKReq)
	if metricsOKResp.Code != http.StatusOK {
		t.Fatalf("expected /metrics 200 with valid token, got %d body=%s", metricsOKResp.Code, metricsOKResp.Body.String())
	}

	apiNoAuthResp := httptest.NewRecorder()
	apiNoAuthReq := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	handler.ServeHTTP(apiNoAuthResp, apiNoAuthReq)
	if apiNoAuthResp.Code != http.StatusUnauthorized {
		t.Fatalf("expected /api/tasks 401 without auth, got %d body=%s", apiNoAuthResp.Code, apiNoAuthResp.Body.String())
	}

	apiForbiddenResp := httptest.NewRecorder()
	apiForbiddenReq := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	apiForbiddenReq.Header.Set("Authorization", "Bearer wrong-token")
	handler.ServeHTTP(apiForbiddenResp, apiForbiddenReq)
	if apiForbiddenResp.Code != http.StatusForbidden {
		t.Fatalf("expected /api/tasks 403 with bad token, got %d body=%s", apiForbiddenResp.Code, apiForbiddenResp.Body.String())
	}

	apiOKResp := httptest.NewRecorder()
	apiOKReq := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	apiOKReq.Header.Set("Authorization", "Bearer secret-token")
	handler.ServeHTTP(apiOKResp, apiOKReq)
	if apiOKResp.Code != http.StatusOK {
		t.Fatalf("expected /api/tasks 200 with valid token, got %d body=%s", apiOKResp.Code, apiOKResp.Body.String())
	}
}

// TestAPI_AuthMiddlewareCanExposeMetricsAndAPIByConfig 验证可按配置放开 /metrics 与 /api/*。
func TestAPI_AuthMiddlewareCanExposeMetricsAndAPIByConfig(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler, WithAuth(AuthConfig{
		Enabled:        true,
		Mode:           AuthModeBearer,
		BearerToken:    "secret-token",
		ProtectAPI:     false,
		ProtectMetrics: false,
	}))

	metricsResp := httptest.NewRecorder()
	metricsReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	handler.ServeHTTP(metricsResp, metricsReq)
	if metricsResp.Code != http.StatusOK {
		t.Fatalf("expected /metrics 200 when protection disabled, got %d body=%s", metricsResp.Code, metricsResp.Body.String())
	}

	apiResp := httptest.NewRecorder()
	apiReq := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	handler.ServeHTTP(apiResp, apiReq)
	if apiResp.Code != http.StatusOK {
		t.Fatalf("expected /api/tasks 200 when protection disabled, got %d body=%s", apiResp.Code, apiResp.Body.String())
	}
}

// TestAPI_AuthMiddlewareAPIKeyMode 验证 API Key 模式的失败/成功路径与自定义 header。
func TestAPI_AuthMiddlewareAPIKeyMode(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler, WithAuth(AuthConfig{
		Enabled:        true,
		Mode:           AuthModeAPIKey,
		APIKey:         "secret-key",
		APIKeyHeader:   "X-Custom-API-Key",
		ProtectAPI:     true,
		ProtectMetrics: true,
	}))

	noHeaderResp := httptest.NewRecorder()
	noHeaderReq := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	handler.ServeHTTP(noHeaderResp, noHeaderReq)
	if noHeaderResp.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when api key header missing, got %d body=%s", noHeaderResp.Code, noHeaderResp.Body.String())
	}

	wrongKeyResp := httptest.NewRecorder()
	wrongKeyReq := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	wrongKeyReq.Header.Set("X-Custom-API-Key", "wrong-key")
	handler.ServeHTTP(wrongKeyResp, wrongKeyReq)
	if wrongKeyResp.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when api key is wrong, got %d body=%s", wrongKeyResp.Code, wrongKeyResp.Body.String())
	}

	wrongHeaderResp := httptest.NewRecorder()
	wrongHeaderReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	wrongHeaderReq.Header.Set("X-API-Key", "secret-key")
	handler.ServeHTTP(wrongHeaderResp, wrongHeaderReq)
	if wrongHeaderResp.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when using wrong header name, got %d body=%s", wrongHeaderResp.Code, wrongHeaderResp.Body.String())
	}

	okResp := httptest.NewRecorder()
	okReq := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	okReq.Header.Set("X-Custom-API-Key", "secret-key")
	handler.ServeHTTP(okResp, okReq)
	if okResp.Code != http.StatusOK {
		t.Fatalf("expected 200 when api key is valid, got %d body=%s", okResp.Code, okResp.Body.String())
	}
}

// TestTaskAPI_MetricsIncludeRetryUploadCounters 验证相关行为。
func TestTaskAPI_MetricsIncludeRetryUploadCounters(t *testing.T) {
	fileStore := newFakeFileStore()
	tmpDir := t.TempDir()
	okFile := filepath.Join(tmpDir, "mysql-bin.000200")
	failFile := filepath.Join(tmpDir, "mysql-bin.000201")
	if err := os.WriteFile(okFile, []byte("ok"), 0o644); err != nil {
		t.Fatalf("write okFile failed: %v", err)
	}
	if err := os.WriteFile(failFile, []byte("fail"), 0o644); err != nil {
		t.Fatalf("write failFile failed: %v", err)
	}
	fileStore.files["1"] = []tasks.BinlogFile{
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000200",
			FilePath:    okFile,
			SealedAt:    time.Now(),
			UploadState: "UPLOAD_FAILED",
			ObjectKey:   "prefix/key/mysql-bin.000200",
		},
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000201",
			FilePath:    failFile,
			SealedAt:    time.Now(),
			UploadState: "UPLOAD_FAILED",
			ObjectKey:   "prefix/key/mysql-bin.000201",
		},
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000202",
			FilePath:    failFile,
			SealedAt:    time.Now(),
			UploadState: "UPLOADED",
			ObjectKey:   "prefix/key/mysql-bin.000202",
		},
	}
	uploader := &fakeRetryUploader{
		errByObject: map[string]error{
			"prefix/key/mysql-bin.000201": errors.New("network timeout"),
		},
	}
	scheduler := tasks.NewScheduler(tasks.WithFileStore(fileStore), tasks.WithFileUploader(uploader))
	handler := NewServer(scheduler)

	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{"name":"cluster-a","cluster_key":"cluster-a-key"}`))
	createReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", createResp.Code, createResp.Body.String())
	}

	retryResp := httptest.NewRecorder()
	retryReq := httptest.NewRequest(http.MethodPost, "/api/tasks/1/files/retry-upload?limit=100", nil)
	handler.ServeHTTP(retryResp, retryReq)
	if retryResp.Code != http.StatusOK {
		t.Fatalf("expected retry 200, got %d body=%s", retryResp.Code, retryResp.Body.String())
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected metrics 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	success, ok, err := readPromMetricValueWithLabels(resp.Body.String(), "binlog_server_upload_retry_total", map[string]string{"result": "success"})
	if err != nil {
		t.Fatalf("read success metric failed: %v", err)
	}
	if !ok || success != 1 {
		t.Fatalf("expected success=1, got ok=%v value=%v body=%s", ok, success, resp.Body.String())
	}
	failed, ok, err := readPromMetricValueWithLabels(resp.Body.String(), "binlog_server_upload_retry_total", map[string]string{"result": "failed"})
	if err != nil {
		t.Fatalf("read failed metric failed: %v", err)
	}
	if !ok || failed != 1 {
		t.Fatalf("expected failed=1, got ok=%v value=%v body=%s", ok, failed, resp.Body.String())
	}
	skipped, ok, err := readPromMetricValueWithLabels(resp.Body.String(), "binlog_server_upload_retry_total", map[string]string{"result": "skipped"})
	if err != nil {
		t.Fatalf("read skipped metric failed: %v", err)
	}
	if !ok || skipped != 1 {
		t.Fatalf("expected skipped=1, got ok=%v value=%v body=%s", ok, skipped, resp.Body.String())
	}

	lastTS, ok, err := readPromMetricValue(resp.Body.String(), "binlog_server_upload_retry_last_ts")
	if err != nil {
		t.Fatalf("read last_ts metric failed: %v", err)
	}
	if !ok || lastTS <= 0 {
		t.Fatalf("expected last_ts > 0, got ok=%v value=%v body=%s", ok, lastTS, resp.Body.String())
	}
}

// readPromMetricValue 实现对应功能逻辑。
func readPromMetricValue(body, metricName string) (float64, bool, error) {
	lines := strings.Split(body, "\n")
	prefix := metricName + " "
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		valueText := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		v, err := strconv.ParseFloat(valueText, 64)
		if err != nil {
			return 0, false, err
		}
		return v, true, nil
	}
	return 0, false, nil
}

// readPromMetricValueWithLabels 实现对应功能逻辑。
func readPromMetricValueWithLabels(body, metricName string, expectedLabels map[string]string) (float64, bool, error) {
	lines := strings.Split(body, "\n")
	prefix := metricName + "{"
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rightBrace := strings.Index(line, "}")
		if rightBrace < 0 || rightBrace+1 >= len(line) {
			continue
		}
		labelsRaw := strings.TrimPrefix(line[:rightBrace+1], metricName+"{")
		labelsRaw = strings.TrimSuffix(labelsRaw, "}")
		labels := map[string]string{}
		if strings.TrimSpace(labelsRaw) != "" {
			parts := strings.Split(labelsRaw, ",")
			for _, part := range parts {
				pair := strings.SplitN(strings.TrimSpace(part), "=", 2)
				if len(pair) != 2 {
					continue
				}
				labels[pair[0]] = strings.Trim(pair[1], `"`)
			}
		}
		matched := true
		for key, want := range expectedLabels {
			if labels[key] != want {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		valueText := strings.TrimSpace(line[rightBrace+1:])
		v, err := strconv.ParseFloat(valueText, 64)
		if err != nil {
			return 0, false, err
		}
		return v, true, nil
	}
	return 0, false, nil
}

// TestTaskAPI_ListUploadFailureReasons 验证相关行为。
func TestTaskAPI_ListUploadFailureReasons(t *testing.T) {
	fileStore := newFakeFileStore()
	now := time.Now()
	fileStore.files["1"] = []tasks.BinlogFile{
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000300",
			UploadState: "UPLOAD_FAILED",
			UploadError: "network timeout",
			SealedAt:    now.Add(-2 * time.Minute),
		},
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000301",
			UploadState: "UPLOAD_FAILED",
			UploadError: " network timeout ",
			SealedAt:    now.Add(-1 * time.Minute),
		},
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000302",
			UploadState: "UPLOAD_FAILED",
			UploadError: "permission denied",
			SealedAt:    now,
		},
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000303",
			UploadState: "UPLOADED",
			UploadError: "",
			SealedAt:    now,
		},
	}
	scheduler := tasks.NewScheduler(tasks.WithFileStore(fileStore))
	handler := NewServer(scheduler)

	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{"name":"cluster-a","cluster_key":"cluster-a-key"}`))
	createReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", createResp.Code, createResp.Body.String())
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/1/upload-failures/reasons?limit=20", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	var reasons []map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &reasons); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}
	if len(reasons) != 2 {
		t.Fatalf("expected 2 aggregated reasons, got %d body=%s", len(reasons), resp.Body.String())
	}
	if reasons[0]["reason"] != "network timeout" || int(reasons[0]["count"].(float64)) != 2 {
		t.Fatalf("unexpected first reason item: %+v", reasons[0])
	}
}

// TestTaskAPI_ListEvents 验证相关行为。
func TestTaskAPI_ListEvents(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{"name":"cluster-a","cluster_key":"cluster-a-key"}`))
	createReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", createResp.Code)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/1/events", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	var events []map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &events); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected events not empty")
	}
}

// TestTaskAPI_ListFiles 验证相关行为。
func TestTaskAPI_ListFiles(t *testing.T) {
	fileStore := newFakeFileStore()
	fileStore.files["1"] = []tasks.BinlogFile{
		{
			TaskID:   "1",
			FileName: "mysql-bin.000001",
			FilePath: "/tmp/mysql-bin.000001",
		},
	}
	scheduler := tasks.NewScheduler(tasks.WithFileStore(fileStore))
	handler := NewServer(scheduler)

	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{"name":"cluster-a","cluster_key":"cluster-a-key"}`))
	createReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", createResp.Code)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/1/files", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	var files []tasks.BinlogFile
	if err := json.Unmarshal(resp.Body.Bytes(), &files); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file item, got %d", len(files))
	}
}

// TestTaskAPI_RetryUploadLimitValidation 验证相关行为。
func TestTaskAPI_RetryUploadLimitValidation(t *testing.T) {
	fileStore := newFakeFileStore()
	uploader := &fakeRetryUploader{}
	scheduler := tasks.NewScheduler(tasks.WithFileStore(fileStore), tasks.WithFileUploader(uploader))
	handler := NewServer(scheduler)

	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{"name":"cluster-a","cluster_key":"cluster-a-key"}`))
	createReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", createResp.Code, createResp.Body.String())
	}

	cases := []string{
		"/api/tasks/1/files/retry-upload?limit=0",
		"/api/tasks/1/files/retry-upload?limit=1001",
		"/api/tasks/1/files/retry-upload?limit=abc",
	}
	for _, path := range cases {
		resp := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, nil)
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for %s, got %d body=%s", path, resp.Code, resp.Body.String())
		}
		if got := strings.TrimSpace(resp.Body.String()); got != "invalid limit" {
			t.Fatalf("expected invalid limit for %s, got %q", path, got)
		}
	}
}

// TestTaskAPI_UploadFailureReasonsLimitValidation 验证相关行为。
func TestTaskAPI_UploadFailureReasonsLimitValidation(t *testing.T) {
	scheduler := tasks.NewScheduler(tasks.WithFileStore(newFakeFileStore()))
	handler := NewServer(scheduler)

	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{"name":"cluster-a","cluster_key":"cluster-a-key"}`))
	createReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", createResp.Code, createResp.Body.String())
	}

	cases := []string{
		"/api/tasks/1/upload-failures/reasons?limit=0",
		"/api/tasks/1/upload-failures/reasons?limit=201",
		"/api/tasks/1/upload-failures/reasons?limit=abc",
	}
	for _, path := range cases {
		resp := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for %s, got %d body=%s", path, resp.Code, resp.Body.String())
		}
		if got := strings.TrimSpace(resp.Body.String()); got != "invalid limit" {
			t.Fatalf("expected invalid limit for %s, got %q", path, got)
		}
	}
}

// TestTaskAPI_RetryUploadReturnsStats 验证相关行为。
func TestTaskAPI_RetryUploadReturnsStats(t *testing.T) {
	fileStore := newFakeFileStore()
	tmpDir := t.TempDir()
	file1 := filepath.Join(tmpDir, "mysql-bin.000100")
	file2 := filepath.Join(tmpDir, "mysql-bin.000101")
	openFile := filepath.Join(tmpDir, "mysql-bin.000102.open.e1")
	if err := os.WriteFile(file1, []byte("a"), 0o644); err != nil {
		t.Fatalf("write file1 failed: %v", err)
	}
	if err := os.WriteFile(file2, []byte("b"), 0o644); err != nil {
		t.Fatalf("write file2 failed: %v", err)
	}
	if err := os.WriteFile(openFile, []byte("c"), 0o644); err != nil {
		t.Fatalf("write open file failed: %v", err)
	}

	fileStore.files["1"] = []tasks.BinlogFile{
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000100",
			FilePath:    file1,
			SealedAt:    time.Now(),
			UploadState: "UPLOAD_FAILED",
			ObjectKey:   "prefix/key/mysql-bin.000100",
		},
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000101",
			FilePath:    file2,
			SealedAt:    time.Now(),
			UploadState: "UPLOAD_FAILED",
			ObjectKey:   "prefix/key/mysql-bin.000101",
		},
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000102.open.e1",
			FilePath:    openFile,
			SealedAt:    time.Now(),
			UploadState: "UPLOAD_FAILED",
			ObjectKey:   "prefix/key/mysql-bin.000102.open.e1",
		},
	}

	uploader := &fakeRetryUploader{
		errByObject: map[string]error{
			"prefix/key/mysql-bin.000101": errors.New("upload failed"),
		},
	}
	scheduler := tasks.NewScheduler(tasks.WithFileStore(fileStore), tasks.WithFileUploader(uploader))
	handler := NewServer(scheduler)

	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{"name":"cluster-a","cluster_key":"cluster-a-key"}`))
	createReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", createResp.Code, createResp.Body.String())
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/1/files/retry-upload?limit=100", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	var stats map[string]int
	if err := json.Unmarshal(resp.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}
	if stats["succeeded"] != 1 || stats["failed"] != 1 || stats["skipped"] != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

// TestTaskAPI_ListEventsWithLimit 验证相关行为。
func TestTaskAPI_ListEventsWithLimit(t *testing.T) {
	scheduler := tasks.NewScheduler(tasks.WithRunner(&fakeAPIRunner{}))
	handler := NewServer(scheduler)

	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{
		"name":"cluster-a",
		"cluster_key":"cluster-a-key",
		"source":{"host":"127.0.0.1","port":3306,"user":"repl","flavor":"mysql","server_id":200001}
	}`))
	createReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", createResp.Code)
	}
	if err := scheduler.StartTask("1"); err != nil {
		t.Fatalf("start task: %v", err)
	}
	if err := scheduler.StopTask("1"); err != nil {
		t.Fatalf("stop task: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/1/events?limit=1", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	var events []tasks.TaskEvent
	if err := json.Unmarshal(resp.Body.Bytes(), &events); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event due to limit, got %d", len(events))
	}
}

// TestUI_RootRedirectAndDashboardPage 验证相关行为。
func TestUI_RootRedirectAndDashboardPage(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	rootResp := httptest.NewRecorder()
	rootReq := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(rootResp, rootReq)
	if rootResp.Code != http.StatusFound {
		t.Fatalf("expected 302 for root, got %d", rootResp.Code)
	}
	if loc := rootResp.Header().Get("Location"); loc != "/ui/" {
		t.Fatalf("expected redirect to /ui/, got %q", loc)
	}

	uiResp := httptest.NewRecorder()
	uiReq := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	handler.ServeHTTP(uiResp, uiReq)
	if uiResp.Code != http.StatusOK {
		t.Fatalf("expected 200 for /ui/, got %d", uiResp.Code)
	}
	if !bytes.Contains(uiResp.Body.Bytes(), []byte("Binlog Server Console")) {
		t.Fatalf("expected ui html to contain console title, got body=%s", uiResp.Body.String())
	}
	if !bytes.Contains(uiResp.Body.Bytes(), []byte(`id="app"`)) {
		t.Fatalf("expected ui html to contain vue mount root, got body=%s", uiResp.Body.String())
	}
}

// TestAPI_Summary 验证相关行为。
func TestAPI_Summary(t *testing.T) {
	scheduler := tasks.NewScheduler(tasks.WithRunner(&fakeAPIRunner{}))
	handler := NewServer(scheduler)

	taskA, err := scheduler.CreateTask("a", "a-key")
	if err != nil {
		t.Fatalf("CreateTask A returned error: %v", err)
	}
	taskB, err := scheduler.CreateTask("b", "b-key")
	if err != nil {
		t.Fatalf("CreateTask B returned error: %v", err)
	}
	if err := scheduler.ConfigureSource(taskA.ID, tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource A returned error: %v", err)
	}
	if err := scheduler.ConfigureSource(taskB.ID, tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource B returned error: %v", err)
	}
	if err := scheduler.StartTask(taskA.ID); err != nil {
		t.Fatalf("StartTask A returned error: %v", err)
	}
	if err := scheduler.StartTask(taskB.ID); err != nil {
		t.Fatalf("StartTask B returned error: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		a, err := scheduler.GetTask(taskA.ID)
		if err != nil {
			t.Fatalf("GetTask A returned error: %v", err)
		}
		b, err := scheduler.GetTask(taskB.ID)
		if err != nil {
			t.Fatalf("GetTask B returned error: %v", err)
		}
		if a.State == tasks.StateRunning && b.State == tasks.StateRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tasks did not reach running state in time: A=%s B=%s", a.State, b.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := scheduler.StopTask(taskB.ID); err != nil {
		t.Fatalf("StopTask B returned error: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/summary", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	var body map[string]int
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["total"] != 2 {
		t.Fatalf("expected total=2, got %d", body["total"])
	}
	if body["running"] != 1 {
		t.Fatalf("expected running=1, got %d", body["running"])
	}
	if body["stopped"] != 1 {
		t.Fatalf("expected stopped=1, got %d", body["stopped"])
	}
}

// TestTaskAPI_ListTasksBySourceFilter 验证相关行为。
func TestTaskAPI_ListTasksBySourceFilter(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	a, err := scheduler.CreateTask("a", "a-key")
	if err != nil {
		t.Fatalf("CreateTask A returned error: %v", err)
	}
	b, err := scheduler.CreateTask("b", "b-key")
	if err != nil {
		t.Fatalf("CreateTask B returned error: %v", err)
	}
	if err := scheduler.ConfigureSource(a.ID, tasks.SourceConfig{Host: "10.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource A returned error: %v", err)
	}
	if err := scheduler.ConfigureSource(b.ID, tasks.SourceConfig{Host: "10.0.0.2", Port: 3307, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource B returned error: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tasks?host=10.0.0.2&port=3307", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	var list []tasks.Task
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 task, got %d", len(list))
	}
	if list[0].Source.Host != "10.0.0.2" || list[0].Source.Port != 3307 {
		t.Fatalf("unexpected source: %+v", list[0].Source)
	}
}

// TestAPI_SourceLookup 验证相关行为。
func TestAPI_SourceLookup(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	a, err := scheduler.CreateTask("a", "a-key")
	if err != nil {
		t.Fatalf("CreateTask A returned error: %v", err)
	}
	b, err := scheduler.CreateTask("b", "b-key")
	if err != nil {
		t.Fatalf("CreateTask B returned error: %v", err)
	}
	if err := scheduler.ConfigureSource(a.ID, tasks.SourceConfig{Host: "10.0.0.9", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource A returned error: %v", err)
	}
	if err := scheduler.ConfigureSource(b.ID, tasks.SourceConfig{Host: "10.0.0.9", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource B returned error: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/sources/lookup?host=10.0.0.9&port=3306", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["exists"] != true {
		t.Fatalf("expected exists=true, got %v", body["exists"])
	}
	if int(body["count"].(float64)) != 2 {
		t.Fatalf("expected count=2, got %v", body["count"])
	}

	notFoundResp := httptest.NewRecorder()
	notFoundReq := httptest.NewRequest(http.MethodGet, "/api/sources/lookup?host=10.0.0.9&port=3310", nil)
	handler.ServeHTTP(notFoundResp, notFoundReq)
	if notFoundResp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", notFoundResp.Code, notFoundResp.Body.String())
	}

	var notFoundBody map[string]any
	if err := json.Unmarshal(notFoundResp.Body.Bytes(), &notFoundBody); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if notFoundBody["exists"] != false {
		t.Fatalf("expected exists=false, got %v", notFoundBody["exists"])
	}
	if int(notFoundBody["count"].(float64)) != 0 {
		t.Fatalf("expected count=0, got %v", notFoundBody["count"])
	}
}

// TestAPI_SourceLookupValidationErrors 验证相关行为。
func TestAPI_SourceLookupValidationErrors(t *testing.T) {
	handler := NewServer(tasks.NewScheduler())

	cases := []struct {
		path string
		want string
	}{
		{path: "/api/sources/lookup?port=3306", want: "host is required"},
		{path: "/api/sources/lookup?host=%20%20%20&port=3306", want: "host is required"},
		{path: "/api/sources/lookup?host=10.0.0.9", want: "port is required"},
		{path: "/api/sources/lookup?host=10.0.0.9&port=abc", want: "invalid port"},
	}
	for _, tc := range cases {
		resp := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for %s, got %d body=%s", tc.path, resp.Code, resp.Body.String())
		}
		if got := strings.TrimSpace(resp.Body.String()); got != tc.want {
			t.Fatalf("expected %q for %s, got %q", tc.want, tc.path, got)
		}
	}
}

// TestTaskAPI_GetReplication 验证相关行为。
func TestTaskAPI_GetReplication(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	task, err := scheduler.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	scheduler.ReportReplicationProgress(task.ID, time.Now().Add(-8*time.Second), "mysql-bin.000777", 456)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/"+task.ID+"/replication", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["task_id"] != task.ID {
		t.Fatalf("unexpected task_id: %v", body["task_id"])
	}
	if body["last_event_file"] != "mysql-bin.000777" {
		t.Fatalf("unexpected last_event_file: %v", body["last_event_file"])
	}
	if body["has_progress"] != true {
		t.Fatalf("expected has_progress=true, got %v", body["has_progress"])
	}
}

// TestTaskAPI_GetReplication_AbnormalReasonNoProgress 验证相关行为。
func TestTaskAPI_GetReplication_AbnormalReasonNoProgress(t *testing.T) {
	scheduler := tasks.NewScheduler(tasks.WithRunner(&fakeAPIRunner{}))
	handler := NewServer(scheduler)

	task, err := scheduler.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := scheduler.ConfigureSource(task.ID, tasks.SourceConfig{
		Host: "127.0.0.1",
		Port: 3306,
		User: "repl",
	}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}
	if err := scheduler.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask returned error: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		cur, err := scheduler.GetTask(task.ID)
		if err != nil {
			t.Fatalf("GetTask returned error: %v", err)
		}
		if cur.State == tasks.StateRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task did not reach running state in time: %s", cur.State)
		}
		time.Sleep(10 * time.Millisecond)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/"+task.ID+"/replication", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "ABNORMAL" {
		t.Fatalf("expected ABNORMAL, got %v", body["status"])
	}
	if body["reason"] != "NO_PROGRESS" {
		t.Fatalf("expected reason=NO_PROGRESS, got %v", body["reason"])
	}
}

// TestAPI_Dashboard 验证相关行为。
func TestAPI_Dashboard(t *testing.T) {
	scheduler := tasks.NewScheduler(tasks.WithRunner(&fakeAPIRunner{}))
	handler := NewServer(scheduler)

	taskA, err := scheduler.CreateTask("a", "a-key")
	if err != nil {
		t.Fatalf("CreateTask A returned error: %v", err)
	}
	taskB, err := scheduler.CreateTask("b", "b-key")
	if err != nil {
		t.Fatalf("CreateTask B returned error: %v", err)
	}
	if err := scheduler.ConfigureSource(taskA.ID, tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource A returned error: %v", err)
	}
	if err := scheduler.ConfigureSource(taskB.ID, tasks.SourceConfig{Host: "127.0.0.1", Port: 3307, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource B returned error: %v", err)
	}
	if err := scheduler.StartTask(taskA.ID); err != nil {
		t.Fatalf("StartTask A returned error: %v", err)
	}

	scheduler.ReportReplicationProgress(taskA.ID, time.Now().Add(-3*time.Second), "mysql-bin.000001", 123)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/dashboard", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := body["summary"]; !ok {
		t.Fatal("expected summary in dashboard response")
	}
	tasksAny, ok := body["tasks"].([]any)
	if !ok {
		t.Fatalf("expected tasks array, got %T", body["tasks"])
	}
	if len(tasksAny) != 2 {
		t.Fatalf("expected 2 dashboard tasks, got %d", len(tasksAny))
	}
}

// TestAPI_ClusterWorkers 验证相关行为。
func TestAPI_ClusterWorkers(t *testing.T) {
	runStore := newFakeAPIRunHistoryStore()
	scheduler := tasks.NewScheduler(
		tasks.WithRunner(&fakeAPIRunner{}),
		tasks.WithStore(runStore),
		tasks.WithClusterLeaseManager(&fakeAPILeaseManager{epoch: 7}),
		tasks.WithClusterWorkerID("worker-a"),
	)
	handler := NewServer(scheduler)

	task, err := scheduler.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := scheduler.ConfigureSource(task.ID, tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}
	if err := scheduler.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask returned error: %v", err)
	}
	runStore.workers = []tasks.WorkerHeartbeat{
		{
			WorkerID:   "worker-a",
			Host:       "host-a",
			Version:    "v1.0.0",
			LastSeenAt: time.Now(),
			Status:     "ONLINE",
		},
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/workers", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	var workers []map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &workers); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(workers) != 1 {
		t.Fatalf("expected 1 worker item, got %d", len(workers))
	}
	if workers[0]["worker_id"] != "worker-a" {
		t.Fatalf("unexpected worker_id: %v", workers[0]["worker_id"])
	}
	if workers[0]["online"] != true {
		t.Fatalf("expected worker online=true, got %v", workers[0]["online"])
	}
}

// TestAPI_ClusterTaskLease 验证相关行为。
func TestAPI_ClusterTaskLease(t *testing.T) {
	scheduler := tasks.NewScheduler(
		tasks.WithRunner(&fakeAPIRunner{}),
		tasks.WithClusterLeaseManager(&fakeAPILeaseManager{epoch: 9}),
		tasks.WithClusterWorkerID("worker-a"),
	)
	handler := NewServer(scheduler)

	task, err := scheduler.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := scheduler.ConfigureSource(task.ID, tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}
	if err := scheduler.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask returned error: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/"+task.ID+"/lease", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	var lease map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &lease); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if lease["task_id"] != task.ID {
		t.Fatalf("unexpected task_id: %v", lease["task_id"])
	}
	if lease["owner_worker_id"] != "worker-a" {
		t.Fatalf("unexpected owner_worker_id: %v", lease["owner_worker_id"])
	}
	if int(lease["epoch"].(float64)) != 9 {
		t.Fatalf("unexpected epoch: %v", lease["epoch"])
	}
}

// TestAPI_ClusterTaskRuns 验证相关行为。
func TestAPI_ClusterTaskRuns(t *testing.T) {
	runStore := newFakeAPIRunHistoryStore()
	scheduler := tasks.NewScheduler(
		tasks.WithRunner(&fakeAPIRunner{}),
		tasks.WithStore(runStore),
		tasks.WithClusterLeaseManager(&fakeAPILeaseManager{epoch: 11}),
		tasks.WithClusterWorkerID("worker-a"),
	)
	handler := NewServer(scheduler)

	task, err := scheduler.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := scheduler.ConfigureSource(task.ID, tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}
	if err := scheduler.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask returned error: %v", err)
	}
	now := time.Now()
	runStore.runs[task.ID] = []tasks.TaskRun{
		{
			RunID:     "run-2",
			TaskID:    task.ID,
			WorkerID:  "worker-b",
			Epoch:     12,
			StartedAt: now,
		},
		{
			RunID:     "run-1",
			TaskID:    task.ID,
			WorkerID:  "worker-a",
			Epoch:     11,
			StartedAt: now.Add(-time.Hour),
			EndedAt:   now.Add(-30 * time.Minute),
			EndReason: "NORMAL_STOP",
		},
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/"+task.ID+"/runs?limit=999", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	var runs []map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if runStore.lastLimit != 200 {
		t.Fatalf("expected limit clamped to 200, got %d", runStore.lastLimit)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 run items, got %d", len(runs))
	}
	if runs[0]["run_id"] != "run-2" {
		t.Fatalf("unexpected latest run: %+v", runs[0])
	}
	if runs[1]["end_reason"] != "NORMAL_STOP" {
		t.Fatalf("unexpected end_reason on historical run: %+v", runs[1])
	}
}

// TestAPI_ClusterOverview 验证相关行为。
func TestAPI_ClusterOverview(t *testing.T) {
	scheduler := tasks.NewScheduler(
		tasks.WithRunner(&fakeAPIRunner{}),
		tasks.WithClusterLeaseManager(&fakeAPILeaseManager{epoch: 13}),
		tasks.WithClusterWorkerID("worker-a"),
	)
	handler := NewServer(scheduler)

	task, err := scheduler.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := scheduler.ConfigureSource(task.ID, tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}
	if err := scheduler.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask returned error: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cluster/overview", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	var overview map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &overview); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if int(overview["task_count"].(float64)) != 1 {
		t.Fatalf("unexpected task_count: %v", overview["task_count"])
	}
	if int(overview["worker_count"].(float64)) != 1 {
		t.Fatalf("unexpected worker_count: %v", overview["worker_count"])
	}
}

// TestAPI_SwaggerUI 验证相关行为。
func TestAPI_SwaggerUI(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/swagger/index.html", nil)
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}
	if !bytes.Contains(resp.Body.Bytes(), []byte("Swagger UI")) {
		t.Fatalf("expected Swagger UI page, got body=%s", resp.Body.String())
	}
}

// TestAPI_SwaggerDocContainsKeyPaths 验证相关行为。
func TestAPI_SwaggerDocContainsKeyPaths(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/swagger/doc.json", nil)
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	paths, ok := body["paths"].(map[string]any)
	if !ok {
		t.Fatalf("expected paths object, got %T", body["paths"])
	}

	required := []string{
		"/healthz",
		"/api/summary",
		"/api/dashboard",
		"/api/sources/lookup",
		"/api/workers",
		"/api/cluster/overview",
		"/api/tasks",
		"/api/tasks/{id}",
		"/api/tasks/{id}/start",
		"/api/tasks/{id}/stop",
		"/api/tasks/{id}/checkpoint",
		"/api/tasks/{id}/events",
		"/api/tasks/{id}/files",
		"/api/tasks/{id}/upload-failures/reasons",
		"/api/tasks/{id}/replication",
		"/api/tasks/{id}/lease",
		"/api/tasks/{id}/runs",
	}
	for _, key := range required {
		if _, exists := paths[key]; !exists {
			t.Fatalf("expected swagger path %s", key)
		}
	}
}
