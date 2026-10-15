// Package meta provides module-level functionality for meta.
// input: a SEALED tasks.BinlogFile that carries seal facts (first/last event time, sha256), or asks to reset them
// output: the facts written to binlog_files (migration 000008) after the row upsert; on a catalog without the columns (MySQL 1054) the write is skipped and remembered, so schema 6 and 7 keep working; ListSealedChecksums reads the sha256 of each sealed row back for the health rehasher (P2-3), empty on schema 6 and 7
// pos: ADR 0009 phase 1, P1-4; the schema floor stays 6
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"errors"
	"strings"

	mysqlDriver "github.com/go-sql-driver/mysql"

	"binlog_server/internal/tasks"
)

const upsertSealFactsSQL = `
UPDATE binlog_files SET first_event_at = ?, last_event_at = ?, sha256 = ?
WHERE task_id = ? AND source_file = ? AND epoch = ?;
`

func (s *MySQLTaskStore) writeSealFacts(ctx context.Context, f tasks.BinlogFile) error {
	if state := strings.TrimSpace(f.State); state != "" && !strings.EqualFold(state, "SEALED") {
		return nil
	}
	if f.FirstEventAt == nil && f.LastEventAt == nil && strings.TrimSpace(f.SHA256) == "" && !f.ResetSealFacts {
		return nil
	}
	if s.sealFactsAbsent.Load() {
		return nil
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
	_, err := s.db.ExecContext(ctx, upsertSealFactsSQL, first, last, sum, f.TaskID, f.FileName, f.Epoch)
	if isMySQLUnknownColumnError(err) {
		// Schema 6 or 7: 000008 is optional. Stop asking until restart.
		s.sealFactsAbsent.Store(true)
		return nil
	}
	return err
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
// with FileName, Epoch, SizeBytes and SHA256 set. Without migration 000008
// it returns nothing, like the seal fact writes.
func (s *MySQLTaskStore) ListSealedChecksums(ctx context.Context, taskID string) ([]tasks.BinlogFile, error) {
	if s.sealFactsAbsent.Load() {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, listSealedChecksumsSQL, taskID)
	if isMySQLUnknownColumnError(err) {
		s.sealFactsAbsent.Store(true)
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
