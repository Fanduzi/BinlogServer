// Package health provides module-level functionality for health.
// input: an open chain-directory finding observed by a new owner, and Facts.NodeGone
// output: PR-5 monotonicity proof — an incomplete observation never closes a chain finding (the old OWNER_CHANGED close is deleted), a still-desired key stays open without a close-then-open pair, UPLOAD_UNVERIFIED is untouched, and NodeGone still yields UNKNOWN(node gone) unless a non-chain CRITICAL is open
// pos: unit coverage for ADR 0009 P2-6.2 reconcile and health (design §30)
// note: if this file changes, update this header and module README.md.
package health

import (
	"testing"
	"time"

	"binlog_server/internal/assurance"
)

func TestOwnerChangedKeepsIncompleteChainFinding(t *testing.T) {
	key := Key{TaskID: "1", Kind: SegmentEmptyLeftover, Subject: "mysql-bin.000002.open.e3"}
	open := []Finding{{TaskID: "1", Kind: SegmentEmptyLeftover, Subject: key.Subject, Severity: Info, Detail: "empty"}}
	f := &Facts{TaskID: "1", Now: time.Unix(1, 0).UTC(), Complete: map[Scope]bool{}}
	ts, _ := DefaultRegistry().Reconcile(open, f)
	if len(ts) != 0 {
		t.Fatalf("transitions %+v, want none — an incomplete read neither opens nor closes", ts)
	}
}

func TestStillDesiredKeyStaysOpenAcrossOwnerChange(t *testing.T) {
	subject := "mysql-bin.000002.open.e3"
	key := Key{TaskID: "1", Kind: SegmentEmptyLeftover, Subject: subject}
	open := []Finding{{TaskID: "1", Kind: SegmentEmptyLeftover, Subject: subject, Severity: Info, Detail: "old"}}
	f := &Facts{TaskID: "1", Now: time.Unix(1, 0).UTC(),
		Complete: map[Scope]bool{ScopeChainLocal: true},
		Chain:    &ChainFacts{Violations: []assurance.Violation{{Kind: assurance.I4EmptyLeftover, Segments: []string{subject}, Detail: "still there"}}},
	}
	ts, _ := DefaultRegistry().Reconcile(open, f)
	for _, tr := range ts {
		if tr.Key == key && (tr.Op == OpClose || tr.Op == OpOpen) {
			t.Fatalf("transitions %+v, a still-true fact must not close then open", ts)
		}
	}
}

func TestOwnerChangedLeavesSignalChainKinds(t *testing.T) {
	open := []Finding{{TaskID: "1", Kind: UploadUnverified, Subject: "mysql-bin.000001", Severity: Critical}}
	f := &Facts{TaskID: "1", Now: time.Unix(1, 0).UTC(), Complete: map[Scope]bool{}}
	ts, _ := DefaultRegistry().Reconcile(open, f)
	if len(ts) != 0 {
		t.Fatalf("transitions %+v, want none (signals incomplete, not a chain-directory finding)", ts)
	}
}

func TestNodeGoneChainIsUnknown(t *testing.T) {
	open := []Finding{{TaskID: "1", Kind: SegmentDamaged, Subject: "mysql-bin.000001", Severity: Critical}}
	f := &Facts{TaskID: "1", Now: time.Unix(1, 0).UTC(), NodeGone: true,
		Complete: map[Scope]bool{ScopeTaskState: true},
		Produced: map[Scope]bool{ScopeTaskState: true, ScopeChainLocal: true},
	}
	h := DefaultRegistry().HealthOf(open, f)
	if h.Status != StatusUnknown || h.UnknownDetail != "node gone" {
		t.Fatalf("health %+v, want UNKNOWN node gone", h)
	}
	if len(h.Unknown) != 1 || h.Unknown[0] != ScopeChainLocal {
		t.Fatalf("unknown %+v", h.Unknown)
	}
	if len(h.Reasons) != 0 {
		t.Fatalf("reasons %+v, chain severity must not decide", h.Reasons)
	}
}

func TestNodeGoneTaskFailedStaysUnhealthy(t *testing.T) {
	open := []Finding{
		{TaskID: "1", Kind: SegmentDamaged, Subject: "mysql-bin.000001", Severity: Critical},
		{TaskID: "1", Kind: TaskFailed, Subject: "", Severity: Critical},
	}
	f := &Facts{TaskID: "1", Now: time.Unix(1, 0).UTC(), NodeGone: true,
		Complete: map[Scope]bool{ScopeTaskState: true},
		Produced: map[Scope]bool{ScopeTaskState: true},
	}
	h := DefaultRegistry().HealthOf(open, f)
	if h.Status != StatusUnhealthy || h.UnknownDetail != "" {
		t.Fatalf("health %+v, want UNHEALTHY without the node-gone detail", h)
	}
}
