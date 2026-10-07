// Package binlog provides module-level functionality for binlog.
// input: crafted binlog segments with MySQL GTID event bodies
// output: assertions that the stop offset is the event start in this file, not the header end_log_pos, and that a previous-GTIDs header is not a transaction
// pos: regression coverage for the GTID stop position
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"testing"
	"time"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

const gtidTestSID = "3e11fa4771ca11e19e33c80aa9429562"

func TestScanSegmentForGTID_StopOffsetIsEventStart(t *testing.T) {
	when := time.Date(2024, 1, 1, 1, 2, 3, 0, time.UTC)
	// Header log_pos is a source end position that does not match this file.
	raw, second := gtidFile(t, when,
		gtidPiece{seq: 1, logPos: 9000},
		gtidPiece{seq: 2, logPos: 9500},
	)
	got, err := ScanSegmentForGTID(bytes.NewReader(raw), "3E11FA47-71CA-11E1-9E33-C80AA9429562", 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hit == nil || got.Hit.Offset != second || !got.Hit.When.Equal(when) {
		t.Fatalf("hit %+v want offset %d", got.Hit, second)
	}
	if !got.OK || !got.First.Equal(when) || !got.Last.Equal(when) {
		t.Fatalf("span ok=%v first=%s last=%s", got.OK, got.First, got.Last)
	}
	miss, err := ScanSegmentForGTID(bytes.NewReader(raw), "3E11FA47-71CA-11E1-9E33-C80AA9429562", 9)
	if err != nil || miss.Hit != nil {
		t.Fatalf("missing seq hit=%+v err=%v", miss.Hit, err)
	}
	other, err := ScanSegmentForGTID(bytes.NewReader(raw), "00000000-0000-0000-0000-000000000001", 1)
	if err != nil || other.Hit != nil {
		t.Fatalf("other uuid hit=%+v err=%v", other.Hit, err)
	}
}

func TestScanSegmentForGTID_IgnoresPreviousGTIDsAndTornTail(t *testing.T) {
	when := time.Date(2024, 1, 1, 1, 2, 3, 0, time.UTC)
	raw := typedBinlog(t,
		typedEvent{goreplication.FORMAT_DESCRIPTION_EVENT, when},
		typedEvent{goreplication.PREVIOUS_GTIDS_EVENT, when},
	)
	got, err := ScanSegmentForGTID(bytes.NewReader(raw), "3e11fa47-71ca-11e1-9e33-c80aa9429562", 1)
	if err != nil || got.Hit != nil || got.OK {
		t.Fatalf("header counted as a transaction: %+v err=%v", got, err)
	}
	body, off := gtidFile(t, when, gtidPiece{seq: 4, logPos: 100, crc: true})
	body = append(body, 1, 2, 3)
	got, err = ScanSegmentForGTID(bytes.NewReader(body), "3e11fa47-71ca-11e1-9e33-c80aa9429562", 4)
	if err != nil || got.Hit == nil || got.Hit.Offset != off {
		t.Fatalf("crc hit %+v off %d err %v", got.Hit, off, err)
	}
	if _, err := ScanSegmentForGTID(bytes.NewReader([]byte("not-a-binlog")), "3e11fa47-71ca-11e1-9e33-c80aa9429562", 1); err != nil {
		t.Fatal(err)
	}
}

type gtidPiece struct {
	seq    int64
	logPos uint32
	crc    bool
}

func gtidFile(t *testing.T, when time.Time, pieces ...gtidPiece) (raw []byte, lastOffset uint64) {
	t.Helper()
	sid, err := hex.DecodeString(gtidTestSID)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.Write(durableMagic)
	offset := uint64(4)
	for _, piece := range pieces {
		body := make([]byte, 1+goreplication.SidLength+8)
		copy(body[1:], sid)
		binary.LittleEndian.PutUint64(body[1+goreplication.SidLength:], uint64(piece.seq))
		if piece.crc {
			body = append(body, 0x11, 0x22, 0x33, 0x44)
		}
		size := uint32(goreplication.EventHeaderSize + len(body))
		var hdr [19]byte
		binary.LittleEndian.PutUint32(hdr[0:4], uint32(when.Unix()))
		hdr[4] = byte(goreplication.GTID_EVENT)
		binary.LittleEndian.PutUint32(hdr[5:9], 1)
		binary.LittleEndian.PutUint32(hdr[9:13], size)
		binary.LittleEndian.PutUint32(hdr[13:17], piece.logPos)
		lastOffset = offset
		buf.Write(hdr[:])
		buf.Write(body)
		offset += uint64(size)
	}
	return buf.Bytes(), lastOffset
}
