package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
