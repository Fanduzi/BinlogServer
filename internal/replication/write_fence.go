// Package replication provides module-level functionality for replication.
// input: the run's task (owner, epoch) and the injected tasks.WriteFence lease clock
// output: fenceWrite, the local write-boundary check every run side effect passes (append, checkpoint/OPEN row, open segment, seal, covered-open release, upload; ADR 0009 P2-6, #417), and renameNoReplace so two owners sealing the same name cannot overwrite each other
// pos: data-plane half of the end-to-end write fence; the metadata half is in meta (task_leases FOR UPDATE in the write's transaction)
// note: if this file changes, update this header and module README.md.
package replication

import (
	"errors"
	"fmt"
	"log"
	"os"

	"binlog_server/internal/tasks"
)

// fenceWrite refuses a side effect once this run's lease epoch is no longer
// fresh on the local lease clock. The error is a lease handoff (finishRun),
// never a task failure. No fence configured, or epoch 0, is a no-op.
func (r *MySQLRunner) fenceWrite(task tasks.Task, boundary string) error {
	if r == nil || r.writeFence == nil || task.Epoch <= 0 {
		return nil
	}
	if r.writeFence.Fresh(task.ID, task.Epoch) {
		return nil
	}
	log.Printf("fence: refused %s task=%s owner=%s epoch=%d", boundary, task.ID, task.OwnerWorkerID, task.Epoch)
	return fmt.Errorf("%w: %w: %s", ErrLeaseEpochMismatch, tasks.ErrWriteFenced, boundary)
}

// renameNoReplace moves from to to and fails with SEALED_FILE_EXISTS when to
// already exists, so two owners sealing the same name cannot overwrite each
// other. Linux uses renameat2(RENAME_NOREPLACE); elsewhere, or on a
// filesystem without it, stat then rename.
func renameNoReplace(from, to string) error {
	err := renameNoReplaceOS(from, to)
	if errors.Is(err, os.ErrExist) {
		return tasks.NewPermanentError(tasks.CodeSealedFileExists, fmt.Sprintf("sealed file already exists: %s", to))
	}
	if !errors.Is(err, errNoReplaceUnsupported) {
		return err
	}
	if _, statErr := os.Stat(to); statErr == nil {
		return tasks.NewPermanentError(tasks.CodeSealedFileExists, fmt.Sprintf("sealed file already exists: %s", to))
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	return os.Rename(from, to)
}

var errNoReplaceUnsupported = errors.New("rename no-replace unsupported")
