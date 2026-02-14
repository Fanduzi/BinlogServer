package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"
)

func TestTaskAPI_CreateListStartStop(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{"name":"cluster-a"}`))
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
	if list[0].State != tasks.StateStopped {
		t.Fatalf("expected final state %s, got %s", tasks.StateStopped, list[0].State)
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
	if created.Start.Mode != tasks.StartModeFilePos || created.Start.File != "mysql-bin.000001" || created.Start.Pos != 4 {
		t.Fatalf("start not persisted: %+v", created.Start)
	}
	if created.Storage.RetentionDays != 15 {
		t.Fatalf("storage not persisted: %+v", created.Storage)
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
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{"name":"cluster-a"}`))
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
	if !bytes.Contains(uiResp.Body.Bytes(), []byte("Binlog Control Tower")) {
		t.Fatalf("expected ui html to contain dashboard title, got body=%s", uiResp.Body.String())
	}
}

func TestAPI_Summary(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)

	taskA, err := scheduler.CreateTask("a")
	if err != nil {
		t.Fatalf("CreateTask A returned error: %v", err)
	}
	taskB, err := scheduler.CreateTask("b")
	if err != nil {
		t.Fatalf("CreateTask B returned error: %v", err)
	}
	if err := scheduler.StartTask(taskA.ID); err != nil {
		t.Fatalf("StartTask A returned error: %v", err)
	}
	if err := scheduler.StartTask(taskB.ID); err != nil {
		t.Fatalf("StartTask B returned error: %v", err)
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
