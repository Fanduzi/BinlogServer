// Package tasks provides module-level functionality for tasks.
// input: a task id, the scheduler data directory, and the stored checkpoint gtid_set
// output: a copy of the task with storage_alert set when one segment has a stray rotate or a backwards event position (the segment, the earlier segments that still restore, the missing GTIDs, and the restart gtid_set), or when an earlier build wrote a failback under the first stint's names (legacy_failback.go: the files, and whether Start repairs it), or when a source switch recorded transactions as stored that no local segment holds between segments that do (stored_hole.go); the replay file choice without the damaged segment and later ones plus a warning (no files when a problem names no segment); out-of-order GTIDs leave the task unchanged
// pos: surface an already-damaged backup on list and get without a schema change; the verdict itself comes from storageHealth (storage_health.go, ADR 0007)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"strings"

	"binlog_server/internal/binlog"
)

// AttachStorageAlert returns a copy of task with StorageAlert set from the
// storage-health verdict (storage_health.go), using the stored checkpoint
// for the missing set. Out-of-order GTIDs, a hole filled by a later
// segment, and a gap left by retention do not set it. The scheduler's stored
// task is not written. A directory that cannot be read leaves the alert unset.
func (s *Scheduler) AttachStorageAlert(ctx context.Context, task Task) Task {
	if s == nil {
		return task
	}
	if _, ok := taskBinlogDir(s.dataDir, task.ID); !ok {
		return task
	}
	claimed := ""
	if s.checkpointReader != nil && ctx != nil {
		cp, found, err := s.checkpointReader.LoadCheckpoint(ctx, task.ID)
		if err == nil && found {
			claimed = strings.TrimSpace(cp.GTIDSet)
		}
	}
	health := s.storageHealth(task, claimed)
	problem := health.Problem
	switch health.Kind {
	case StorageOK:
		return task
	case StorageLegacyFailback:
		problem.Valid = s.validBefore(task, problem.Segment)
	case StorageDamaged:
		// A task failed by an older build keeps that build's wording until the
		// next Start. Show the current STORAGE_INCONSISTENT text instead.
		if task.State == StateFailed && strings.HasPrefix(strings.TrimSpace(task.LastError), CodeStorageInconsistent+":") {
			task.LastError = CodeStorageInconsistent + ": " + problem.Message
		}
	}
	task.StorageAlert = storageAlertOf(problem)
	return task
}

// StartGTIDText is the task's start gtid_set in GTID start mode and "" otherwise.
func StartGTIDText(task Task) string {
	if task.Start.Mode != StartModeGTID {
		return ""
	}
	return strings.TrimSpace(task.Start.GTIDSet)
}

func storageAlertOf(problem binlog.StorageProblem) *StorageAlert {
	return &StorageAlert{
		Code:           CodeStorageInconsistent,
		Message:        problem.Message,
		Segment:        problem.Segment,
		Detail:         problem.Detail,
		MissingGTIDs:   problem.Missing,
		ValidSegments:  problem.Valid,
		RestartGTIDSet: problem.Restart,
	}
}

// validBefore lists the replay files of task before segment, from disk.
func (s *Scheduler) validBefore(task Task, segment string) []string {
	files, err := listTaskBinlogFilesOnDisk(s.dataDir, task.ID, segmentInventoryLimit)
	if err != nil {
		return nil
	}
	kept := excludeDamagedSegments(files, SelectReplayFiles(files), segment)
	out := make([]string, 0, len(kept))
	for _, file := range kept {
		out = append(out, segmentInventoryBasename(file))
	}
	return out
}

// SelectReplayFilesFor is SelectReplayFiles without the damaged segment and
// every segment after it. warning says what was left out. A task without
// damage gets SelectReplayFiles unchanged and an empty warning.
func (s *Scheduler) SelectReplayFilesFor(task Task, files []BinlogFile) ([]BinlogFile, string) {
	health := s.storageHealth(task, "")
	return health.replayCut(files, SelectReplayFiles(files)), health.ReplayWarning()
}

// excludeDamagedSegments keeps the chosen files that sort before the damaged
// segment in replay order. Any copy of the damaged binlog index, open or
// sealed, is dropped too.
func excludeDamagedSegments(inventory, chosen []BinlogFile, damaged string) []BinlogFile {
	if damaged == "" {
		return chosen
	}
	mark := BinlogFile{FilePath: damaged}
	for _, file := range inventory {
		if segmentInventoryBasename(file) == damaged {
			mark = file
			break
		}
	}
	gen := segmentGenerations(inventory)
	markKey := binlogSegmentKey(mark)
	out := make([]BinlogFile, 0, len(chosen))
	for _, file := range chosen {
		key := binlogSegmentKey(file)
		if key.ok && markKey.ok && segmentIndexKey(key) == segmentIndexKey(markKey) {
			continue
		}
		if !segmentGenerationLess(file, mark, gen) {
			continue
		}
		out = append(out, file)
	}
	return out
}

// ReplayWarning is the replay warning for a task with a storage problem and
// "" otherwise.
func (s *Scheduler) ReplayWarning(task Task) string {
	return s.storageHealth(task, "").ReplayWarning()
}
