// Package meta provides module-level functionality for meta.
// input: a write context that may carry tasks.FenceIdentity (task, owner, epoch) and the statement to run
// output: execUnderFence: with an identity for this task, one transaction that locks task_leases (leaseRowForUpdate, shared with the P2-6a task-row fence), requires owner and epoch to match a held row, runs the write on that transaction and commits, or returns tasks.ErrWriteFenced without writing; without an identity, the old single statement (ADR 0009 P2-6, #417)
// pos: metadata half of the end-to-end write fence (checkpoint, binlog_files upsert and delete)
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql"
	"fmt"

	"binlog_server/internal/tasks"
)

type fenceExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// execUnderFence runs fn under the run's fencing token. The lease row lock
// orders this write against Acquire: a takeover that committed epoch+1
// first makes this write fail, and a write that committed first is visible
// to the new owner's resume. A released or missing lease row is not
// ownership. Retries cover the whole transaction; ErrWriteFenced is not
// transient.
func (s *MySQLTaskStore) execUnderFence(ctx context.Context, taskID string, fn func(context.Context, fenceExecer) error) error {
	id, ok := tasks.FenceIdentityFrom(ctx)
	if !ok || id.TaskID != taskID {
		return WithRetry(ctx, DefaultMySQLRetryPolicy(), func() error { return fn(ctx, s.db) })
	}
	return WithRetry(ctx, DefaultMySQLRetryPolicy(), func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		owner, epoch, found, err := leaseRowForUpdate(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: task %s has no lease row (writer %s epoch %d)", tasks.ErrWriteFenced, taskID, id.Owner, id.Epoch)
		}
		if owner == "" || owner != id.Owner || epoch != id.Epoch {
			return fmt.Errorf("%w: task %s lease is %q epoch %d (writer %s epoch %d)", tasks.ErrWriteFenced, taskID, owner, epoch, id.Owner, id.Epoch)
		}
		if err := fn(ctx, tx); err != nil {
			return err
		}
		return tx.Commit()
	})
}
