// Package binlog provides module-level functionality for binlog.
// input: one binlog segment reader, beginning with the 4-byte magic header
// output: every event's start and end offset, the copied-event time, and the MySQL GTID when the event is one; previous-GTIDs text; a sealed segment with a bad size or CRC32 returns ErrCorruptEventSize
// pos: the one pass coverage uses to place a time or GTID inside a segment (ADR 0009 phase 4)
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

// CoverEvent is one event in a segment.
// Offset is the byte where it starts, the same number mysqlbinlog prints
// after "# at". End is the next byte. UUID and Seq are set only for a MySQL
// GTID event. Counts is false for the format description and previous-GTIDs
// events: those timestamps are when the source file was created.
type CoverEvent struct {
	When   time.Time
	Offset uint64
	End    uint64
	UUID   string
	Seq    int64
	Counts bool
}

// CoverScan is one pass over a segment. End is the byte after the last
// complete event. A torn tail on an open segment keeps that prefix.
type CoverScan struct {
	Events      []CoverEvent
	Previous    string
	HasPrevious bool
	End         uint64
}

// ScanCover reads one segment. sealed reports a size past the reader or a
// CRC32 mismatch as ErrCorruptEventSize, the same rule as a sealed GTID scan.
// An open segment treats a torn tail as the end of the complete prefix.
// The caller owns r.
func ScanCover(r io.Reader, sealed bool) (CoverScan, error) {
	return scanCover(r, readerRemaining(r), sealed)
}

func scanCover(r io.Reader, remaining int64, sealed bool) (CoverScan, error) {
	var out CoverScan
	magic := make([]byte, 4)
	if n, err := io.ReadFull(r, magic); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			if sealed {
				return CoverScan{}, segmentCutShort("it holds %d bytes, fewer than the binlog magic", n)
			}
			return out, nil
		}
		return out, err
	}
	if string(magic) != string(durableMagic) {
		return out, nil
	}
	if remaining >= 0 {
		remaining -= 4
	}
	offset := uint64(4)
	hdr := make([]byte, goreplication.EventHeaderSize)
	hasCRC := false
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if err == io.ErrUnexpectedEOF && sealed {
				return CoverScan{}, segmentCutShort("it ends inside an event header at offset %d", offset)
			}
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				out.End = offset
				return out, nil
			}
			return CoverScan{}, err
		}
		if remaining >= 0 {
			remaining -= int64(goreplication.EventHeaderSize)
		}
		eventSize := uint64(binary.LittleEndian.Uint32(hdr[9:13]))
		if eventSize < uint64(goreplication.EventHeaderSize) {
			if sealed {
				return CoverScan{}, ErrCorruptEventSize
			}
			out.End = offset
			return out, nil
		}
		rest, err := eventBodyBytes(eventSize, remaining, sealed)
		if err != nil {
			return CoverScan{}, err
		}
		if rest < 0 {
			out.End = offset
			return out, nil
		}
		var body []byte
		if rest > 0 {
			body = makeEventBody(rest)
			if _, err := io.ReadFull(r, body); err != nil {
				if sealed {
					return CoverScan{}, ErrCorruptEventSize
				}
				out.End = offset
				return out, nil
			}
		}
		if remaining >= 0 {
			remaining -= int64(rest)
		}
		eventType := goreplication.EventType(hdr[4])
		if sealed && (hasCRC || (eventType == goreplication.FORMAT_DESCRIPTION_EVENT && fdChecksum(body))) && !eventCRCOK(hdr, body) {
			return CoverScan{}, fmt.Errorf("%w: %w at offset %d", ErrCorruptEventSize, ErrEventChecksum, offset)
		}
		ev := CoverEvent{Offset: offset, End: offset + eventSize, Counts: spanEventCounts(hdr[4])}
		if ts := binary.LittleEndian.Uint32(hdr[0:4]); ts != 0 {
			ev.When = time.Unix(int64(ts), 0).UTC()
		}
		switch eventType {
		case goreplication.FORMAT_DESCRIPTION_EVENT:
			if fdChecksum(body) {
				hasCRC = true
			}
		case goreplication.PREVIOUS_GTIDS_EVENT:
			text, ok := decodePreviousGTIDs(body, hasCRC)
			if !ok {
				return CoverScan{}, errPreviousGTIDs
			}
			out.HasPrevious = true
			out.Previous = text
		case goreplication.GTID_EVENT:
			if ref, ok := gtidEventRef(body, offset, hdr); ok {
				ev.UUID = ref.UUID
				ev.Seq = ref.Seq
				ev.When = ref.When
			}
		}
		out.Events = append(out.Events, ev)
		offset += eventSize
	}
}
