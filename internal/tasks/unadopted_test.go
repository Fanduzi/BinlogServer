// Package tasks provides module-level functionality for tasks.
// input: a standalone data_dir (no task store) holding on-disk backups that no in-memory task drives, as after a restart
// output: #385 as tests: a leftover directory opens CRITICAL TASK_UNADOPTED naming the adopt call; adopt closes it; moving the directory out of data_dir closes it, also in a later process; a task created in this process never opens it
// pos: ADR 0009 phase 2 §19 (docs/design/0009-phase2-health-evaluator.md)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
)

const unadoptedKey = "TASK_UNADOPTED/"

// leftoverDataDir is a data_dir after a standalone restart: task 7's
// segments are on disk and no process has a record of the task.
func leftoverDataDir(t *testing.T) (string, string) {
	t.Helper()
	data := t.TempDir()
	dir := filepath.Join(data, "7")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000001"), 1, 2)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000002.open.e1"), 3, 4)
	return data, dir
}

func adoptSource() *SourceConfig {
	return &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "s3cret", Flavor: "mysql"}
}

func TestEvaluator_StandaloneLeftoverOpensTaskUnadopted(t *testing.T) {
	data, _ := leftoverDataDir(t)
	s := NewScheduler(WithDataDir(data))
	store := healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))
	e := newEval(t, s, store, EvaluatorOn)
	ctx := context.Background()
	if err := e.SweepNow(ctx); err != nil {
		t.Fatal(err)
	}
	f, ok := openOf(t, store, "7")[unadoptedKey]
	if !ok {
		t.Fatalf("leftover directory after a standalone restart opens nothing: %+v", openOf(t, store, "7"))
	}
	if f.Severity != health.Critical {
		t.Fatalf("severity %s, want CRITICAL", f.Severity)
	}
	if !strings.Contains(f.Detail, "POST /api/tasks/7/adopt") || !strings.Contains(f.Detail, "replication") {
		t.Fatalf("detail does not say replication stopped and how to adopt: %q", f.Detail)
	}
	h, err := e.EvaluateNow(ctx, "7")
	if err != nil {
		t.Fatal(err)
	}
	if h.Status == health.StatusHealthy {
		t.Fatalf("leftover task is HEALTHY: %+v", h)
	}
	if _, err := s.AdoptDiskBackup("7", TaskPatch{Source: adoptSource()}); err != nil {
		t.Fatal(err)
	}
	if err := e.SweepNow(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := openOf(t, store, "7")[unadoptedKey]; ok {
		t.Fatalf("adopted task still TASK_UNADOPTED: %+v", openOf(t, store, "7"))
	}
}

func TestEvaluator_TaskUnadoptedClosesWhenDirectoryMoved(t *testing.T) {
	data, dir := leftoverDataDir(t)
	s := NewScheduler(WithDataDir(data))
	store := healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))
	e := newEval(t, s, store, EvaluatorOn)
	ctx := context.Background()
	if err := e.SweepNow(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := openOf(t, store, "7")[unadoptedKey]; !ok {
		t.Fatal("not open")
	}
	if err := os.Rename(dir, filepath.Join(t.TempDir(), "7")); err != nil {
		t.Fatal(err)
	}
	if err := e.SweepNow(ctx); err != nil {
		t.Fatal(err)
	}
	if open := openOf(t, store, "7"); len(open) != 0 {
		t.Fatalf("moved-away directory still has open findings: %+v", open)
	}
}

func TestEvaluator_TaskUnadoptedClosesInLaterProcessWhenDirectoryGone(t *testing.T) {
	data, dir := leftoverDataDir(t)
	store := healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))
	ctx := context.Background()
	first := newEval(t, NewScheduler(WithDataDir(data)), store, EvaluatorOn)
	if err := first.SweepNow(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := openOf(t, store, "7")[unadoptedKey]; !ok {
		t.Fatal("not open")
	}
	// The process stops, the operator moves the directory, a new process starts.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	second := newEval(t, NewScheduler(WithDataDir(data)), store, EvaluatorOn)
	if err := second.SweepNow(ctx); err != nil {
		t.Fatal(err)
	}
	if open := openOf(t, store, "7"); len(open) != 0 {
		t.Fatalf("directory gone before restart, row still open: %+v", open)
	}
}

func TestEvaluator_TaskCreatedHereIsNotUnadopted(t *testing.T) {
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, dir := evalTask(t, s, data)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000001"), 37, 38)
	store := healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))
	e := newEval(t, s, store, EvaluatorOn)
	if err := e.SweepNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := openOf(t, store, task.ID)[unadoptedKey]; ok {
		t.Fatalf("a task this process created is TASK_UNADOPTED")
	}
}
