package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"binlog_server/internal/tasks"
)

// GET /api/tasks and /api/tasks/{id} carry open_findings like /api/dashboard,
// including an explicit 0 (tip 53a9 P3: the field was missing, jq read null).
func TestTaskListAndDetailCarryOpenFindings(t *testing.T) {
	sched := tasks.NewScheduler()
	hot, err := sched.CreateTask("hot", "c1")
	if err != nil {
		t.Fatal(err)
	}
	cold, err := sched.CreateTask("cold", "c2")
	if err != nil {
		t.Fatal(err)
	}
	sched.Report(tasks.Finding{TaskID: hot.ID, Kind: tasks.FindingUploadUnverified, Subject: "a", Severity: tasks.FindingWarning})
	sched.Report(tasks.Finding{TaskID: hot.ID, Kind: tasks.FindingUploadUnverified, Subject: "b", Severity: tasks.FindingWarning})
	srv := NewServer(sched)

	get := func(path string) []byte {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		return rec.Body.Bytes()
	}
	var one map[string]any
	if err := json.Unmarshal(get("/api/tasks/"+hot.ID), &one); err != nil {
		t.Fatal(err)
	}
	if one["open_findings"] != float64(2) {
		t.Fatalf("GET /api/tasks/%s open_findings=%v, want 2", hot.ID, one["open_findings"])
	}
	if err := json.Unmarshal(get("/api/tasks/"+cold.ID), &one); err != nil {
		t.Fatal(err)
	}
	if v, ok := one["open_findings"]; !ok || v != float64(0) {
		t.Fatalf("GET /api/tasks/%s open_findings=%v (present %v), want an explicit 0", cold.ID, v, ok)
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(get("/api/tasks"), &list); err != nil {
		t.Fatal(err)
	}
	got := map[any]any{}
	for _, it := range list.Items {
		got[it["id"]] = it["open_findings"]
	}
	if got[hot.ID] != float64(2) || got[cold.ID] != float64(0) {
		t.Fatalf("GET /api/tasks open_findings %v, want %s:2 %s:0", got, hot.ID, cold.ID)
	}
}
