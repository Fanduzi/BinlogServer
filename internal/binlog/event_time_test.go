// Package binlog provides module-level functionality for binlog.
// input: in-memory binlog segments with event-header timestamps
// output: assertions for the first and last non-zero event time, and for a segment with no timed event
// pos: regression coverage for the point-in-time span reader
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func TestEventTimeSpan(t *testing.T) {
	early := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	late := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	raw := timedBinlog(t, time.Unix(0, 0).UTC(), early, late)
	first, last, ok, err := EventTimeSpan(bytes.NewReader(raw))
	if err != nil || !ok || !first.Equal(early) || !last.Equal(late) {
		t.Fatalf("span ok=%v first=%s last=%s err=%v", ok, first, last, err)
	}

	first, last, ok, err = EventTimeSpan(bytes.NewReader([]byte{0xfe, 'b', 'i', 'n'}))
	if err != nil || ok || !first.IsZero() || !last.IsZero() {
		t.Fatalf("magic only ok=%v first=%s last=%s err=%v", ok, first, last, err)
	}
	first, last, ok, err = EventTimeSpan(bytes.NewReader([]byte("not-a-binlog")))
	if err != nil || ok {
		t.Fatalf("bad magic ok=%v err=%v", ok, err)
	}

	torn := timedBinlog(t, early)
	torn = append(torn, 1, 2, 3)
	first, last, ok, err = EventTimeSpan(bytes.NewReader(torn))
	if err != nil || !ok || !first.Equal(early) || !last.Equal(early) {
		t.Fatalf("torn ok=%v first=%s last=%s err=%v", ok, first, last, err)
	}
}

func timedBinlog(t *testing.T, times ...time.Time) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write(durableMagic)
	pos := uint32(4)
	for _, ts := range times {
		const payload = 1
		size := uint32(19 + payload)
		pos += size
		var hdr [19]byte
		binary.LittleEndian.PutUint32(hdr[0:4], uint32(ts.Unix()))
		hdr[4] = 2
		binary.LittleEndian.PutUint32(hdr[5:9], 1)
		binary.LittleEndian.PutUint32(hdr[9:13], size)
		binary.LittleEndian.PutUint32(hdr[13:17], pos)
		buf.Write(hdr[:])
		buf.WriteByte(0)
	}
	return buf.Bytes()
}
