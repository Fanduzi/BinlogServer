// Package binlog provides module-level functionality for binlog.
// input: a sealed segment whose event-size high byte was flipped to 0xff
// output: scanSegmentGTIDs and ScanSegmentForGTID return ErrCorruptEventSize without allocating the claimed body; the process stays alive
// pos: regression for issue 358
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

func TestScanSegmentGTIDs_CorruptEventSizeDoesNotAllocate(t *testing.T) {
	const uuid = "3e11fa47-71ca-11e1-9e33-c80aa9429562"
	when := time.Date(2024, 1, 1, 1, 20, 0, 0, time.UTC)
	body, _ := testGTIDSegment(t, when, uuid+":1-2", true, uuid, 3)
	// Flip the high byte of the second event's size (after magic + FDE + previous).
	// That asks for about 4 GiB; the old scanner called make([]byte, rest) and OOMed.
	corrupt := append([]byte(nil), body...)
	hdrAt := findNthEventHeader(t, corrupt, 2)
	corrupt[hdrAt+9+3] = 0xff
	claimed := binary.LittleEndian.Uint32(corrupt[hdrAt+9 : hdrAt+13])
	if claimed < 1<<30 {
		t.Fatalf("corrupt size %d, want >= 1GiB", claimed)
	}

	old := makeEventBody
	var asked int
	makeEventBody = func(n int) []byte {
		asked = n
		if n > 1<<20 {
			t.Fatalf("would allocate %d bytes for a corrupt event size (issue 358)", n)
		}
		return make([]byte, n)
	}
	t.Cleanup(func() { makeEventBody = old })

	_, err := scanSegmentGTIDs(bytes.NewReader(corrupt))
	if !errors.Is(err, ErrCorruptEventSize) {
		t.Fatalf("err=%v asked=%d, want ErrCorruptEventSize", err, asked)
	}
}

func TestScanSegmentForGTID_CorruptEventSizeDoesNotAllocate(t *testing.T) {
	const uuid = "3e11fa47-71ca-11e1-9e33-c80aa9429562"
	when := time.Date(2024, 1, 1, 1, 20, 0, 0, time.UTC)
	body, _ := testGTIDSegment(t, when, "", false, uuid, 7)
	corrupt := append([]byte(nil), body...)
	hdrAt := findNthEventHeader(t, corrupt, 1)
	corrupt[hdrAt+9+3] = 0xff

	old := makeEventBody
	makeEventBody = func(n int) []byte {
		if n > 1<<20 {
			t.Fatalf("would allocate %d bytes for a corrupt event size (issue 358)", n)
		}
		return make([]byte, n)
	}
	t.Cleanup(func() { makeEventBody = old })

	_, err := ScanSegmentForGTID(bytes.NewReader(corrupt), uuid, 7)
	if !errors.Is(err, ErrCorruptEventSize) {
		t.Fatalf("err=%v, want ErrCorruptEventSize", err)
	}
}

func TestSegmentContentsAt_CorruptEventSizeStaysAlive(t *testing.T) {
	const uuid = "3e11fa47-71ca-11e1-9e33-c80aa9429562"
	when := time.Date(2024, 1, 1, 1, 20, 0, 0, time.UTC)
	body, _ := testGTIDSegment(t, when, uuid+":1-2", true, uuid, 3)
	corrupt := append([]byte(nil), body...)
	hdrAt := findNthEventHeader(t, corrupt, 2)
	corrupt[hdrAt+9+3] = 0xff
	dir := t.TempDir()
	path := filepath.Join(dir, "mysql-bin.000001")
	if err := os.WriteFile(path, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	old := makeEventBody
	makeEventBody = func(n int) []byte {
		if n > 1<<20 {
			t.Fatalf("would allocate %d bytes for a corrupt event size (issue 358)", n)
		}
		return make([]byte, n)
	}
	t.Cleanup(func() { makeEventBody = old })
	_, err := segmentContentsAt(path)
	if !errors.Is(err, ErrCorruptEventSize) {
		t.Fatalf("err=%v, want ErrCorruptEventSize", err)
	}
}

func findNthEventHeader(t *testing.T, body []byte, n int) int {
	t.Helper()
	if len(body) < 4 || string(body[:4]) != string(durableMagic) {
		t.Fatal("no magic")
	}
	off := 4
	for i := 0; i < n; i++ {
		if off+goreplication.EventHeaderSize > len(body) {
			t.Fatalf("event %d past end", i)
		}
		if i == n-1 {
			return off
		}
		sz := int(binary.LittleEndian.Uint32(body[off+9 : off+13]))
		if sz < goreplication.EventHeaderSize {
			t.Fatalf("bad size at %d", off)
		}
		off += sz
	}
	t.Fatal("unreachable")
	return 0
}
