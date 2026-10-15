// Package tasks provides module-level functionality for tasks.
// input: a real Scheduler with a task directory, a healthstore.FileStore, a settable clock, and hot-path signals through SignalFailure/SignalSuccess and the legacy Report/Resolve
// output: the P2-4 QA plan (design §14) end to end: a restart with meta still read-only never closes the open signal rows; a black-holed source keeps at least one finding open (DUMP_STALLED, then SOURCE_UNREACHABLE with DUMP_STALLED handed off) and RETRY_BACKOFF cycling does not close it; a deliberate Stop closes DUMP_STALLED; no findings is HEALTHY on the first pass; a FAILED task opens TASK_FAILED and is not HEALTHY; a _cluster signal is evaluated in standalone; the legacy sink never writes a managed kind while the evaluator owns it, and still does when the evaluator is off
// pos: ADR 0009 phase 2 P2-4 (docs/design/0009-phase2-health-evaluator.md §5.5, §5.6, §14)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
)

type recStore struct {
	*healthstore.FileStore
	mu      sync.Mutex
	applied []health.Transition
}

func (r *recStore) Unwrap() healthstore.Store { return r.FileStore }

func (r *recStore) ApplyTransitions(ctx context.Context, id string, f health.Fence, ts []health.Transition, h health.TaskHealth) (healthstore.Applied, error) {
	a, err := r.FileStore.ApplyTransitions(ctx, id, f, ts, h)
	if err == nil {
		r.mu.Lock()
		r.applied = append(r.applied, ts...)
		r.mu.Unlock()
	}
	return a, err
}

func (r *recStore) closeReason(k health.Kind) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.applied {
		if t.Kind == k && t.Op == health.OpClose {
			return t.Reason
		}
	}
	return ""
}

type p24Env struct {
	s     *Scheduler
	e     *Evaluator
	store *recStore
	task  Task
	mu    sync.Mutex
	now   time.Time
}

func newP24(t *testing.T, mode EvaluatorMode) *p24Env {
	t.Helper()
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, dir := evalTask(t, s, data)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000001"), 37, 38)
	env := &p24Env{s: s, task: task, now: time.Now(),
		store: &recStore{FileStore: healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))}}
	env.e = NewEvaluator(s, EvaluatorConfig{Mode: mode, Store: env.store, NodeID: evalNode, Instance: "i1", Sweep: time.Minute, Debounce: time.Millisecond,
		Now: func() time.Time { env.mu.Lock(); defer env.mu.Unlock(); return env.now }})
	return env
}

func (p *p24Env) advance(d time.Duration) { p.mu.Lock(); p.now = p.now.Add(d); p.mu.Unlock() }

func (p *p24Env) setTask(t *testing.T, state State, desired string, mutate func(*Task)) {
	t.Helper()
	p.s.mu.Lock()
	task := p.s.tasks[p.task.ID]
	task.State, task.DesiredRun = state, desired
	if mutate != nil {
		mutate(&task)
	}
	p.s.tasks[p.task.ID] = task
	p.s.mu.Unlock()
}

