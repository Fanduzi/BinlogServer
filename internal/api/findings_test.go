// Package api provides module-level functionality for api.
// input: a scheduler with a reported finding, HTTP GET on the findings routes and /metrics
// output: assertions that a finding is listed per task and cluster-wide, that resolved ones need all=1, and that the finding gauges are exported (always at least one series)
// pos: HTTP coverage for the ADR 0009 finding sink
// note: if this file changes, update this header and module README.md.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"binlog_server/internal/tasks"
)

func getFindings(t *testing.T, h http.Handler, path string) []tasks.Finding {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", path, resp.Code, resp.Body.String())
	}
	var out []tasks.Finding
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return out
}

func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("metrics %d", resp.Code)
	}
	return resp.Body.String()
}

func TestFindingsAPIAndMetrics(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)
	if body := scrape(t, handler); !strings.Contains(body, `binlog_server_finding_open{kind="",severity="",task_id=""} 0`) {
		t.Fatalf("no zero finding_open series:\n%s", body)
	}
	if body := scrape(t, handler); !strings.Contains(body, "binlog_server_finding_store_active 0") {
		t.Fatalf("store_active missing")
	}

	scheduler.Report(tasks.Finding{TaskID: "1", Kind: tasks.FindingTaskPersistFailed, Detail: "meta down"})
	scheduler.Report(tasks.Finding{Kind: tasks.FindingDiskListFailed, Subject: "/data", Severity: tasks.FindingWarning})

	got := getFindings(t, handler, "/api/tasks/1/findings")
	if len(got) != 1 || got[0].Kind != tasks.FindingTaskPersistFailed || got[0].Severity != tasks.FindingCritical || !got[0].Open() {
		t.Fatalf("task findings %+v", got)
	}
	if all := getFindings(t, handler, "/api/findings"); len(all) != 2 {
		t.Fatalf("open findings %+v", all)
	}
	body := scrape(t, handler)
	for _, want := range []string{
		`binlog_server_finding_open{kind="TASK_PERSIST_FAILED",severity="CRITICAL",task_id="1"} 1`,
		`binlog_server_finding_open{kind="DISK_LIST_FAILED",severity="WARNING",task_id="_cluster"} 1`,
		`binlog_server_findings_reported_total{kind="TASK_PERSIST_FAILED"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in\n%s", want, body)
		}
	}

	scheduler.Resolve("1", tasks.FindingTaskPersistFailed, "")
	if open := getFindings(t, handler, "/api/tasks/1/findings"); len(open) != 0 {
		t.Fatalf("resolved finding still listed as open: %+v", open)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/1/findings?limit=0", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("limit=0 got %d", resp.Code)
	}
}
