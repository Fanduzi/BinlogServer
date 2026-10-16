package health

import (
	"testing"

	"binlog_server/internal/assurance"
)

// design 0009 §20: P2-5.1 flips the chain.local structural kinds.

var p251Kinds = []Kind{SegmentDamaged, ChainIndexGap, LegacyFailback, SegmentMissing}

func TestP251ManagedTable(t *testing.T) {
	r := DefaultRegistry()
	for _, k := range p251Kinds {
		if !r[k].Managed || !r[k].Confirm {
			t.Errorf("%s: managed=%v confirm=%v, want both", k, r[k].Managed, r[k].Confirm)
		}
	}
	for _, k := range []Kind{GTIDHole, CheckpointAhead, OrphanRow, UploadLate, SealFactsAbsent} {
		if r[k].Managed {
			t.Errorf("%s is managed; it is not part of P2-5.1", k)
		}
	}
	for _, k := range []Kind{SegmentUnreadable, SegmentOverlap, SegmentEmptyLeftover} {
		if r[k].Confirm {
			t.Errorf("%s gained Confirm; P2-3 kinds stay single-pass", k)
		}
	}
	if s := r[SegmentMissing].Scopes; len(s) != 1 || s[0] != ScopeChainLocal {
		t.Errorf("SEGMENT_MISSING scopes %v, want only chain.local until P2-5.2", s)
	}
	if _, ok := FromAssurance(assurance.I6ObjectSizeMismatch); !ok {
		t.Error("I6_OBJECT_SIZE_MISMATCH has no finding kind")
	}
}

func chainFactsOf(confirmAll bool, vs ...assurance.Violation) *Facts {
	f := &Facts{TaskID: "1", Now: t0, Chain: &ChainFacts{Violations: vs},
		Complete: map[Scope]bool{ScopeChainLocal: true}, Confirmed: map[Key]bool{}}
	if confirmAll {
		for _, v := range vs {
			k, _ := FromAssurance(v.Kind)
			f.Confirmed[Key{TaskID: "1", Kind: k, Subject: v.Segments[0]}] = true
		}
	}
	return f
}

func v(k assurance.Kind, seg string) assurance.Violation {
	return assurance.Violation{Kind: k, Segments: []string{seg}, Detail: string(k)}
}

// Only the assurance kinds whose facts are trusted in P2-5.1 derive.
func TestP251DerivesOnlyTrustedAssuranceKinds(t *testing.T) {
	f := chainFactsOf(true,
		v(assurance.I1Regression, "a"), v(assurance.I2IndexGap, "b"), v(assurance.I2OrderInvalid, "c"),
		v(assurance.I3LegacyFailback, "d"), v(assurance.I6MissingLocal, "e"), v(assurance.I6SizeMismatch, "f"),
		v(assurance.I2SwitchUnrecorded, "x1"), v(assurance.I6MissingRow, "x2"), v(assurance.I6MissingObject, "x3"),
		v(assurance.I6ObjectSizeMismatch, "x4"))
	d := DefaultRegistry().Desire(f)
	want := map[Key]bool{
		{"1", SegmentDamaged, "a"}: true, {"1", ChainIndexGap, "b"}: true, {"1", ChainIndexGap, "c"}: true,
		{"1", LegacyFailback, "d"}: true, {"1", SegmentMissing, "e"}: true, {"1", SegmentMissing, "f"}: true,
	}
	for k := range want {
		if _, ok := d[k]; !ok {
			t.Errorf("%v not desired", k)
		}
	}
	for k := range d {
		if !want[k] {
			t.Errorf("%v desired; its assurance kind is not trusted in P2-5.1", k)
		}
	}
}

// SEGMENT_MISSING closes on chain.local alone (chain.objects is produced by nobody yet).
func TestP251SegmentMissingClosesOnLocalScope(t *testing.T) {
	open := []Finding{{TaskID: "1", Kind: SegmentMissing, Subject: "f", Severity: Critical}}
	ts, _ := DefaultRegistry().Reconcile(open, chainFactsOf(true))
	if len(ts) != 1 || ts[0].Op != OpClose {
		t.Fatalf("transitions %+v, want one CLOSE", ts)
	}
}

// Confirm: the first pass that sees an instance does not open it; the
// second does; an open finding is not closed for lack of confirmation.
func TestP251ConfirmTwoPasses(t *testing.T) {
	r := DefaultRegistry()
	first := chainFactsOf(false, v(assurance.I2OrderInvalid, "c"))
	ts, _ := r.Reconcile(nil, first)
	if len(ts) != 0 {
		t.Fatalf("first pass opened %+v", ts)
	}
	key := Key{"1", ChainIndexGap, "c"}
	if p := r.Pending(first); len(p) != 1 || p[0] != key {
		t.Fatalf("pending %v, want [%v]", p, key)
	}
	second := chainFactsOf(true, v(assurance.I2OrderInvalid, "c"))
	ts, _ = r.Reconcile(nil, second)
	if len(ts) != 1 || ts[0].Op != OpOpen || ts[0].Key != key {
		t.Fatalf("second pass %+v, want OPEN %v", ts, key)
	}
	if p := r.Pending(second); len(p) != 0 {
		t.Fatalf("confirmed key still pending: %v", p)
	}
	open := []Finding{{TaskID: "1", Kind: ChainIndexGap, Subject: "c", Severity: Critical, Detail: string(assurance.I2OrderInvalid), Segments: []string{"c"}}}
	if ts, _ := r.Reconcile(open, first); len(ts) != 0 {
		t.Fatalf("open finding without confirmation transitioned: %+v", ts)
	}
	// P2-3 kinds open in one pass, as before.
	if ts, _ := r.Reconcile(nil, chainFactsOf(false, v(assurance.I4Overlap, "o"))); len(ts) != 1 || ts[0].Op != OpOpen {
		t.Fatalf("SEGMENT_OVERLAP needs confirmation now: %+v", ts)
	}
}
