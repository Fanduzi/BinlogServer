// Package binlog provides module-level functionality for binlog.
// input: a reader of one binlog segment, beginning with the 4-byte magic header
// output: the first and last non-zero event-header timestamps in that segment
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
// non-zero event-header timestamps. A timestamp of 0 is skipped. ok is false
// when the reader has no magic header or no timed event. A torn tail keeps the
// timestamps of the complete events already read. The caller owns the reader.
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
