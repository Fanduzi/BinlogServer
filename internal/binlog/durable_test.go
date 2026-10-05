// Package binlog provides module-level functionality for binlog.
// input: temporary segment files with complete and torn binlog events
// output: proof that DurableResume returns the highest open segment's last end log_pos, and that a trailing rotate names the next file
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

func TestLastRotateTarget_NextFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mysql-bin.000003")
	body := make([]byte, 8+len("mysql-bin.000004"))
	binary.LittleEndian.PutUint64(body[:8], 4)
	copy(body[8:], "mysql-bin.000004")
	raw := framedEvent(byte(goreplication.ROTATE_EVENT), body, 4)
	writeRawSegment(t, path, raw)
	next, pos, end, ok := LastRotateTarget(path)
	if !ok || next != "mysql-bin.000004" || pos != 4 || end != uint32(4+len(raw)) {
		t.Fatalf("rotate target next=%s pos=%d end=%d ok=%v", next, pos, end, ok)
	}

	crc := framedEvent(byte(goreplication.FORMAT_DESCRIPTION_EVENT), formatDescriptionWithCRC(), 4)
	rotBody := append(append([]byte{}, body...), 0x01, 0x02, 0x03, 0x04)
	rot := framedEvent(byte(goreplication.ROTATE_EVENT), rotBody, uint32(4+len(crc)))
	writeRawSegment(t, path, append(crc, rot...))
	next, pos, end, ok = LastRotateTarget(path)
	if !ok || next != "mysql-bin.000004" || pos != 4 {
		t.Fatalf("checksum rotate target next=%s pos=%d end=%d ok=%v", next, pos, end, ok)
	}
}

func framedEvent(eventType byte, body []byte, start uint32) []byte {
	size := uint32(goreplication.EventHeaderSize + len(body))
	raw := make([]byte, size)
	raw[4] = eventType
	binary.LittleEndian.PutUint32(raw[9:13], size)
	binary.LittleEndian.PutUint32(raw[13:17], start+size)
	copy(raw[goreplication.EventHeaderSize:], body)
	return raw
}

func formatDescriptionWithCRC() []byte {
	body := make([]byte, 62)
	binary.LittleEndian.PutUint16(body[0:2], 4)
	copy(body[2:52], []byte("8.0.36"))
	body[56] = byte(goreplication.EventHeaderSize)
	body[57] = byte(goreplication.BINLOG_CHECKSUM_ALG_CRC32)
	return body
}

func writeRawSegment(t *testing.T, path string, raw []byte) {
	t.Helper()
	var buf bytes.Buffer
	buf.Write(durableMagic)
	buf.Write(raw)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}