func (p *p24Env) eval(t *testing.T) health.TaskHealth {
	t.Helper()
	h, err := p.e.EvaluateNow(context.Background(), p.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (p *p24Env) open(t *testing.T) map[string]health.Finding {
	return openOf(t, p.store.FileStore, p.task.ID)
}

func TestP24_NoFindingsIsHealthyOnTheFirstPass(t *testing.T) {
	p := newP24(t, EvaluatorOn)
	p.setTask(t, StateRunning, TaskDesiredRun, nil)
	if h := p.eval(t); h.Status != health.StatusHealthy {
		t.Fatalf("first pass: %+v", h)
	}
}

func TestP24_RestartWithMetaStillReadOnlyKeepsTheRowOpen(t *testing.T) {
	p := newP24(t, EvaluatorOn)
	p.setTask(t, StateRetryBackoff, TaskDesiredRun, nil)
	p.s.SignalFailure(Finding{TaskID: p.task.ID, Kind: FindingMetadataWriteBlocked, Severity: FindingCritical, Detail: "Error 1290: --read-only"})
	p.eval(t)
	if _, ok := p.open(t)["METADATA_WRITE_BLOCKED/"]; !ok {
		t.Fatalf("failure did not open: %+v", p.open(t))
	}
	// Restart: a new evaluator with an empty signal book over the same store.
	p.e = NewEvaluator(p.s, EvaluatorConfig{Mode: EvaluatorOn, Store: p.store, NodeID: evalNode, Instance: "i2", Debounce: time.Millisecond, Now: p.e.cfg.Now})
	for i := 0; i < 3; i++ {
		if i == 1 {
			p.s.SignalFailure(Finding{TaskID: p.task.ID, Kind: FindingMetadataWriteBlocked, Severity: FindingCritical, Detail: "Error 1290: --read-only"})
		}
		if h := p.eval(t); h.Status == health.StatusHealthy {
			t.Fatalf("pass %d: read-only meta reported HEALTHY", i)
		}
		if _, ok := p.open(t)["METADATA_WRITE_BLOCKED/"]; !ok {
			t.Fatalf("pass %d closed the row with no success", i)
		}
	}
	p.s.SignalSuccess(p.task.ID, FindingMetadataWriteBlocked, "")
	p.eval(t)
	if _, ok := p.open(t)["METADATA_WRITE_BLOCKED/"]; ok {
		t.Fatal("first success did not close")
	}
	if st := p.s.FindingStats(); len(st.Open) != 0 {
		t.Fatalf("the legacy sink wrote a managed kind: %+v", st.Open)
	}
}

func TestP24_BlackHoleKeepsAFindingOpenAndHandsOff(t *testing.T) {
	p := newP24(t, EvaluatorOn)
	p.setTask(t, StateRunning, TaskDesiredRun, func(t *Task) { t.Epoch = 3 })
	p.s.SignalFailure(Finding{TaskID: p.task.ID, Kind: FindingDumpStalled, Severity: FindingWarning, Detail: "no event for 30s"})
	p.eval(t)
	if _, ok := p.open(t)["DUMP_STALLED/"]; !ok {
		t.Fatalf("stall not open: %+v", p.open(t))
	}
	for i, st := range []State{StateRetryBackoff, StateStarting, StateRetryBackoff, StateStarting, StateRetryBackoff} {
		p.setTask(t, st, TaskDesiredRun, func(t *Task) { t.Epoch = 3; t.LastError = "SOURCE_UNREACHABLE: dial tcp 10.0.0.9:3306: i/o timeout" })
		p.advance(25 * time.Second)
		if h := p.eval(t); h.Status == health.StatusHealthy {
			t.Fatalf("step %d (%s): black hole reported HEALTHY", i, st)
		}
		if len(p.open(t)) == 0 {
			t.Fatalf("step %d (%s): no finding open", i, st)
		}
	}
	p.eval(t) // the pass after SOURCE_UNREACHABLE opened hands DUMP_STALLED off
	open := p.open(t)
	if _, ok := open["SOURCE_UNREACHABLE/"]; !ok {
		t.Fatalf("SOURCE_UNREACHABLE not open after 125s: %+v", open)
	}
	if _, ok := open["DUMP_STALLED/"]; ok {
		t.Fatal("DUMP_STALLED not handed off")
	}
	if r := p.store.closeReason(health.DumpStalled); r != health.ReasonHandedOff {
		t.Fatalf("DUMP_STALLED close reason %q", r)
	}
	// Data again: the dump's first event closes SOURCE_UNREACHABLE.
	p.setTask(t, StateRunning, TaskDesiredRun, func(t *Task) { t.Epoch = 3 })
	p.s.SignalSuccess(p.task.ID, FindingDumpStalled, "")
	if h := p.eval(t); h.Status != health.StatusHealthy || len(p.open(t)) != 0 {
		t.Fatalf("recovered: %+v %+v", h, p.open(t))
	}
}

func TestP24_StopClosesDumpStalled(t *testing.T) {
	p := newP24(t, EvaluatorOn)
	p.setTask(t, StateRunning, TaskDesiredRun, func(t *Task) { t.Epoch = 3 })
	p.s.SignalFailure(Finding{TaskID: p.task.ID, Kind: FindingDumpStalled, Severity: FindingWarning, Detail: "silent"})
	p.eval(t)
	p.setTask(t, StateStopped, TaskDesiredStop, nil)
	p.eval(t)
	if _, ok := p.open(t)["DUMP_STALLED/"]; ok {
		t.Fatalf("Stop did not close DUMP_STALLED: %+v", p.open(t))
	}
}

func TestP24_FailedTaskOpensTaskFailedAndIsNotHealthy(t *testing.T) {
	p := newP24(t, EvaluatorOn)
	p.setTask(t, StateFailed, TaskDesiredStop, func(t *Task) {
		t.SpecRevision, t.FailedSpecRevision = 4, 4
		t.LastError = "STREAM_REGRESSION: rotate to mysql-bin.000001 goes backwards"
	})
	if h := p.eval(t); h.Status != health.StatusUnhealthy {
		t.Fatalf("FAILED task health %+v", h)
	}
	if _, ok := p.open(t)["TASK_FAILED/"+health.FailStreamRegression]; !ok {
		t.Fatalf("TASK_FAILED not open: %+v", p.open(t))
	}
	p.setTask(t, StateStarting, TaskDesiredRun, func(t *Task) { t.SpecRevision = 5 })
	p.eval(t)
	if _, ok := p.open(t)["TASK_FAILED/"+health.FailStreamRegression]; ok {
		t.Fatal("restart did not close TASK_FAILED")
	}
}

func TestP24_ClusterSignalIsEvaluatedStandalone(t *testing.T) {
	p := newP24(t, EvaluatorOn)
	p.s.SignalFailure(Finding{TaskID: ClusterFindingTask, Kind: FindingRetryLoopFailed, Subject: "sync", Severity: FindingCritical, Detail: "task list read failed"})
	if err := p.e.SweepNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := openOf(t, p.store.FileStore, ClusterFindingTask)["RETRY_LOOP_FAILED/sync"]; !ok {
		t.Fatalf("_cluster signal not open: %+v", openOf(t, p.store.FileStore, ClusterFindingTask))
	}
	p.s.SignalSuccess(ClusterFindingTask, FindingRetryLoopFailed, "sync")
	if err := p.e.SweepNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(openOf(t, p.store.FileStore, ClusterFindingTask)) != 0 {
		t.Fatal("_cluster success did not close")
	}
}

func TestP24_LegacyReportOfAManagedKindNeverWritesTheSink(t *testing.T) {
	p := newP24(t, EvaluatorOn)
	p.s.Report(Finding{TaskID: p.task.ID, Kind: FindingEventPersistFailed, Subject: "STARTED", Severity: FindingWarning, Detail: "x"})
	p.s.Report(Finding{TaskID: p.task.ID, Kind: FindingTransitionStalled, Severity: FindingWarning, Detail: "x"})
	if st := p.s.FindingStats(); len(st.Open) != 0 {
		t.Fatalf("legacy sink wrote a managed kind: %+v", st.Open)
	}
	p.eval(t)
	if _, ok := p.open(t)["EVENT_PERSIST_FAILED/STARTED"]; !ok {
		t.Fatalf("the Report became no signal: %+v", p.open(t))
	}
}

func TestP24_OffModeKeepsTheLegacySink(t *testing.T) {
	s := NewScheduler(WithDataDir(t.TempDir()))
	s.SignalFailure(Finding{TaskID: "1", Kind: FindingEventPersistFailed, Subject: "STARTED", Severity: FindingWarning, Detail: "x"})
	if st := s.FindingStats(); len(st.Open) != 1 {
		t.Fatalf("off: %+v", st.Open)
	}
	s.SignalSuccess("1", FindingEventPersistFailed, "STARTED")
	if st := s.FindingStats(); len(st.Open) != 0 {
		t.Fatalf("off resolve: %+v", st.Open)
	}
}
