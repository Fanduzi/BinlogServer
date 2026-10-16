package health

import (
	"testing"
	"time"

	"binlog_server/internal/assurance"
)

func qmanaged(r Registry, ks ...Kind) Registry {
	out := Registry{}
	for k, v := range r {
		out[k] = v
	}
	for _, k := range ks {
		s := out[k]
		s.Managed = true
		out[k] = s
	}
	return out
}

// withObjects is SEGMENT_MISSING as P2-5.2 will judge it: chain.local and
// chain.objects, objects derived (design §20.1 keeps it local for now).
func withObjects(r Registry) Registry {
	s := r[SegmentMissing]
	s.Scopes = []Scope{ScopeChainLocal, ScopeChainObjects}
	r[SegmentMissing] = s
	return r
}

var qt0 = time.Date(2026, 10, 15, 1, 0, 0, 0, time.UTC)

func qall() map[Scope]bool {
	return map[Scope]bool{ScopeChainLocal: true, ScopeChainObjects: true, ScopeTaskState: true, ScopeSignals: true, ScopeStore: true}
}

// QA scratch tests from BinlogServerQA's review of #376 (evidence-376), MariaDB cases adjusted to the documented canonical form.

// Q1: CanonicalGTIDRanges on a MySQL multi-interval set "uuid:1-5:7-9"
func TestQA_Canon_MySQLMultiInterval(t *testing.T) {
	got, err := CanonicalGTIDRanges("3E11FA47-71CA-11E1-9E33-C80AA9429562:1-5:7-9")
	t.Logf("got=%q err=%v", got, err)
	if err == nil && got != "3e11fa47-71ca-11e1-9e33-c80aa9429562:1-5,3e11fa47-71ca-11e1-9e33-c80aa9429562:7-9" {
		t.Errorf("multi-interval MySQL set mis-parsed: %q", got)
	}
}

func TestQA_Canon_Cases(t *testing.T) {
	u, v := "aaaaaaaa-0000-0000-0000-000000000001", "bbbbbbbb-0000-0000-0000-000000000002"
	cases := []struct {
		in, want string
		err      bool
	}{
		{"", "", false},
		{"  ", "", false},
		{v + ":10-20," + u + ":5-6," + v + ":1-9", u + ":5-6," + v + ":1-20", false},
		{u + ":5-10," + u + ":7-8", u + ":5-10", false},
		{u + ":5-10," + u + ":11-12", u + ":5-12", false},
		{u + ":5-10," + u + ":12-13", u + ":5-10," + u + ":12-13", false},
		{u + ":7", u + ":7-7", false},
		{"AAAAAAAA-0000-0000-0000-000000000001:1-2", u + ":1-2", false},
		{u + ":0-3", "", true},
		{u + ":9-3", "", true},
		{u + ":x", "", true},
		{"0-1-100", "0-1:100-100", false},                         // MariaDB gtid
		{"1-2-5,0-1-100", "0-1:100-100,1-2:5-5", false},           // MariaDB gtid list, sorted by domain then server
		{"0-1:100-200,0-1:150-300,0-1-301", "0-1:100-301", false}, // MariaDB per domain-server ranges merged
		{"10-1-1,9-1-1", "9-1:1-1,10-1:1-1", false},               // numeric domain order
		{"0-1-100," + u + ":1-2", "", true},                       // mixed flavors
		{"0-1", "", true},
		{u + ":1-5:7-9," + v + ":3-4:1-2", u + ":1-5," + u + ":7-9," + v + ":1-4", false},
		{u + ":18446744073709551615", u + ":18446744073709551615-18446744073709551615", false},
	}
	for _, c := range cases {
		got, err := CanonicalGTIDRanges(c.in)
		if (err != nil) != c.err || (!c.err && got != c.want) {
			t.Errorf("%q: got %q err=%v want %q err=%v", c.in, got, err, c.want, c.err)
		}
	}
	// overflow on merge: b = MaxUint64 then +1 wraps
	got, err := CanonicalGTIDRanges(u + ":1-18446744073709551615," + u + ":5-6")
	t.Logf("maxuint merge got=%q err=%v", got, err)
	if got != u+":1-18446744073709551615" {
		t.Errorf("max uint merge: %q", got)
	}
}

