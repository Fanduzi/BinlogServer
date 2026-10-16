// Package assurance provides module-level functionality for assurance.
// input: the live and failback ledger fixtures with the #348 QA mutations
// output: assertions for the QA findings: the default upload grace, I5 for a checkpoint file after the tail or a position past a sealed file, the order self-check (coverage of local segments, increasing index per prefix, a dropped failback stint), I6 for a chain segment with no row and no file, both size mismatches, an UPLOADED row without an object key, and a second interior GTID hole
// pos: regression coverage for #348 QA (ADR 0009 phase 1)
// note: if this file changes, update this header and module README.md.
package assurance

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"binlog_server/internal/binlog"
)

const qs3, qs4, qs5, qs6, qs7 = "mysql-bin.000003", "mysql-bin.000004", "mysql-bin.000005", "mysql-bin.000006", "mysql-bin.000007.open.e1"

func TestUploadGraceDefaultsTo15Minutes(t *testing.T) {
	for _, c := range []struct {
		age  time.Duration
		late bool
	}{{time.Second, false}, {15 * time.Minute, false}, {15*time.Minute + time.Second, true}} {
		f := live(t)
		f.in.UploadGrace = 0
		r := f.row(t, qs5)
		r.UploadState, r.Checksum, r.SealedAt = "PENDING", "", testNow.Add(-c.age)
		if got := has(f.check(t), I7UploadLate, qs5); got != c.late {
			t.Errorf("sealed %s ago with UploadGrace unset: late=%v, want %v", c.age, got, c.late)
		}
	}
}

func TestCheckpointAheadOfTheChain(t *testing.T) {
	info, err := os.Stat(filepath.Join("..", "binlog", "testdata", "ledger", "live", qs6))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		cp    Checkpoint
		ahead bool
	}{
		{"file after the tail", Checkpoint{File: "mysql-bin.000008", Pos: 4}, true},
		{"far file after the tail", Checkpoint{File: "mysql-bin.000012", Pos: 120}, true},
		{"past a sealed file", Checkpoint{File: qs6, Pos: 999999}, true},
		{"one byte past a sealed file", Checkpoint{File: qs6, Pos: uint32(info.Size()) + 1}, true},
		{"end of a sealed file", Checkpoint{File: qs6, Pos: uint32(info.Size())}, false},
		{"an earlier file", Checkpoint{File: qs3, Pos: 4}, false},
	} {
		f := live(t)
		f.in.Checkpoint.File, f.in.Checkpoint.Pos = c.cp.File, c.cp.Pos
		vs := f.check(t)
		got := false
		for _, v := range vs {
			if v.Kind == I5CheckpointAhead && v.Evidence != "" && v.Evidence[0] != 'e' {
				got = true
			}
		}
		if got != c.ahead {
			t.Errorf("%s: I5 by position=%v, want %v (%s)", c.name, got, c.ahead, kinds(vs))
		}
	}
}

func TestOrderSelfCheck(t *testing.T) {
	f := live(t)
	f.dropOrder(qs5)
	if vs := f.check(t); !has(vs, I2OrderInvalid, qs5) {
		t.Errorf("local segment left out of the order: %s", kinds(vs))
	}
	r := live(t)
	r.in.Order = []string{qs3, qs4, qs6, qs5, qs7}
	if vs := r.check(t); !has(vs, I2OrderInvalid, qs5) {
		t.Errorf("index goes backwards: %s", kinds(vs))
	}

	dir := copyDir(t, filepath.Join("..", "binlog", "testdata", "ledger", "failback"))
	full := []string{failbackB + ".mysql-bin.000001", "mysql-bin.000002", "mysql-bin.000003", "mysql-bin.000003.sealed.e1", "mysql-bin.000003.open.e1"}
	in := Inputs{Flavor: "mysql", Order: full, Local: binlog.LedgerAt(dir), Switches: []Switch{{Identity: failbackB}}}
	if vs := ChainOf(in).Check(testNow); has(vs, I2OrderInvalid, "") {
		t.Errorf("complete failback order reported: %s", kinds(vs))
	}
	in.Order = full[1:]
	if vs := ChainOf(in).Check(testNow); !has(vs, I2OrderInvalid, failbackB+".mysql-bin.000001") {
		t.Errorf("dropped failback stint not reported: %s", kinds(vs))
	}
}

func TestI6NoRowNoFileOnChain(t *testing.T) {
	f := live(t)
	f.removeFile(t, qs5)
	f.dropRow(qs5)
	delete(f.in.Objects, "t/"+qs5)
	if vs := f.check(t); !has(vs, I6MissingRow, qs5) {
		t.Fatalf("chain segment with no row and no file: %s", kinds(vs))
	}
	f.in.Flavor = "mariadb"
	if vs := f.check(t); !has(vs, I6MissingRow, qs5) {
		t.Fatalf("mariadb: %s", kinds(vs))
	}
}

func TestI6BothSizesDiffer(t *testing.T) {
	f := live(t)
	f.row(t, qs5).Size = 100
	f.in.Objects["t/"+qs5] = 200
	vs := f.check(t)
	if countKind(vs, I6SizeMismatch, qs5) != 1 || countKind(vs, I6ObjectSizeMismatch, qs5) != 1 {
		t.Fatalf("local and object both differ: want one I6_SIZE_MISMATCH and one I6_OBJECT_SIZE_MISMATCH, got %s", kinds(vs))
	}
}

func TestI6UploadedWithoutKey(t *testing.T) {
	for _, local := range []bool{false, true} {
		f := live(t)
		if !local {
			f.removeFile(t, qs4)
		}
		r := f.row(t, qs4)
		delete(f.in.Objects, r.ObjectKey)
		r.ObjectKey = ""
		if vs := f.check(t); !has(vs, I6MissingObject, qs4) {
			t.Errorf("local=%v: UPLOADED row with no object key: %s", local, kinds(vs))
		}
	}
}

