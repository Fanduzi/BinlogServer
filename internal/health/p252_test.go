package health

import (
	"strings"
	"testing"
	"time"

	"binlog_server/internal/assurance"
)

// design 0009 §21: object kinds, UPLOAD_LATE, UPLOAD_UNVERIFIED chain part.

func TestP252ManagedTable(t *testing.T) {
	r := DefaultRegistry()
	if s := r[UploadLate]; !s.Managed || !s.Confirm {
		t.Errorf("UPLOAD_LATE managed=%v confirm=%v", s.Managed, s.Confirm)
	}
	if r[OrphanRow].Managed {
		t.Error("ORPHAN_ROW waits for the retention markers (P2-5.4)")
	}
	if r[UploadUnverified].Confirm {
		t.Error("UPLOAD_UNVERIFIED kind-level Confirm would delay its signal")
	}
	if s := r[SegmentMissing].Scopes; len(s) != 2 {
		t.Errorf("SEGMENT_MISSING scopes %v, want chain.local and chain.objects (§6)", s)
	}
	if ObjectSubject("mysql-bin.000004") != "mysql-bin.000004#object" {
		t.Errorf("object subject %q", ObjectSubject("mysql-bin.000004"))
	}
}

func objFacts(vs ...assurance.Violation) *Facts {
	return &Facts{TaskID: "1", Now: t0, Chain: &ChainFacts{Violations: vs},
		Complete:       map[Scope]bool{ScopeChainLocal: true, ScopeChainObjects: true},
		Produced:       map[Scope]bool{ScopeChainLocal: true, ScopeChainObjects: true},
		ObjectSegments: map[string]bool{"s4": true},
		Confirmed:      map[Key]bool{}, UploadLateCriticalAfter: 24 * time.Hour}
}

func confirmAll(r Registry, f *Facts) *Facts {
	for k := range r.Candidates(f) {
		f.Confirmed[k] = true
	}
	return f
}

func TestP252ObjectOriginSubjectAndScopes(t *testing.T) {
	r := DefaultRegistry()
	f := confirmAll(r, objFacts(
		assurance.Violation{Kind: assurance.I6MissingObject, Segments: []string{"s4"}, Evidence: "t/s4"},
		assurance.Violation{Kind: assurance.I6MissingLocal, Segments: []string{"s4"}, Evidence: "LOCAL_ONLY"}))
	d := r.Desire(f)
	for _, k := range []Key{{"1", SegmentMissing, "s4#object"}, {"1", SegmentMissing, "s4"}} {
		if _, ok := d[k]; !ok {
			t.Fatalf("%v not desired: %v", k, d)
		}
	}
	open := []Finding{
		{TaskID: "1", Kind: SegmentMissing, Subject: "s4#object", Severity: Critical},
		{TaskID: "1", Kind: SegmentMissing, Subject: "s4", Severity: Critical},
	}
	// Objects not listed this pass: the local instance closes, the object one stays.
	g := objFacts()
	g.Complete[ScopeChainObjects] = false
	ts, _ := r.Reconcile(open, g)
	if len(ts) != 1 || ts[0].Op != OpClose || ts[0].Subject != "s4" {
		t.Fatalf("objects unlisted: %+v", ts)
	}
	// Listed, but s4's prefix was not listed completely: still stays.
	g = objFacts()
	g.ObjectSegments = map[string]bool{}
	ts, _ = r.Reconcile(open[:1], g)
	if len(ts) != 0 {
		t.Fatalf("prefix incomplete: %+v", ts)
	}
	// Listed completely and gone: closes.
	ts, _ = r.Reconcile(open[:1], objFacts())
	if len(ts) != 1 || ts[0].Op != OpClose || ts[0].Reason != ReasonEval {
		t.Fatalf("listed: %+v", ts)
	}
}

