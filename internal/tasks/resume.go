// Package tasks provides module-level functionality for tasks.
// input: task id, data dir, KeepLocalSegments, epoch, and an optional stored checkpoint
// output: the file, pos, and gtid_set the next Start continues from when one exists
// pos: resume identity shared by the replication runner and GET /api/tasks/{id}/checkpoint
// note: if this file changes, update this header and module README.md.
package tasks

import "binlog_server/internal/binlog"

// NextResumePosition is the position the next Start uses instead of the
// configured start. A local complete event wins unless the task keeps adopted
// segments. A stored checkpoint is used when that event is absent. Epoch
// greater than 1 rewinds that checkpoint to position 4, the same takeover
// rule the runner applies to the epoch it passes in. gtid_set is copied when
// the file and pos are the stored checkpoint's. This does not contact the source.
func NextResumePosition(dataDir string, task Task, checkpoint binlog.Checkpoint, checkpointOK bool) (binlog.Checkpoint, bool) {
	if !task.KeepLocalSegments {
		if file, pos, ok := binlog.DurableResume(dataDir, task.ID); ok {
			out := binlog.Checkpoint{File: file, Pos: pos}
			if checkpointOK && checkpoint.File == file && checkpoint.Pos == pos {
				out.GTIDSet = checkpoint.GTIDSet
				out.UpdatedAt = checkpoint.UpdatedAt
			}
			return out, true
		}
	}
	if !checkpointOK || checkpoint.File == "" || checkpoint.Pos == 0 {
		return binlog.Checkpoint{}, false
	}
	pos := checkpoint.Pos
	if task.Epoch > 1 && pos > 4 {
		pos = 4
	}
	return binlog.Checkpoint{
		File:      checkpoint.File,
		Pos:       pos,
		GTIDSet:   checkpoint.GTIDSet,
		UpdatedAt: checkpoint.UpdatedAt,
	}, true
}
