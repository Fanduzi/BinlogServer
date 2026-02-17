package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"
)

type fakeAPIRunner struct{}

func (r *fakeAPIRunner) Run(_ context.Context, _ tasks.Task) error {
	return nil
}

type fakeAPILeaseManager struct {
	epoch int64
}

func (m *fakeAPILeaseManager) Acquire(_ context.Context, _ string, _ string, _ time.Time, _ time.Duration) (int64, bool, error) {
	return m.epoch, true, nil
}

func (m *fakeAPILeaseManager) Renew(_ context.Context, _ string, _ string, _ int64, _ time.Time, _ time.Duration) (bool, error) {
	return true, nil
}

func (m *fakeAPILeaseManager) Release(_ context.Context, _ string, _ string, _ int64) (bool, error) {
	return true, nil
}

func TestTaskAPI_CreateListStartStop(t *testing.T) {
	scheduler := tasks.NewScheduler(tasks.WithRunner(&fakeAPIRunner{}))
	handler := NewServer(scheduler)

	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{
		"name":"cluster-a",
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

func TestTaskAPI_CreateWithSourceAndStart(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	reqBody := `{
		"name":"cluster-a",
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

type fakeCheckpointReader struct {
	checkpoints map[string]binlog.Checkpoint
}

func (f *fakeCheckpointReader) LoadCheckpoint(_ context.Context, taskID string) (binlog.Checkpoint, bool, error) {
	cp, ok := f.checkpoints[taskID]
	return cp, ok, nil
}

type fakeFileStore struct {
	files map[string][]tasks.BinlogFile
}

func newFakeFileStore() *fakeFileStore {
	return &fakeFileStore{files: make(map[string][]tasks.BinlogFile)}
}

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
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{"name":"cluster-a"}`))
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

func TestTaskAPI_UpdateAndDeleteTask(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{"name":"cluster-a"}`))
	createReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", createResp.Code)
	}

	updateBody := `{
		"name":"cluster-b",
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

func TestTaskAPI_ListEvents(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{"name":"cluster-a"}`))
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
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{"name":"cluster-a"}`))
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

func TestTaskAPI_ListEventsWithLimit(t *testing.T) {
	scheduler := tasks.NewScheduler(tasks.WithRunner(&fakeAPIRunner{}))
	handler := NewServer(scheduler)

	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{
		"name":"cluster-a",
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

func TestAPI_Summary(t *testing.T) {
	scheduler := tasks.NewScheduler(tasks.WithRunner(&fakeAPIRunner{}))
	handler := NewServer(scheduler)

	taskA, err := scheduler.CreateTask("a")
	if err != nil {
		t.Fatalf("CreateTask A returned error: %v", err)
	}
	taskB, err := scheduler.CreateTask("b")
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

func TestTaskAPI_ListTasksBySourceFilter(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	a, err := scheduler.CreateTask("a")
	if err != nil {
		t.Fatalf("CreateTask A returned error: %v", err)
	}
	b, err := scheduler.CreateTask("b")
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

func TestAPI_SourceLookup(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	a, err := scheduler.CreateTask("a")
	if err != nil {
		t.Fatalf("CreateTask A returned error: %v", err)
	}
	b, err := scheduler.CreateTask("b")
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

func TestTaskAPI_GetReplication(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	task, err := scheduler.CreateTask("cluster-a")
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

func TestTaskAPI_GetReplication_AbnormalReasonNoProgress(t *testing.T) {
	scheduler := tasks.NewScheduler(tasks.WithRunner(&fakeAPIRunner{}))
	handler := NewServer(scheduler)

	task, err := scheduler.CreateTask("cluster-a")
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

func TestAPI_Dashboard(t *testing.T) {
	scheduler := tasks.NewScheduler(tasks.WithRunner(&fakeAPIRunner{}))
	handler := NewServer(scheduler)

	taskA, err := scheduler.CreateTask("a")
	if err != nil {
		t.Fatalf("CreateTask A returned error: %v", err)
	}
	taskB, err := scheduler.CreateTask("b")
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

func TestAPI_ClusterWorkers(t *testing.T) {
	scheduler := tasks.NewScheduler(
		tasks.WithRunner(&fakeAPIRunner{}),
		tasks.WithClusterLeaseManager(&fakeAPILeaseManager{epoch: 7}),
		tasks.WithClusterWorkerID("worker-a"),
	)
	handler := NewServer(scheduler)

	task, err := scheduler.CreateTask("cluster-a")
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
}

func TestAPI_ClusterTaskLease(t *testing.T) {
	scheduler := tasks.NewScheduler(
		tasks.WithRunner(&fakeAPIRunner{}),
		tasks.WithClusterLeaseManager(&fakeAPILeaseManager{epoch: 9}),
		tasks.WithClusterWorkerID("worker-a"),
	)
	handler := NewServer(scheduler)

	task, err := scheduler.CreateTask("cluster-a")
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

func TestAPI_ClusterTaskRuns(t *testing.T) {
	scheduler := tasks.NewScheduler(
		tasks.WithRunner(&fakeAPIRunner{}),
		tasks.WithClusterLeaseManager(&fakeAPILeaseManager{epoch: 11}),
		tasks.WithClusterWorkerID("worker-a"),
	)
	handler := NewServer(scheduler)

	task, err := scheduler.CreateTask("cluster-a")
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
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/"+task.ID+"/runs", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}

	var runs []map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run item, got %d", len(runs))
	}
	if runs[0]["run_id"] == "" {
		t.Fatalf("expected non-empty run_id, got %+v", runs[0])
	}
}

func TestAPI_ClusterOverview(t *testing.T) {
	scheduler := tasks.NewScheduler(
		tasks.WithRunner(&fakeAPIRunner{}),
		tasks.WithClusterLeaseManager(&fakeAPILeaseManager{epoch: 13}),
		tasks.WithClusterWorkerID("worker-a"),
	)
	handler := NewServer(scheduler)

	task, err := scheduler.CreateTask("cluster-a")
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
		"/api/tasks",
		"/api/tasks/{id}",
		"/api/tasks/{id}/start",
		"/api/tasks/{id}/stop",
		"/api/tasks/{id}/checkpoint",
		"/api/tasks/{id}/events",
		"/api/tasks/{id}/files",
		"/api/tasks/{id}/replication",
	}
	for _, key := range required {
		if _, exists := paths[key]; !exists {
			t.Fatalf("expected swagger path %s", key)
		}
	}
}
