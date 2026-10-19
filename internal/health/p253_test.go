// Package health provides module-level functionality for health.
// input: DefaultRegistry and GTID / checkpoint violations
// output: proof that GTID_HOLE and CHECKPOINT_AHEAD are Managed; ORPHAN_ROW is managed in P2-5.4
// pos: P2-5.3 kind-table tests (design 0009 §22)
// note: if this file changes, update this header and module README.md.
package health

import (
	"testing"

	"binlog_server/internal/assurance"
)

// design 0009 §22: P2-5.3 Managed table.

func TestP253ManagedTable(t *testing.T) {
	r := DefaultRegistry()
	for _, k := range []Kind{GTIDHole, CheckpointAhead} {
		if !r[k].Managed {
			t.Errorf("%s: managed=false, want true", k)
		}
		if r[k].Confirm {
			t.Errorf("%s: Confirm=true, want false (§22.1)", k)
		}
	}
	if !r[GTIDHole].Waivable {
		t.Fatal("GTID_HOLE must stay Waivable")
	}
	// ORPHAN_ROW and SEAL_FACTS_ABSENT flip in P2-5.4 (§26), not here.
}

func TestP253DerivesGTIDHoleAndCheckpointAhead(t *testing.T) {
	f := &Facts{TaskID: "1", Now: t0, Chain: &ChainFacts{Violations: []assurance.Violation{
		{Kind: assurance.I3GTIDHole, Segments: []string{"mysql-bin.000004"}, Evidence: "aaaaaaaa-0000-0000-0000-000000000001:10-12", Detail: "hole"},
		{Kind: assurance.I5CheckpointAhead, Segments: []string{"mysql-bin.000005"}, Evidence: "ahead", Detail: "ahead"},
	}}, Complete: map[Scope]bool{ScopeChainLocal: true}}
	d := DefaultRegistry().Desire(f)
	if _, ok := d[Key{"1", GTIDHole, "mysql-bin.000004"}]; !ok {
		t.Fatal("GTID_HOLE not desired")
	}
	if _, ok := d[Key{"1", CheckpointAhead, "mysql-bin.000005"}]; !ok {
		t.Fatal("CHECKPOINT_AHEAD not desired")
	}
}
