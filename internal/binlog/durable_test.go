// Package binlog provides module-level functionality for binlog.
// input: temporary segment files with complete and torn binlog events
// output: proof that DurableResume returns the highest open segment's last end log_pos, that a trailing artificial rotate with log_pos 0 does not hide that position, that magic and a log_pos 0 header are not resume points, that a trailing rotate names the next file, that EventSpan is the contiguous event chain in the file, and that SegmentPositions uses that DurableCursor end
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

// TestDurableCursor_ArtificialRotatePosZero is the #214 resume cursor.
// A source restart sends an artificial Rotate (end_log_pos 0) after the last
// real event. That event must not make the whole segment look empty: the
// resume position is the previous event, and the cursor ends before the
// artificial bytes. A segment whose only event has log_pos 0 is still not a
// resume point. On current main the last log_pos of 0 makes DurableCursor
// return ok=false, so DurableResume falls back to an older segment.
func TestDurableCursor_ArtificialRotatePosZero(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "7")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSegment(t, filepath.Join(taskDir, "mysql-bin.000009.open.e1"), []uint32{40}, false)

	path := filepath.Join(taskDir, "mysql-bin.000010.open.e20")
	writeSegment(t, path, []uint32{126, 197, 220}, false)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	artificial := artificialRotateRaw("mysql-bin.000011")
	if err := os.WriteFile(path, append(before, artificial...), 0o644); err != nil {
		t.Fatal(err)
	}

	pos, end, size, ok := DurableCursor(path)
	if !ok || pos != 220 || end != int64(len(before)) || size != int64(len(before)+len(artificial)) {
		t.Fatalf("cursor pos=%d end=%d size=%d ok=%v, want pos 220 end %d size %d", pos, end, size, ok, len(before), len(before)+len(artificial))
	}
	file, resumePos, resumeOK := DurableResume(dir, "7")
	if !resumeOK || file != "mysql-bin.000010" || resumePos != 220 {
		t.Fatalf("resume file=%q pos=%d ok=%v, want mysql-bin.000010:220 (not the older segment)", file, resumePos, resumeOK)
	}

	only := filepath.Join(taskDir, "only-zero.open.e1")
	writeSegment(t, only, []uint32{0}, false)
	if _, _, _, ok := DurableCursor(only); ok {
		t.Fatal("a segment whose only event has log_pos 0 is not a resume point")
	}
	if !PreambleOnly(only) {
		t.Fatal("log_pos 0 header should be adoptable")
	}
}

func TestSegmentPositions_UsesDurableCursor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mysql-bin.000003")
	writeSegment(t, path, []uint32{23, 80}, false)
	cursor, _, _, cursorOK := DurableCursor(path)
	start, end, ok := SegmentPositions(path)
	if !cursorOK || !ok || end != cursor || start == 0 || end <= start {
		t.Fatalf("span start=%d end=%d ok=%v cursor=%d cursorOK=%v", start, end, ok, cursor, cursorOK)
	}
	magic := filepath.Join(dir, "mysql-bin.000004")
	writeSegment(t, magic, nil, false)
	if _, _, ok := SegmentPositions(magic); ok {
		t.Fatal("magic-only file has no position")
	}
	torn := filepath.Join(dir, "mysql-bin.000005")
	writeSegment(t, torn, nil, true)
	if _, _, ok := SegmentPositions(torn); ok {
		t.Fatal("torn file has no position")
	}
}

func TestPreambleOnly(t *testing.T) {
	dir := t.TempDir()
	magic := filepath.Join(dir, "mysql-bin.000001.open.e1")
	writeSegment(t, magic, nil, false)
	if !PreambleOnly(magic) {
		t.Fatal("magic-only")
	}
	header := filepath.Join(dir, "mysql-bin.000002.open.e1")
	writeSegment(t, header, []uint32{0, 0}, false)
	if !PreambleOnly(header) {
		t.Fatal("header-only")
	}
	real := filepath.Join(dir, "mysql-bin.000003.open.e1")
	writeSegment(t, real, []uint32{0, 197}, false)
	if PreambleOnly(real) {
		t.Fatal("durable event")
	}
	torn := filepath.Join(dir, "mysql-bin.000004.open.e1")
	writeSegment(t, torn, nil, true)
	if PreambleOnly(torn) {
		t.Fatal("torn tail")
	}
	if PreambleOnly(filepath.Join(dir, "missing")) {
		t.Fatal("missing file")
	}
}

func artificialRotateRaw(next string) []byte {
	name := []byte(next)
	body := make([]byte, 8+len(name)+4) // position, name, crc32 trailer
	binary.LittleEndian.PutUint64(body[:8], 4)
	copy(body[8:], name)
	size := uint32(goreplication.EventHeaderSize + len(body))
	raw := make([]byte, size)
	raw[4] = byte(goreplication.ROTATE_EVENT)
	binary.LittleEndian.PutUint32(raw[9:13], size)
	binary.LittleEndian.PutUint32(raw[13:17], 0)
	binary.LittleEndian.PutUint16(raw[17:19], 0x0020) // LOG_EVENT_ARTIFICIAL_F
	copy(raw[goreplication.EventHeaderSize:], body)
	return raw
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

func TestEventSpan_ContiguousChainAndGap(t *testing.T) {
	dir := t.TempDir()
	first := framedEvent(byte(goreplication.QUERY_EVENT), []byte("a"), 4)
	second := framedEvent(byte(goreplication.QUERY_EVENT), []byte("bb"), uint32(4+len(first)))
	zero := framedEvent(byte(goreplication.ROTATE_EVENT), []byte("mysql-bin.000010"), 0)
	binary.LittleEndian.PutUint32(zero[13:17], 0)
	full := filepath.Join(dir, "full")
	writeRawSegment(t, full, append(append(first, second...), append(zero, 1, 2, 3)...))
	start, end, ok := EventSpan(full)
	wantEnd := uint32(4 + len(first) + len(second))
	if !ok || start != 4 || end != wantEnd {
		t.Fatalf("contiguous span %d..%d ok=%v, want 4..%d", start, end, ok, wantEnd)
	}

	fde := framedEvent(byte(goreplication.FORMAT_DESCRIPTION_EVENT), bytes.Repeat([]byte{0}, 103), 4)
	if len(fde) != 122 {
		t.Fatalf("format description size %d", len(fde))
	}
	data := framedEvent(byte(goreplication.QUERY_EVENT), []byte("mid"), 543)
	gapped := filepath.Join(dir, "gap")
	writeRawSegment(t, gapped, append(fde, data...))
	start, end, ok = EventSpan(gapped)
	if !ok || start != 543 || end != uint32(543+len(data)) {
		t.Fatalf("gapped span %d..%d ok=%v, want 543..%d", start, end, ok, 543+len(data))
	}

	magic := filepath.Join(dir, "magic")
	writeRawSegment(t, magic, nil)
	if _, _, ok := EventSpan(magic); ok {
		t.Fatal("magic-only segment has no event span")
	}
}
