// Package tasks provides module-level functionality for tasks.
// input: a real Scheduler with a task directory, a healthstore.FileStore, a settable clock, and hot-path signals through SignalFailure/SignalSuccess and the legacy Report/Resolve
// output: the P2-4 QA plan (design §14) end to end: a restart with meta still read-only never closes the open signal rows; a black-holed source raises DUMP_STALLED from the liveness clock 45s after the last read in any state, keeps a finding open through the retry loop and hands off to SOURCE_UNREACHABLE in the same pass with the real silence in the detail; a 63s planned restart with 92s of backoff never opens SOURCE_UNREACHABLE; a deliberate Stop closes DUMP_STALLED and a restart starts the clock over; the scheduler probes a silent source and counts runner SOURCE_UNREACHABLE errors as failed attempts; no findings is HEALTHY on the first pass; a FAILED task opens TASK_FAILED and is not HEALTHY; a _cluster signal is evaluated in standalone; the legacy sink never writes a managed kind while the evaluator owns it, and still does when the evaluator is off
// pos: ADR 0009 phase 2 P2-4 (docs/design/0009-phase2-health-evaluator.md §5.5, §5.6, §14)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

func (p *p24Env) at() time.Time { p.mu.Lock(); defer p.mu.Unlock(); return p.now }

// #383 QA F1: a black-holed source raises DUMP_STALLED from the liveness
// clock 45s after the last byte, in any state, keeps a finding open through
// the retry loop, and hands off to SOURCE_UNREACHABLE in the same pass.
func TestP24_BlackHoleKeepsAFindingOpenAndHandsOff(t *testing.T) {
	p := newP24(t, EvaluatorOn)
	p.setTask(t, StateRunning, TaskDesiredRun, func(t *Task) { t.Epoch = 3 })
	p.s.noteSourceRead(p.task.ID, p.at())
	p.eval(t)
	if len(p.open(t)) != 0 {
		t.Fatalf("healthy source: %+v", p.open(t))
	}
	p.advance(46 * time.Second) // nothing read: not even a heartbeat
	p.eval(t)
	if _, ok := p.open(t)["DUMP_STALLED/"]; !ok {
		t.Fatalf("no DUMP_STALLED after 46s of silence while RUNNING: %+v", p.open(t))
	}
	for i, st := range []State{StateRetryBackoff, StateStarting, StateRetryBackoff, StateStarting} {
		p.setTask(t, st, TaskDesiredRun, func(t *Task) { t.Epoch = 3; t.LastError = "SOURCE_UNREACHABLE: i/o timeout" })
		p.advance(10 * time.Second)
		p.s.noteSourceFailure(p.task.ID, p.at(), errors.New("readInitialHandshake: i/o timeout"))
		if h := p.eval(t); h.Status == health.StatusHealthy || len(p.open(t)) == 0 {
			t.Fatalf("step %d (%s): no finding open for a dead source: %+v", i, st, h)
		}
	}
	open := p.open(t) // 86s: still DUMP_STALLED
	if _, ok := open["DUMP_STALLED/"]; !ok {
		t.Fatalf("at 86s: %+v", open)
	}
	p.advance(5 * time.Second)
	p.s.noteSourceFailure(p.task.ID, p.at(), errors.New("readInitialHandshake: i/o timeout"))
	p.eval(t) // 91s: the same pass opens SOURCE_UNREACHABLE and hands DUMP_STALLED off
	open = p.open(t)
	if _, ok := open["SOURCE_UNREACHABLE/"]; !ok || len(open) != 1 {
		t.Fatalf("at 91s: %+v", open)
	}
	if r := p.store.closeReason(health.DumpStalled); r != health.ReasonHandedOff {
		t.Fatalf("DUMP_STALLED close reason %q", r)
	}
	if d := open["SOURCE_UNREACHABLE/"].Detail; !strings.Contains(d, "for 1m31s") {
		t.Fatalf("detail %q does not tell the 91s since the last read", d)
	}
	// The source answers again (a probe or the dump reads a byte).
	p.advance(time.Second)
	p.s.noteSourceRead(p.task.ID, p.at())
	if h := p.eval(t); len(p.open(t)) != 0 {
		t.Fatalf("recovered: %+v %+v", h, p.open(t))
	}
}

