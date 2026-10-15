// Package health provides module-level functionality for health.
// input: hand-built Facts for every P2-4 signal and task-state kind
// output: the P2-4 QA plan at the pure layer: each signal kind opens on failure and closes on the first later success; an open row with no success since start never closes or flaps (meta still read-only); a probe closes a rarely-succeeding signal; a stale open signal is shown as INFO; DUMP_STALLED and SOURCE_UNREACHABLE follow the one source liveness clock (the last byte read at the socket): DUMP_STALLED after 45s of silence in any state, SOURCE_UNREACHABLE once an attempt failed 90s after the last read (backoff does not count), with a same-pass HANDED_OFF and no open-and-close in one pass; TASK_FAILED opens per failure class and closes on restart or stop; TASK_TRANSITION_STALLED derives from the task row; no findings after a restart is HEALTHY and a FAILED task never is
// pos: ADR 0009 phase 2 P2-4 (docs/design/0009-phase2-health-evaluator.md §5.5, §6, §14)
// note: if this file changes, update this header and module README.md.
package health

import (
	"strings"
	"testing"
	"time"
)

var st0 = time.Date(2026, 10, 15, 4, 0, 0, 0, time.UTC)

func allScopes() map[Scope]bool {
	return map[Scope]bool{ScopeChainLocal: true, ScopeTaskState: true, ScopeSignals: true, ScopeStore: true}
}

func runningTask() *TaskFacts {
	return &TaskFacts{State: stateRunning, Desired: desiredRun, Epoch: 3, StateSince: st0.Add(-time.Hour), RunHere: true}
}

func sigFacts(t *TaskFacts, sigs map[SignalKey]SignalState) *Facts {
	return &Facts{TaskID: "1", Now: st0, Task: t, Signals: sigs, Complete: allScopes()}
}

func opsOf(ts []Transition) map[Key]Transition {
	out := map[Key]Transition{}
	for _, t := range ts {
		out[t.Key] = t
	}
	return out
}

func asOpen(ts []Transition) []Finding {
	var out []Finding
	for _, t := range ts {
		if t.Op == OpOpen {
			out = append(out, Finding{TaskID: t.TaskID, Kind: t.Kind, Subject: t.Subject, Severity: t.Instance.Severity, Detail: t.Instance.Detail})
		}
	}
	return out
}

var signalKinds = []Kind{TaskPersistFailed, MetadataWriteBlocked, EventPersistFailed, EventReadFailed, DumpCleanupPersistFailed,
	DiskListFailed, UploadUnverified, UploadRecordFailed, RetentionRecordFailed, RetryLoopFailed, OpenTailCutFailed, MetricSourceFailed}

func TestP24_EverySignalKindOpensOnFailureAndClosesOnFirstSuccess(t *testing.T) {
	r := DefaultRegistry()
	for _, k := range signalKinds {
		sk := SignalKey{Kind: k, Subject: "s"}
		key := Key{TaskID: "1", Kind: k, Subject: "s"}
		ts, _ := r.Reconcile(nil, sigFacts(runningTask(), map[SignalKey]SignalState{sk: {FailureSeq: 1, Failures: 1, Detail: "boom"}}))
		if tr, ok := opsOf(ts)[key]; !ok || tr.Op != OpOpen || tr.Instance.Severity != r[k].Severity {
			t.Fatalf("%s: failure did not open: %+v", k, ts)
		}
		open := asOpen(ts)
		ts, _ = r.Reconcile(open, sigFacts(runningTask(), map[SignalKey]SignalState{sk: {FailureSeq: 1, SuccessSeq: 2}}))
		if tr, ok := opsOf(ts)[key]; !ok || tr.Op != OpClose || tr.Reason != ReasonEval {
			t.Fatalf("%s: first success did not close: %+v", k, ts)
		}
	}
}

func TestP24_RestartWithMetaStillReadOnlyNeverClosesOrFlaps(t *testing.T) {
	r := DefaultRegistry()
	for _, k := range signalKinds {
		open := []Finding{{TaskID: "1", Kind: k, Subject: "s", Severity: r[k].Severity, Detail: "boom"}}
		sk := SignalKey{Kind: k, Subject: "s"}
		// After a restart: nothing observed yet, then the first write fails again.
		for _, st := range []map[SignalKey]SignalState{{}, {sk: {FailureSeq: 1, Failures: 1, Detail: "boom"}}} {
			ts, _ := r.Reconcile(open, sigFacts(runningTask(), st))
			for _, tr := range ts {
				if tr.Op == OpClose || tr.Op == OpOpen {
					t.Fatalf("%s %+v: restart gave %s", k, st, tr.Op)
				}
			}
		}
		if h := r.HealthOf(open, sigFacts(runningTask(), nil)); h.Status == StatusHealthy || len(h.Unknown) != 0 {
			t.Fatalf("%s: an open row decides health, never UNKNOWN: %+v", k, h)
		}
	}
}

