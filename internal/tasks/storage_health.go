// Package tasks provides module-level functionality for tasks.
// input: a task, the checkpoint gtid_set it claims (or "" when the caller does not need the missing set), the scheduler data directory and the task's source-switch events
// output: one storage-health verdict per task directory: ok, damaged (a stray rotate or a backwards event position, from the segment ledger), a failback an earlier build wrote under the first stint's names (legacy_failback.go), or a GTID hole a source switch recorded as stored (stored_hole.go); the classes are checked in that order and the first one found wins; the verdict carries the StorageProblem every consumer shows and its replay warning
// pos: adapter over assurance.Chain.Health (ADR 0009 phase 1, P1-3): tasks supplies the ledger, checkpoint, start set and its planners (legacy failback, then stored hole) as lazy Plan; list/get alerts, /replay, /window and the replay warning all ask it; the replay cut is assurance.KeepBefore
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"strings"

	"binlog_server/internal/assurance"
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
	chain := assurance.ChainOf(assurance.Inputs{
		Flavor:     task.Source.Flavor,
		StartGTID:  StartGTIDText(task),
		Checkpoint: assurance.Checkpoint{GTIDSet: checkpointGTID},
		Local:      ledger,
		Plan:       func() []assurance.Violation { return s.plannedProblems(task, dir, ledger) },
	})
	v, found := chain.Health()
	if !found || v.Problem == nil {
		return StorageHealth{}
	}
	kind := StorageDamaged
	switch v.Kind {
	case assurance.I3LegacyFailback:
		kind = StorageLegacyFailback
	case assurance.I3StoredHole:
		kind = StorageStoredHole
	}
	return StorageHealth{Kind: kind, Problem: *v.Problem}
}

// plannedProblems is what the tasks planners conclude, in verdict order: a
// legacy failback, else a stored GTID hole. The stored-hole planner only
// runs when there is no legacy failback.
func (s *Scheduler) plannedProblems(task Task, dir string, ledger *binlog.Ledger) []assurance.Violation {
	if legacy := s.legacyFailbackIn(task, dir, ledger); legacy.found() {
		p := legacyProblem(legacy)
		return []assurance.Violation{{Kind: assurance.I3LegacyFailback, Segments: []string{p.Segment}, Detail: p.Message, Problem: &p}}
	}
	if p, found := s.storedHoleIn(task, dir, ledger); found {
		return []assurance.Violation{{Kind: assurance.I3StoredHole, Segments: []string{p.Segment}, Evidence: p.Missing, Detail: p.Message, Problem: &p}}
	}
	return nil
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
	return keepBeforeSegment(files, chosen, h.Problem.Segment)
}
