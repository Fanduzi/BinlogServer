// Package api provides module-level functionality for api.
// input: a health sample list and a live evaluator sweep behind /metrics
// output: the exposition text for task_healthy and the SLO gauges
// pos: F2 metrics contract
// note: if this file changes, update this header and module README.md.
package api

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/healthstore"
	"binlog_server/internal/tasks"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type healthStub struct {
	samples []tasks.HealthSample
}

func (h healthStub) HealthSamples() []tasks.HealthSample { return h.samples }

func TestHealthMetricsExposition(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(newHealthCollector(healthStub{samples: []tasks.HealthSample{
		{TaskID: "9", Value: 0, Reason: "critical", ProgressAge: 50, UploadLag: 1200, OpenCritical: 2},
		{TaskID: "3", Value: 1, Reason: "ok", ProgressAge: 1, UploadLag: 0, OpenCritical: 0},
	}}))
	body := gatherMetrics(t, reg)
	for _, want := range []string{
		`binlog_server_task_healthy{reason="critical",task_id="9"} 0`,
		`binlog_server_task_healthy{reason="ok",task_id="3"} 1`,
		`binlog_server_task_progress_age_seconds{task_id="9"} 50`,
		`binlog_server_task_upload_lag_seconds{task_id="9"} 1200`,
		`binlog_server_task_open_critical{task_id="9"} 2`,
		`binlog_server_task_open_critical{task_id="3"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in\n%s", want, body)
		}
	}
}

func TestHealthMetricsSentinelWhenNoEvaluator(t *testing.T) {
	body := scrape(t, NewServer(tasks.NewScheduler()))
	if !strings.Contains(body, `binlog_server_task_healthy{reason="ok",task_id=""} 1`) {
		t.Fatalf("sentinel missing:\n%s", body)
	}
}

func TestHealthMetricsFromEvaluator(t *testing.T) {
	data := t.TempDir()
	s := tasks.NewScheduler(tasks.WithDataDir(data))
	task, err := s.CreateTaskFromSpec("mysql", "k", &tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: "mysql"}, &tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: "3e11fa47-71ca-11e1-9e33-c80aa9429562:1-36"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(data+"/"+task.ID, 0o755); err != nil {
		t.Fatal(err)
	}
	store := healthstore.NewFileStore(data + "/.health.json")
	e := tasks.NewEvaluator(s, tasks.EvaluatorConfig{Mode: tasks.EvaluatorOn, Store: store, NodeID: "n", Instance: "i", Sweep: time.Minute, Debounce: time.Millisecond})
	ctx := context.Background()
	if _, err := e.EvaluateNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	body := scrape(t, NewServer(s))
	want := `binlog_server_task_healthy{reason="ok",task_id="` + task.ID + `"} 1`
	if !strings.Contains(body, want) {
		t.Fatalf("missing %s in\n%s", want, body)
	}
	if !strings.Contains(body, `binlog_server_task_progress_age_seconds{task_id="`+task.ID+`"}`) ||
		!strings.Contains(body, `binlog_server_task_upload_lag_seconds{task_id="`+task.ID+`"}`) ||
		!strings.Contains(body, `binlog_server_task_open_critical{task_id="`+task.ID+`"} 0`) {
		t.Fatalf("SLO series missing:\n%s", body)
	}

	e.Signal(task.ID, tasks.FindingTaskPersistFailed, "", false, "meta down")
	if _, err := e.EvaluateNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	body = scrape(t, NewServer(s))
	if !strings.Contains(body, `binlog_server_task_healthy{reason="critical",task_id="`+task.ID+`"} 0`) {
		t.Fatalf("critical verdict missing:\n%s", body)
	}
	if !strings.Contains(body, `binlog_server_task_open_critical{task_id="`+task.ID+`"} 1`) {
		t.Fatalf("open critical missing:\n%s", body)
	}
}

func gatherMetrics(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	rr := newRecorder()
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(rr, req)
	if rr.code != http.StatusOK {
		t.Fatalf("status %d", rr.code)
	}
	return rr.body
}

type memResponse struct {
	code   int
	header http.Header
	body   string
}

func newRecorder() *memResponse {
	return &memResponse{code: 200, header: http.Header{}}
}

func (m *memResponse) Header() http.Header { return m.header }

func (m *memResponse) Write(b []byte) (int, error) {
	m.body += string(b)
	return len(b), nil
}

func (m *memResponse) WriteHeader(status int) { m.code = status }
