// Package binlog provides module-level functionality for binlog.
// input: the real GTID task directory in testdata/ledger/live and temporary copies with a segment removed, rewritten or added
// output: assertions on the ledger interface: binlog order, per-segment GTID ranges and headers, the stored set, holes with and without the start set, a missing directory, a rewritten file read again, the open tail, the sealed set and the resume set
// pos: unit coverage at the segment ledger's interface (ADR 0006)
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

const ledgerLiveUUID = "e2e732df-c5fa-11f1-9e48-822b383dbcd0"

func TestLedgerSegmentsAndStored(t *testing.T) {
	dir := copyLedgerDir(t, filepath.Join("testdata", "ledger", "live"))
	l := LedgerAt(dir)
	segs, err := l.Segments()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range segs {
		names = append(names, s.Name)
	}
	want := []string{"mysql-bin.000003", "mysql-bin.000004", "mysql-bin.000005", "mysql-bin.000006", "mysql-bin.000007.open.e1"}
	if len(names) != len(want) {
		t.Fatalf("segments %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("segments %v want %v", names, want)
		}
	}
	if got := l.Stored().String(); got != ledgerLiveUUID+":10-184" {
		t.Fatalf("stored %q", got)
	}
	c, err := l.Contents("mysql-bin.000004")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Log.HasPrevious || c.GTIDs.String() == "" || len(c.Log.Events) == 0 {
		t.Fatalf("contents %+v", c)
	}
	if !l.Holes(nil).IsEmpty() {
		t.Fatalf("holes %q", l.Holes(nil).String())
	}
	if _, err := l.Contents("no-such-segment"); err == nil {
		t.Fatal("missing segment read")
	}
}

func TestLedgerHolesAfterRemovedSegment(t *testing.T) {
	dir := copyLedgerDir(t, filepath.Join("testdata", "ledger", "live"))
	before, err := LedgerAt(dir).Contents("mysql-bin.000004")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "mysql-bin.000004")); err != nil {
		t.Fatal(err)
	}
	l := LedgerAt(dir)
	holes := l.Holes(nil)
	if holes.String() != before.GTIDs.String() {
		t.Fatalf("holes %q want the removed segment %q", holes.String(), before.GTIDs.String())
	}
	if covered := l.Holes(before.GTIDs); !covered.IsEmpty() {
		t.Fatalf("start set does not cover the hole: %q", covered.String())
	}
	if _, damaged := l.Damage("", "", "mysql"); damaged {
		t.Fatal("a missing segment is not damage")
	}
}

func TestLedgerRereadsChangedFile(t *testing.T) {
	dir := copyLedgerDir(t, filepath.Join("testdata", "ledger", "live"))
	p := filepath.Join(dir, "mysql-bin.000006")
	first, err := LedgerAt(dir).Contents("mysql-bin.000006")
	if err != nil || first.GTIDs.IsEmpty() {
		t.Fatalf("%v %v", first, err)
	}
	// Same path, different bytes: a new ledger must not answer from the old read.
	if err := os.WriteFile(p, durableMagic, 0o644); err != nil {
		t.Fatal(err)
	}
	again, err := LedgerAt(dir).Contents("mysql-bin.000006")
	if err != nil || !again.GTIDs.IsEmpty() {
		t.Fatalf("stale read %v %v", again.GTIDs, err)
	}
}

func TestLedgerMissingDirectory(t *testing.T) {
	l := LedgerAt(filepath.Join(t.TempDir(), "gone"))
	if _, err := l.Segments(); err == nil {
		t.Fatal("listing a missing directory")
	}
	if !l.Stored().IsEmpty() {
		t.Fatal("stored in a missing directory")
	}
	if _, ok := l.Tail(); ok {
		t.Fatal("tail in a missing directory")
	}
	if _, set, ok := l.ResumeGTID("mysql", ledgerLiveUUID+":1-3"); ok || set != "" {
		t.Fatal("resume set in a missing directory")
	}
	if _, damaged := l.Damage("", "", "mysql"); damaged {
		t.Fatal("damage in a missing directory")
	}
	if _, err := LedgerAt("  ").Segments(); err == nil {
		t.Fatal("blank directory listed")
	}
}

func TestLedgerTailAndResume(t *testing.T) {
	dir := copyLedgerDir(t, filepath.Join("testdata", "ledger", "live"))
	tail, set, ok := LedgerAt(dir).ResumeGTID("mysql", ledgerLiveUUID+":1-9")
	if !ok || tail.File != "mysql-bin.000007" || tail.Truncate {
		t.Fatalf("tail %+v ok=%v", tail, ok)
	}
	if set != ledgerLiveUUID+":1-184" {
		t.Fatalf("resume set %q", set)
	}
	durable := tail.Durable()
	// Previous-GTIDs are a history claim, not durable bytes: only the
	// complete transactions listed in the tail may enter a resume set.
	if len(durable) != len(tail.GTIDs) || durable[0] != tail.GTIDs[0] {
		t.Fatalf("durable %v", durable)
	}
	// Tear the last transaction: the resume set drops it and the cut is offered.
	p := filepath.Join(dir, "mysql-bin.000007.open.e1")
	info, _ := os.Stat(p)
	if err := os.Truncate(p, info.Size()-37); err != nil {
		t.Fatal(err)
	}
	torn, set, ok := LedgerAt(dir).ResumeGTID("mysql", ledgerLiveUUID+":1-9")
	if !ok || !torn.Truncate || set != ledgerLiveUUID+":1-183" {
		t.Fatalf("torn %+v %q", torn, set)
	}
}

func TestReadSegmentContentsMatchesLedger(t *testing.T) {
	dir := filepath.Join("testdata", "ledger", "live")
	raw, err := os.ReadFile(filepath.Join(dir, "mysql-bin.000005"))
	if err != nil {
		t.Fatal(err)
	}
	fromBytes, err := ReadSegmentContents(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	fromDir, err := LedgerAt(dir).Contents("mysql-bin.000005")
	if err != nil {
		t.Fatal(err)
	}
	if fromBytes.GTIDs.String() != fromDir.GTIDs.String() || fromBytes.Log.Previous != fromDir.Log.Previous {
		t.Fatalf("bytes %q dir %q", fromBytes.GTIDs.String(), fromDir.GTIDs.String())
	}
}

// #391: the sealed set leaves the open tail out and skips other flavors.
func TestLedgerSealedStored(t *testing.T) {
	dir := copyLedgerDir(t, filepath.Join("testdata", "ledger", "live"))
	l := LedgerAt(dir)
	tail, ok := l.Tail()
	if !ok || len(tail.GTIDs) == 0 {
		t.Fatalf("tail %+v", tail)
	}
	sealed := l.SealedStored()
	if sealed.IsEmpty() || sealed.String() == l.Stored().String() {
		t.Fatalf("sealed %q stored %q", sealed.String(), l.Stored().String())
	}
	if l.SealedText("mariadb") != "" {
		t.Fatal("sealed text for mariadb")
	}
	if l.SealedText("mysql") != sealed.String() {
		t.Fatalf("sealed text %q", l.SealedText("mysql"))
	}
}