func TestP24_NoFindingsAfterRestartIsHealthyInOnePass(t *testing.T) {
	if h := DefaultRegistry().HealthOf(nil, sigFacts(runningTask(), nil)); h.Status != StatusHealthy {
		t.Fatalf("restart with no findings: %+v", h)
	}
}

func TestP24_ProbeClosesARarelySucceedingSignal(t *testing.T) {
	r := DefaultRegistry()
	key := Key{TaskID: "1", Kind: UploadRecordFailed, Subject: "mysql-bin.000004"}
	open := []Finding{{TaskID: "1", Kind: UploadRecordFailed, Subject: key.Subject, Severity: Critical}}
	f := sigFacts(runningTask(), nil)
	if ts, _ := r.Reconcile(open, f); len(ts) != 0 {
		t.Fatalf("no proof yet: %+v", ts)
	}
	f.Probes = map[Key]bool{key: true}
	if ts, _ := r.Reconcile(open, f); len(ts) != 1 || ts[0].Op != OpClose {
		t.Fatalf("probe did not close: %+v", ts)
	}
}

func TestP24_StaleOpenSignalIsShownAsInfo(t *testing.T) {
	r := DefaultRegistry()
	key := Key{TaskID: "1", Kind: RetentionRecordFailed, Subject: "mysql-bin.000001"}
	open := []Finding{{TaskID: "1", Kind: key.Kind, Subject: key.Subject, Severity: Warning}}
	f := sigFacts(runningTask(), nil)
	f.StaleSignals = map[Key]bool{key: true}
	ts, _ := r.Reconcile(open, f)
	if len(ts) != 1 || ts[0].Op != OpDowngrade || ts[0].Instance.Severity != Info {
		t.Fatalf("stale signal: %+v", ts)
	}
	open[0].Severity, open[0].Detail = Info, ts[0].Instance.Detail
	if ts, _ := r.Reconcile(open, f); len(ts) != 0 {
		t.Fatalf("stale INFO row flaps: %+v", ts)
	}
	if h := r.HealthOf(open, f); h.Status != StatusHealthy {
		t.Fatalf("an INFO row must not affect health: %+v", h)
	}
}

// live is a task with desired RUN whose last byte from the source was
// readAgo ago and whose latest failed connection attempt was failAgo ago
// (negative: none).
func live(state string, readAgo, failAgo time.Duration) *TaskFacts {
	t := &TaskFacts{State: state, Desired: desiredRun, Epoch: 3, StateSince: st0.Add(-time.Hour), RunHere: true,
		Liveness: &Liveness{LastRead: st0.Add(-readAgo)}}
	if failAgo >= 0 {
		t.Liveness.LastFailure = st0.Add(-failAgo)
		t.Liveness.FailureDetail = "dial tcp 127.0.0.1:3306: i/o timeout"
	}
	return t
}

// #383 QA F1: DUMP_STALLED comes from the one liveness clock (the last byte
// or heartbeat read at the socket), in RUNNING, STARTING and RETRY_BACKOFF
// alike, never from a runner signal.
func TestP24_DumpStalledFollowsTheLivenessClock(t *testing.T) {
	r := DefaultRegistry()
	key := Key{TaskID: "1", Kind: DumpStalled}
	for _, state := range []string{stateRunning, stateStarting, stateRetryBackoff} {
		if _, ok := r.Desire(sigFacts(live(state, 44*time.Second, -1), nil))[key]; ok {
			t.Fatalf("%s: 44s of silence is not a stall", state)
		}
		d := r.Desire(sigFacts(live(state, 46*time.Second, -1), nil))
		in, ok := d[key]
		if !ok || silence(in.Detail) < 45*time.Second {
			t.Fatalf("%s: 46s of silence: %+v", state, d)
		}
	}
	open := []Finding{{TaskID: "1", Kind: DumpStalled, Severity: Warning}}
	for _, c := range []struct {
		name string
		task *TaskFacts
	}{
		{"a byte arrived", live(stateRunning, time.Second, -1)},
		{"deliberate stop", &TaskFacts{State: stateStopped, Desired: desiredStop, Liveness: &Liveness{LastRead: st0.Add(-time.Hour)}}},
		{"lease moved", func() *TaskFacts { t := live(stateStarting, time.Hour, -1); t.OwnedElsewhere = true; return t }()},
	} {
		ts, _ := r.Reconcile(open, sigFacts(c.task, nil))
		if len(ts) != 1 || ts[0].Op != OpClose || ts[0].Reason == ReasonHandedOff {
			t.Fatalf("%s: %+v", c.name, ts)
		}
	}
	// No liveness facts (no runner here): nothing is derived.
	if d := r.Desire(sigFacts(&TaskFacts{State: stateRunning, Desired: desiredRun}, nil)); len(d) != 0 {
		t.Fatalf("no clock, no stall: %+v", d)
	}
}

