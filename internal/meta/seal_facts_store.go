// Package meta provides module-level functionality for meta.
// input: a SEALED tasks.BinlogFile that carries seal facts (first/last event time, sha256), or asks to reset them; a probe of schema_migrations and of the 000008 columns
// output: the facts written to binlog_files (migration 000008) after the row upsert, under the run's fence token when the context carries one; on a catalog without the columns (MySQL 1054) the write is skipped, one warning is logged, and the next write or ProbeSealFacts asks again so an online migrate up resumes without a restart (#353); ListSealedChecksums reads the sha256 of each sealed row back for the health rehasher (P2-3), empty while the columns are absent
// pos: ADR 0009 phase 1 P1-4 and phase 2 P2-5.4 (§26.5); the schema floor stays 6
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"

	mysqlDriver "github.com/go-sql-driver/mysql"

	"binlog_server/internal/tasks"
)

const upsertSealFactsSQL = `
UPDATE binlog_files SET first_event_at = ?, last_event_at = ?, sha256 = ?
WHERE task_id = ? AND source_file = ? AND epoch = ?;
`

// probeSealColumnsSQL is the per-sweep and per-write probe for migration
// 000008. LIMIT 0 still reports 1054 when the column is gone, and ErrNoRows
// when the column exists.
const probeSealColumnsSQL = "SELECT first_event_at FROM binlog_files LIMIT 0"

func (s *MySQLTaskStore) writeSealFacts(ctx context.Context, f tasks.BinlogFile) error {
	if state := strings.TrimSpace(f.State); state != "" && !strings.EqualFold(state, "SEALED") {
		return nil
	}
	if f.FirstEventAt == nil && f.LastEventAt == nil && strings.TrimSpace(f.SHA256) == "" && !f.ResetSealFacts {
		return nil
	}
	if s.sealFactsAbsent.Load() {
		present, err := s.probeSealColumns(ctx)
		if err != nil {
			return err
		}
		s.noteSealColumns(present, 0)
		if !present {
			return nil
		}
	}
	var first, last, sum any
	if f.FirstEventAt != nil {
		first = f.FirstEventAt.UTC()
	}
	if f.LastEventAt != nil {
		last = f.LastEventAt.UTC()
	}
	if v := strings.TrimSpace(f.SHA256); v != "" {
		sum = v
	}
	err := s.execUnderFence(ctx, f.TaskID, func(ctx context.Context, q fenceExecer) error {
		_, err := q.ExecContext(ctx, upsertSealFactsSQL, first, last, sum, f.TaskID, f.FileName, f.Epoch)
		return err
	})
	if isMySQLUnknownColumnError(err) {
		s.noteSealColumns(false, 0)
		return nil
	}
	return err
}

// ProbeSealFacts reports the schema version and whether SEAL_FACTS_ABSENT
// should be open: the 000008 columns are missing and either the schema is
// at least 8 or this process has seen the columns before. A probe error
// leaves the in-process memory unchanged.
func (s *MySQLTaskStore) ProbeSealFacts(ctx context.Context) (int, bool, error) {
	var version int64
	var dirty bool
	if err := s.db.QueryRowContext(ctx, currentSchemaVersionSQL).Scan(&version, &dirty); err != nil {
		return 0, false, err
	}
	present, err := s.probeSealColumns(ctx)
	if err != nil {
		return int(version), false, err
	}
	s.noteSealColumns(present, int(version))
	absent := !present && (version >= 8 || s.sealFactsSeen.Load())
	return int(version), absent, nil
}

func (s *MySQLTaskStore) probeSealColumns(ctx context.Context) (bool, error) {
	var at sql.NullTime
	err := s.db.QueryRowContext(ctx, probeSealColumnsSQL).Scan(&at)
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if isMySQLUnknownColumnError(err) {
		return false, nil
	}
	return false, err
}

func (s *MySQLTaskStore) noteSealColumns(present bool, version int) {
	if present {
		s.sealFactsSeen.Store(true)
		if s.sealFactsAbsent.Swap(false) {
			log.Printf("seal facts: 000008 columns are present again; seal fact writes resumed")
		}
		s.sealFactsWarned.Store(false)
		return
	}
	s.sealFactsAbsent.Store(true)
	if s.sealFactsWarned.CompareAndSwap(false, true) {
		if version > 0 {
			log.Printf("seal facts: binlog_files has no 000008 columns (schema %d); seal fact writes skipped until the columns return", version)
			return
		}
		log.Printf("seal facts: binlog_files has no 000008 columns; seal fact writes skipped until the columns return")
	}
}

func isMySQLUnknownColumnError(err error) bool {
	var mysqlErr *mysqlDriver.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1054
}

// listSealedChecksumsSQL is the sha256 recorded at seal for each sealed row
// of one task, the health rehasher's expected checksum (ADR 0009 P2-3).
const listSealedChecksumsSQL = `
SELECT file_name, epoch, size_bytes, sha256 FROM binlog_files
WHERE task_id = ? AND state = 'SEALED' AND sha256 IS NOT NULL AND sha256 <> ''
`

// ListSealedChecksums returns the sealed rows of taskID that carry a sha256,
// with FileName, Epoch, SizeBytes and SHA256 set. While the 000008 columns
// are absent it returns nothing. ProbeSealFacts or the next seal write
// clears that memory when the columns return.
func (s *MySQLTaskStore) ListSealedChecksums(ctx context.Context, taskID string) ([]tasks.BinlogFile, error) {
	if s.sealFactsAbsent.Load() {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, listSealedChecksumsSQL, taskID)
	if isMySQLUnknownColumnError(err) {
		s.noteSealColumns(false, 0)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tasks.BinlogFile
	for rows.Next() {
		f := tasks.BinlogFile{TaskID: taskID, State: "SEALED"}
		if err := rows.Scan(&f.FileName, &f.Epoch, &f.SizeBytes, &f.SHA256); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
