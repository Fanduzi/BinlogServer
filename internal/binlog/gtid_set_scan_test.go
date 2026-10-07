// Package binlog provides module-level functionality for binlog.
// input: a segment built from a MySQL GTID set encoding plus GTID events, including a checksum tail
// output: assertions that previous-GTIDs text round-trips and each GTID event keeps its start offset
// pos: regression coverage for the segment scan used by executed-GTID replay
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

func TestScanSegmentGTIDs_PreviousAndEvents(t *testing.T) {
	const uuid = "3e11fa47-71ca-11e1-9e33-c80aa9429562"
	when := time.Date(2024, 1, 1, 1, 20, 0, 0, time.UTC)
	body, offsets := testGTIDSegment(t, when, uuid+":1-2", true, uuid, 3, 4)
	log, err := ScanSegmentGTIDs(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if !log.HasPrevious {
		t.Fatal("missing previous")
	}
	prev, err := mysql.ParseGTIDSet(mysql.MySQLFlavor, log.Previous)
	if err != nil {
		t.Fatal(err)
	}
	want, err := mysql.ParseGTIDSet(mysql.MySQLFlavor, uuid+":1-2")
	if err != nil {
		t.Fatal(err)
	}
	if !prev.Equal(want) || !want.Contain(prev) {
		t.Fatalf("previous %q", log.Previous)
	}
	if len(log.Events) != 2 || log.Events[0].Seq != 3 || log.Events[1].Seq != 4 {
		t.Fatalf("events %+v", log.Events)
	}
	if log.Events[0].UUID != uuid || log.Events[0].Offset != offsets[3] || log.Events[1].Offset != offsets[4] {
		t.Fatalf("offsets %+v want %v", log.Events, offsets)
	}
	if !log.Events[0].When.Equal(when) {
		t.Fatalf("when %s", log.Events[0].When)
	}
}

func TestScanSegmentGTIDs_UnreadablePrevious(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(durableMagic)
	var hdr [19]byte
	hdr[4] = byte(goreplication.PREVIOUS_GTIDS_EVENT)
	binary.LittleEndian.PutUint32(hdr[9:13], uint32(goreplication.EventHeaderSize+1))
	buf.Write(hdr[:])
	buf.WriteByte(0xff)
	if _, err := ScanSegmentGTIDs(&buf); err == nil || !strings.Contains(err.Error(), "unreadable previous gtid event") {
		t.Fatalf("err %v", err)
	}
}

func testGTIDSegment(t *testing.T, when time.Time, previous string, checksum bool, uuid string, seqs ...int64) ([]byte, map[int64]uint64) {
	t.Helper()
	var buf bytes.Buffer
	buf.Write(durableMagic)
	offsets := map[int64]uint64{}
	if previous != "" {
		set, err := mysql.ParseMysqlGTIDSet(previous)
		if err != nil {
			t.Fatal(err)
		}
		payload := set.Encode()
		if checksum {
			payload = append(payload, 0, 0, 0, 0)
		}
		writeTestEvent(&buf, when, goreplication.PREVIOUS_GTIDS_EVENT, payload)
	}
	sid, err := hex.DecodeString(strings.ReplaceAll(uuid, "-", ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, seq := range seqs {
		payload := make([]byte, 1+goreplication.SidLength+8)
		copy(payload[1:], sid)
		binary.LittleEndian.PutUint64(payload[1+goreplication.SidLength:], uint64(seq))
		if checksum {
			payload = append(payload, 0, 0, 0, 0)
		}
		offsets[seq] = uint64(buf.Len())
		writeTestEvent(&buf, when, goreplication.GTID_EVENT, payload)
	}
	return buf.Bytes(), offsets
}

func writeTestEvent(buf *bytes.Buffer, when time.Time, kind goreplication.EventType, payload []byte) {
	size := uint32(goreplication.EventHeaderSize + len(payload))
	var hdr [19]byte
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(when.Unix()))
	hdr[4] = byte(kind)
	binary.LittleEndian.PutUint32(hdr[9:13], size)
	buf.Write(hdr[:])
	buf.Write(payload)
}
