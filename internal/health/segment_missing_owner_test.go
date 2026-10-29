// Package health provides module-level functionality for health.
// input: an open SEGMENT_MISSING and whether chain.local is complete and still derives the key
// output: PR-5 proof that an incomplete chain.local never closes SEGMENT_MISSING, that a close happens only when chain.local is complete and the violation is gone, with reason EVAL; I6_MISSING_LOCAL detail names chain_owner or node, and a size or object SEGMENT_MISSING does not
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
		{TaskID: "1", Now: time.Unix(1, 0).UTC(), Complete: map[Scope]bool{}},
		// A complete read where the violation is genuinely gone closes with
		// EVAL even across an owner change (PR-5); what must stay open is an
		// incomplete read, or a complete read where the fact still derives.
		{TaskID: "1", Now: time.Unix(1, 0).UTC(), Complete: map[Scope]bool{ScopeChainLocal: true},
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
		localComplete := seed&2 == 2
		violation := seed&4 == 4
		f := &Facts{TaskID: "1", Now: time.Unix(int64(seed+1), 0).UTC(), Complete: map[Scope]bool{}, Chain: &ChainFacts{}}
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
		wantClose := localComplete && !violation
		if wantClose {
			if closed == nil || closed.Reason != ReasonEval {
				t.Fatalf("seed %d: close %+v, want one EVAL close", seed, closed)
			}
			continue
		}
		if closed != nil {
			t.Fatalf("seed %d complete=%v violation=%v closed with %s", seed, localComplete, violation, closed.Reason)
		}
	}
}

func TestI6MissingLocalNamesDirectoryNode(t *testing.T) {
	local := assurance.Violation{Kind: assurance.I6MissingLocal, Segments: []string{"mysql-bin.000002"}, Detail: "segment mysql-bin.000002 is LOCAL_ONLY but not on local disk", Evidence: "LOCAL_ONLY"}
	size := assurance.Violation{Kind: assurance.I6SizeMismatch, Segments: []string{"mysql-bin.000003"}, Detail: "size", Evidence: "size"}
	obj := assurance.Violation{Kind: assurance.I6MissingObject, Segments: []string{"mysql-bin.000004"}, Detail: "no object", Evidence: "missing"}
	derive := chainDerive(SegmentMissing)
	cluster := &Facts{DirectoryNode: "n1", NameChainOwner: true, Chain: &ChainFacts{Violations: []assurance.Violation{local, size, obj}}}
	by := map[string]Instance{}
	for _, in := range derive(cluster) {
		by[in.Subject] = in
	}
	if d := by["mysql-bin.000002"].Detail; d != local.Detail+" chain_owner=n1" {
		t.Fatalf("cluster detail %q", d)
	}
	if by["mysql-bin.000003"].Detail != "size" {
		t.Fatalf("size detail %q", by["mysql-bin.000003"].Detail)
	}
	if by["mysql-bin.000004#object"].Detail != "no object" {
		t.Fatalf("object detail %q", by["mysql-bin.000004#object"].Detail)
	}
	stand := &Facts{DirectoryNode: "node-test", Chain: &ChainFacts{Violations: []assurance.Violation{local}}}
	got := derive(stand)
	if len(got) != 1 || got[0].Detail != local.Detail+" node=node-test" {
		t.Fatalf("standalone %+v", got)
	}
	none := &Facts{Chain: &ChainFacts{Violations: []assurance.Violation{local}}}
	got = derive(none)
	if len(got) != 1 || got[0].Detail != local.Detail {
		t.Fatalf("unnamed %+v", got)
	}
	if again := withDirectoryNode(local.Detail+" chain_owner=n1", cluster); again != local.Detail+" chain_owner=n1" {
		t.Fatalf("doubled %q", again)
	}
}
