// Package binlog provides module-level functionality for binlog.
// input: a reader of one binlog segment, beginning with the 4-byte magic header
// output: the first and last non-zero event-header timestamps in that segment, ignoring the format description and previous-GTIDs headers
// pos: time span used to choose which replay segment covers a point-in-time window
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"encoding/binary"
	"io"
	"time"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

// EventTimeSpan reads one binlog segment and returns the earliest and latest
// non-zero event-header timestamps of the copied stream. A timestamp of 0 is
// skipped. The format description and the previous-GTIDs event are skipped
// too: those timestamps are when the source file was created, which is before
// the first event a mid-file backup actually copied. ok is false when the
// reader has no magic header or no remaining timed event. A torn tail keeps
// the timestamps of the complete events already read. The caller owns the reader.
func EventTimeSpan(r io.Reader) (first, last time.Time, ok bool, err error) {
	magic := make([]byte, 4)
	if _, err := io.ReadFull(r, magic); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return time.Time{}, time.Time{}, false, nil
		}
		return time.Time{}, time.Time{}, false, err
	}
	if string(magic) != string(durableMagic) {
		return time.Time{}, time.Time{}, false, nil
	}
	hdr := make([]byte, goreplication.EventHeaderSize)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return first, last, ok, nil
			}
			return time.Time{}, time.Time{}, false, err
		}
		eventSize := int64(binary.LittleEndian.Uint32(hdr[9:13]))
		if eventSize < int64(goreplication.EventHeaderSize) {
			return first, last, ok, nil
		}
		rest := eventSize - int64(goreplication.EventHeaderSize)
		if rest > 0 {
			if _, err := io.CopyN(io.Discard, r, rest); err != nil {
				return first, last, ok, nil
			}
		}
		if !spanEventCounts(hdr[4]) {
			continue
		}
		ts := binary.LittleEndian.Uint32(hdr[0:4])
		if ts == 0 {
			continue
		}
		when := time.Unix(int64(ts), 0).UTC()
		if !ok || when.Before(first) {
			first = when
		}
		if !ok || when.After(last) {
			last = when
		}
		ok = true
	}
}

// spanEventCounts reports whether an event header is a copied stream time.
// The format description and previous-GTIDs event record source-file creation.
func spanEventCounts(eventType byte) bool {
	switch goreplication.EventType(eventType) {
	case goreplication.FORMAT_DESCRIPTION_EVENT, goreplication.PREVIOUS_GTIDS_EVENT:
		return false
	default:
		return true
	}
}
