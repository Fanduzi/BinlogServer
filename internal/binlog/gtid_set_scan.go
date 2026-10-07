// Package binlog provides module-level functionality for binlog.
// input: one binlog segment reader, beginning with the 4-byte magic header
// output: the previous-GTIDs set and every MySQL GTID event in that segment, with the byte offset and event-header time of each event
// pos: read the transactions a segment actually contains so replay can exclude ones a restored backup already has
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"encoding/binary"
	"errors"
	"io"
	"time"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

// errPreviousGTIDs is a previous-GTIDs event whose body did not decode.
var errPreviousGTIDs = errors.New("unreadable previous gtid event")

// GTIDEventRef is one MySQL GTID event inside a segment.
// Offset is the byte where that event starts, the same position
// mysqlbinlog --stop-position excludes. UUID is lowercase.
type GTIDEventRef struct {
	UUID   string
	Seq    int64
	Offset uint64
	When   time.Time
}

// SegmentGTIDLog is every MySQL GTID transaction in one segment.
// Previous is the previous-GTIDs event text when that event was present.
// Previous is the set of transactions that committed before this segment,
// not a transaction stored in the segment. A torn tail keeps the events
// already read. The caller owns r.
type SegmentGTIDLog struct {
	Previous    string
	HasPrevious bool
	Events      []GTIDEventRef
}

// ScanSegmentGTIDs walks one segment and records each GTID event.
// A previous-GTIDs event is recorded once and is not a transaction.
// A segment without the binlog magic returns an empty log.
func ScanSegmentGTIDs(r io.Reader) (SegmentGTIDLog, error) {
	var out SegmentGTIDLog
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
	crc32 := false
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return out, nil
			}
			return SegmentGTIDLog{}, err
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
		eventType := goreplication.EventType(hdr[4])
		switch eventType {
		case goreplication.FORMAT_DESCRIPTION_EVENT:
			if fdChecksum(body) {
				crc32 = true
			}
		case goreplication.PREVIOUS_GTIDS_EVENT:
			text, ok := decodePreviousGTIDs(body, crc32)
			if !ok {
				return SegmentGTIDLog{}, errPreviousGTIDs
			}
			out.HasPrevious = true
			out.Previous = text
		case goreplication.GTID_EVENT:
			if ref, ok := gtidEventRef(body, offset, hdr); ok {
				out.Events = append(out.Events, ref)
			}
		}
		offset += eventSize
	}
}

func fdChecksum(body []byte) bool {
	if len(body) < 57 {
		return false
	}
	fde := &goreplication.FormatDescriptionEvent{}
	if err := fde.Decode(body); err != nil {
		return false
	}
	return fde.ChecksumAlgorithm == goreplication.BINLOG_CHECKSUM_ALG_CRC32
}

func decodePreviousGTIDs(body []byte, crc32 bool) (string, bool) {
	if !crc32 {
		if text, ok := decodePreviousBody(body); ok {
			return text, true
		}
	}
	if len(body) >= goreplication.BinlogChecksumLength {
		trimmed := body[:len(body)-goreplication.BinlogChecksumLength]
		if text, ok := decodePreviousBody(trimmed); ok {
			return text, true
		}
	}
	if crc32 {
		return "", false
	}
	return decodePreviousBody(body)
}

func decodePreviousBody(body []byte) (string, bool) {
	pe := &goreplication.PreviousGTIDsEvent{}
	if err := pe.Decode(body); err != nil {
		return "", false
	}
	return pe.GTIDSets, true
}

func gtidEventRef(body []byte, offset uint64, hdr []byte) (GTIDEventRef, bool) {
	need := 1 + goreplication.SidLength + 8
	if len(body) < need {
		return GTIDEventRef{}, false
	}
	seq := int64(binary.LittleEndian.Uint64(body[1+goreplication.SidLength : need]))
	if seq <= 0 {
		return GTIDEventRef{}, false
	}
	uuid := formatSID(body[1 : 1+goreplication.SidLength])
	if uuid == "" {
		return GTIDEventRef{}, false
	}
	when := time.Unix(int64(binary.LittleEndian.Uint32(hdr[0:4])), 0).UTC()
	return GTIDEventRef{UUID: uuid, Seq: seq, Offset: offset, When: when}, true
}
