// Package tasks provides module-level functionality for tasks.
// input: a task, the checkpoint gtid_set it claims (or "" when the caller does not need the missing set), the scheduler data directory and the task's source-switch events
// output: one storage-health verdict per task directory: ok, damaged (a stray rotate or a backwards event position, from the segment ledger), a failback an earlier build wrote under the first stint's names (legacy_failback.go), or a GTID hole a source switch recorded as stored (stored_hole.go); the classes are checked in that order and the first one found wins; the verdict carries the StorageProblem every consumer shows and its replay warning
// pos: the one storage detector: list/get alerts, /replay, /window and the replay warning all ask it; one segment-ledger listing per question (ADR 0007)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"strings"

	"binlog_server/internal/binlog"
)

// StorageHealthKind is the class of a storage-health verdict.
type StorageHealthKind int

const (
	// StorageOK: nothing found, or the directory cannot be read.
	StorageOK StorageHealthKind = iota
	// StorageDamaged: a segment rotates to its own or an older file, or an
	// event position goes backwards (binlog.Ledger.Damage). Start refuses.
	StorageDamaged
	// StorageLegacyFailback: an earlier build reused the first stint's file
	// names after a failback. Start repairs it when it can.
	StorageLegacyFailback
	// StorageStoredHole: a source switch recorded transactions as stored
	// that no local segment holds, between segments that do. Read-only.
	StorageStoredHole
)

// StorageHealth is the verdict for one task directory. Problem is set for
// every kind but StorageOK; its Segment is where replay and the window stop.
type StorageHealth struct {
	Kind    StorageHealthKind
	Problem binlog.StorageProblem
}

// Found reports a verdict other than StorageOK.
func (h StorageHealth) Found() bool { return h.Kind != StorageOK }

// storageHealth is the verdict for task. checkpointGTID only fills the
// damaged class's Missing set; it never changes the kind.
func (s *Scheduler) storageHealth(task Task, checkpointGTID string) StorageHealth {
	if s == nil {
		return StorageHealth{}
	}
	s.mu.Lock()
	dataDir := s.dataDir
	s.mu.Unlock()
	dir, ok := taskBinlogDir(dataDir, task.ID)
	if !ok {
		return StorageHealth{}
	}
	ledger := binlog.LedgerAt(dir)
	if problem, found := ledger.Damage(checkpointGTID, StartGTIDText(task), task.Source.Flavor); found {
		return StorageHealth{Kind: StorageDamaged, Problem: problem}
	}
	if legacy := s.legacyFailbackIn(task, dir, ledger); legacy.found() {
		return StorageHealth{Kind: StorageLegacyFailback, Problem: legacyProblem(legacy)}
	}
	if problem, found := s.storedHoleIn(task, dir, ledger); found {
		return StorageHealth{Kind: StorageStoredHole, Problem: problem}
	}
	return StorageHealth{}
}

// ReplayWarning is what /replay says about the verdict, "" when it is ok.
func (h StorageHealth) ReplayWarning() string {
	p := h.Problem
	switch h.Kind {
	case StorageStoredHole:
		return "Segment " + p.Segment + " follows a GTID hole (STORAGE_INCONSISTENT): " + p.Missing + " is stored in no segment. Replay stops before it: that segment and every later one are left out. " + p.Message
	case StorageLegacyFailback:
		return "Segment " + p.Segment + " is in a failback written by an earlier build (STORAGE_INCONSISTENT). Replay stops before it: that segment and every later one are left out. " + p.Message
	case StorageDamaged:
		msg := "Segment " + p.Segment + " is damaged (STORAGE_INCONSISTENT). Replay stops before it: that segment and every later one are left out."
		if p.Restart != "" {
			msg += " Take the transactions after these files from a new task started with gtid_set " + p.Restart + "."
		}
		return msg
	}
	return ""
}

// replayCut is the files replay keeps for the verdict: SelectReplayFiles
// without the problem segment and every later one, nothing when the problem
// names no segment, and chosen unchanged when the verdict is ok.
func (h StorageHealth) replayCut(files, chosen []BinlogFile) []BinlogFile {
	if !h.Found() {
		return chosen
	}
	if strings.TrimSpace(h.Problem.Segment) == "" {
		return []BinlogFile{}
	}
	return excludeDamagedSegments(files, chosen, h.Problem.Segment)
}
