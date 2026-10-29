// Package health provides module-level functionality for health.
// input: randomized open finding sets and Facts across ScopeChainLocal completeness and violation presence
// output: PR-5 property proof on production Reconcile — a chain finding whose fact still derives never emits OpClose (the open interval is monotonic across ownership changes; there is no OWNER_CHANGED close anymore), an incomplete chain.local emits nothing for chain-scoped keys (an incomplete observation neither opens nor closes), and a fact gone on a complete read closes once with EVAL and stays closed
// pos: property coverage of the derived-finding model (ADR 0011 PR-5, checklist C6)
// note: if this file changes, update this header and module README.md.
package health

import (
	"math/rand"
	"testing"
	"time"

	"binlog_server/internal/assurance"
)

// pr5Seeds is the subject pool; each subject's violation kind derives a
// different chain finding, so a random subset exercises several managed
// kinds at once.
var pr5Violations = []assurance.Violation{
	v(assurance.I1Regression, "seg-a"),
	v(assurance.I2IndexGap, "seg-b"),
	v(assurance.I3LegacyFailback, "seg-c"),
	v(assurance.I4EmptyLeftover, "seg-d"),
	v(assurance.I6MissingLocal, "seg-e"),
}

// pr5OpenFindings builds one stored open finding per violation subject
// with the kind that violation derives.
func pr5OpenFindings(r *rand.Rand) []Finding {
	kindOf := map[assurance.Kind]Kind{
		assurance.I1Regression:     SegmentDamaged,
		assurance.I2IndexGap:       ChainIndexGap,
		assurance.I3LegacyFailback: LegacyFailback,
		assurance.I4EmptyLeftover:  SegmentEmptyLeftover,
		assurance.I6MissingLocal:   SegmentMissing,
	}
	var open []Finding
	for _, violation := range pr5Violations {
		if r.Intn(2) == 0 {
			continue
		}
		for _, seg := range violation.Segments {
			open = append(open, Finding{TaskID: "1", Kind: kindOf[violation.Kind], Subject: seg, Severity: Critical})
		}
	}
	return open
}

func pr5SubsetViolations(r *rand.Rand) []assurance.Violation {
	var vs []assurance.Violation
	for _, violation := range pr5Violations {
		if r.Intn(2) == 0 {
			continue
		}
		vs = append(vs, violation)
	}
	return vs
}

// An incomplete chain.local observation must produce no transition at all
// for chain-scoped keys: not an open, not a close, not a rewrite.
func TestPR5IncompleteObservationIsSilent(t *testing.T) {
	r := rand.New(rand.NewSource(20260529))
	for seed := 0; seed < 400; seed++ {
		open := pr5OpenFindings(r)
		// chainFactsOf always marks chain.local complete; build the facts
		// by hand so this pass's local observation is genuinely incomplete.
		f := &Facts{TaskID: "1", Now: time.Unix(int64(seed+1), 0).UTC(),
			Chain:     &ChainFacts{Violations: pr5SubsetViolations(r)},
			Complete:  map[Scope]bool{},
			Confirmed: map[Key]bool{}}
		ts, _ := DefaultRegistry().Reconcile(open, f)
		for _, tr := range ts {
			if spec, ok := DefaultRegistry()[tr.Kind]; ok && chainDirectoryOnly(spec) {
				t.Fatalf("seed %d: incomplete chain.local emitted %+v (open=%+v)", seed, tr, open)
			}
		}
	}
}

// A fact that still derives keeps its finding open: no OpClose may ever
// appear for a still-desired key, on any pass. This is the monotonic-open
// property the deleted OWNER_CHANGED path violated.
func TestPR5StillDerivedNeverCloses(t *testing.T) {
	r := rand.New(rand.NewSource(20260530))
	reg := DefaultRegistry()
	for seed := 0; seed < 400; seed++ {
		open := pr5OpenFindings(r)
		f := chainFactsOf(true, pr5SubsetViolations(r)...)
		f.Now = time.Unix(int64(seed+1), 0).UTC()
		desired, _ := reg.derive(f, nil)
		ts, _ := reg.Reconcile(open, f)
		for _, tr := range ts {
			if _, ok := desired[tr.Key]; tr.Op == OpClose && ok {
				t.Fatalf("seed %d: still-desired key closed %+v", seed, tr)
			}
		}
	}
}

// A chain violation that disappears on a complete read closes its finding
// exactly once with EVAL; replaying the same facts emits nothing — a
// closed key never reopens without its fact.
func TestPR5GoneFactClosesOnceAndStaysClosed(t *testing.T) {
	reg := DefaultRegistry()
	facts := chainFactsOf(true)
	for seed := 0; seed < 100; seed++ {
		r := rand.New(rand.NewSource(int64(seed)))
		open := pr5OpenFindings(r)
		if len(open) == 0 {
			continue
		}
		facts.Now = time.Unix(int64(seed+1), 0).UTC()
		ts, _ := reg.Reconcile(open, facts)
		var kept []Finding
		closedKeys := map[Key]bool{}
		for _, tr := range ts {
			if tr.Op != OpClose || tr.Reason != ReasonEval {
				t.Fatalf("seed %d: transition %+v, want EVAL closes only", seed, tr)
			}
			closedKeys[tr.Key] = true
		}
		for _, o := range open {
			if !closedKeys[o.Key()] {
				kept = append(kept, o)
			}
		}
		if len(kept) != 0 {
			t.Fatalf("seed %d: %+v not closed though their facts are gone", seed, kept)
		}
		ts2, _ := reg.Reconcile(nil, facts)
		if len(ts2) != 0 {
			t.Fatalf("seed %d: second pass reopened %+v with no fact", seed, ts2)
		}
	}
}
