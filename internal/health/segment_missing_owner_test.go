// Package health provides module-level functionality for health.
// input: an open SEGMENT_MISSING, Facts.ChainOwnerChanged, and whether chain.local is complete and still derives the key
// output: proof that OWNER_CHANGED and an incomplete chain.local never close SEGMENT_MISSING, and that a close happens only when the owner did not change, chain.local is complete, and the violation is gone, with reason EVAL
// pos: ADR 0011 PR-3 temporary no-close ban (docs/adr/0011-cluster-ownership-and-failover.md §3.5 and rollout row 3); PR-5 replaces it
// note: if this file changes, update this header and module README.md.
package health

import (
	"testing"
	"time"

	"binlog_server/internal/assurance"
)

func TestRecordChainOwnerRequiresVerifiedRead(t *testing.T) {
	holder := Fence{Mode: FenceCluster, Node: "n1", TaskEpoch: 1}
	if holder.RecordChainOwner() {
		t.Fatal("an unverified holder must not record chain_owner")
	}
	holder.ChainVerified = true
	if !holder.RecordChainOwner() {
		t.Fatal("a verified holder must record chain_owner")
	}
	for _, f := range []Fence{
		{Mode: FenceCluster, Proxy: true, ChainVerified: true, Node: "n1"},
		{Mode: FenceCluster, Delegated: true, ChainVerified: true, Node: "n1"},
		{Mode: FenceStandalone, ChainVerified: true, Node: "n1"},
		{Mode: FenceNeverLeased, ChainVerified: true, Node: "n1"},
	} {
		if f.RecordChainOwner() {
			t.Fatalf("%+v recorded chain_owner", f)
		}
	}
}

func TestSegmentMissingStaysOpenWhenOwnerChanges(t *testing.T) {
	key := Key{TaskID: "1", Kind: SegmentMissing, Subject: "mysql-bin.000002"}
	open := []Finding{{TaskID: "1", Kind: SegmentMissing, Subject: key.Subject, Severity: Critical, Detail: "gone"}}
	cases := []*Facts{
		{TaskID: "1", Now: time.Unix(1, 0).UTC(), ChainOwnerChanged: true, Complete: map[Scope]bool{}},
		{TaskID: "1", Now: time.Unix(1, 0).UTC(), ChainOwnerChanged: true, Complete: map[Scope]bool{ScopeChainLocal: true}},
		{TaskID: "1", Now: time.Unix(1, 0).UTC(), ChainOwnerChanged: true, Complete: map[Scope]bool{ScopeChainLocal: true},
			Chain: &ChainFacts{Violations: []assurance.Violation{{Kind: assurance.I6MissingLocal, Segments: []string{key.Subject}, Evidence: "LOCAL_ONLY"}}}},
	}
	for i, f := range cases {
		ts, _ := DefaultRegistry().Reconcile(open, f)
		for _, tr := range ts {
			if tr.Key == key && (tr.Op == OpClose || tr.Op == OpOpen) {
				t.Fatalf("case %d: %+v, SEGMENT_MISSING must stay open across an ownership change", i, ts)
			}
		}
	}
}

func TestSegmentMissingClosesOnlyWhenVerifiedAndGone(t *testing.T) {
	const subject = "mysql-bin.000002"
	key := Key{TaskID: "1", Kind: SegmentMissing, Subject: subject}
	open := []Finding{{TaskID: "1", Kind: key.Kind, Subject: subject, Severity: Critical}}
	for seed := 0; seed < 200; seed++ {
		ownerChanged := seed&1 == 1
		localComplete := seed&2 == 2
		violation := seed&4 == 4
		f := &Facts{TaskID: "1", Now: time.Unix(int64(seed+1), 0).UTC(), ChainOwnerChanged: ownerChanged, Complete: map[Scope]bool{}, Chain: &ChainFacts{}}
		if localComplete {
			f.Complete[ScopeChainLocal] = true
		}
		if violation {
			f.Chain.Violations = []assurance.Violation{{Kind: assurance.I6MissingLocal, Segments: []string{subject}, Evidence: "LOCAL_ONLY"}}
		}
		ts, _ := DefaultRegistry().Reconcile(open, f)
		var closed *Transition
		for i := range ts {
			if ts[i].Key != key {
				continue
			}
			if ts[i].Op == OpOpen {
				t.Fatalf("seed %d opened SEGMENT_MISSING again: %+v", seed, ts)
			}
			if ts[i].Op == OpClose {
				tr := ts[i]
				closed = &tr
			}
		}
		wantClose := !ownerChanged && localComplete && !violation
		if wantClose {
			if closed == nil || closed.Reason != ReasonEval {
				t.Fatalf("seed %d: close %+v, want one EVAL close", seed, closed)
			}
			continue
		}
		if closed != nil {
			t.Fatalf("seed %d ownerChanged=%v complete=%v violation=%v closed with %s", seed, ownerChanged, localComplete, violation, closed.Reason)
		}
	}
}
