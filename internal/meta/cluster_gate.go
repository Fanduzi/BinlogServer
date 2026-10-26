// Package meta provides module-level functionality for meta.
// input: cluster.storage, worker_registrations.cluster_protocol_version, and task_node_refusal
// output: the registered storage mode, the count of older live protocols, one transaction that writes a yield with its refusal row, and a claim-time read of an open refusal
// pos: ADR 0011 F11 and F12 on the metadata store
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"binlog_server/internal/tasks"
)

// RegisterClusterStorage inserts cluster.storage once. A later call keeps the
// first mode and returns it so the caller can refuse a mismatch.
func (s *MySQLTaskStore) RegisterClusterStorage(ctx context.Context, mode string) (string, error) {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		mode = "per_node"
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO cluster_storage (id, storage_mode, registered_at)
VALUES (1, ?, UTC_TIMESTAMP(6))
ON DUPLICATE KEY UPDATE registered_at = registered_at`, mode); err != nil {
		return "", err
	}
	var got string
	err := s.db.QueryRowContext(ctx, `SELECT storage_mode FROM cluster_storage WHERE id = 1`).Scan(&got)
	return got, err
}

// CountOlderProtocols counts live registrations still below version, excluding self.
// It does not join task claim queries (ADR 0011 PR-2).
func (s *MySQLTaskStore) CountOlderProtocols(ctx context.Context, self string, version int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM worker_registrations
WHERE cluster_protocol_version < ?
  AND lease_expire_at > NOW(6)
  AND worker_id <> ?`, version, self).Scan(&n)
	return n, err
}

// CommitYield writes the task row, one refusal, and the lease release in one
// transaction. Zero rows means the epoch or generation moved; that is a no-op.
// A missing lease row is the same no-op.
func (s *MySQLTaskStore) CommitYield(ctx context.Context, row tasks.YieldCommit) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var epoch int64
	err = tx.QueryRowContext(ctx, `SELECT epoch FROM task_leases WHERE task_id = ? FOR UPDATE`, row.TaskID).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var failedAt any
	if !row.FailedAt.IsZero() {
		failedAt = row.FailedAt.UTC()
	}
	var tail any
	if !row.TailSince.IsZero() {
		tail = row.TailSince.UTC()
	}
	pending := 0
	if row.PendingDelete {
		pending = 1
	}
	res, err := tx.ExecContext(ctx, `
UPDATE backup_tasks
SET state = ?, desired_run = ?, last_error = ?, owner_worker_id = ?, epoch = ?, run_id = ?,
    failed_reason = ?, failed_at = ?, tail_unreadable_since = ?, pending_delete = ?,
    failed_spec_revision = ?, updated_at = ?, row_generation = row_generation + 1
WHERE id = ? AND epoch = ? AND (row_generation = ? OR ? = 0)`,
		row.State, row.Desired, row.LastError, row.Owner, row.Epoch, row.RunID,
		row.FailedReason, failedAt, tail, pending, row.FailedSpec, row.UpdatedAt.UTC(),
		row.TaskID, row.ExpectEpoch, row.ExpectGeneration, row.ExpectGeneration)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	if row.RefusalNode != "" {
		var until any
		if !row.RefusalOpen && !row.RefusalUntil.IsZero() {
			until = row.RefusalUntil.UTC()
		}
		if _, err = tx.ExecContext(ctx, `
INSERT INTO task_node_refusal (task_id, node_id, refuse_until, reason, attempts)
VALUES (?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE refuse_until = VALUES(refuse_until), reason = VALUES(reason), attempts = VALUES(attempts)`,
			row.TaskID, row.RefusalNode, until, row.RefusalReason, row.RefusalAttempts); err != nil {
			return 0, err
		}
	}
	if row.Release {
		if _, err = tx.ExecContext(ctx, `
UPDATE task_leases
SET owner_worker_id = '', lease_expire_at = NOW(6)
WHERE task_id = ? AND epoch = ?`, row.TaskID, row.ReleaseEpoch); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// RefusalOpen reports a refusal row that still blocks this node.
// NULL refuse_until is open-ended. A past timestamp does not block.
func (s *MySQLTaskStore) RefusalOpen(ctx context.Context, taskID, nodeID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM task_node_refusal
WHERE task_id = ? AND node_id = ?
  AND (refuse_until IS NULL OR refuse_until > NOW(6))`, taskID, nodeID).Scan(&n)
	return n > 0, err
}

// DeleteTaskRefusals removes every refusal for the task.
// Operator Start and DumpReady both clear them.
func (s *MySQLTaskStore) DeleteTaskRefusals(ctx context.Context, taskID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM task_node_refusal WHERE task_id = ?`, taskID)
	return err
}