// Q2: I6_MISSING_OBJECT finding must not close while objects are not listed (design P2-5 QA).
func TestQA_SegmentMissingObjectNotClosedWhenUnlisted(t *testing.T) {
	r := withObjects(qmanaged(DefaultRegistry(), SegmentMissing))
	open := []Finding{{TaskID: "1", Kind: SegmentMissing, Subject: "mysql-bin.000006", Severity: Critical, Evidence: "object missing"}}
	c := qall()
	c[ScopeChainObjects] = false // objects not enumerated this pass
	f := &Facts{TaskID: "1", Now: qt0, Chain: &ChainFacts{}, Complete: c}
	ts, _ := r.Reconcile(open, f)
	for _, tr := range ts {
		if tr.Op == OpClose {
			t.Errorf("SEGMENT_MISSING (object) closed while chain.objects incomplete: %+v", tr)
		}
	}
}

// Q3: DUMP_STALLED must not close when task.state is incomplete (row/lease mismatch, design §8.4)
func TestQA_DumpStalledNotClosedWhenTaskStateIncomplete(t *testing.T) {
	r := qmanaged(DefaultRegistry(), DumpStalled)
	open := []Finding{{TaskID: "1", Kind: DumpStalled, Severity: Warning}}
	c := qall()
	c[ScopeTaskState] = false
	f := &Facts{TaskID: "1", Now: qt0, Task: nil, Complete: c}
	ts, _ := r.Reconcile(open, f)
	for _, tr := range ts {
		if tr.Op == OpClose {
			t.Errorf("DUMP_STALLED closed with task.state incomplete: %+v", tr)
		}
	}
}

// Q4: health of a task with an ORPHAN_ROW-like objects kind managed but objects never listed (phase 2 has no lister)
func TestQA_HealthUnknownWhenObjectsNeverListed(t *testing.T) {
	r := qmanaged(DefaultRegistry(), OrphanRow, SegmentUnreadable)
	c := qall()
	c[ScopeChainObjects] = false
	h := r.HealthOf(nil, &Facts{TaskID: "1", Now: qt0, Complete: c})
	t.Logf("status=%s unknown=%v", h.Status, h.Unknown)
	if h.Status == StatusUnknown {
		t.Errorf("task with no findings is UNKNOWN only because objects are not listed (no lister until phase 3)")
	}
}

// Q5: signal: open row, no success since start, still failing -> no CLOSE, no re-OPEN; success -> CLOSE
func TestQA_SignalFlapAndClose(t *testing.T) {
	r := qmanaged(DefaultRegistry(), MetadataWriteBlocked)
	open := []Finding{{TaskID: "1", Kind: MetadataWriteBlocked, Severity: Critical, Detail: "ro"}}
	sk := SignalKey{Kind: MetadataWriteBlocked}
	for _, st := range []SignalState{{}, {FailureSeq: 3, Failures: 1, Detail: "ro"}} {
		ts, _ := r.Reconcile(open, &Facts{TaskID: "1", Now: qt0, Signals: map[SignalKey]SignalState{sk: st}, Complete: qall()})
		for _, tr := range ts {
			if tr.Op == OpClose || tr.Op == OpOpen {
				t.Errorf("state %+v gave %s", st, tr.Op)
			}
		}
	}
	ts, _ := r.Reconcile(open, &Facts{TaskID: "1", Now: qt0, Signals: map[SignalKey]SignalState{sk: {FailureSeq: 3, SuccessSeq: 4}}, Complete: qall()})
	if len(ts) != 1 || ts[0].Op != OpClose {
		t.Errorf("first success should close: %+v", ts)
	}
	// unmanaged: nothing ever
	un := DefaultRegistry()
	spec := un[MetadataWriteBlocked]
	spec.Managed = false
	un[MetadataWriteBlocked] = spec
	ts, _ = un.Reconcile(open, &Facts{TaskID: "1", Now: qt0, Signals: map[SignalKey]SignalState{sk: {SuccessSeq: 9}}, Complete: qall()})
	if len(ts) != 0 {
		t.Errorf("unmanaged produced %+v", ts)
	}
	// no open row + no success -> HEALTHY
	if h := r.HealthOf(nil, &Facts{TaskID: "1", Now: qt0, Complete: qall()}); h.Status != StatusHealthy {
		t.Errorf("want HEALTHY got %s", h.Status)
	}
}

