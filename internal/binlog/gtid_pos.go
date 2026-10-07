// Package binlog provides module-level functionality for binlog.
// input: one binlog segment reader, beginning with the 4-byte magic header, and one MySQL GTID uuid:seq
// output: the byte offset where that GTID event starts, its event-header time, and the copied-event time span of the bytes read
// pos: locate a MySQL GTID inside a sealed, open, or object-backed segment so replay can stop before it
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"encoding/binary"
	"encoding/hex"
	"io"
	"strings"
	"time"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

// GTIDHit is one MySQL GTID event inside a segment.
// Offset is the byte where that event starts in this segment, which is the
// position mysqlbinlog --stop-position excludes. It is not the source
// end_log_pos stored in the event header: a mid-file copy prepends a format
// description, so those two numbers differ.
type GTIDHit struct {
	Offset uint64
	When   time.Time
}

// SegmentGTIDScan is the result of reading one segment for a single GTID.
// Hit is set when that GTID event was read. First and Last are the copied
// event times of the bytes read, using the same header rules as EventTimeSpan.
// A torn tail keeps the hit and the times of the complete events already read.
type SegmentGTIDScan struct {
	Hit   *GTIDHit
	First time.Time
	Last  time.Time
	OK    bool
}

// ScanSegmentForGTID walks one segment and returns the first GTID event whose
// server UUID and sequence match uuid and seq. uuid is compared without case.
// Previous-GTIDs events are not transactions in this segment and are ignored.
// A segment without the binlog magic returns an empty scan. The caller owns r.
func ScanSegmentForGTID(r io.Reader, uuid string, seq int64) (SegmentGTIDScan, error) {
	var out SegmentGTIDScan
	if seq <= 0 {
		return out, nil
	}
	want := strings.ToLower(strings.TrimSpace(uuid))
	magic := make([]byte, 4)
	if _, err := io.ReadFull(r, magic); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return out, nil
		}
		return out, err
	}
	if string(magic) != string(durableMagic) {
		return out, nil
	}
	offset := uint64(4)
	hdr := make([]byte, goreplication.EventHeaderSize)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return out, nil
			}
			return SegmentGTIDScan{}, err
		}
		eventSize := uint64(binary.LittleEndian.Uint32(hdr[9:13]))
		if eventSize < uint64(goreplication.EventHeaderSize) {
			return out, nil
		}
		rest := eventSize - uint64(goreplication.EventHeaderSize)
		var body []byte
		if rest > 0 {
			body = make([]byte, rest)
			if _, err := io.ReadFull(r, body); err != nil {
				return out, nil
			}
		}
		noteSpan(&out, hdr)
		if goreplication.EventType(hdr[4]) == goreplication.GTID_EVENT && gtidBodyMatches(body, want, seq) {
			when := time.Unix(int64(binary.LittleEndian.Uint32(hdr[0:4])), 0).UTC()
			out.Hit = &GTIDHit{Offset: offset, When: when}
			return out, nil
		}
		offset += eventSize
	}
}

func noteSpan(out *SegmentGTIDScan, hdr []byte) {
	if !spanEventCounts(hdr[4]) {
		return
	}
	ts := binary.LittleEndian.Uint32(hdr[0:4])
	if ts == 0 {
		return
	}
	when := time.Unix(int64(ts), 0).UTC()
	if !out.OK || when.Before(out.First) {
		out.First = when
	}
	if !out.OK || when.After(out.Last) {
		out.Last = when
	}
	out.OK = true
}

func gtidBodyMatches(body []byte, uuid string, seq int64) bool {
	need := 1 + goreplication.SidLength + 8
	if len(body) < need {
		return false
	}
	gotSeq := int64(binary.LittleEndian.Uint64(body[1+goreplication.SidLength : need]))
	if gotSeq != seq {
		return false
	}
	return formatSID(body[1:1+goreplication.SidLength]) == uuid
}

func formatSID(raw []byte) string {
	h := hex.EncodeToString(raw)
	if len(h) != 32 {
		return ""
	}
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