// #383 QA F2/F3: SOURCE_UNREACHABLE needs a failed attempt at least
// unreachable_after after the last byte read; the detail tells the real
// silence; backoff that keeps the task out of RUNNING longer than the
// outage does not count.
func TestP24_SourceUnreachableCountsFromTheLastRead(t *testing.T) {
	r := DefaultRegistry()
	su := Key{TaskID: "1", Kind: SourceUnreachable}
	ds := Key{TaskID: "1", Kind: DumpStalled}
	// Killed 274s ago, the latest attempt failed 1s ago.
	d := r.Desire(sigFacts(live(stateRetryBackoff, 274*time.Second, time.Second), nil))
	in, ok := d[su]
	if !ok || silence(in.Detail) != 274*time.Second {
		t.Fatalf("4m34s outage: %+v", d)
	}
	if _, both := d[ds]; both {
		t.Fatal("DUMP_STALLED and SOURCE_UNREACHABLE desired together")
	}
	for _, c := range []struct {
		name             string
		readAgo, failAgo time.Duration
	}{
		{"no failed attempt yet", 120 * time.Second, -1},
		{"last failure only 70s after the last read", 95 * time.Second, 25 * time.Second},
		// A 63s planned restart: out of RUNNING for 92s because of backoff,
		// but the source answered 29s ago and the last failure was at 63s.
		{"63s restart, 92s out of RUNNING", 29 * time.Second, 30 * time.Second},
		{"read after the failure", 2 * time.Second, 3 * time.Second},
	} {
		if _, ok := r.Desire(sigFacts(live(stateRetryBackoff, c.readAgo, c.failAgo), nil))[su]; ok {
			t.Fatalf("%s: SOURCE_UNREACHABLE opened", c.name)
		}
	}
	f := sigFacts(live(stateRetryBackoff, 100*time.Second, 5*time.Second), nil)
	f.UnreachableAfter = 120 * time.Second
	if _, ok := r.Desire(f)[su]; ok {
		t.Fatal("the per-task threshold is not used")
	}
	open := []Finding{{TaskID: "1", Kind: SourceUnreachable, Severity: Critical}}
	for _, c := range []struct {
		name string
		task *TaskFacts
	}{
		{"source answered", live(stateStarting, time.Second, 4*time.Second)},
		{"deliberate stop", &TaskFacts{State: stateStopped, Desired: desiredStop}},
		{"task FAILED", &TaskFacts{State: stateFailed, Desired: desiredStop, FailedCurrent: true, LastError: "SOURCE_UNREACHABLE: x"}},
	} {
		ts, _ := r.Reconcile(open, sigFacts(c.task, nil))
		if tr, ok := opsOf(ts)[su]; !ok || tr.Op != OpClose {
			t.Fatalf("%s did not close: %+v", c.name, ts)
		}
	}
}

// DUMP_STALLED hands off to SOURCE_UNREACHABLE in the same pass: one
// ApplyTransitions closes one and opens the other, so a dead source never
// shows zero findings.
func TestP24_DumpStalledHandsOffInTheSamePass(t *testing.T) {
	r := DefaultRegistry()
	open := []Finding{{TaskID: "1", Kind: DumpStalled, Severity: Warning}}
	ts, _ := r.Reconcile(open, sigFacts(live(stateRetryBackoff, 100*time.Second, time.Second), nil))
	got := opsOf(ts)
	if got[Key{TaskID: "1", Kind: DumpStalled}].Op != OpClose || got[Key{TaskID: "1", Kind: DumpStalled}].Reason != ReasonHandedOff ||
		got[Key{TaskID: "1", Kind: SourceUnreachable}].Op != OpOpen {
		t.Fatalf("handoff: %+v", ts)
	}
	if after := afterOps(open, ts); len(after) != 1 || after[0].Kind != SourceUnreachable {
		t.Fatalf("after the handoff: %+v", after)
	}
}

// A flap cannot open and close in one pass: the evaluator reads one
// snapshot of the clock, so an outage that already recovered (a read after
// the failure) opens nothing, and an open row closes only on a later pass.
func TestP24_NoOpenAndCloseInOnePass(t *testing.T) {
	r := DefaultRegistry()
	f := sigFacts(live(stateStarting, time.Second, 2*time.Second), nil)
	f.Task.Liveness.LastFailure = st0.Add(-2 * time.Second)
	ts, _ := r.Reconcile(nil, f)
	for _, tr := range ts {
		t.Fatalf("recovered outage produced %+v", tr)
	}
}

