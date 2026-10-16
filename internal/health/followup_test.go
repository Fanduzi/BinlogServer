package health

import (
	"testing"

	"binlog_server/internal/assurance"
)

// BinlogServerQA #376 round 2 (zz_qa2), object-origin instance since P2-5.2
// (§21.4: one subject, two object facts); a pass without an object listing must not rewrite the
// evidence (UPDATE) or lower the severity (DOWNGRADE); ESCALATE still goes.
func TestUpdateAndDowngradeNeedEveryScope(t *testing.T) {
	r := withObjects(qmanaged(DefaultRegistry(), SegmentMissing))
	facts := func(objects bool, vs ...assurance.Violation) *Facts {
		c := qall()
		c[ScopeChainObjects] = objects
		return &Facts{TaskID: "1", Now: qt0, Chain: &ChainFacts{Violations: vs}, Complete: c, Confirmed: map[Key]bool{{"1", SegmentMissing, "s6#object"}: true}, ObjectSegments: map[string]bool{"s6": true}}
	}
	loc := assurance.Violation{Kind: assurance.I6ObjectSizeMismatch, Segments: []string{"s6"}, Evidence: "local"}
	obj := assurance.Violation{Kind: assurance.I6MissingObject, Segments: []string{"s6"}, Evidence: "object"}
	_, d := r.Reconcile(nil, facts(true, obj, loc))
	in := d[Key{"1", SegmentMissing, "s6#object"}]
	open := []Finding{{TaskID: "1", Kind: SegmentMissing, Subject: "s6#object", Severity: in.Severity, Evidence: in.Evidence, Segments: in.Segments}}
	for i := 0; i < 4; i++ {
		ts, _ := r.Reconcile(open, facts(i%2 == 1, obj, loc))
		if i%2 == 0 && len(ts) != 0 {
			t.Fatalf("unlisted pass %d rewrote the finding: %+v", i, ts)
		}
		if i%2 == 1 && len(ts) != 0 {
			t.Fatalf("listed pass %d flapped back: %+v", i, ts)
		}
	}
	ts, _ := r.Reconcile(open, facts(false, loc))
	if len(ts) != 0 {
		t.Fatalf("unlisted pass with only local evidence: %+v", ts)
	}

	spec := r[SegmentMissing]
	spec.Severity = Warning
	r[SegmentMissing] = spec
	if ts, _ := r.Reconcile(open, facts(false, loc)); len(ts) != 0 {
		t.Fatalf("DOWNGRADE with objects unlisted: %+v", ts)
	}
	if ts, _ := r.Reconcile(open, facts(true, loc)); len(ts) != 1 || ts[0].Op != OpDowngrade {
		t.Fatalf("DOWNGRADE with every scope complete: %+v", ts)
	}

	spec.Severity = Critical
	r[SegmentMissing] = spec
	low := []Finding{{TaskID: "1", Kind: SegmentMissing, Subject: "s6#object", Severity: Warning, Evidence: "local", Segments: []string{"s6"}}}
	if ts, _ := r.Reconcile(low, facts(false, loc)); len(ts) != 1 || ts[0].Op != OpEscalate {
		t.Fatalf("ESCALATE must go through with objects unlisted: %+v", ts)
	}
}

func TestCanonicalGTIDUUIDHexAndTags(t *testing.T) {
	u := "aaaaaaaa-0000-0000-0000-000000000001"
	cases := []struct {
		in, want string
		err      bool
	}{
		{"zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz:1-2", "", true},
		{"aaaaaaaa-0000-0000-0000-00000000000g:1-2", "", true},
		{"aaaaaaaa0-000-0000-0000-000000000001:1-2", "", true},
		// MySQL 8.4 tagged GTIDs: tags are case-insensitive, canonical lowercase,
		// untagged intervals first, then tags in order.
		{u + ":Tag_1:1-5", u + ":tag_1:1-5", false},
		{u + ":tb:3-4," + u + ":1-2:ta:1-1:2-3", u + ":1-2," + u + ":ta:1-3," + u + ":tb:3-4", false},
		{u + ":tag:", "", true},
		{u + ":1tag:1-2", "", true},
		{u + ":" + "abcdefghijklmnopqrstuvwxyzabcdefg" + ":1-2", "", true}, // 33 chars
		{u + "::1-2", "", true},
	}
	for _, c := range cases {
		got, err := CanonicalGTIDRanges(c.in)
		if (err != nil) != c.err || (!c.err && got != c.want) {
			t.Errorf("%q: got %q err=%v want %q err=%v", c.in, got, err, c.want, c.err)
		}
	}
}
