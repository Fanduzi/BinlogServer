// Package binlog provides module-level functionality for binlog.
// input: sealed segment bytes cut short (0 bytes, inside the magic, no magic, inside an event header) and a sealed file truncated at an event boundary whose size at seal is in the .seal-sha256 ledger
// output: the sealed reader returns ErrCorruptEventSize (wrapping ErrSegmentTruncated) for each, the open reader keeps the torn-tail reading, and Ledger.Unreadable names the segment with the reason
// pos: regression for issue 381 (a 0-byte sealed segment was readable and empty)
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadSealedSegmentContents_CutShortIsUnreadable(t *testing.T) {
	const uuid = "3e11fa47-71ca-11e1-9e33-c80aa9429562"
	body, _ := testGTIDSegment(t, time.Date(2024, 1, 1, 1, 20, 0, 0, time.UTC), uuid+":1-2", true, uuid, 3)
	fde := 4 + int(binary.LittleEndian.Uint32(body[4+9:4+13]))
	for name, b := range map[string][]byte{
		"empty":         {},
		"inside-magic":  body[:3],
		"no-magic":      append([]byte("xbin"), body[4:]...),
		"inside-header": body[:fde+10],
	} {
		if _, err := ReadSealedSegmentContents(bytes.NewReader(b)); !errors.Is(err, ErrCorruptEventSize) || !errors.Is(err, ErrSegmentTruncated) {
			t.Fatalf("%s: sealed err=%v, want ErrCorruptEventSize wrapping ErrSegmentTruncated", name, err)
		}
		if _, err := ReadSegmentContents(bytes.NewReader(b)); err != nil {
			t.Fatalf("%s: open reader err=%v, want the torn-tail reading", name, err)
		}
	}
	if _, err := ReadSealedSegmentContents(bytes.NewReader(body[:fde])); err != nil {
		t.Fatalf("magic + format description is a whole segment: err=%v", err)
	}
}

func TestLedgerUnreadable_SizeAtSealDiffers(t *testing.T) {
	const uuid = "3e11fa47-71ca-11e1-9e33-c80aa9429562"
	body, _ := testGTIDSegment(t, time.Date(2024, 1, 1, 1, 20, 0, 0, time.UTC), uuid+":1-2", true, uuid, 3)
	fde := int64(4 + binary.LittleEndian.Uint32(body[4+9:4+13]))
	dir := t.TempDir()
	for _, n := range []string{"mysql-bin.000001", "mysql-bin.000002"} {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := RecordSealSum(dir, n, int64(len(body)), strings.Repeat("a", 64)); err != nil {
			t.Fatal(err)
		}
	}
	if p, ok := LedgerAt(dir).Unreadable(); ok {
		t.Fatalf("clean: %+v", p)
	}
	if err := os.Truncate(filepath.Join(dir, "mysql-bin.000002"), fde); err != nil {
		t.Fatal(err)
	}
	p, ok := LedgerAt(dir).Unreadable()
	if !ok || p.Segment != "mysql-bin.000002" || len(p.Valid) != 1 || !strings.Contains(p.Detail, "at seal") {
		t.Fatalf("Unreadable = %+v %v, want mysql-bin.000002 with its size at seal", p, ok)
	}
}