func silence(detail string) time.Duration {
	i := strings.Index(detail, " for ")
	if i < 0 {
		return 0
	}
	rest := detail[i+5:]
	if j := strings.IndexAny(rest, " ;,("); j >= 0 {
		rest = rest[:j]
	}
	d, _ := time.ParseDuration(rest)
	return d
}

func afterOps(open []Finding, ts []Transition) []Finding {
	by := map[Key]Finding{}
	for _, f := range open {
		by[f.Key()] = f
	}
	for _, t := range ts {
		if t.Op == OpClose {
			delete(by, t.Key)
			continue
		}
		by[t.Key] = Finding{TaskID: t.TaskID, Kind: t.Kind, Subject: t.Subject, Severity: t.Instance.Severity}
	}
	var out []Finding
	for _, f := range by {
		out = append(out, f)
	}
	return out
}

func TestP24_TaskFailedPerClassAndNeverHealthy(t *testing.T) {
	r := DefaultRegistry()
	for msg, class := range map[string]string{
		"STREAM_REGRESSION: rotate to mysql-bin.000003 goes backwards":                    FailStreamRegression,
		"ERROR 1236 (HY000): Could not find first log file name in binary log index file": FailSourceError1236,
		"open /data/1/mysql-bin.000004.open.e1: permission denied":                        FailPermissionDenied,
		"SOURCE_ACCESS_DENIED: Error 1045: Access denied for user 'repl'":                 FailPermissionDenied,
		"STORAGE_INCONSISTENT: segment mysql-bin.000003 rotates to itself":                FailStorage,
		"something nobody classified":                                                     FailOther,
	} {
		failed := &TaskFacts{State: stateFailed, Desired: desiredStop, FailedCurrent: true, LastError: msg}
		f := sigFacts(failed, nil)
		ts, _ := r.Reconcile(nil, f)
		key := Key{TaskID: "1", Kind: TaskFailed, Subject: class}
		if tr, ok := opsOf(ts)[key]; !ok || tr.Op != OpOpen || tr.Instance.Severity != Critical {
			t.Fatalf("%q: want TASK_FAILED/%s, got %+v", msg, class, ts)
		}
		open := asOpen(ts)
		if h := r.HealthOf(open, f); h.Status != StatusUnhealthy {
			t.Fatalf("FAILED task health %s", h.Status)
		}
		for _, next := range []*TaskFacts{
			{State: stateStarting, Desired: desiredRun},                      // restarted
			{State: stateStopped, Desired: desiredStop},                      // stopped
			{State: stateFailed, Desired: desiredStop, FailedCurrent: false}, // a newer ask (Stop) on the FAILED row
		} {
			if ts, _ := r.Reconcile(open, sigFacts(next, nil)); len(ts) != 1 || ts[0].Op != OpClose {
				t.Fatalf("%+v did not close TASK_FAILED: %+v", next, ts)
			}
		}
	}
	// A new class closes the old subject and opens the new one.
	open := []Finding{{TaskID: "1", Kind: TaskFailed, Subject: FailOther, Severity: Critical}}
	ts, _ := r.Reconcile(open, sigFacts(&TaskFacts{State: stateFailed, Desired: desiredStop, FailedCurrent: true, LastError: "STREAM_REGRESSION: x"}, nil))
	got := opsOf(ts)
	if got[Key{TaskID: "1", Kind: TaskFailed, Subject: FailOther}].Op != OpClose || got[Key{TaskID: "1", Kind: TaskFailed, Subject: FailStreamRegression}].Op != OpOpen {
		t.Fatalf("class change: %+v", ts)
	}
}

func TestP24_TransitionStalledDerivesFromTheTaskRow(t *testing.T) {
	r := DefaultRegistry()
	key := Key{TaskID: "1", Kind: TransitionStalled}
	for _, c := range []struct {
		task *TaskFacts
		open bool
	}{
		{&TaskFacts{State: stateStarting, Desired: desiredRun, StateSince: st0.Add(-61 * time.Second)}, true},
		{&TaskFacts{State: stateStopping, Desired: desiredStop, StateSince: st0.Add(-2 * time.Minute)}, true},
		{&TaskFacts{State: stateStarting, Desired: desiredRun, StateSince: st0.Add(-30 * time.Second)}, false},
		{&TaskFacts{State: stateStarting, Desired: desiredRun, StateSince: st0.Add(-2 * time.Minute), RunHere: true}, false},
		{&TaskFacts{State: stateStarting, Desired: desiredRun, StateSince: st0.Add(-2 * time.Minute), OwnedElsewhere: true}, false},
		{&TaskFacts{State: stateRunning, Desired: desiredRun, StateSince: st0.Add(-2 * time.Minute)}, false},
	} {
		if _, ok := r.Desire(sigFacts(c.task, nil))[key]; ok != c.open {
			t.Fatalf("%+v: open=%v", c.task, ok)
		}
	}
}
