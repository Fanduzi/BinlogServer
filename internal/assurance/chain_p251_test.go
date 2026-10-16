package assurance

import (
	"os"
	"path/filepath"
	"testing"

	"binlog_server/internal/binlog"
)

// design 0009 §20.2: two witnesses know a sealed segment's size at seal.
// The .seal-sha256 ledger wins; the catalog row is compared only for a
// legacy segment the ledger has no line for.

func sealSum(t *testing.T, dir, name string, size int64) {
	t.Helper()
	if err := binlog.RecordSealSum(dir, name, size, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
}

func localSize(t *testing.T, f *fixture, name string) int64 {
	t.Helper()
	info, err := os.Stat(filepath.Join(f.dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func countKind(vs []Violation, k Kind, seg string) int {
	n := 0
	for _, v := range vs {
		if v.Kind == k && len(v.Segments) > 0 && v.Segments[0] == seg {
			n++
		}
	}
	return n
}

// A legacy segment (no ledger line) whose catalog size differs: I6_SIZE_MISMATCH.
func TestLegacySegmentRowSizeMismatch(t *testing.T) {
	f := live(t)
	f.row(t, "mysql-bin.000004").Size = 1
	vs := f.check(t)
	if countKind(vs, I6SizeMismatch, "mysql-bin.000004") != 1 {
		t.Fatalf("legacy segment with a different row size: %s", kinds(vs))
	}
	if has(vs, I1Unreadable, "mysql-bin.000004") {
		t.Fatalf("a catalog size alone must not make the segment unreadable: %s", kinds(vs))
	}
}

// The ledger agrees with the file: a stale row (legacy failback rewrite) is not reported.
func TestLedgerAgreesStaleRowNotReported(t *testing.T) {
	f := live(t)
	sealSum(t, f.dir, "mysql-bin.000004", localSize(t, f, "mysql-bin.000004"))
	f.row(t, "mysql-bin.000004").Size = 1
	vs := f.check(t)
	if has(vs, I6SizeMismatch, "mysql-bin.000004") {
		t.Fatalf("row size compared although the seal ledger has the segment: %s", kinds(vs))
	}
}

// The ledger disagrees with the file: that is I1 (§17.2), not also I6.
func TestLedgerDisagreesIsUnreadableOnly(t *testing.T) {
	f := live(t)
	sealSum(t, f.dir, "mysql-bin.000004", localSize(t, f, "mysql-bin.000004")+100)
	f.row(t, "mysql-bin.000004").Size = 1
	vs := f.check(t)
	if !has(vs, I1Unreadable, "mysql-bin.000004") {
		t.Fatalf("ledger size differs: want I1_UNREADABLE, got %s", kinds(vs))
	}
	if has(vs, I6SizeMismatch, "mysql-bin.000004") {
		t.Fatalf("ledger case also reported as I6: %s", kinds(vs))
	}
}

// An unrepaired legacy failback points shared rows at another file; the
// planner's LEGACY_FAILBACK explains it, so no local size mismatch.
func TestLegacyFailbackSuppressesRowSize(t *testing.T) {
	f := live(t)
	f.row(t, "mysql-bin.000005").Size = 1
	f.in.Planned = []Violation{{Kind: I3LegacyFailback, Segments: []string{"mysql-bin.000005"}, Detail: "legacy failback"}}
	vs := f.check(t)
	if has(vs, I6SizeMismatch, "mysql-bin.000005") {
		t.Fatalf("size mismatch reported inside a legacy failback: %s", kinds(vs))
	}
}

// An OPEN row's size is still moving: never compared.
func TestOpenRowSizeNotCompared(t *testing.T) {
	f := live(t)
	r := f.row(t, "mysql-bin.000006")
	r.Sealed, r.Size = false, 1
	if vs := f.check(t); has(vs, I6SizeMismatch, "mysql-bin.000006") {
		t.Fatalf("unsealed row compared: %s", kinds(vs))
	}
}

// Object size differs: its own kind, so the origin is the kind (§6.1, §20.2).
func TestObjectSizeMismatchIsItsOwnKind(t *testing.T) {
	f := live(t)
	f.in.Objects["t/mysql-bin.000005"] = 7
	vs := f.check(t)
	if !has(vs, I6ObjectSizeMismatch, "mysql-bin.000005") || has(vs, I6SizeMismatch, "mysql-bin.000005") {
		t.Fatalf("object size: %s", kinds(vs))
	}
}
