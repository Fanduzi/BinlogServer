// Package tasks provides module-level functionality for tasks.
// input: a task, the checkpoint gtid_set it claims (or "" when the caller does not need the missing set), the scheduler data directory and the task's source-switch events
// output: one storage-health verdict per task directory: ok, damaged (a stray rotate or a backwards event position, from the segment ledger), unreadable (a sealed segment with a corrupt event size, cut short, or not its size at seal, #381; reported as a SEGMENT_UNREADABLE finding), a failback an earlier build wrote under the first stint's names (legacy_failback.go), or a GTID hole a source switch recorded as stored (stored_hole.go), or a sealed source file held twice in the directory (a SEGMENT_OVERLAP finding, #370); the classes are checked in that order and the first one found wins; the verdict carries the StorageProblem every consumer shows and its replay warning; the two managed kinds are reported only while the health evaluator does not own them (reportUnmanaged, P2-3), and markHealthDirty is the trigger entry for the evaluator
// pos: adapter over assurance.Chain.Health (ADR 0009 phase 1, P1-3): tasks supplies the ledger, checkpoint, start set, the replay order (SelectReplayFiles over ListFiles, for I4) and its planners (legacy failback, then stored hole) as lazy Plan; list/get alerts, /replay, /window and the replay warning all ask it; the replay cut is assurance.KeepBefore
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
	// StorageUnreadable: a sealed segment has a corrupt event size (above
	// 1 GiB or past the end of the file; binlog.Ledger.Unreadable). Replay
	// and the window stop before it. Read-only; Start is not refused.
	StorageUnreadable
	// StorageOverlap: the task directory holds a sealed source file twice
	// (for example mysql-bin.000006 and mysql-bin.000006.sealed.e3) with the
	// same events, or one copy cannot be read (I4_OVERLAP, #370). Replay and
	// the window stop before both copies. Read-only; Start is not refused.
	StorageOverlap
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
		Order:      s.replayOrder(task.ID),
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
	case assurance.I1Unreadable:
		kind = StorageUnreadable
		s.reportUnmanaged(Finding{TaskID: task.ID, Kind: FindingSegmentUnreadable, Subject: v.Problem.Segment, Severity: FindingCritical, Detail: v.Problem.Message})
	case assurance.I4Overlap:
		kind = StorageOverlap
		s.reportUnmanaged(Finding{TaskID: task.ID, Kind: FindingSegmentOverlap, Subject: v.Problem.Segment, Severity: FindingCritical, Detail: v.Problem.Message})
	}
	return StorageHealth{Kind: kind, Problem: *v.Problem}
}

// reportUnmanaged is the legacy Report for a kind the health evaluator
// manages since P2-3: it writes only while the evaluator does not own the
// kind (off, shadow, or a store without migration 000009), so a row has one
// writer (design §5.6).
func (s *Scheduler) reportUnmanaged(f Finding) {
	if s.healthEval.Load().ownsManaged() {
		return
	}
	s.findings.report(f)
}

// resolveUnmanaged is the matching legacy Resolve.
func (s *Scheduler) resolveUnmanaged(taskID string, kind FindingKind, subject string) {
	if s.healthEval.Load().ownsManaged() {
		return
	}
	s.findings.resolve(taskID, kind, subject, "")
}

// markHealthDirty tells the evaluator, when there is one, that the task's
// facts changed. Never blocks; safe with s.mu held.
func (s *Scheduler) markHealthDirty(taskID string, r EvalReason) {
	if s == nil {
		return
	}
	s.healthEval.Load().MarkDirty(taskID, r)
}

// replayOrder is the base names of the segments /replay would return for
// the task before any storage cut (SelectReplayFiles over ListFiles), the
// chain order the I4 check reads. A listing error leaves it empty; /replay
// and /window fail on the same listing themselves.
func (s *Scheduler) replayOrder(taskID string) []string {
	files, err := s.ListFiles(taskID, segmentInventoryLimit)
	if err != nil {
		return nil
	}
	chosen := SelectReplayFiles(files)
	out := make([]string, 0, len(chosen))
	for _, file := range chosen {
		if name := segmentInventoryBasename(file); name != "" {
			out = append(out, name)
		}
	}
	return out
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
	case StorageOverlap:
		return p.Message + " (STORAGE_INCONSISTENT)"
	case StorageUnreadable:
		return "Segment " + p.Segment + " is not readable (" + p.Reason + "). Replay stops before it: that segment and every later one are left out. " + p.Message
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