// #383 QA F3: a 63s planned restart whose backoff keeps the task out of
// RUNNING for 92s never opens SOURCE_UNREACHABLE: the source answered a
// probe at 70s, and every failure was within 63s of the last read.
func TestP24_PlannedRestartWithLongBackoffNeverOpensUnreachable(t *testing.T) {
	p := newP24(t, EvaluatorOn)
	p.setTask(t, StateRunning, TaskDesiredRun, nil)
	p.s.noteSourceRead(p.task.ID, p.at())
	for sec := 1; sec <= 92; sec++ {
		p.advance(time.Second)
		switch {
		case sec <= 63 && sec%5 == 0:
			p.setTask(t, StateRetryBackoff, TaskDesiredRun, nil)
			p.s.noteSourceFailure(p.task.ID, p.at(), errors.New("connect: connection refused"))
		case sec == 70:
			p.s.noteSourceRead(p.task.ID, p.at()) // a probe read the greeting
		}
		p.eval(t)
		if _, ok := p.open(t)["SOURCE_UNREACHABLE/"]; ok {
			t.Fatalf("SOURCE_UNREACHABLE opened at %ds of a 63s restart", sec)
		}
	}
	p.setTask(t, StateRunning, TaskDesiredRun, nil)
	p.s.noteSourceRead(p.task.ID, p.at())
	p.eval(t)
	if len(p.open(t)) != 0 {
		t.Fatalf("after the restart: %+v", p.open(t))
	}
}

func TestP24_StopClosesDumpStalled(t *testing.T) {
	p := newP24(t, EvaluatorOn)
	p.setTask(t, StateRunning, TaskDesiredRun, func(t *Task) { t.Epoch = 3 })
	p.s.noteSourceRead(p.task.ID, p.at())
	p.advance(time.Minute)
	p.eval(t)
	if _, ok := p.open(t)["DUMP_STALLED/"]; !ok {
		t.Fatalf("no stall: %+v", p.open(t))
	}
	p.setTask(t, StateStopped, TaskDesiredStop, nil)
	p.eval(t)
	if _, ok := p.open(t)["DUMP_STALLED/"]; ok {
		t.Fatalf("Stop did not close DUMP_STALLED: %+v", p.open(t))
	}
	// Start again: the clock starts over, no stale stall.
	p.setTask(t, StateStarting, TaskDesiredRun, nil)
	p.eval(t)
	if len(p.open(t)) != 0 {
		t.Fatalf("restart reused the old silence: %+v", p.open(t))
	}
}

// The scheduler probes a silent task's source on its own (a TCP connect
// and the server greeting, bounded), and a runner SOURCE_UNREACHABLE error
// is a failed attempt; a probe that reads the greeting is a read.
func TestP24_SchedulerProbesASilentSource(t *testing.T) {
	prober := &fakeProber{err: errors.New("dial tcp: i/o timeout")}
	s := NewScheduler(WithDataDir(t.TempDir()), WithSourceLiveness(20*time.Millisecond, 50*time.Millisecond))
	s.SetRunner(&proberRunner{fakeProber: prober})
	task, err := s.CreateTaskFromSpec("p", "p", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "u", Password: "p", Flavor: "mysql"}, &StartConfig{Mode: StartModeLatest}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	row := s.tasks[task.ID]
	row.DesiredRun, row.State = TaskDesiredRun, StateRetryBackoff
	s.tasks[task.ID] = row
	s.mu.Unlock()
	s.noteSourceRead(task.ID, time.Now().Add(-time.Minute))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartSourceLiveness(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for {
		snap, _ := s.LivenessSnapshot(task.ID)
		if !snap.LastFailure.IsZero() && strings.Contains(snap.FailureDetail, "i/o timeout") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no failed probe recorded: %+v (probes %d)", snap, prober.calls.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	prober.setErr(nil)
	deadline = time.Now().Add(3 * time.Second)
	for {
		snap, _ := s.LivenessSnapshot(task.ID)
		if time.Since(snap.LastRead) < time.Second {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a probe that reads the greeting is not a read: %+v", snap)
		}
		time.Sleep(10 * time.Millisecond)
	}
	before, _ := s.LivenessSnapshot(task.ID)
	s.noteRunnerError(task.ID, NewRetryableSourceError(CodeSourceUnreachable, "dial tcp: connection refused"))
	after, _ := s.LivenessSnapshot(task.ID)
	if !after.LastFailure.After(before.LastFailure) && !after.LastFailure.Equal(before.LastFailure) || !strings.Contains(after.FailureDetail, "connection refused") {
		t.Fatalf("runner SOURCE_UNREACHABLE is not a failed attempt: %+v", after)
	}
}

type fakeProber struct {
	mu    sync.Mutex
	err   error
	calls atomic.Int64
}

func (f *fakeProber) setErr(err error) { f.mu.Lock(); f.err = err; f.mu.Unlock() }

func (f *fakeProber) ProbeSource(ctx context.Context, src SourceConfig) error {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

type proberRunner struct {
	*fakeProber
}

func (proberRunner) Run(ctx context.Context, task Task) error { <-ctx.Done(); return nil }

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
