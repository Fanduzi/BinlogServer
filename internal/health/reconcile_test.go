package health

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	"binlog_server/internal/assurance"
)

var t0 = time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)

// testRegistry is a registry with one chain kind and one signal kind, both managed.
func testRegistry(managed bool) Registry {
	r := Registry{}
	for k, spec := range DefaultRegistry() {
		if k == SegmentOverlap || k == UploadRecordFailed || k == GTIDHole {
			spec.Managed = managed
			r[k] = spec
		}
	}
	return r
}

func overlapFacts(now time.Time, segs ...string) *Facts {
	var vs []assurance.Violation
	for _, s := range segs {
		vs = append(vs, assurance.Violation{Kind: assurance.I4Overlap, Segments: []string{s}, Detail: "twice"})
	}
	return &Facts{TaskID: "1", Now: now, Chain: &ChainFacts{Violations: vs},
		Complete: map[Scope]bool{ScopeChainLocal: true, ScopeSignals: true}}
}

func openOf(ts []Transition, open []Finding) []Finding {
	m := map[Key]Finding{}
	for _, f := range open {
		m[f.Key()] = f
	}
	for _, t := range ts {
		switch t.Op {
		case OpClose:
			delete(m, t.Key)
		default:
			m[t.Key] = Finding{TaskID: t.TaskID, Kind: t.Kind, Subject: t.Subject, Severity: t.Instance.Severity,
				Detail: t.Instance.Detail, Evidence: t.Instance.Evidence, Segments: t.Instance.Segments, LastSeen: t.At}
		}
	}
	out := []Finding{}
	for _, f := range m {
		out = append(out, f)
	}
	return out
}

func ops(ts []Transition) map[Op]int {
	m := map[Op]int{}
	for _, t := range ts {
		m[t.Op]++
	}
	return m
}

func TestReconcileOpensAndClosesManagedChainKind(t *testing.T) {
	r := testRegistry(true)
	ts, _ := r.Reconcile(nil, overlapFacts(t0, "mysql-bin.000003.sealed.e5"))
	if len(ts) != 1 || ts[0].Op != OpOpen || ts[0].Kind != SegmentOverlap || ts[0].Subject != "mysql-bin.000003.sealed.e5" {
		t.Fatalf("open: %+v", ts)
	}
	if ts[0].Generation != 0 {
		t.Fatalf("generation must be left to the store, got %d", ts[0].Generation)
	}
	open := openOf(ts, nil)
	ts, _ = r.Reconcile(open, overlapFacts(t0.Add(time.Minute)))
	if len(ts) != 1 || ts[0].Op != OpClose {
		t.Fatalf("close: %+v", ts)
	}
}

func TestReconcileIncompleteScopeNeverCloses(t *testing.T) {
	r := testRegistry(true)
	open := openOf(mustReconcile(t, r, nil, overlapFacts(t0, "a")), nil)
	f := overlapFacts(t0.Add(time.Minute))
	f.Chain = nil
	f.Complete[ScopeChainLocal] = false
	if ts, _ := r.Reconcile(open, f); len(ts) != 0 {
		t.Fatalf("incomplete scope produced %+v", ts)
	}
	if h := r.HealthOf(open, f); h.Status != StatusUnhealthy {
		t.Fatalf("open CRITICAL finding with incomplete scope: %s", h.Status)
	}
}

func TestReconcileUnmanagedKindsNeverTransition(t *testing.T) {
	r := testRegistry(false)
	rng := rand.New(rand.NewSource(1))
	var open []Finding
	for i := 0; i < 500; i++ {
		f := overlapFacts(t0.Add(time.Duration(i) * time.Second))
		if rng.Intn(2) == 0 {
			f = overlapFacts(f.Now, "a", "b")
		}
		f.Signals = map[SignalKey]SignalState{{Kind: UploadRecordFailed}: {FailureSeq: uint64(rng.Intn(3)), SuccessSeq: uint64(rng.Intn(3))}}
		if rng.Intn(3) == 0 {
			open = append(open, Finding{TaskID: "1", Kind: SegmentOverlap, Subject: "z", Severity: Critical})
		}
		if ts, _ := r.Reconcile(open, f); len(ts) != 0 {
			t.Fatalf("unmanaged kinds transitioned: %+v", ts)
		}
	}
}

