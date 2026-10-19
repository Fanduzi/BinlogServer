// Package meta provides module-level functionality for meta.
// input: a task id and a binlog_files (source_file, epoch) whose local file local retention deleted while the uploaded object stayed
// output: retention_removed_at (migration 000010) written under the run's fence token when the context carries one; ListRetentionMarkers for the chain; ErrRetentionMarkerUnsupported and one warning when the column is absent, re-probed on the next call (design 0009 §26.2, #341)
// pos: the catalog half of the local-retention deletion marker; the schema floor stays 6
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	"binlog_server/internal/tasks"
)

const markRetentionRemovedSQL = `
UPDATE binlog_files SET retention_removed_at = ?
WHERE task_id = ? AND source_file = ? AND epoch = ? AND retention_removed_at IS NULL
`

const retentionMarkedSQL = `
SELECT retention_removed_at FROM binlog_files
WHERE task_id = ? AND source_file = ? AND epoch = ?
`

const listRetentionMarkersSQL = `
SELECT source_file, epoch, retention_removed_at FROM binlog_files
WHERE task_id = ? AND retention_removed_at IS NOT NULL
`

const probeRetentionColumnSQL = "SELECT retention_removed_at FROM binlog_files LIMIT 0"

// MarkRetentionRemoved sets retention_removed_at when it is still NULL.
// A catalog without migration 000010 returns tasks.ErrRetentionMarkerUnsupported.
func (s *MySQLTaskStore) MarkRetentionRemoved(ctx context.Context, taskID, fileName string, epoch int64, at time.Time) error {
	if s.retentionMarkerAbsent.Load() {
		present, err := s.probeRetentionColumn(ctx)
		if err != nil {
			return err
		}
		if !present {
			s.noteRetentionColumn(false)
			return tasks.ErrRetentionMarkerUnsupported
		}
		s.noteRetentionColumn(true)
	}
	if at.IsZero() {
		at = time.Now()
	}
	err := s.execUnderFence(ctx, taskID, func(ctx context.Context, q fenceExecer) error {
		_, err := q.ExecContext(ctx, markRetentionRemovedSQL, at.UTC(), taskID, fileName, epoch)
		return err
	})
	if isMySQLUnknownColumnError(err) {
		s.noteRetentionColumn(false)
		return tasks.ErrRetentionMarkerUnsupported
	}
	return err
}

// RetentionMarked reports whether this row already carries the marker.
// A missing column returns tasks.ErrRetentionMarkerUnsupported.
func (s *MySQLTaskStore) RetentionMarked(ctx context.Context, taskID, fileName string, epoch int64) (bool, error) {
	var at sql.NullTime
	err := s.db.QueryRowContext(ctx, retentionMarkedSQL, taskID, fileName, epoch).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if isMySQLUnknownColumnError(err) {
		s.noteRetentionColumn(false)
		return false, tasks.ErrRetentionMarkerUnsupported
	}
	if err != nil {
		return false, err
	}
	s.noteRetentionColumn(true)
	return at.Valid, nil
}

// ListRetentionMarkers returns source_file+epoch → retention_removed_at for
// one task. A missing column is the supported schema 6–9 path: empty map,
// nil error, one warning. Any other error is returned so the chain scope
// stays incomplete.
func (s *MySQLTaskStore) ListRetentionMarkers(ctx context.Context, taskID string) (map[string]time.Time, error) {
	if s.retentionMarkerAbsent.Load() {
		present, err := s.probeRetentionColumn(ctx)
		if err != nil {
			return nil, err
		}
		if !present {
			s.noteRetentionColumn(false)
			return map[string]time.Time{}, nil
		}
		s.noteRetentionColumn(true)
	}
	rows, err := s.db.QueryContext(ctx, listRetentionMarkersSQL, taskID)
	if isMySQLUnknownColumnError(err) {
		s.noteRetentionColumn(false)
		return map[string]time.Time{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var name string
		var epoch int64
		var at time.Time
		if err := rows.Scan(&name, &epoch, &at); err != nil {
			return nil, err
		}
		out[tasks.RetentionMarkerKey(name, epoch)] = at
	}
	return out, rows.Err()
}

func (s *MySQLTaskStore) probeRetentionColumn(ctx context.Context) (bool, error) {
	var at sql.NullTime
	err := s.db.QueryRowContext(ctx, probeRetentionColumnSQL).Scan(&at)
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if isMySQLUnknownColumnError(err) {
		return false, nil
	}
	return false, err
}

func (s *MySQLTaskStore) noteRetentionColumn(present bool) {
	if present {
		if s.retentionMarkerAbsent.Swap(false) {
			log.Printf("retention: retention_removed_at is present again")
		}
		s.retentionMarkerWarned.Store(false)
		return
	}
	s.retentionMarkerAbsent.Store(true)
	if s.retentionMarkerWarned.CompareAndSwap(false, true) {
		log.Printf("retention: binlog_files has no retention_removed_at column; RETENTION_REMOVED stays idempotent only until this process restarts (migration 000010)")
	}
}
