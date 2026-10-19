// Package meta provides module-level functionality for meta.
// input: tasks.Finding rows from the scheduler's finding sink
// output: task_findings upsert/list (migration 000007; a resolved row written again takes the new first_seen and count, §25.8 R4); tasks.ErrFindingStoreAbsent on schema 6 so the sink keeps findings in memory
// pos: metadata persistence for ADR 0009 findings
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql"

	"binlog_server/internal/tasks"
)

// While the row is open first_seen keeps the earlier value; a resolved row
// that is written again is a new occurrence and takes the new first_seen
// and count (design 0009 §25.8 R4). Assignments run left to right, so
// first_seen and count read resolved_at before it is overwritten.
const upsertFindingSQL = `
INSERT INTO task_findings (task_id, kind, subject, severity, detail, first_seen, last_seen, count, resolved_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
  severity = VALUES(severity),
  detail = VALUES(detail),
  first_seen = IF(resolved_at IS NULL, LEAST(first_seen, VALUES(first_seen)), VALUES(first_seen)),
  count = IF(resolved_at IS NULL, GREATEST(count, VALUES(count)), VALUES(count)),
  last_seen = GREATEST(last_seen, VALUES(last_seen)),
  resolved_at = VALUES(resolved_at);
`

const listTaskFindingsSQL = `
SELECT task_id, kind, subject, severity, detail, first_seen, last_seen, count, resolved_at
FROM task_findings
WHERE task_id = ? AND (? OR resolved_at IS NULL)
ORDER BY (resolved_at IS NULL) DESC, last_seen DESC
LIMIT ?;
`

const listOpenFindingsSQL = `
SELECT task_id, kind, subject, severity, detail, first_seen, last_seen, count, resolved_at
FROM task_findings
WHERE resolved_at IS NULL
ORDER BY last_seen DESC
LIMIT ?;
`

func findingErr(err error) error {
	if isMySQLTableNotFoundError(err) {
		return tasks.ErrFindingStoreAbsent
	}
	return err
}

// UpsertFinding writes one finding row (ADR 0009).
func (s *MySQLTaskStore) UpsertFinding(ctx context.Context, f tasks.Finding) error {
	var resolved any
	if f.ResolvedAt != nil {
		resolved = f.ResolvedAt.UTC()
	}
	_, err := s.db.ExecContext(ctx, upsertFindingSQL,
		f.TaskID, string(f.Kind), f.Subject, string(f.Severity), f.Detail,
		f.FirstSeen.UTC(), f.LastSeen.UTC(), f.Count, resolved)
	return findingErr(err)
}

// ListFindings returns a task's findings, open first.
func (s *MySQLTaskStore) ListFindings(ctx context.Context, taskID string, includeResolved bool, limit int) ([]tasks.Finding, error) {
	rows, err := s.db.QueryContext(ctx, listTaskFindingsSQL, taskID, includeResolved, limit)
	if err != nil {
		return nil, findingErr(err)
	}
	return scanFindings(rows)
}

// ListOpenFindings returns open findings across all tasks.
func (s *MySQLTaskStore) ListOpenFindings(ctx context.Context, limit int) ([]tasks.Finding, error) {
	rows, err := s.db.QueryContext(ctx, listOpenFindingsSQL, limit)
	if err != nil {
		return nil, findingErr(err)
	}
	return scanFindings(rows)
}

func scanFindings(rows *sql.Rows) ([]tasks.Finding, error) {
	defer rows.Close()
	var out []tasks.Finding
	for rows.Next() {
		var f tasks.Finding
		var kind, severity string
		var detail sql.NullString
		var resolved sql.NullTime
		if err := rows.Scan(&f.TaskID, &kind, &f.Subject, &severity, &detail, &f.FirstSeen, &f.LastSeen, &f.Count, &resolved); err != nil {
			return nil, err
		}
		f.Kind = tasks.FindingKind(kind)
		f.Severity = tasks.FindingSeverity(severity)
		f.Detail = detail.String
		if resolved.Valid {
			at := resolved.Time
			f.ResolvedAt = &at
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

const countOpenFindingsSQL = `
SELECT task_id, COUNT(*)
FROM task_findings
WHERE resolved_at IS NULL
GROUP BY task_id;
`

// CountOpenFindings returns open findings per task (tasks.FindingCounter).
func (s *MySQLTaskStore) CountOpenFindings(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, countOpenFindingsSQL)
	if err != nil {
		return nil, findingErr(err)
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var task string
		var n int
		if err := rows.Scan(&task, &n); err != nil {
			return nil, err
		}
		out[task] = n
	}
	return out, rows.Err()
}
