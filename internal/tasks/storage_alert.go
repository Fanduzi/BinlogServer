// Package tasks provides module-level functionality for tasks.
// input: a task id, the scheduler data directory, and the stored checkpoint gtid_set
// output: a copy of the task with storage_alert set when one segment has a stray rotate or a backwards event position (the segment, the earlier segments that still restore, the missing GTIDs, and the restart gtid_set); the replay file choice without the damaged segment and later ones plus a warning; out-of-order GTIDs leave the task unchanged
// pos: surface an already-damaged backup on list and get without a schema change
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"strings"

	"binlog_server/internal/binlog"
)

// AttachStorageAlert returns a copy of task with StorageAlert set when one
// segment rotates to its own or an older file, or an event position goes
// backwards inside a segment. Out-of-order GTIDs, a hole filled by a later
// segment, and a gap left by retention do not set it. The scheduler's stored
// task is not written. A directory that cannot be read leaves the alert unset.
func (s *Scheduler) AttachStorageAlert(ctx context.Context, task Task) Task {
	if s == nil {
		return task
	}
	dir, ok := taskBinlogDir(s.dataDir, task.ID)
	if !ok {
		return task
	}
	claimed := ""
	if s.checkpointReader != nil && ctx != nil {
		cp, found, err := s.checkpointReader.LoadCheckpoint(ctx, task.ID)
		if err == nil && found {
			claimed = strings.TrimSpace(cp.GTIDSet)
		}
	}
	problem, found := binlog.DetectStorageProblem(dir, claimed, StartGTIDText(task), task.Source.Flavor)
	if !found {
		return task
	}
	task.StorageAlert = storageAlertOf(problem)
	// A task failed by an older build keeps that build's wording until the
	// next Start. Show the current STORAGE_INCONSISTENT text instead.
	if task.State == StateFailed && strings.HasPrefix(strings.TrimSpace(task.LastError), CodeStorageInconsistent+":") {
		task.LastError = CodeStorageInconsistent + ": " + problem.Message
	}
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

// storageProblem scans the task's local segments for the re-dump damage
// signature. It does not read the checkpoint, so Missing is empty.
func (s *Scheduler) storageProblem(task Task) (binlog.StorageProblem, bool) {
	if s == nil {
		return binlog.StorageProblem{}, false
	}
	dir, ok := taskBinlogDir(s.dataDir, task.ID)
	if !ok {
		return binlog.StorageProblem{}, false
	}
	return binlog.DetectStorageProblem(dir, "", StartGTIDText(task), task.Source.Flavor)
}

// SelectReplayFilesFor is SelectReplayFiles without the damaged segment and
// every segment after it. warning says what was left out. A task without
// damage gets SelectReplayFiles unchanged and an empty warning.
func (s *Scheduler) SelectReplayFilesFor(task Task, files []BinlogFile) ([]BinlogFile, string) {
	chosen := SelectReplayFiles(files)
	problem, found := s.storageProblem(task)
	if !found {
		return chosen, ""
	}
	return excludeDamagedSegments(files, chosen, problem.Segment), replayDamageWarning(problem)
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

// ReplayWarning is the replay warning for a task with a damaged segment and
// "" otherwise.
func (s *Scheduler) ReplayWarning(task Task) string {
	problem, found := s.storageProblem(task)
	if !found {
		return ""
	}
	return replayDamageWarning(problem)
}

func replayDamageWarning(problem binlog.StorageProblem) string {
	msg := "Segment " + problem.Segment + " is damaged (STORAGE_INCONSISTENT). Replay stops before it: that segment and every later one are left out."
	if problem.Restart != "" {
		msg += " Take the transactions after these files from a new task started with gtid_set " + problem.Restart + "."
	}
	return msg
}
