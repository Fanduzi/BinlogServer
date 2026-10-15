package tasks

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
	"binlog_server/internal/taskrecords"
)

// Tip QA b4c52f4 P1: with the default health.evaluator=off a standalone
// leftover directory (every task upgraded from v0.5.66, and every task
// created while no key was configured) raised nothing and /api/health
// stayed 200, so the backup stopped silently. TASK_UNADOPTED must be raised
// in every evaluator mode; while the evaluator does not own it, through the
// legacy sink.

func unadoptedOpen(t *testing.T, s *Scheduler, id string) *Finding {
	t.Helper()
	fs, err := s.ListFindings(context.Background(), id, false, 50)
	if err != nil {
		t.Fatal(err)
	}
	for i := range fs {
		if string(fs[i].Kind) == string(health.TaskUnadopted) {
			return &fs[i]
		}
	}
	return nil
}

func TestUnadopted_EvaluatorOffRaisesItAndDegradesHealth(t *testing.T) {
	data, dir := leftoverDataDir(t)
	s := NewScheduler(WithDataDir(data))
	if err := s.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := unadoptedOpen(t, s, "7")
	if f == nil || f.Severity != FindingCritical {
		t.Fatalf("evaluator off: leftover directory raised %+v, want CRITICAL TASK_UNADOPTED", f)
	}
	if got := s.HealthSummary(context.Background()); got.Status != HealthDegraded {
		t.Fatalf("health %+v with a leftover directory, want degraded", got)
	}
	if _, err := s.AdoptDiskBackup("7", TaskPatch{ClusterKey: "adopted-7", Source: adoptSource(), Start: &StartConfig{Mode: StartModeFilePos, File: "mysql-bin.000002", Pos: 4}}); err != nil {
		t.Fatal(err)
	}
	if f := unadoptedOpen(t, s, "7"); f != nil {
		t.Fatalf("adopted task still TASK_UNADOPTED: %+v", f)
	}
	if got := s.HealthSummary(context.Background()); got.Status != HealthOK {
		t.Fatalf("health %+v after adopt, want ok", got)
	}
	_ = dir
}

func TestUnadopted_EvaluatorOffClosesWhenDirectoryMoved(t *testing.T) {
	data, dir := leftoverDataDir(t)
	s := NewScheduler(WithDataDir(data))
	if err := s.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if unadoptedOpen(t, s, "7") == nil {
		t.Fatal("setup: no TASK_UNADOPTED")
	}
	if err := os.Rename(dir, filepath.Join(t.TempDir(), "7")); err != nil {
		t.Fatal(err)
	}
	s.reconcileUnadoptedLegacy()
	if f := unadoptedOpen(t, s, "7"); f != nil {
		t.Fatalf("directory moved out of data_dir, TASK_UNADOPTED still open: %+v", f)
	}
}

func TestUnadopted_EvaluatorShadowRaisesItThroughLegacySink(t *testing.T) {
	data, _ := leftoverDataDir(t)
	s := NewScheduler(WithDataDir(data))
	newEval(t, s, healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName)), EvaluatorShadow)
	if err := s.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if unadoptedOpen(t, s, "7") == nil {
		t.Fatal("evaluator shadow: leftover directory raised nothing")
	}
}

// The notes' step 3: a task created while no key was configured is a
// leftover after the restart that adds the key. It must not go silent.
func TestUnadopted_TaskCreatedWithoutKeyIsRaisedAfterKeyRestart(t *testing.T) {
	data := recordDir(t)
	s1, r1 := recordProcess(t, data, taskrecords.Open(data, nil, nil), taskrecords.ErrKeyMissing)
	task, err := s1.CreateTaskFromSpec("k", "k-key", recordSourceCfg(), &StartConfig{Mode: StartModeLatest}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.StartTask(task.ID); err != nil {
		t.Fatal(err)
	}
	r1.next(t)
	if err := os.MkdirAll(filepath.Join(data, task.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAlertSegment(t, filepath.Join(data, task.ID, "mysql-bin.000001"), 1, 2)
	s2, _ := recordProcess(t, data, taskrecords.Open(data, recordKey(t, 1), nil), nil)
	deadline := time.Now().Add(2 * time.Second)
	for unadoptedOpen(t, s2, task.ID) == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if unadoptedOpen(t, s2, task.ID) == nil {
		t.Fatalf("task %s created without a key is a silent leftover after the key restart", task.ID)
	}
	if got := s2.HealthSummary(context.Background()); got.Status != HealthDegraded {
		t.Fatalf("health %+v, want degraded", got)
	}
}