func TestP252ConfigRemovalClosesObjectInstances(t *testing.T) {
	r := DefaultRegistry()
	open := []Finding{
		{TaskID: "1", Kind: SegmentMissing, Subject: "s4#object", Severity: Critical},
		{TaskID: "1", Kind: UploadLate, Subject: "s5", Severity: Warning},
		{TaskID: "1", Kind: SegmentMissing, Subject: "s6", Severity: Critical, Segments: []string{"s6"}},
	}
	f := &Facts{TaskID: "1", Now: t0, Chain: &ChainFacts{Violations: []assurance.Violation{{Kind: assurance.I6MissingLocal, Segments: []string{"s6"}}}},
		Complete:        map[Scope]bool{ScopeChainLocal: true},
		Produced:        map[Scope]bool{ScopeChainLocal: true},
		NoObjectStorage: true, ObjectConfigAudit: "was 1a2b3c; source: config file",
		Confirmed:       map[Key]bool{{"1", SegmentMissing, "s6"}: true}}
	ts, _ := r.Reconcile(open, f)
	closed := map[string]Transition{}
	for _, tr := range ts {
		if tr.Op == OpClose {
			closed[string(tr.Kind)+"/"+tr.Subject] = tr
		}
	}
	for _, k := range []string{"SEGMENT_MISSING/s4#object", "UPLOAD_LATE/s5"} {
		tr, ok := closed[k]
		if !ok || tr.Reason != ReasonConfig || !strings.Contains(tr.Instance.Detail, "object storage no longer configured") || !strings.Contains(tr.Instance.Detail, "1a2b3c") {
			t.Errorf("%s: %+v (ok=%v)", k, tr, ok)
		}
	}
	if _, ok := closed["SEGMENT_MISSING/s6"]; ok {
		t.Error("a local instance closed for the config change")
	}
}

func TestP252ProducedPerTask(t *testing.T) {
	r := DefaultRegistry()
	f := &Facts{TaskID: "1", Now: t0, Complete: map[Scope]bool{ScopeChainLocal: true, ScopeTaskState: true, ScopeSignals: true, ScopeStore: true},
		Produced: map[Scope]bool{ScopeChainLocal: true, ScopeTaskState: true, ScopeSignals: true, ScopeStore: true, ScopeChainObjects: true}}
	if h := r.HealthOf(nil, f); h.Status != StatusUnknown {
		t.Fatalf("objects configured but not listed: %s", h.Status)
	}
	f.Produced[ScopeChainObjects] = false
	if h := r.HealthOf(nil, f); h.Status != StatusHealthy {
		t.Fatalf("no object storage: %s", h.Status)
	}
}

func TestP252UploadLateEscalates(t *testing.T) {
	r := DefaultRegistry()
	late := func(age time.Duration) *Facts {
		return confirmAll(r, objFacts(assurance.Violation{Kind: assurance.I7UploadLate, Segments: []string{"s5"}, Since: t0.Add(-age)}))
	}
	if in := r.Desire(late(time.Hour))[Key{"1", UploadLate, "s5"}]; in.Severity != Warning {
		t.Fatalf("1h: %+v", in)
	}
	open := []Finding{{TaskID: "1", Kind: UploadLate, Subject: "s5", Severity: Warning, Segments: []string{"s5"}}}
	ts, _ := r.Reconcile(open, late(25*time.Hour))
	if len(ts) != 1 || ts[0].Op != OpEscalate || ts[0].Instance.Severity != Critical {
		t.Fatalf("25h: %+v", ts)
	}
}

func TestP252UnverifiedChainConfirmSignalNot(t *testing.T) {
	r := DefaultRegistry()
	f := objFacts(assurance.Violation{Kind: assurance.I7Unverified, Segments: []string{"s7"}})
	if ts, _ := r.Reconcile(nil, f); len(ts) != 0 {
		t.Fatalf("chain UPLOAD_UNVERIFIED opened in one pass: %+v", ts)
	}
	confirmAll(r, f)
	if ts, _ := r.Reconcile(nil, f); len(ts) != 1 || ts[0].Op != OpOpen {
		t.Fatalf("second pass: %+v", ts)
	}
}
