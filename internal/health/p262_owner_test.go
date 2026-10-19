// Package health provides module-level functionality for health.
// input: an open chain-directory finding, Facts.ChainOwnerChanged, and Facts.NodeGone
// output: proof that OWNER_CHANGED closes that finding even when the new directory is incomplete and reopens a still-desired key after the close, that an unhashed SEGMENT_UNREADABLE stays open (§31), that UPLOAD_UNVERIFIED is not closed that way, and that NodeGone yields UNKNOWN(node gone) unless a non-chain CRITICAL is open
// pos: unit coverage for ADR 0009 P2-6.2 reconcile and health (design §30)
// note: if this file changes, update this header and module README.md.
package health

import (
	"testing"
	"time"

	"binlog_server/internal/assurance"
)

func TestOwnerChangedClosesIncompleteChain(t *testing.T) {
	key := Key{TaskID: "1", Kind: SegmentEmptyLeftover, Subject: "mysql-bin.000002.open.e3"}
	open := []Finding{{TaskID: "1", Kind: SegmentEmptyLeftover, Subject: key.Subject, Severity: Info, Detail: "empty"}}
	f := &Facts{TaskID: "1", Now: time.Unix(1, 0).UTC(), ChainOwnerChanged: true, Complete: map[Scope]bool{}}
	ts, _ := DefaultRegistry().Reconcile(open, f)
	if len(ts) != 1 || ts[0].Op != OpClose || ts[0].Reason != ReasonOwnerChanged || ts[0].Key != key {
		t.Fatalf("transitions %+v, want one OWNER_CHANGED close", ts)
	}
}

func TestOwnerChangedReopensTheSameKey(t *testing.T) {
	subject := "mysql-bin.000002.open.e3"
	key := Key{TaskID: "1", Kind: SegmentEmptyLeftover, Subject: subject}
	open := []Finding{{TaskID: "1", Kind: SegmentEmptyLeftover, Subject: subject, Severity: Info, Detail: "old"}}
	f := &Facts{TaskID: "1", Now: time.Unix(1, 0).UTC(), ChainOwnerChanged: true,
		Complete: map[Scope]bool{ScopeChainLocal: true},
		Chain:    &ChainFacts{Violations: []assurance.Violation{{Kind: assurance.I4EmptyLeftover, Segments: []string{subject}, Detail: "still there"}}},
	}
	ts, _ := DefaultRegistry().Reconcile(open, f)
	if len(ts) != 2 || ts[0].Op != OpClose || ts[0].Reason != ReasonOwnerChanged || ts[1].Op != OpOpen || ts[1].Reason != ReasonEval {
		t.Fatalf("transitions %+v, want CLOSE OWNER_CHANGED then OPEN EVAL", ts)
	}
	if ts[0].Key != key || ts[1].Key != key {
		t.Fatalf("keys %+v %+v", ts[0].Key, ts[1].Key)
	}
}

func TestOwnerChangedInheritsUnhashedUnreadable(t *testing.T) {
	subject := "mysql-bin.000001"
	open := []Finding{{TaskID: "1", Kind: SegmentUnreadable, Subject: subject, Severity: Critical, Detail: "sha"}}
	f := &Facts{TaskID: "1", Now: time.Unix(1, 0).UTC(), ChainOwnerChanged: true,
		Complete:    map[Scope]bool{ScopeChainLocal: true},
		AwaitRehash: map[string]bool{subject: true},
	}
	ts, _ := DefaultRegistry().Reconcile(open, f)
	if len(ts) != 0 {
		t.Fatalf("transitions %+v, want none while rehash has not reported", ts)
	}
}

func TestOwnerChangedReopensUnreadableDerivedThisPass(t *testing.T) {
	subject := "mysql-bin.000001"
	key := Key{TaskID: "1", Kind: SegmentUnreadable, Subject: subject}
	open := []Finding{{TaskID: "1", Kind: SegmentUnreadable, Subject: subject, Severity: Critical, Detail: "old"}}
	f := &Facts{TaskID: "1", Now: time.Unix(1, 0).UTC(), ChainOwnerChanged: true,
		Complete:    map[Scope]bool{ScopeChainLocal: true},
		AwaitRehash: map[string]bool{subject: true},
		Chain:       &ChainFacts{Violations: []assurance.Violation{{Kind: assurance.I1Unreadable, Segments: []string{subject}, Detail: "still unreadable"}}},
	}
	ts, _ := DefaultRegistry().Reconcile(open, f)
	if len(ts) != 2 || ts[0].Op != OpClose || ts[0].Reason != ReasonOwnerChanged || ts[1].Op != OpOpen || ts[0].Key != key || ts[1].Key != key {
		t.Fatalf("transitions %+v, want CLOSE then OPEN when this pass derived it", ts)
	}
}

func TestOwnerChangedLeavesSignalChainKinds(t *testing.T) {
	open := []Finding{{TaskID: "1", Kind: UploadUnverified, Subject: "mysql-bin.000001", Severity: Critical}}
	f := &Facts{TaskID: "1", Now: time.Unix(1, 0).UTC(), ChainOwnerChanged: true, Complete: map[Scope]bool{}}
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
