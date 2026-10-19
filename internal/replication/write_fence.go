// Package replication provides module-level functionality for replication.
// input: the run's task (owner, epoch), the injected tasks.WriteFence lease clock, and the lease verifier
// output: fenceWrite, the local write-boundary check every run side effect passes (append, checkpoint/OPEN row, open segment, seal, covered-open release, upload; ADR 0009 P2-6, #417); missingOpenHandoff, which turns ENOENT on this run's .open.eN into a lease handoff after that epoch is lost or no longer held (#457); and renameNoReplace so two owners sealing the same name cannot overwrite each other
// pos: data-plane half of the end-to-end write fence; the metadata half is in meta (task_leases FOR UPDATE in the write's transaction)
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"binlog_server/internal/binlog"
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

// missingOpenHandoff turns ENOENT on this run's .open.eN into a lease handoff
// when this epoch no longer owns the task. A missing segment while the epoch
// still holds the lease is returned unchanged, so SEGMENT_NOT_ON_WORKER and a
// missing unuploaded tail still fail (#457).
func (r *MySQLRunner) missingOpenHandoff(ctx context.Context, task tasks.Task, path string, err error) error {
	if r == nil || err == nil || !os.IsNotExist(err) || !openEpochPath(path, task.Epoch) {
		return err
	}
	if !r.openEpochGone(ctx, task) {
		return err
	}
	log.Printf("open segment missing after lease handoff task=%s epoch=%d path=%s", task.ID, task.Epoch, path)
	return fmt.Errorf("%w: %w", ErrLeaseEpochMismatch, err)
}

func openEpochPath(path string, epoch int64) bool {
	if epoch <= 0 || path == "" {
		return false
	}
	named, ok := binlog.ClassifySegment(filepath.Base(path))
	return ok && named.Open && named.Epoch == epoch
}

// openEpochGone reports that this run must not treat a missing .open.eN as
// its own failure. Lost is local. Verify false means the lease row moved.
// A verify error is not a handoff: the epoch may still own the file.
func (r *MySQLRunner) openEpochGone(ctx context.Context, task tasks.Task) bool {
	if r == nil || task.Epoch <= 0 {
		return false
	}
	if r.writeFence != nil {
		if lost, ok := r.writeFence.(interface{ Lost(string, int64) bool }); ok && lost.Lost(task.ID, task.Epoch) {
			return true
		}
	}
	if r.leaseVerifier == nil || task.OwnerWorkerID == "" {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ok, err := r.leaseVerifier.Verify(ctx, task.ID, task.OwnerWorkerID, task.Epoch)
	return err == nil && !ok
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
