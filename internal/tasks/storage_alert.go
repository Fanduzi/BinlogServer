// Package tasks provides module-level functionality for tasks.
// input: a task id, the scheduler data directory, and the stored checkpoint gtid_set
// output: a copy of the task with storage_alert set when one segment shows a stale re-dump; a gap between files leaves the task unchanged
// pos: surface an already-damaged backup on list and get without a schema change
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"strings"

	"binlog_server/internal/binlog"
)

// AttachStorageAlert returns a copy of task with StorageAlert set when one
// segment rotates to its own or an older file, a position or GTID goes
// backwards inside a segment, or a GTID hole inside a segment is claimed by
// the checkpoint. A gap left by retention or expiry does not set it. The
// scheduler's stored task is not written. A directory that cannot be read
// leaves the alert unset.
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
	problem, found := binlog.DetectStorageProblem(dir, claimed, task.Source.Flavor)
	if !found {
		return task
	}
	task.StorageAlert = &StorageAlert{Code: CodeStorageInconsistent, Message: problem.Message}
	return task
}