// Q6: waiver exact vs superset/subset; different subject; MySQL multi interval evidence
func TestQA_Waiver(t *testing.T) {
	r := qmanaged(DefaultRegistry(), GTIDHole)
	u := "aaaaaaaa-0000-0000-0000-000000000001"
	v := assurance.Violation{Kind: assurance.I3GTIDHole, Segments: []string{"mysql-bin.000007"}, Evidence: u + ":363-381"}
	mk := func(ev string) *Facts {
		return &Facts{TaskID: "1", Now: qt0, Chain: &ChainFacts{Violations: []assurance.Violation{v}}, Complete: qall(),
			Waivers: []Waiver{{Key: Key{"1", GTIDHole, "mysql-bin.000007"}, Evidence: ev}}}
	}
	open := []Finding{{TaskID: "1", Kind: GTIDHole, Subject: "mysql-bin.000007", Severity: Critical, Evidence: v.Evidence}}
	for ev, want := range map[string]bool{u + ":363-381": true, strings_upper(u) + ":363-370," + u + ":371-381": true, u + ":363-380": false, u + ":363-382": false} {
		ts, _ := r.Reconcile(open, mk(ev))
		closed := len(ts) == 1 && ts[0].Op == OpClose && ts[0].Reason == ReasonWaived
		if closed != want {
			t.Errorf("waiver %q: closed=%v want %v (%+v)", ev, closed, want, ts)
		}
	}
}

func strings_upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}

// Q7: duplicate stored rows for the same key -> at most one transition per key
func TestQA_DuplicateOpenRows(t *testing.T) {
	r := qmanaged(DefaultRegistry(), SegmentUnreadable)
	open := []Finding{{TaskID: "1", Kind: SegmentUnreadable, Subject: "s"}, {TaskID: "1", Kind: SegmentUnreadable, Subject: "s"}}
	ts, _ := r.Reconcile(open, &Facts{TaskID: "1", Now: qt0, Chain: &ChainFacts{}, Complete: qall()})
	if len(ts) > 1 {
		t.Errorf("two transitions for one key: %+v", ts)
	}
}

// Q8: two violations for the same segment with different severity override -> one instance, escalated
func TestQA_IdempotentAfterApply(t *testing.T) {
	r := qmanaged(DefaultRegistry(), SegmentUnreadable, SegmentOverlap)
	f := &Facts{TaskID: "1", Now: qt0, Complete: qall(), Chain: &ChainFacts{Violations: []assurance.Violation{
		{Kind: assurance.I1Unreadable, Segments: []string{"a"}, Detail: "d1"},
		{Kind: assurance.I1Unreadable, Segments: []string{"a"}, Detail: "d2"},
		{Kind: assurance.I4Overlap, Segments: []string{"b", "a"}, Evidence: "x"}}}}
	ts, d := r.Reconcile(nil, f)
	var open []Finding
	for k, in := range d {
		open = append(open, Finding{TaskID: k.TaskID, Kind: k.Kind, Subject: k.Subject, Severity: in.Severity, Detail: in.Detail, Evidence: in.Evidence, Segments: in.Segments})
	}
	ts2, _ := r.Reconcile(open, f)
	t.Logf("first=%d second=%+v", len(ts), ts2)
	if len(ts2) != 0 {
		t.Errorf("not idempotent: %+v", ts2)
	}
	// determinism with duplicates of the same key: run 50 times, detail must be stable
	first := ""
	for i := 0; i < 50; i++ {
		_, d := r.Reconcile(nil, f)
		got := d[Key{"1", SegmentUnreadable, "a"}].Detail
		if first == "" {
			first = got
		} else if got != first {
			t.Fatalf("non-deterministic detail: %q vs %q", got, first)
		}
	}
}
