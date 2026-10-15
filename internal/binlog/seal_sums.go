// Package binlog provides module-level functionality for binlog.
// input: a sealed segment's directory, base name, size and sha256 at seal
// output: stub of the seal checksum ledger (P2-3 QA red commit): records nothing
// pos: ADR 0009 P2-3 rehash source (#380 QA)
// note: if this file changes, update this header and module README.md.
package binlog

import "errors"

// SealSumsFileName is the ledger file in a task directory.
const SealSumsFileName = ".seal-sha256"

// SealSum is one segment's checksum and size at seal.
type SealSum struct {
	SHA256 string
	Size   int64
}

// ErrEventChecksum is a sealed event whose CRC32 does not match.
var ErrEventChecksum = errors.New("binlog event checksum mismatch")

// RecordSealSum is not implemented yet.
func RecordSealSum(dir, name string, size int64, sha string) error { return nil }

// LoadSealSums is not implemented yet.
func LoadSealSums(dir string) (map[string]SealSum, error) { return map[string]SealSum{}, nil }

func compactSealSums(dir string) error { return nil }

// RecordSealFacts is not implemented yet.
func RecordSealFacts(path string, f SealFacts) error { return nil }
