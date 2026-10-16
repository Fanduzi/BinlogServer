// Package meta provides module-level functionality for meta.
// input: taskID + (owner, epoch) fence identity and a callback that mutates meta under one transaction
// output: FencedExec / UpsertTaskFenced: SELECT … FROM task_leases … FOR UPDATE, then run the write only when the lease still matches (or is released / absent); otherwise healthstore.ErrFenced and no task-row write (ADR 0009 P2-6a / #360)
// pos: write fence for STOPPED/STOPPING task-row paths after lease loss
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql"
	"errors"

	"binlog_server/internal/healthstore"
	"binlog_server/internal/tasks"
)

// selectLeaseForUpdate locks the task_leases row for the fence check.
const selectLeaseForUpdate = `SELECT owner_worker_id, epoch FROM task_leases WHERE task_id = ? FOR UPDATE`

// checkLeaseFenceLocked inspects the locked lease row. A different non-empty
// holder (owner or epoch) fences the write. A released row (owner "") or a
// missing row lets the old owner converge to STOPPED (0346 R3).
func checkLeaseFenceLocked(ctx context.Context, tx *sql.Tx, taskID, owner string, epoch int64) error {
	var leaseOwner string
	var leaseEpoch int64
	err := tx.QueryRowContext(ctx, selectLeaseForUpdate, taskID).Scan(&leaseOwner, &leaseEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if leaseOwner == "" {
		return nil
	}
	if leaseOwner == owner && leaseEpoch == epoch {
		return nil
	}
	return healthstore.ErrFenced
}

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
