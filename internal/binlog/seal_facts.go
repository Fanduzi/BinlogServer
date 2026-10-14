// Package binlog provides module-level functionality for binlog.
// input: the path of a sealed segment
// output: SealFacts: the first and last event-header time and the SHA-256 of the file bytes, from one read
// pos: what a seal writes into the catalog row (ADR 0009 phase 1, P1-4: first_event_at, last_event_at, sha256)
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
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
}

// ReadSealFacts reads path once.
func ReadSealFacts(path string) (SealFacts, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return SealFacts{}, err
	}
	sum := sha256.Sum256(raw)
	out := SealFacts{SHA256: hex.EncodeToString(sum[:])}
	first, last, ok, err := EventTimeSpan(bytes.NewReader(raw))
	if err != nil {
		return SealFacts{}, err
	}
	if ok {
		out.First, out.Last, out.HasSpan = first.UTC(), last.UTC(), true
	}
	return out, nil
}