func TestReconcileUpdateAndEscalate(t *testing.T) {
	r := testRegistry(true)
	open := []Finding{{TaskID: "1", Kind: SegmentOverlap, Subject: "a", Severity: Warning, Detail: "old"}}
	ts, _ := r.Reconcile(open, overlapFacts(t0, "a"))
	if len(ts) != 1 || ts[0].Op != OpEscalate {
		t.Fatalf("escalate: %+v", ts)
	}
	open[0].Severity = Critical
	ts, _ = r.Reconcile(open, overlapFacts(t0, "a"))
	if len(ts) != 1 || ts[0].Op != OpUpdate {
		t.Fatalf("update: %+v", ts)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	r := testRegistry(true)
	rng := rand.New(rand.NewSource(7))
	var open []Finding
	names := []string{"a", "b", "c", "d"}
	for i := 0; i < 300; i++ {
		var segs []string
		for _, n := range names {
			if rng.Intn(2) == 0 {
				segs = append(segs, n)
			}
		}
		f := overlapFacts(t0.Add(time.Duration(i)*time.Minute), segs...)
		ts1, _ := r.Reconcile(open, f)
		ts2, _ := r.Reconcile(open, f)
		if len(ts1) != len(ts2) {
			t.Fatalf("same input, different output: %d vs %d", len(ts1), len(ts2))
		}
		open = openOf(ts1, open)
		if again, _ := r.Reconcile(open, f); len(again) != 0 {
			t.Fatalf("reconciling the output again gave %+v", again)
		}
	}
}

func TestReconcileHoldDown(t *testing.T) {
	r := testRegistry(true)
	spec := r[SegmentOverlap]
	spec.Clear.HoldDown = 2 * time.Minute
	r[SegmentOverlap] = spec
	// LastSeen is a stored wall-clock time from some node: it must not matter.
	open := []Finding{{TaskID: "1", Kind: SegmentOverlap, Subject: "a", Severity: Critical, Detail: "twice", LastSeen: t0.Add(-time.Hour)}}
	key := Key{"1", SegmentOverlap, "a"}
	f := overlapFacts(t0.Add(time.Minute))
	if ts, _ := r.Reconcile(open, f); len(ts) != 0 {
		t.Fatalf("closed with no AbsentSince (just now): %+v", ts)
	}
	f.AbsentSince = map[Key]time.Time{key: t0}
	if ts, _ := r.Reconcile(open, f); len(ts) != 0 {
		t.Fatalf("closed inside hold-down: %+v", ts)
	}
	f = overlapFacts(t0.Add(3 * time.Minute))
	f.AbsentSince = map[Key]time.Time{key: t0}
	if ts, _ := r.Reconcile(open, f); ops(ts)[OpClose] != 1 {
		t.Fatalf("not closed after hold-down: %+v", ts)
	}
}

func signalFacts(st SignalState) *Facts {
	return &Facts{TaskID: "1", Now: t0, Complete: map[Scope]bool{ScopeSignals: true, ScopeChainLocal: true},
		Chain:   &ChainFacts{},
		Signals: map[SignalKey]SignalState{{Kind: UploadRecordFailed, Subject: "seg"}: st}}
}

func TestSignalNotClosedBeforeFirstSuccessSinceStart(t *testing.T) {
	r := testRegistry(true)
	open := []Finding{{TaskID: "1", Kind: UploadRecordFailed, Subject: "seg", Severity: Critical}}
	// Restarted: no observation at all.
	f := signalFacts(SignalState{})
	f.Signals = nil
	if ts, _ := r.Reconcile(open, f); len(ts) != 0 {
		t.Fatalf("closed with no success since start: %+v", ts)
	}
	// Still failing after restart: no close, no reopen.
	if ts, _ := r.Reconcile(open, signalFacts(SignalState{FailureSeq: 1})); ops(ts)[OpClose] != 0 || ops(ts)[OpOpen] != 0 {
		t.Fatalf("flapped while still failing: %+v", ts)
	}
	// First success after the failure: close.
	if ts, _ := r.Reconcile(open, signalFacts(SignalState{FailureSeq: 1, SuccessSeq: 2})); ops(ts)[OpClose] != 1 {
		t.Fatalf("not closed after success: %+v", ts)
	}
	// The internal Unknown never reaches health.
	if h := r.HealthOf(nil, signalFacts(SignalState{})); h.Status != StatusHealthy {
		t.Fatalf("no open row and no success yet: %s, want HEALTHY", h.Status)
	}
}

func TestSignalOpensOnFailure(t *testing.T) {
	r := testRegistry(true)
	ts, _ := r.Reconcile(nil, signalFacts(SignalState{FailureSeq: 3, SuccessSeq: 1, Detail: "write failed"}))
	if len(ts) != 1 || ts[0].Op != OpOpen || ts[0].Subject != "seg" {
		t.Fatalf("%+v", ts)
	}
}

func gtidFacts(hole string, waivers ...Waiver) *Facts {
	return &Facts{TaskID: "1", Now: t0, Complete: map[Scope]bool{ScopeChainLocal: true},
		Chain:   &ChainFacts{Violations: []assurance.Violation{{Kind: assurance.I3GTIDHole, Segments: []string{"mysql-bin.000004"}, Evidence: hole}}},
		Waivers: waivers}
}

func TestWaiverExactEvidenceOnly(t *testing.T) {
	r := testRegistry(true)
	hole := "3E11FA47-71CA-11E1-9E33-C80AA9429562:363-370,3e11fa47-71ca-11e1-9e33-c80aa9429562:371-381"
	open := openOf(mustReconcile(t, r, nil, gtidFacts(hole)), nil)
	w := Waiver{Key: Key{TaskID: "1", Kind: GTIDHole, Subject: "mysql-bin.000004"}, Evidence: "3e11fa47-71ca-11e1-9e33-c80aa9429562:363-381", Reason: "gtid_next skip", Actor: "dba"}
	ts, _ := r.Reconcile(open, gtidFacts(hole, w))
	if len(ts) != 1 || ts[0].Op != OpClose || ts[0].Reason != ReasonWaived {
		t.Fatalf("waiver: %+v", ts)
	}
	if h := r.HealthOf(nil, gtidFacts(hole, w)); h.Status != StatusHealthy {
		t.Fatalf("waived hole health %s", h.Status)
	}
	wider := gtidFacts("3e11fa47-71ca-11e1-9e33-c80aa9429562:363-390", w)
	if ts, _ := r.Reconcile(nil, wider); ops(ts)[OpOpen] != 1 {
		t.Fatalf("a different hole must open: %+v", ts)
	}
}

func TestCanonicalGTIDRanges(t *testing.T) {
	cases := map[string]string{
		"BBBBBBBB-0000-0000-0000-000000000002:5-6,aaaaaaaa-0000-0000-0000-000000000001:3-4,AAAAAAAA-0000-0000-0000-000000000001:1-2": "aaaaaaaa-0000-0000-0000-000000000001:1-4,bbbbbbbb-0000-0000-0000-000000000002:5-6",
		"aaaaaaaa-0000-0000-0000-000000000001:7, aaaaaaaa-0000-0000-0000-000000000001:1-3,aaaaaaaa-0000-0000-0000-000000000001:4-5":  "aaaaaaaa-0000-0000-0000-000000000001:1-5,aaaaaaaa-0000-0000-0000-000000000001:7-7",
		"": "",
	}
	for in, want := range cases {
		got, err := CanonicalGTIDRanges(in)
		if err != nil || got != want {
			t.Fatalf("%q: got %q %v, want %q", in, got, err, want)
		}
	}
	if _, err := CanonicalGTIDRanges("aaaaaaaa-0000-0000-0000-000000000001:9-3"); err == nil {
		t.Fatal("reversed range accepted")
	}
}

func TestHealthOf(t *testing.T) {
	r := testRegistry(true)
	f := overlapFacts(t0)
	if h := r.HealthOf(nil, f); h.Status != StatusHealthy {
		t.Fatalf("%s", h.Status)
	}
	if h := r.HealthOf([]Finding{{Kind: UploadRecordFailed, Severity: Warning}}, f); h.Status != StatusDegraded {
		t.Fatalf("%s", h.Status)
	}
	if h := r.HealthOf([]Finding{{Kind: UploadRecordFailed, Severity: Info}}, f); h.Status != StatusHealthy {
		t.Fatalf("INFO must not degrade: %s", h.Status)
	}
	g := overlapFacts(t0)
	g.Complete[ScopeChainLocal] = false
	if h := r.HealthOf(nil, g); h.Status != StatusUnknown || len(h.Unknown) == 0 {
		t.Fatalf("%+v", h)
	}
}

// wantManagedP24: P2-3's three segment kinds, and since P2-4 every signal
// and task-state kind of the sixteen plus SOURCE_UNREACHABLE and TASK_FAILED.
var wantManagedP24 = map[Kind]bool{SegmentUnreadable: true, SegmentOverlap: true, SegmentEmptyLeftover: true,
	SegmentDamaged: true, ChainIndexGap: true, LegacyFailback: true, SegmentMissing: true, // P2-5.1, design §20.1
	UploadLate: true,                        // P2-5.2, design §21.7
	GTIDHole:   true, CheckpointAhead: true, // P2-5.3, design §22.1
	TaskPersistFailed: true, MetadataWriteBlocked: true, EventPersistFailed: true, EventReadFailed: true,
	DumpCleanupPersistFailed: true, DiskListFailed: true, UploadUnverified: true, UploadRecordFailed: true,
	RetentionRecordFailed: true, RetryLoopFailed: true, DumpStalled: true, OpenTailCutFailed: true,
	MetricSourceFailed: true, TransitionStalled: true, SourceUnreachable: true, TaskFailed: true,
	TaskUnadopted: true, TaskRecordsFailed: true}

func TestEmptyLeftoverDerivesFromChain(t *testing.T) {
	f := &Facts{TaskID: "t", Complete: map[Scope]bool{ScopeChainLocal: true, ScopeTaskState: true, ScopeSignals: true, ScopeStore: true},
		Chain: &ChainFacts{Violations: []assurance.Violation{{Kind: assurance.I4EmptyLeftover, Segments: []string{"mysql-bin.000002.open.e3"}, Detail: "empty"}}}}
	d := DefaultRegistry().Desire(f)
	in, ok := d[Key{TaskID: "t", Kind: SegmentEmptyLeftover, Subject: "mysql-bin.000002.open.e3"}]
	if !ok || in.Severity != Info {
		t.Fatalf("desired %+v", d)
	}
	if h := DefaultRegistry().HealthOf([]Finding{{TaskID: "t", Kind: SegmentEmptyLeftover, Subject: "x", Severity: Info}}, f); h.Status != StatusHealthy {
		t.Fatalf("INFO leftover made the task %s", h.Status)
	}
}

func TestRegistryComplete(t *testing.T) {
	reg := DefaultRegistry()
	for k, spec := range reg {
		if spec.Kind != k || spec.Severity == "" || len(spec.Scopes) == 0 || spec.Clear.Invariant == "" || spec.Clear.Text == "" || spec.Version < 1 {
			t.Errorf("incomplete spec %s: %+v", k, spec)
		}
		if want := wantManagedP24[k]; spec.Managed != want {
			t.Errorf("%s managed=%v, want %v (P2-3 segment kinds plus the P2-4 signal and task-state kinds)", k, spec.Managed, want)
		}
	}
	for _, k := range LegacyKinds() {
		if _, ok := reg[k]; !ok {
			t.Errorf("legacy kind %s not registered", k)
		}
	}
	for _, ak := range assurance.AllKinds() {
		k, ok := FromAssurance(ak)
		if !ok {
			t.Errorf("assurance kind %s has no finding kind", ak)
			continue
		}
		if _, ok := reg[k]; !ok {
			t.Errorf("assurance kind %s maps to unregistered %s", ak, k)
		}
	}
}

func mustReconcile(t *testing.T, r Registry, open []Finding, f *Facts) []Transition {
	t.Helper()
	ts, _ := r.Reconcile(open, f)
	return ts
}

func TestReconcileDowngradeIsExplicit(t *testing.T) {
	r := testRegistry(true)
	f := overlapFacts(t0, "a")
	f.Chain.Violations[0].Detail = "twice"
	open := []Finding{{TaskID: "1", Kind: SegmentOverlap, Subject: "a", Severity: Critical, Detail: "twice", Segments: []string{"a"}}}
	spec := r[SegmentOverlap]
	spec.Severity = Warning
	r[SegmentOverlap] = spec
	ts, _ := r.Reconcile(open, f)
	if len(ts) != 1 || ts[0].Op != OpDowngrade {
		t.Fatalf("lowered severity: %+v, want one DOWNGRADE", ts)
	}
}

func TestTransitionFactAt(t *testing.T) {
	r := testRegistry(true)
	at := t0.Add(-30 * time.Second)
	f := signalFacts(SignalState{FailureSeq: 2, FactAt: at})
	ts, _ := r.Reconcile(nil, f)
	if len(ts) != 1 || !ts[0].FactAt.Equal(at) || !ts[0].At.Equal(t0) {
		t.Fatalf("signal FactAt: %+v", ts)
	}
	ts, _ = r.Reconcile(nil, overlapFacts(t0, "a"))
	if len(ts) != 1 || !ts[0].FactAt.Equal(t0) {
		t.Fatalf("FactAt defaults to Now: %+v", ts)
	}
}

// designScopes is design §6 (and §8.4): the scopes each kind needs complete before it may close.
var designScopes = map[Kind][]Scope{
	SegmentUnreadable:        {ScopeChainLocal},
	SegmentOverlap:           {ScopeChainLocal},
	SegmentEmptyLeftover:     {ScopeChainLocal},
	SegmentDamaged:           {ScopeChainLocal},
	ChainIndexGap:            {ScopeChainLocal},
	GTIDHole:                 {ScopeChainLocal},
	LegacyFailback:           {ScopeChainLocal},
	CheckpointAhead:          {ScopeChainLocal},
	SegmentMissing:           {ScopeChainLocal, ScopeChainObjects}, // per instance since P2-5.2 (§21.4)
	OrphanRow:                {ScopeChainLocal, ScopeChainObjects},
	UploadLate:               {ScopeChainLocal},
	UploadUnverified:         {ScopeChainLocal, ScopeSignals},
	UploadRecordFailed:       {ScopeSignals},
	DumpStalled:              {ScopeTaskState, ScopeSignals},
	MetadataWriteBlocked:     {ScopeSignals},
	TaskPersistFailed:        {ScopeSignals},
	EventPersistFailed:       {ScopeSignals},
	EventReadFailed:          {ScopeSignals},
	DumpCleanupPersistFailed: {ScopeSignals},
	DiskListFailed:           {ScopeSignals},
	RetentionRecordFailed:    {ScopeSignals},
	RetryLoopFailed:          {ScopeSignals},
	OpenTailCutFailed:        {ScopeSignals},
	MetricSourceFailed:       {ScopeSignals},
	TransitionStalled:        {ScopeTaskState},
	TaskFailed:               {ScopeTaskState},
	SourceUnreachable:        {ScopeTaskState, ScopeSignals},
	TaskUnadopted:            {ScopeTaskState},
	TaskRecordsFailed:        {ScopeSignals},
	SealFactsAbsent:          {ScopeStore},
	HealthStale:              {ScopeTaskState},
	HealthStoreDegraded:      {ScopeStore},
}

func TestRegistryScopesMatchDesign(t *testing.T) {
	reg := DefaultRegistry()
	if len(reg) != len(designScopes) {
		t.Errorf("registry has %d kinds, design table %d", len(reg), len(designScopes))
	}
	for k, want := range designScopes {
		got := append([]Scope(nil), reg[k].Scopes...)
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		w := append([]Scope(nil), want...)
		sort.Slice(w, func(i, j int) bool { return w[i] < w[j] })
		if fmt.Sprint(got) != fmt.Sprint(w) {
			t.Errorf("%s scopes %v, design %v", k, got, w)
		}
	}
}

func TestCloseNeedsEveryScope(t *testing.T) {
	for k, scopes := range designScopes {
		if len(scopes) < 2 {
			continue
		}
		r := qmanaged(DefaultRegistry(), k)
		subject := "s"
		if k == SegmentMissing {
			subject = ObjectSubject("s") // per instance (§21.4): the object-origin instance reads both
		}
		open := []Finding{{TaskID: "1", Kind: k, Subject: subject, Severity: Critical}}
		for _, missing := range scopes {
			c := qall()
			c[missing] = false
			f := &Facts{TaskID: "1", Now: t0, Chain: &ChainFacts{}, Task: &TaskFacts{}, Complete: c,
				Signals: map[SignalKey]SignalState{{Kind: k, Subject: "s"}: {FailureSeq: 1, SuccessSeq: 2}}}
			if ts, _ := r.Reconcile(open, f); ops(ts)[OpClose] != 0 {
				t.Errorf("%s closed with %s incomplete: %+v", k, missing, ts)
			}
		}
	}
}

func TestHealthIgnoresScopesNotProduced(t *testing.T) {
	if Produced[ScopeChainObjects] || Produced[ScopeVerify] {
		t.Fatalf("chain.objects and verify are produced only from phase 3: %v", Produced)
	}
	r := qmanaged(DefaultRegistry(), SegmentUnreadable)
	c := qall()
	c[ScopeChainLocal] = false
	if h := r.HealthOf(nil, &Facts{TaskID: "1", Now: t0, Complete: c}); h.Status != StatusUnknown {
		t.Fatalf("a produced scope incomplete must still be UNKNOWN: %s", h.Status)
	}
}
