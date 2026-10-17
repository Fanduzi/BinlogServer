// Package meta provides module-level functionality for meta.
// input: taskID + (owner, epoch) fence identity and a callback that mutates meta under one transaction
// output: leaseRowForUpdate (the one task_leases FOR UPDATE read both fences share); FencedExec / UpsertTaskFenced: lock the lease row, then run the write only when tasks.CheckTaskRowFence allows it (the held lease is the writer's, a released lease was released at the writer's epoch, or with no lease row the backup_tasks row names no other owner or newer epoch); otherwise healthstore.ErrFenced and no task-row write (ADR 0009 P2-6a / #360)
// pos: write fence for STOPPED/STOPPING task-row paths after lease loss (P2-6c / #421: a release at a newer epoch fences the old owner)
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql"
	"errors"

	"binlog_server/internal/tasks"
)

// leaseRowForUpdate locks the task_leases row inside tx and returns its
// holder. It is the one lease read both fences use: the task-row fence here
// (a release at the writer's own epoch lets it converge to STOPPED) and the
// data-plane fence in write_fence.go (only the current holder writes).
func leaseRowForUpdate(ctx context.Context, tx *sql.Tx, taskID string) (owner string, epoch int64, found bool, err error) {
	err = tx.QueryRowContext(ctx, `SELECT owner_worker_id, epoch FROM task_leases WHERE task_id = ? FOR UPDATE`, taskID).Scan(&owner, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	return owner, epoch, true, nil
}

// checkLeaseFenceLocked locks the lease row (and, when there is none, the
// task row) and applies tasks.CheckTaskRowFence. A release at a newer epoch
// fences the old owner: another worker ran the task after it and ended it
// (FAILED, #421), and the old owner must not rewrite that row as STOPPED.
func checkLeaseFenceLocked(ctx context.Context, tx *sql.Tx, taskID, owner string, epoch int64) error {
	var in tasks.TaskRowFenceInput
	var err error
	in.LeaseOwner, in.LeaseEpoch, in.LeaseFound, err = leaseRowForUpdate(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if !in.LeaseFound {
		err = tx.QueryRowContext(ctx, selectTaskOwnerForUpdateSQL, taskID).Scan(&in.RowOwner, &in.RowEpoch)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			in.RowFound = true
		}
	}
	return tasks.CheckTaskRowFence(in, owner, epoch)
}

// selectTaskOwnerForUpdateSQL locks the task row when task_leases has none.
const selectTaskOwnerForUpdateSQL = `SELECT COALESCE(owner_worker_id, ''), epoch FROM backup_tasks WHERE id = ? FOR UPDATE`

// FencedExec runs fn in one transaction after locking task_leases and
// confirming (owner, epoch). On mismatch it returns healthstore.ErrFenced
// without calling fn (ADR 0009 §8.4 meta.fencedExec).
func (s *MySQLTaskStore) FencedExec(ctx context.Context, taskID, owner string, epoch int64, fn func(*sql.Tx) error) error {
	if s == nil || s.db == nil {
		return errors.New("nil meta store")
	}
	if owner == "" || epoch <= 0 {
		return errors.New("fenced exec requires owner and epoch")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := checkLeaseFenceLocked(ctx, tx, taskID, owner, epoch); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// UpsertTaskFenced is UpsertTask under FencedExec: the STOPPED/STOPPING write
// from a lease-loss path cannot overwrite a newer holder's row (#360).
func (s *MySQLTaskStore) UpsertTaskFenced(ctx context.Context, task tasks.Task, owner string, epoch int64) error {
	ctx, span := startMetaSpan(ctx, "meta.mysql_store.upsert_task_fenced")
	defer endMetaSpan(span)
	return s.FencedExec(ctx, task.ID, owner, epoch, func(tx *sql.Tx) error {
		return s.upsertTaskInTx(ctx, tx, task)
	})
}
