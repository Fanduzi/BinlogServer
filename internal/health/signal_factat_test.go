// Package health provides module-level functionality for health.
// input: a managed signal kind (META_UNREACHABLE) open in the store, and signal states with and without SuccessFactAt
// output: proof that the CLOSE of a signal that succeeded carries SuccessFactAt as its FactAt (the first answer, not the closing pass), and the pass time when no SuccessFactAt was given
// pos: ADR 0009 phase 2, docs/design/0009-phase2-health-evaluator.md §25.8 R3 (#427 QA r1)
// note: if this file changes, update this header and module README.md.
package health

import (
	"testing"
	"time"
)

func TestSignalCloseFactAtIsTheSuccessFactAt(t *testing.T) {
	r := DefaultRegistry()
	open := []Finding{{TaskID: "_cluster", Kind: MetaUnreachable, Subject: "n1", Severity: Critical, FirstSeen: t0}}
	recovered := t0.Add(30 * time.Second)
	now := t0.Add(40 * time.Second)
	key := SignalKey{Kind: MetaUnreachable, Subject: "n1"}
	for _, tc := range []struct {
		name string
		st   SignalState
		want time.Time
	}{
		{"success with its fact time", SignalState{FailureSeq: 1, SuccessSeq: 2, FactAt: t0, SuccessFactAt: recovered}, recovered},
		{"success without a fact time", SignalState{FailureSeq: 1, SuccessSeq: 2, FactAt: t0}, now},
		{"fact time after the pass is ignored", SignalState{FailureSeq: 1, SuccessSeq: 2, FactAt: t0, SuccessFactAt: now.Add(time.Second)}, now},
	} {
		f := &Facts{TaskID: "_cluster", Now: now, Complete: map[Scope]bool{ScopeSignals: true}, Signals: map[SignalKey]SignalState{key: tc.st}}
		ts, _ := r.Reconcile(open, f)
		if len(ts) != 1 || ts[0].Op != OpClose {
			t.Fatalf("%s: transitions %+v, want one CLOSE", tc.name, ts)
		}
		if !ts[0].FactAt.Equal(tc.want) {
			t.Errorf("%s: CLOSE FactAt %s, want %s", tc.name, ts[0].FactAt, tc.want)
		}
	}
}
