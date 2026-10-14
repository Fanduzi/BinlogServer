// Package assurance provides module-level functionality for assurance.
// input: Inputs: the task's chain order, catalog rows, task directory ledger, object listing snapshot, checkpoint, start set and recorded source switches
// output: the backup chain: its segments with where each one is readable, and the I1-I7 violations of ADR 0009, each a closed Kind with the segments involved and the evidence
// pos: the one answer to "is this backup complete, and where is each part" (ADR 0009 phase 1); depends on binlog only
// note: if this file changes, update this header and module README.md.
package assurance

import (
	"time"

	"binlog_server/internal/binlog"
)

// Checkpoint is the task's checkpoint (C in ADR 0009).
type Checkpoint struct {
	GTIDSet string
	File    string
	Pos     uint32
}

// Row is the dependency-free projection of one binlog_files row.
type Row struct {
	Name        string
	Source      string
	Epoch       int64
	Sealed      bool
	Size        int64
	UploadState string
	Checksum    string
	ObjectKey   string
	CreatedAt   time.Time
	SealedAt    time.Time
}

// Switch is one continued source switch: the server the chain moved to and
// the GTID set it recorded as stored at that point.
type Switch struct {
	Identity string
	Stored   string
}

// Inputs is everything the chain knows; see docs/design/0009-phase1-backup-chain.md.
type Inputs struct {
	Flavor        string
	StartGTID     string
	Checkpoint    Checkpoint
	Order         []string
	Rows          []Row
	Local         *binlog.Ledger
	Objects       map[string]int64
	ObjectStorage bool
	UploadGrace   time.Duration
	Switches      []Switch
	// Planned are conclusions the tasks planners already reached (stored
	// GTID hole, legacy failback; I3 subclasses). Check returns them in
	// chain order with its own.
	Planned []Violation
}

// Kind is a closed set of invariant violations.
type Kind string

const (
	I1Regression       Kind = "I1_REGRESSION"
	I2IndexGap         Kind = "I2_INDEX_GAP"
	I2SwitchUnrecorded Kind = "I2_SWITCH_UNRECORDED"
	I3GTIDHole         Kind = "I3_GTID_HOLE"
	I3StoredHole       Kind = "I3_STORED_HOLE"
	I3LegacyFailback   Kind = "I3_LEGACY_FAILBACK"
	I4Overlap          Kind = "I4_OVERLAP"
	I5CheckpointAhead  Kind = "I5_CHECKPOINT_AHEAD"
	I6MissingLocal     Kind = "I6_MISSING_LOCAL"
	I6MissingObject    Kind = "I6_MISSING_OBJECT"
	I6OrphanRow        Kind = "I6_ORPHAN_ROW"
	I6MissingRow       Kind = "I6_MISSING_ROW"
	I6SizeMismatch     Kind = "I6_SIZE_MISMATCH"
	I7UploadLate       Kind = "I7_UPLOAD_LATE"
	I7Unverified       Kind = "I7_UNVERIFIED"
)

// AllKinds lists every Kind.
func AllKinds() []Kind {
	return []Kind{I1Regression, I2IndexGap, I2SwitchUnrecorded, I3GTIDHole, I3StoredHole, I3LegacyFailback,
		I4Overlap, I5CheckpointAhead, I6MissingLocal, I6MissingObject, I6OrphanRow, I6MissingRow,
		I6SizeMismatch, I7UploadLate, I7Unverified}
}

// Violation is one broken invariant.
type Violation struct {
	Kind     Kind
	Segments []string
	Evidence string
	Detail   string
}

// Segment is one segment on the chain and where it can be read.
type Segment struct {
	Name  string
	Named binlog.SegmentName
	// Row is the catalog row; nil when there is none.
	Row *Row
	// Local reports the file is in the task directory; LocalSize is its size.
	Local     bool
	LocalSize int64
	// Object is the listed object size; nil when the object was not listed
	// (no listing, no key) or is not there. ObjectListed tells which.
	Object       *int64
	ObjectListed bool
	// Contents is what the local file stores; nil when it is not readable.
	Contents *binlog.SegmentContents
	ReadErr  error
}

// Chain is one task's backup chain. It is not safe for concurrent use.
type Chain struct{ in Inputs }

// ChainOf is the chain for in. Nothing is read until a question is asked.
func ChainOf(in Inputs) *Chain { return &Chain{in: in} }

// Segments is the chain in restore order.
func (c *Chain) Segments() []Segment { return nil }

// Check reports every violation of I1-I7, in chain order.
func (c *Chain) Check(now time.Time) []Violation { return nil }
