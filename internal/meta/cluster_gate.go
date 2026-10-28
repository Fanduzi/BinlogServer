// Package meta provides module-level functionality for meta.
// input: cluster.storage, worker_registrations.cluster_protocol_version, and task_node_refusal
// output: the registered storage mode, the count of older live protocols, one transaction that writes a yield with its refusal row through rowGenerationBump, and a claim-time read of an open refusal. Zero rows on that update are a lost lease
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

// commitYieldSQL bumps row_generation in the same place as the other task
// writes. Zero rows means the epoch or generation moved. The caller treats
// that as a lost lease and must not follow it with an unfenced upsert.
// row_generation = ? is equality even when the loaded value is 0. There is
// no unread escape: a yield that has not loaded the column must not match
// every generation.
const commitYieldSQL = `
UPDATE backup_tasks
SET state = ?, desired_run = ?, last_error = ?, owner_worker_id = ?, epoch = ?, run_id = ?,
    failed_reason = ?, failed_at = ?, tail_unreadable_since = ?, pending_delete = ?,
    failed_spec_revision = ?, updated_at = ?, ` + rowGenerationBump + `
WHERE id = ? AND epoch = ? AND row_generation = ?`

// CommitYield writes the task row, one refusal, and the lease release in one
// transaction. Zero rows means the epoch or generation moved; that is a lost
// lease, not an error. A missing lease row is the same no-op.
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
	res, err := tx.ExecContext(ctx, commitYieldSQL,
		row.State, row.Desired, row.LastError, row.Owner, row.Epoch, row.RunID,
		row.FailedReason, failedAt, tail, pending, row.FailedSpec, row.UpdatedAt.UTC(),
		row.TaskID, row.ExpectEpoch, row.ExpectGeneration)
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

// rowGenerationBump is the only application write of backup_tasks.row_generation.
// Every store UPDATE that changes a task row includes it. CommitYield also
// requires the previous generation, so a stale owner misses the predicate.
const rowGenerationBump = "row_generation = row_generation + 1"

// reconcileLegacyDesiredRunSQL rewrites desired_run for rows no step-4 writer
// has touched. spec_revision stays 0. FAILED is RUN: a historical FAILED row
// is not an operator Stop. A later operator Start or Stop moves spec_revision
// off 0, so this UPDATE does not cover that ask. Repeating it changes zero
// rows once desired_run already matches the state. row_generation increases
// through rowGenerationBump, the same assignment as the other task writes.
const reconcileLegacyDesiredRunSQL = `
UPDATE backup_tasks
SET desired_run = CASE
  WHEN state IN ('RUNNING', 'STARTING', 'RETRY_BACKOFF', 'LEASE_DEGRADED', 'REBUILDING_FILE', 'FAILED') THEN 'RUN'
  ELSE 'STOP'
END,
` + rowGenerationBump + `
WHERE spec_revision = 0
  AND applied_spec_revision = 0
  AND desired_run <> CASE
    WHEN state IN ('RUNNING', 'STARTING', 'RETRY_BACKOFF', 'LEASE_DEGRADED', 'REBUILDING_FILE', 'FAILED') THEN 'RUN'
    ELSE 'STOP'
  END
`

// acquireWorkerRegistrationSQL 以单条 UPSERT 表达“同 session 续租 + 过期接管”语义。
// acquireWorkerRegistrationSQL writes ClusterProtocolVersion only when this
// session creates the row or takes over an expired one. A live foreign
// session keeps the protocol it registered, including 0 from an old binary.
const acquireWorkerRegistrationSQL = `
INSERT INTO worker_registrations (worker_id, session_id, lease_expire_at, renewed_at, cluster_protocol_version)
VALUES (?, ?, DATE_ADD(NOW(6), INTERVAL ? MICROSECOND), NOW(6), 11)
ON DUPLICATE KEY UPDATE
  session_id = IF(session_id = VALUES(session_id) OR lease_expire_at <= NOW(6), VALUES(session_id), session_id),
  lease_expire_at = IF(session_id = VALUES(session_id) OR lease_expire_at <= NOW(6), VALUES(lease_expire_at), lease_expire_at),
  renewed_at = IF(session_id = VALUES(session_id) OR lease_expire_at <= NOW(6), NOW(6), renewed_at),
  cluster_protocol_version = IF(session_id = VALUES(session_id) OR lease_expire_at <= NOW(6), 11, cluster_protocol_version);
`

func pendingDeleteArg(on bool) int {
	if on {
		return 1
	}
	return 0
}

// withPendingColumn adds pending_dump_cleanup when migration 000004 is present.
// Queries that do not use the column stay valid on schema 3.
func (s *MySQLTaskStore) withPendingColumn(query string) string {
	if s == nil {
		return query
	}
	if s.hasPendingDump && !strings.Contains(query, "pending_dump_cleanup") {
		if strings.Contains(query, "t.consecutive_source_failures") {
			query = strings.Replace(query, "t.consecutive_source_failures", "t.consecutive_source_failures, t.pending_dump_cleanup", 1)
		} else {
			query = strings.Replace(query, taskSelectColumns, taskSelectColumns+", pending_dump_cleanup", 1)
		}
	}
	return s.withOwnershipColumns(query)
}

// withOwnershipColumns adds pending_delete and row_generation once migration
// 000011 is present. The insert points follow pending_dump_cleanup first, then
// the select column list, so a schema-3 query stays valid unchanged.
func (s *MySQLTaskStore) withOwnershipColumns(query string) string {
	if !s.hasOwnershipCols {
		return query
	}
	if !strings.Contains(query, "pending_delete") {
		switch {
		case strings.Contains(query, "t.pending_dump_cleanup"):
			query = strings.Replace(query, "t.pending_dump_cleanup", "t.pending_dump_cleanup, t.pending_delete", 1)
		case strings.Contains(query, "pending_dump_cleanup"):
			query = strings.Replace(query, "pending_dump_cleanup", "pending_dump_cleanup, pending_delete", 1)
		case strings.Contains(query, "t.consecutive_source_failures"):
			query = strings.Replace(query, "t.consecutive_source_failures", "t.consecutive_source_failures, t.pending_delete", 1)
		default:
			query = strings.Replace(query, taskSelectColumns, taskSelectColumns+", pending_delete", 1)
		}
	}
	if !strings.Contains(query, "row_generation") {
		switch {
		case strings.Contains(query, "t.pending_delete"):
			query = strings.Replace(query, "t.pending_delete", "t.pending_delete, t.row_generation", 1)
		case strings.Contains(query, "pending_delete"):
			query = strings.Replace(query, "pending_delete", "pending_delete, row_generation", 1)
		}
	}
	return query
}
