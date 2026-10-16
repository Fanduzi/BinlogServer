package assurance

import (
	"path/filepath"
	"testing"

	"binlog_server/internal/binlog"
)

// design 0009 §22.2 / §22.5: Switches suppress cross-stint I3; CheckCheckpoint is separate.

func TestP253_SwitchesSuppressCrossStintGTIDHole(t *testing.T) {
	dir := copyDir(t, filepath.Join("..", "binlog", "testdata", "ledger", "failback"))
	bName := failbackB + ".mysql-bin.000001"
	// Chronological A→B→A-style order: B between A's segments causes switchHole.
	order := []string{"mysql-bin.000002", bName, "mysql-bin.000003", "mysql-bin.000003.sealed.e1", "mysql-bin.000003.open.e1"}
	in := Inputs{Flavor: "mysql", Order: order, Local: binlog.LedgerAt(dir), NoCatalog: true}
	vs := ChainOf(in).CheckBody(testNow)
	if !has(vs, I3GTIDHole, "") {
		t.Fatalf("without Switches want I3_GTID_HOLE, got %s", kinds(vs))
	}
	in.Switches = []Switch{{Identity: failbackB}}
	vs = ChainOf(in).CheckBody(testNow)
	if has(vs, I3GTIDHole, "") {
		t.Fatalf("with Switches still reported I3_GTID_HOLE: %s", kinds(vs))
	}
	if has(vs, I2SwitchUnrecorded, "") {
		t.Fatalf("recorded switch still I2_SWITCH_UNRECORDED: %s", kinds(vs))
	}
}

func TestP253_CheckCheckpointSeparateFromBody(t *testing.T) {
	f := live(t)
	body := ChainOf(f.in).CheckBody(testNow)
	for _, v := range body {
		if v.Kind == I5CheckpointAhead {
			t.Fatalf("CheckBody must not report I5: %s", kinds(body))
		}
	}
	f.in.Checkpoint = Checkpoint{GTIDSet: "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee:1-1"}
	i5 := ChainOf(f.in).CheckCheckpoint()
	if !has(i5, I5CheckpointAhead, "") {
		t.Fatalf("CheckCheckpoint want I5, got %s", kinds(i5))
	}
	// Empty checkpoint: no I5.
	f.in.Checkpoint = Checkpoint{}
	if vs := ChainOf(f.in).CheckCheckpoint(); has(vs, I5CheckpointAhead, "") {
		t.Fatalf("empty checkpoint reported I5: %s", kinds(vs))
	}
	// Check = body + I5
	f.in.Checkpoint = Checkpoint{GTIDSet: "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee:1-1"}
	all := ChainOf(f.in).Check(testNow)
	if !has(all, I5CheckpointAhead, "") {
		t.Fatalf("Check lost I5: %s", kinds(all))
	}
}
