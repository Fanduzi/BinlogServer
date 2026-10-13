// Package api provides module-level functionality for api.
// input: a task service whose progress, checkpoint, recovery window and heartbeat reads fail
// output: proof that S10 (a failed metric source drops the series) and S11 (a failed progress read looks like no progress) are now visible
// pos: fault-injection tests for ADR 0009 phase 0, metrics and the task list
// note: if this file changes, update this header and module README.md.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"

	"github.com/prometheus/client_golang/prometheus"
)

var errSource = errors.New("meta down")

type brokenSources struct{ *tasks.Scheduler }

func (brokenSources) GetReplicationProgress(string) (tasks.ReplicationProgress, bool, error) {
	return tasks.ReplicationProgress{}, false, errSource
}
func (brokenSources) GetCheckpoint(context.Context, string) (binlog.Checkpoint, bool, error) {
	return binlog.Checkpoint{}, false, errSource
}
func (brokenSources) RecoveryWindow(string) (tasks.RecoveryWindow, error) {
	return tasks.RecoveryWindow{}, errSource
}
func (brokenSources) ListWorkerHeartbeats(int) ([]tasks.WorkerHeartbeat, error) {
	return nil, errSource
}

// S10: a failed per-task read is an explicit 0 on metric_source_up plus a finding, not a missing series.
func TestS10_FailedMetricSourceIsVisible(t *testing.T) {
	sched := tasks.NewScheduler()
	reg := prometheus.NewRegistry()
	reg.MustRegister(newAPIMetricsCollector(brokenSources{sched}, []tasks.Task{{ID: "10", State: tasks.StateRunning}}))
	if _, err := reg.Gather(); err != nil {
		t.Fatal(err)
	}
	up := sourceUp(t, reg)
	for _, k := range []string{"10/progress", "10/checkpoint", "10/recovery_window", "_cluster/worker_heartbeats"} {
		if v, ok := up[k]; !ok || v != 0 {
			t.Fatalf("metric_source_up %s = %v (present %t), want an explicit 0; got %v", k, v, ok, up)
		}
	}
	open := sched.FindingStats().Open
	var got []string
	for _, f := range open {
		if f.Kind == tasks.FindingMetricSourceFailed {
			got = append(got, f.TaskID+"/"+f.Subject)
		}
	}
	if len(got) != 4 {
		t.Fatalf("METRIC_SOURCE_FAILED findings %v, want progress/checkpoint/recovery_window on 10 and worker_heartbeats on _cluster", got)
	}
}

// S11: a RUNNING task whose progress read failed says so instead of a bare NO_PROGRESS.
func TestS11_ProgressReadErrorIsCarried(t *testing.T) {
	s := &Server{tasks: brokenSources{tasks.NewScheduler()}}
	rep := s.replicationFor(tasks.Task{ID: "11", State: tasks.StateRunning}, time.Now())
	if rep.Reason != "PROGRESS_UNAVAILABLE" || !strings.Contains(rep.LastError, "meta down") || rep.Status != "ABNORMAL" {
		t.Fatalf("replication %+v, want ABNORMAL/PROGRESS_UNAVAILABLE carrying the error", rep)
	}
}

func sourceUp(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, fam := range fams {
		if fam.GetName() != "binlog_server_metric_source_up" {
			continue
		}
		for _, m := range fam.GetMetric() {
			l := map[string]string{}
			for _, p := range m.GetLabel() {
				l[p.GetName()] = p.GetValue()
			}
			out[l["task_id"]+"/"+l["source"]] = m.GetGauge().GetValue()
		}
	}
	return out
}

// S14 (API half): a dashboard row carries its open finding count, so the Console alert view can see it.
func TestS14_DashboardRowCarriesOpenFindings(t *testing.T) {
	sched := tasks.NewScheduler()
	task, err := sched.CreateTask("s14", "c")
	if err != nil {
		t.Fatal(err)
	}
	sched.Report(tasks.Finding{TaskID: task.ID, Kind: tasks.FindingUploadUnverified, Subject: "mysql-bin.000001", Severity: tasks.FindingWarning})
	rec := httptest.NewRecorder()
	NewServer(sched).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/dashboard", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Tasks []struct {
			OpenFindings int `json:"open_findings"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tasks) != 1 || out.Tasks[0].OpenFindings != 1 {
		t.Fatalf("dashboard rows %+v, want open_findings 1", out.Tasks)
	}
}
