// Package binlog provides module-level functionality for binlog.
// input: one binlog segment reader, beginning with the 4-byte magic header
// output: the previous-GTIDs set and every MySQL GTID event in that segment, with the byte offset and event-header time of each event; a damaged event size past the file or above 1 GiB returns ErrCorruptEventSize without allocating (#358); in a sealed segment a short event size, a short body or a CRC32 trailer that does not match (ErrEventChecksum, wrapped in ErrCorruptEventSize) is also not readable (#380 QA)
// pos: read the transactions a segment actually contains so replay can exclude ones a restored backup already has
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"time"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

// errPreviousGTIDs is a previous-GTIDs event whose body did not decode.
var errPreviousGTIDs = errors.New("unreadable previous gtid event")

// ErrCorruptEventSize means an event header's size is larger than maxEventBody,
// or runs past the end of a sealed segment. Callers must not allocate that
// size. A size past the end of an open segment is a torn tail, not this error.
var ErrCorruptEventSize = errors.New("corrupt binlog event size")

// maxEventBody is 1 GiB: above MySQL's maximum max_allowed_packet, and far
// above any real binlog event. A damaged size field used to ask for ~4 GiB
// and crash the process (#358).
const maxEventBody = 1 << 30

// makeEventBody allocates an event body. Tests replace it to refuse huge sizes
// without OOMing the host.
var makeEventBody = func(n int) []byte { return make([]byte, n) }

// eventBodyBytes is the body length after the 19-byte header. -1 is a torn
// tail (size below the header, or past remaining in an unsealed segment).
// ErrCorruptEventSize is a size above maxEventBody, or past remaining in a
// sealed segment. remaining < 0 means the length is unknown.
func eventBodyBytes(eventSize uint64, remaining int64, sealed bool) (int, error) {
	hdr := uint64(goreplication.EventHeaderSize)
	if eventSize < hdr {
		return -1, nil // torn tail: caller stops with what it has
	}
	rest := eventSize - hdr
	if rest > maxEventBody {
		return 0, ErrCorruptEventSize
	}
	if remaining >= 0 && int64(rest) > remaining {
		if sealed {
			return 0, ErrCorruptEventSize
		}
		return -1, nil // torn: stop quietly
	}
	return int(rest), nil
}

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

// scanSegmentGTIDs walks one segment and records each GTID event.
// A previous-GTIDs event is recorded once and is not a transaction.
// A segment without the binlog magic returns an empty log.
func scanSegmentGTIDs(r io.Reader) (SegmentGTIDLog, error) {
	return scanSegmentGTIDsBounded(r, readerRemaining(r), false)
}

// readerRemaining is how many bytes r still holds when r can say (a file or
// an in-memory reader), else -1. Event sizes are checked against it so a
// wrong size is never allocated past the data that exists.
func readerRemaining(r io.Reader) int64 {
	switch v := r.(type) {
	case interface{ Len() int }:
		return int64(v.Len())
	case *os.File:
		info, err := v.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return -1
		}
		at, err := v.Seek(0, io.SeekCurrent)
		if err != nil || at > info.Size() {
			return -1
		}
		return info.Size() - at
	}
	return -1
}

// scanSegmentGTIDsBounded scans with remaining bytes left in r (-1 unknown).
// sealed makes a size past remaining ErrCorruptEventSize instead of a torn tail.
func scanSegmentGTIDsBounded(r io.Reader, remaining int64, sealed bool) (SegmentGTIDLog, error) {
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
	if remaining >= 0 {
		remaining -= 4 // magic already read
	}
	offset := uint64(4)
	hdr := make([]byte, goreplication.EventHeaderSize)
	hasCRC := false
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return out, nil
			}
			return SegmentGTIDLog{}, err
		}
		if remaining >= 0 {
			remaining -= int64(goreplication.EventHeaderSize)
		}
		eventSize := uint64(binary.LittleEndian.Uint32(hdr[9:13]))
		if eventSize < uint64(goreplication.EventHeaderSize) {
			if sealed {
				return SegmentGTIDLog{}, ErrCorruptEventSize
			}
			return out, nil
		}
		rest, err := eventBodyBytes(eventSize, remaining, sealed)
		if err != nil {
			return out, err
		}
		if rest < 0 {
			return out, nil
		}
		var body []byte
		if rest > 0 {
			body = makeEventBody(rest)
			if _, err := io.ReadFull(r, body); err != nil {
				if sealed {
					return SegmentGTIDLog{}, ErrCorruptEventSize
				}
				return out, nil
			}
		}
		if remaining >= 0 {
			remaining -= int64(rest)
		}
		eventType := goreplication.EventType(hdr[4])
		if sealed && (hasCRC || (eventType == goreplication.FORMAT_DESCRIPTION_EVENT && fdChecksum(body))) && !eventCRCOK(hdr, body) {
			return SegmentGTIDLog{}, fmt.Errorf("%w: %w at offset %d", ErrCorruptEventSize, ErrEventChecksum, offset)
		}
		switch eventType {
		case goreplication.FORMAT_DESCRIPTION_EVENT:
			if fdChecksum(body) {
				hasCRC = true
			}
		case goreplication.PREVIOUS_GTIDS_EVENT:
			text, ok := decodePreviousGTIDs(body, hasCRC)
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

// ErrEventChecksum is a sealed segment event whose CRC32 trailer does not
// match its bytes (binlog_checksum=CRC32). It is always wrapped in
// ErrCorruptEventSize, so every reader that treats that error as "not
// readable" (/window, /replay, the chain's I1) agrees on it.
var ErrEventChecksum = errors.New("binlog event checksum mismatch")

// eventCRCOK checks the CRC32 trailer of one event: the IEEE CRC of the
// header and body without the last four bytes.
func eventCRCOK(hdr, body []byte) bool {
	if len(body) < goreplication.BinlogChecksumLength {
		return false
	}
	n := len(body) - goreplication.BinlogChecksumLength
	sum := crc32.Update(crc32.ChecksumIEEE(hdr), crc32.IEEETable, body[:n])
	return sum == binary.LittleEndian.Uint32(body[n:])
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
