package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"binlog_server/internal/tasks"
)

// #395 QA P2: an open CRITICAL finding, served from memory even when it could
// not be persisted, degrades GET /api/health (503, status "degraded", the
// reasons listed). /healthz stays the liveness probe and is not affected.
func TestAPIHealth_DegradesOnOpenCriticalFinding(t *testing.T) {
	s := tasks.NewScheduler()
	s.Report(tasks.Finding{TaskID: tasks.ClusterFindingTask, Kind: tasks.FindingFindingsPersistFailed, Severity: tasks.FindingCritical,
		Detail: "write .health.json: no space left on device"})
	handler := NewServer(s)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", resp.Code, resp.Body.String())
	}
	var body struct {
		Status  string   `json:"status"`
		Reasons []string `json:"reasons"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "degraded" || len(body.Reasons) == 0 || !strings.Contains(strings.Join(body.Reasons, " "), string(tasks.FindingFindingsPersistFailed)) {
		t.Fatalf("unexpected body %+v", body)
	}
	live := httptest.NewRecorder()
	handler.ServeHTTP(live, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if live.Code != http.StatusOK {
		t.Fatalf("/healthz %d while degraded", live.Code)
	}
}
