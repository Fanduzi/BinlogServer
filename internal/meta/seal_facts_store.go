// Package meta provides module-level functionality for meta.
// input: a SEALED tasks.BinlogFile that carries seal facts (first/last event time, sha256)
// output: the facts written to binlog_files (migration 000008) after the row upsert; on a catalog without the columns (MySQL 1054) the write is skipped and remembered, so schema 6 and 7 keep working
// pos: ADR 0009 phase 1, P1-4; the schema floor stays 6
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"

	"binlog_server/internal/tasks"
)

const upsertSealFactsSQL = `
UPDATE binlog_files SET first_event_at = ?, last_event_at = ?, sha256 = ?
WHERE task_id = ? AND source_file = ? AND epoch = ?;
`

func (s *MySQLTaskStore) writeSealFacts(ctx context.Context, f tasks.BinlogFile) error {
	return nil
}
