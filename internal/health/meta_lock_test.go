// Package health provides module-level functionality for health.
// input: an open META_LOCK_STALLED row that is not in the managed registry
// output: proof that Reconcile leaves that row alone
// pos: ADR 0011 N6, the _cluster pass does not close a finding it did not derive
// note: if this file changes, update this header and module README.md.
package health

import (
	"testing"
	"time"
)

func TestReconcileDoesNotCloseMetaLockStalled(t *testing.T) {
	open := []Finding{{
		TaskID: "_cluster", Kind: "META_LOCK_STALLED", Subject: "t1", Severity: Critical,
	}}
	f := &Facts{TaskID: "_cluster", Now: time.Unix(0, 0), Complete: map[Scope]bool{ScopeTaskState: true, ScopeSignals: true}}
	ts, _ := DefaultRegistry().Reconcile(open, f)
	for _, tr := range ts {
		if tr.Kind == "META_LOCK_STALLED" {
			t.Fatalf("reconcile touched META_LOCK_STALLED: %+v", tr)
		}
	}
}