// A hole the neighbour check names must not hide another one it cannot see
// (s4 is only in the bucket, so nothing reads its neighbours' GTIDs).
func TestSecondInteriorHole(t *testing.T) {
	f := live(t)
	f.removeFile(t, qs4)
	delete(f.in.Objects, "t/"+qs4)
	f.in.Objects["t/"+qs4] = f.row(t, qs4).Size
	f.dropOrder(qs6)
	vs := f.check(t)
	if !has(vs, I3GTIDHole, qs7) {
		t.Fatalf("neighbour hole at %s: %s", qs7, kinds(vs))
	}
	if !has(vs, I3GTIDHole, qs5) {
		t.Fatalf("interior hole before %s missed: %s", qs5, kinds(vs))
	}
}

// SelectReplayFiles keeps only the highest open epoch of a source file; the
// lower one it drops on purpose is not missing from the order.
func TestOrderSkipsSupersededOpenEpoch(t *testing.T) {
	f := live(t)
	raw, err := os.ReadFile(filepath.Join(f.dir, qs7))
	if err != nil {
		t.Fatal(err)
	}
	e2 := "mysql-bin.000007.open.e2"
	if err := os.WriteFile(filepath.Join(f.dir, e2), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	f.in.Order = []string{qs3, qs4, qs5, qs6, e2}
	if vs := f.check(t); has(vs, I2OrderInvalid, "") {
		t.Fatalf("lower open epoch reported: %s", kinds(vs))
	}
	f.in.Order = []string{qs3, qs4, qs5, qs6, qs7}
	if vs := f.check(t); !has(vs, I2OrderInvalid, e2) {
		t.Fatalf("highest open epoch left out of the order: %s", kinds(vs))
	}
}

// The failback fixture's mysql-bin.000003 and mysql-bin.000003.sealed.e1 are
// two epochs of one source file that continue each other (GTIDs 1156-1255,
// then 1256-1555): not an overlap. A copy of the same bytes still is.
func TestEpochContinuationIsNotOverlap(t *testing.T) {
	dir := copyDir(t, filepath.Join("..", "binlog", "testdata", "ledger", "failback"))
	order := []string{"mysql-bin.000002", "mysql-bin.000003", "mysql-bin.000003.sealed.e1", "mysql-bin.000003.open.e1", failbackB + ".mysql-bin.000001"}
	for _, flavor := range []string{"mysql", "mariadb"} {
		in := Inputs{Flavor: flavor, Order: order, Local: binlog.LedgerAt(dir), Switches: []Switch{{Identity: failbackB}}}
		if vs := ChainOf(in).Check(testNow); has(vs, I4Overlap, "") {
			t.Errorf("%s: epoch continuation reported as overlap: %s", flavor, kinds(vs))
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "mysql-bin.000003.sealed.e1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mysql-bin.000003.sealed.e2"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	dup := []string{"mysql-bin.000002", "mysql-bin.000003", "mysql-bin.000003.sealed.e1", "mysql-bin.000003.sealed.e2", "mysql-bin.000003.open.e1", failbackB + ".mysql-bin.000001"}
	for _, flavor := range []string{"mysql", "mariadb"} {
		in := Inputs{Flavor: flavor, Order: dup, Local: binlog.LedgerAt(dir), Switches: []Switch{{Identity: failbackB}}}
		if vs := ChainOf(in).Check(testNow); !has(vs, I4Overlap, "mysql-bin.000003.sealed.e2") {
			t.Errorf("%s: a copy of the same epoch bytes is an overlap: %s", flavor, kinds(vs))
		}
	}
}

// A second sealed copy of a source file that carries no GTIDs (junk,
// magic-only, header-only, all zeros) next to one that does is still an
// overlap on MySQL: an empty GTID set intersects nothing, so positions decide.
func TestNoGTIDSecondCopyIsOverlap(t *testing.T) {
	src := filepath.Join("..", "binlog", "testdata", "ledger", "failback")
	raw, err := os.ReadFile(filepath.Join(src, "mysql-bin.000003.sealed.e1"))
	if err != nil {
		t.Fatal(err)
	}
	fdeEnd := 4 + int(binary.LittleEndian.Uint32(raw[4+9:4+13]))
	shapes := map[string][]byte{
		"junk":        []byte("this is not a binlog file at all"),
		"magic-only":  append([]byte(nil), raw[:4]...),
		"header-only": append([]byte(nil), raw[:fdeEnd]...),
		"all-zeros":   make([]byte, 4096),
	}
	order := []string{"mysql-bin.000002", "mysql-bin.000003", "mysql-bin.000003.sealed.e1", "mysql-bin.000003.sealed.e2", "mysql-bin.000003.open.e1", failbackB + ".mysql-bin.000001"}
	for name, body := range shapes {
		dir := copyDir(t, src)
		if err := os.WriteFile(filepath.Join(dir, "mysql-bin.000003.sealed.e2"), body, 0o644); err != nil {
			t.Fatal(err)
		}
		for _, flavor := range []string{"mysql", "mariadb"} {
			in := Inputs{Flavor: flavor, Order: order, Local: binlog.LedgerAt(dir), Switches: []Switch{{Identity: failbackB}}}
			if vs := ChainOf(in).Check(testNow); !has(vs, I4Overlap, "mysql-bin.000003.sealed.e2") {
				t.Errorf("%s/%s: no-GTID second copy not reported as overlap: %s", name, flavor, kinds(vs))
			}
		}
	}
}
