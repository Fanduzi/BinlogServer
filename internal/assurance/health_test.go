// Package assurance provides module-level functionality for assurance.
// input: the live ledger fixture, with and without a regressed segment, and stub planner conclusions
// output: assertions for Health priority (damage before planners, planners in their order, then a sealed source file held twice on the order, other kinds ignored), that Plan is not asked when damaged and is asked once, and for KeepBefore
// pos: unit coverage for the storage verdict and replay cut (ADR 0009 phase 1, P1-3)
// note: if this file changes, update this header and module README.md.
package assurance

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"binlog_server/internal/binlog"
)

func TestHealthPriority(t *testing.T) {
	hole := binlog.StorageProblem{Segment: "mysql-bin.000005", Missing: "x:3"}
	legacy := binlog.StorageProblem{Segment: "mysql-bin.000004"}
	calls := 0
	plan := func() []Violation {
		calls++
		return []Violation{{Kind: I6OrphanRow}, {Kind: I3LegacyFailback, Problem: &legacy}, {Kind: I3StoredHole, Problem: &hole}}
	}

	f := live(t)
	f.in.Local = binlog.LedgerAt(f.dir)
	f.in.Plan = plan
	c := ChainOf(f.in)
	v, ok := c.Health()
	if !ok || v.Kind != I3LegacyFailback || v.Problem.Segment != "mysql-bin.000004" {
		t.Fatalf("healthy dir with planners: %+v %v", v, ok)
	}
	c.Check(testNow)
	if calls != 1 {
		t.Fatalf("Plan asked %d times, want 1", calls)
	}

	d := live(t)
	d.regress(t, "mysql-bin.000005", "mysql-bin.000004", 2000)
	d.in.Local = binlog.LedgerAt(d.dir)
	calls = 0
	d.in.Plan = plan
	v, ok = ChainOf(d.in).Health()
	if !ok || v.Kind != I1Regression || v.Problem == nil || v.Problem.Segment != "mysql-bin.000005" {
		t.Fatalf("damaged: %+v %v", v, ok)
	}
	if calls != 0 {
		t.Fatalf("Plan asked on a damaged chain")
	}

	ok2 := live(t)
	ok2.in.Local = binlog.LedgerAt(ok2.dir)
	ok2.in.Plan = func() []Violation { return []Violation{{Kind: I7Unverified}} }
	if v, found := ChainOf(ok2.in).Health(); found {
		t.Fatalf("non-verdict kind became a verdict: %+v", v)
	}
}

func TestKeepBefore(t *testing.T) {
	chain := []Link{{Name: "a", Before: true}, {Name: "b.open", Before: true, SameIndex: true}, {Name: "b"}, {Name: "c"}}
	if got := KeepBefore(chain, "b"); !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("keep %v", got)
	}
	if got := KeepBefore(chain, " "); len(got) != 0 || got == nil {
		t.Fatalf("blank segment keeps %v", got)
	}
}

// #370: a sealed source file held twice on the chain order is a verdict
// after the planners, naming the later copy; an object-only copy is not.
func TestHealthSealedTwice(t *testing.T) {
	for _, tc := range []struct{ src, dup string }{
		{"mysql-bin.000004", "mysql-bin.000004.sealed.e1"},
		{"mysql-bin.000006", "mysql-bin.000006.sealed.e3"},
	} {
		f := live(t)
		f.duplicate(t, tc.src, tc.dup)
		f.in.Local = binlog.LedgerAt(f.dir)
		v, ok := ChainOf(f.in).Health()
		if !ok || v.Kind != I4Overlap || v.Problem == nil || v.Problem.Segment != tc.dup {
			t.Fatalf("%s: %+v %v, want I4_OVERLAP at %s", tc.dup, v, ok, tc.dup)
		}
		for _, name := range v.Problem.Valid {
			if name == tc.src || name == tc.dup {
				t.Fatalf("%s: a copy is listed as valid: %v", tc.dup, v.Problem.Valid)
			}
		}

		legacy := binlog.StorageProblem{Segment: "mysql-bin.000002"}
		f.in.Plan = func() []Violation { return []Violation{{Kind: I3LegacyFailback, Problem: &legacy}} }
		if v, _ := ChainOf(f.in).Health(); v.Kind != I3LegacyFailback {
			t.Fatalf("%s: planner verdict did not win over the overlap: %+v", tc.dup, v)
		}
		f.in.Plan = nil

		if err := os.Remove(filepath.Join(f.dir, tc.dup)); err != nil {
			t.Fatal(err)
		}
		f.in.Local = binlog.LedgerAt(f.dir)
		if v, found := ChainOf(f.in).Health(); found {
			t.Fatalf("%s: object-only second copy became a verdict: %+v", tc.dup, v)
		}
	}
}
