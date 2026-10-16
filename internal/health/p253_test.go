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
	// Still not managed this step.
	for _, k := range []Kind{OrphanRow, UploadLate, SealFactsAbsent} {
		if r[k].Managed {
			t.Errorf("%s managed early", k)
		}
	}
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
