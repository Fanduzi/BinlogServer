// Package health provides module-level functionality for health.
// input: hand-built Facts for every P2-4 signal and task-state kind
// output: the P2-4 QA plan at the pure layer: each signal kind opens on failure and closes on the first later success; an open row with no success since start never closes or flaps (meta still read-only); a probe closes a rarely-succeeding signal; a stale open signal is shown as INFO; DUMP_STALLED survives the retry loop and closes on data, a deliberate stop, a newer lease epoch, or HANDED_OFF once SOURCE_UNREACHABLE is open; SOURCE_UNREACHABLE needs the threshold and two passes; TASK_FAILED opens per failure class and closes on restart or stop; TASK_TRANSITION_STALLED derives from the task row; no findings after a restart is HEALTHY and a FAILED task never is
// pos: ADR 0009 phase 2 P2-4 (docs/design/0009-phase2-health-evaluator.md §5.5, §6, §14)
// note: if this file changes, update this header and module README.md.
package health

import (
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
	for _, k := range append(signalKinds, DumpStalled) {
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

func TestP24_DumpStalledSurvivesTheRetryLoop(t *testing.T) {
	r := DefaultRegistry()
	sk := SignalKey{Kind: DumpStalled}
	key := Key{TaskID: "1", Kind: DumpStalled}
	open := []Finding{{TaskID: "1", Kind: DumpStalled, Severity: Warning, Detail: "silent"}}
	failing := map[SignalKey]SignalState{sk: {FailureSeq: 5, Failures: 1, Detail: "silent", Epoch: 3}}
	for _, state := range []string{stateStarting, stateRetryBackoff, stateStarting} {
		task := runningTask()
		task.State = state
		if ts, _ := r.Reconcile(open, sigFacts(task, failing)); len(ts) != 0 {
			t.Fatalf("%s closed or changed DUMP_STALLED: %+v", state, ts)
		}
	}
	stopped := &TaskFacts{State: stateStopped, Desired: desiredStop, Epoch: 3}
	if ts, _ := r.Reconcile(open, sigFacts(stopped, failing)); len(ts) != 1 || ts[0].Op != OpClose {
		t.Fatalf("deliberate stop did not close: %+v", ts)
	}
	moved := runningTask()
	moved.State, moved.Epoch = stateStarting, 4
	if ts, _ := r.Reconcile(open, sigFacts(moved, failing)); len(ts) != 1 || ts[0].Op != OpClose {
		t.Fatalf("a newer lease epoch did not close (#366): %+v", ts)
	}
	loop := runningTask()
	loop.State = stateRetryBackoff
	f := sigFacts(loop, failing)
	f.Open = map[Key]bool{key: true, {TaskID: "1", Kind: SourceUnreachable}: true}
	ts, _ := r.Reconcile(open, f)
	if len(ts) != 1 || ts[0].Op != OpClose || ts[0].Reason != ReasonHandedOff {
		t.Fatalf("SOURCE_UNREACHABLE open: want HANDED_OFF close, got %+v", ts)
	}
}

func TestP24_SourceUnreachableNeedsThresholdAndTwoPasses(t *testing.T) {
	r := DefaultRegistry()
	key := Key{TaskID: "1", Kind: SourceUnreachable}
	for _, c := range []struct {
		state  string
		for_   time.Duration
		passes int
		open   bool
	}{
		{stateRetryBackoff, 60 * time.Second, 5, false}, // a planned 60s restart
		{stateRetryBackoff, 95 * time.Second, 1, false}, // one pass is not enough
		{stateStarting, 95 * time.Second, 2, true},
	} {
		task := &TaskFacts{State: c.state, Desired: desiredRun, Epoch: 3, UnreachableFor: c.for_, UnreachablePasses: c.passes, LastError: "SOURCE_UNREACHABLE: dial tcp: i/o timeout"}
		_, ok := r.Desire(sigFacts(task, nil))[key]
		if ok != c.open {
			t.Fatalf("%+v: open=%v", c, ok)
		}
	}
	f := sigFacts(&TaskFacts{State: stateRetryBackoff, Desired: desiredRun, UnreachableFor: time.Second}, nil)
	f.UnreachableAfter = time.Millisecond
	f.Task.UnreachablePasses = 2
	if _, ok := r.Desire(f)[key]; !ok {
		t.Fatal("the task override threshold is not used")
	}
	open := []Finding{{TaskID: "1", Kind: SourceUnreachable, Severity: Critical}}
	still := &TaskFacts{State: stateStarting, Desired: desiredRun}
	if ts, _ := r.Reconcile(open, sigFacts(still, nil)); len(ts) != 0 {
		t.Fatalf("closed without a dump receiving data: %+v", ts)
	}
	back := sigFacts(runningTask(), map[SignalKey]SignalState{{Kind: DumpStalled}: {FailureSeq: 1, SuccessSeq: 7}})
	if ts, _ := r.Reconcile(open, back); len(ts) != 1 || ts[0].Op != OpClose {
		t.Fatalf("data again did not close: %+v", ts)
	}
	stop := sigFacts(&TaskFacts{State: stateStopped, Desired: desiredStop}, nil)
	if ts, _ := r.Reconcile(open, stop); len(ts) != 1 || ts[0].Op != OpClose {
		t.Fatalf("stop did not close: %+v", ts)
	}
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
