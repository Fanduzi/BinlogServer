// Package binlog provides module-level functionality for binlog.
// input: temporary segment files with complete and torn binlog events
// output: proof that DurableResume returns the highest open segment's last end log_pos
// pos: regression coverage for the shared resume cursor
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

func writeSegment(t *testing.T, path string, positions []uint32, tornTail bool) {
	t.Helper()
	var buf bytes.Buffer
	buf.Write(durableMagic)
	for _, pos := range positions {
		hdr := make([]byte, goreplication.EventHeaderSize)
		binary.LittleEndian.PutUint32(hdr[9:13], uint32(goreplication.EventHeaderSize))
		binary.LittleEndian.PutUint32(hdr[13:17], pos)
		buf.Write(hdr)
	}
	if tornTail {
		buf.Write([]byte{1, 2, 3, 4})
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDurableResume_HighestOpenEvent(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "7")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSegment(t, filepath.Join(taskDir, "mysql-bin.000009"), []uint32{999}, false)
	writeSegment(t, filepath.Join(taskDir, "mysql-bin.000003.open.e1"), []uint32{40}, false)
	writeSegment(t, filepath.Join(taskDir, "mysql-bin.000003.open.e2"), []uint32{80, 120}, true)
	writeSegment(t, filepath.Join(taskDir, "mysql-bin.000004.open.e1"), nil, false)

	file, pos, ok := DurableResume(dir, "7")
	if !ok || file != "mysql-bin.000003" || pos != 120 {
		t.Fatalf("resume file=%q pos=%d ok=%v", file, pos, ok)
	}

	if _, _, _, ok := DurableCursor(filepath.Join(taskDir, "mysql-bin.000004.open.e1")); ok {
		t.Fatal("magic-only segment is not a resume point")
	}
	if _, got, ok := DurableResume(dir, "../7"); ok || got != 0 {
		t.Fatalf("path escape ok=%v pos=%d", ok, got)
	}
}
