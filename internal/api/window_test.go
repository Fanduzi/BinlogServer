// Package api provides module-level functionality for api.
// input: HTTP GET /api/tasks/{id}/window and GET /metrics for a task with on-disk segments
// output: assertions that a continuous chain names its UTC span, a removed middle segment is a break in the API and in binlog_server_recovery_breaks, and a stop inside the continuous span still returns a replay command
// pos: HTTP coverage for the recoverable window and its gauges
// note: if this file changes, update this header and module README.md.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/tasks"
)

func TestTaskAPI_RecoveryWindowAndMetric(t *testing.T) {
	dir := t.TempDir()
	scheduler := tasks.NewScheduler(tasks.WithDataDir(dir))
	handler := NewServer(scheduler)
	create(`{"name":"mysql","cluster_key":"mysql-key","source":{"host":"127.0.0.1","port":3306,"user":"repl","password":"secret","flavor":"mysql"}}`, t, handler)

	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	first := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	second := first.Add(30 * time.Minute)
	third := first.Add(time.Hour)
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000001"), apiExecutedSegment(t, first, "", 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000002"), apiExecutedSegment(t, second, apiGTIDUUID+":1", 2), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000003"), apiExecutedSegment(t, third, apiGTIDUUID+":1-2", 3), 0o644); err != nil {
		t.Fatal(err)
	}

	window := getRecoveryWindow(t, handler, "/api/tasks/1/window")
	if !window.Continuous || len(window.Breaks) != 0 {
		t.Fatalf("window %+v", window)
	}
	if window.Earliest == nil || !window.Earliest.Equal(first) || window.Latest == nil || !window.Latest.Equal(third) {
		t.Fatalf("span %+v", window)
	}
	if window.GTIDSet == "" {
		t.Fatal("missing gtid_set")
	}
	inside := getPITR(t, handler, "/api/tasks/1/replay", url.Values{
		"stop_datetime": {third.Add(time.Second).UTC().Format("2006-01-02 15:04:05")},
	})
	if inside.Command == "" || len(inside.Paths) != 3 {
		t.Fatalf("inside replay %+v", inside)
	}

	if err := os.Remove(filepath.Join(taskDir, "mysql-bin.000002")); err != nil {
		t.Fatal(err)
	}
	broken := getRecoveryWindow(t, handler, "/api/tasks/1/window")
	if broken.Continuous || !recoveryReason(broken, "missing source file mysql-bin.000002") {
		t.Fatalf("broken %+v", broken)
	}

	metrics := httptest.NewRecorder()
	handler.ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if metrics.Code != http.StatusOK {
		t.Fatalf("metrics %d %s", metrics.Code, metrics.Body.String())
	}
	got, ok, err := readPromMetricValueWithLabels(metrics.Body.String(), "binlog_server_recovery_breaks", map[string]string{"task_id": "1"})
	if err != nil || !ok || got < 1 {
		t.Fatalf("breaks metric ok=%v err=%v value=%v body=%s", ok, err, got, metrics.Body.String())
	}
	age, ok, err := readPromMetricValueWithLabels(metrics.Body.String(), "binlog_server_recovery_earliest_age_seconds", map[string]string{"task_id": "1"})
	if err != nil || !ok || age <= 0 {
		t.Fatalf("age metric ok=%v err=%v value=%v", ok, err, age)
	}

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/api/tasks/missing/window", nil))
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), "task not found") {
		t.Fatalf("missing status=%d body=%s", missing.Code, missing.Body.String())
	}
}

func getRecoveryWindow(t *testing.T, handler http.Handler, path string) tasks.RecoveryWindow {
	t.Helper()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, path, nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("window status=%d body=%s", resp.Code, resp.Body.String())
	}
	var got tasks.RecoveryWindow
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func recoveryReason(window tasks.RecoveryWindow, text string) bool {
	for _, br := range window.Breaks {
		if strings.Contains(br.Reason, text) {
			return true
		}
	}
	return false
}

func TestMetricsRecoveryBreakCount(t *testing.T) {
	dir := t.TempDir()
	scheduler := tasks.NewScheduler(tasks.WithDataDir(dir))
	handler := NewServer(scheduler)
	create(`{"name":"mysql","cluster_key":"mysql-key","source":{"host":"127.0.0.1","port":3306,"user":"repl","password":"secret","flavor":"mysql"}}`, t, handler)
	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000001"), apiExecutedSegment(t, when, "", 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000003"), apiExecutedSegment(t, when.Add(time.Minute), "", 3), 0o644); err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	got, ok, err := readPromMetricValueWithLabels(resp.Body.String(), "binlog_server_recovery_breaks", map[string]string{"task_id": "1"})
	if err != nil || !ok || got != 2 {
		t.Fatalf("want index gap and gtid hole, ok=%v err=%v value=%v body has %s", ok, err, got, strconv.FormatFloat(got, 'f', -1, 64))
	}
}
