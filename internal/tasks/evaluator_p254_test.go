// Package tasks provides module-level functionality for tasks.
// input: a task store that probes seal-fact columns, and a runner that records a finished retention pass
// output: proof that one evaluation opens SEAL_FACTS_ABSENT and the next closes it without leaving health UNKNOWN, and that a retention pass finishing after a RETENTION_RECORD_FAILED failure probes that signal closed (design 0009 §26)
// pos: P2-5.4 evaluator wiring for #353 and #341
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
)

type p254Store struct {
	task   Task
	ver    int
	absent bool
}

func (s *p254Store) ProbeSealFacts(context.Context) (int, bool, error) {
	return s.ver, s.absent, nil
}
func (s *p254Store) UpsertTask(_ context.Context, task Task) error { s.task = task; return nil }
func (s *p254Store) GetTask(_ context.Context, id string) (Task, error) {
	if s.task.ID == id {
		return s.task, nil
	}
	return Task{}, ErrTaskNotFound
}
func (s *p254Store) ListTasks(context.Context) ([]Task, error) { return []Task{s.task}, nil }
func (s *p254Store) ListTasksPage(context.Context, TaskListFilter) ([]Task, int, error) {
	return []Task{s.task}, 1, nil
}
func (s *p254Store) ListStartingUnownedTasks(context.Context) ([]Task, error) { return nil, nil }
func (s *p254Store) DeleteTask(context.Context, string) error                 { return nil }

func TestP254EvaluatorSealFactsAbsentOpensAndCloses(t *testing.T) {
	data := t.TempDir()
	st := &p254Store{ver: 7, absent: true}
	s := NewScheduler(WithDataDir(data), WithStore(st))
	task, err := s.CreateTaskFromSpec("mysql", "k", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "u", Password: "p", Flavor: "mysql"},
		&StartConfig{Mode: StartModeLatest}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(data, task.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	store := healthstore.NewFileStore(filepath.Join(t.TempDir(), "h.json"))
	e := newEval(t, s, store, EvaluatorOn)
	h, err := e.EvaluateNow(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := openOf(t, store, task.ID)[string(health.SealFactsAbsent)+"/"]; !ok {
		t.Fatalf("SEAL_FACTS_ABSENT not open, health=%s findings=%v", h.Status, openOf(t, store, task.ID))
	}
	if h.Status == health.StatusUnknown {
		t.Fatalf("open finding left health UNKNOWN: %+v", h)
	}
	st.absent = false
	st.ver = 8
	h, err = e.EvaluateNow(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := openOf(t, store, task.ID)[string(health.SealFactsAbsent)+"/"]; ok {
		t.Fatal("SEAL_FACTS_ABSENT stayed open after the columns returned")
	}
	if h.Status == health.StatusUnknown {
		t.Fatalf("closed finding left health UNKNOWN: %+v", h)
	}
}

// p254CleanRunner is the retention-pass clock the evaluator reads. It does
// not run a task; the test only plants RetentionCleanAt.
type p254CleanRunner struct {
	at map[string]time.Time
}

func (r *p254CleanRunner) Run(context.Context, Task) error { return nil }

func (r *p254CleanRunner) RetentionCleanAt(taskID string) time.Time {
	if r.at == nil {
		return time.Time{}
	}
	return r.at[taskID]
}

func TestP254RetentionCleanProbesRecordFailure(t *testing.T) {
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	s := NewScheduler()
	runner := &p254CleanRunner{}
	s.SetRunner(runner)
	e := NewEvaluator(s, EvaluatorConfig{Mode: EvaluatorOn, Now: func() time.Time { return now }})
	e.Signal("1", FindingRetentionRecordFailed, "mysql-bin.000001", false, "event lost")
	key := health.Key{TaskID: "1", Kind: health.RetentionRecordFailed, Subject: "mysql-bin.000001"}
	open := []health.Finding{{TaskID: "1", Kind: health.RetentionRecordFailed, Subject: "mysql-bin.000001", LastSeen: now, Severity: health.Warning}}

	facts := &health.Facts{TaskID: "1", Now: now, Complete: map[health.Scope]bool{health.ScopeSignals: true}}
	e.openAndProbes(context.Background(), "1", open, facts)
	if facts.Probes[key] {
		t.Fatal("probe closed the failure before any clean retention pass")
	}

	runner.at = map[string]time.Time{"1": now.Add(-time.Second)}
	facts = &health.Facts{TaskID: "1", Now: now, Complete: map[health.Scope]bool{health.ScopeSignals: true}}
	e.openAndProbes(context.Background(), "1", open, facts)
	if facts.Probes[key] {
		t.Fatal("an older retention pass closed a newer failure")
	}

	runner.at["1"] = now.Add(time.Second)
	facts = &health.Facts{TaskID: "1", Now: now, Complete: map[health.Scope]bool{health.ScopeSignals: true}}
	e.openAndProbes(context.Background(), "1", open, facts)
	if !facts.Probes[key] {
		t.Fatal("a retention pass after the failure did not probe it closed")
	}
}
