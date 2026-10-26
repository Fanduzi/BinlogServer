// Package binlog provides module-level functionality for binlog.
// input: the path of a sealed segment
// output: SealFacts: the first and last event-header time and the SHA-256 of the file bytes, from one streaming read (memory does not grow with the segment); the file is opened through the counted walk seam
// pos: what a seal writes into the catalog row (ADR 0009 phase 1, P1-4: first_event_at, last_event_at, sha256)
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"time"
)

// SealFacts is what a seal records about a sealed segment's bytes.
type SealFacts struct {
	// First and Last are UTC event-header times; HasSpan is false when the
	// file holds no timed event.
	First, Last time.Time
	HasSpan     bool
	// SHA256 is the lowercase hex digest of the whole file.
	SHA256 string
	// Size is the number of bytes the digest covers.
	Size int64
}

// ReadSealFacts reads path once, streaming: the bytes go through the hash
// while the event headers are read, so memory does not grow with the file.
func ReadSealFacts(path string) (SealFacts, error) {
	f, err := Open(path)
	if err != nil {
		return SealFacts{}, err
	}
	defer f.Close()
	h := sha256.New()
	r := io.TeeReader(bufio.NewReaderSize(f, 256<<10), h)
	first, last, ok, err := EventTimeSpan(r)
	if err != nil {
		return SealFacts{}, err
	}
	// A torn tail stops the event walk early; the digest covers every byte.
	if _, err := io.Copy(io.Discard, r); err != nil {
		return SealFacts{}, err
	}
	info, err := f.Stat()
	if err != nil {
		return SealFacts{}, err
	}
	out := SealFacts{SHA256: hex.EncodeToString(h.Sum(nil)), Size: info.Size()}
	if ok {
		out.First, out.Last, out.HasSpan = first.UTC(), last.UTC(), true
	}
	return out, nil
}
