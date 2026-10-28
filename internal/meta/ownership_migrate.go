// Package meta provides module-level functionality for meta.
// input: a MySQL connection and information_schema.innodb_trx when the account can read it
// output: a warning when PROCESS is missing, or an error when a transaction older than 5s is visible; no trigger and no procedure
// pos: pre-step of migrate up and goto before schema version 11; the SQL file itself stays inside the documented schema grant
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql"
	"fmt"
)

// longTrxPrecheck counts transactions older than the published idle cap.
// Five seconds is the longest cap this build publishes. SQL cannot see a
// shorter per-process idle_kill.
const longTrxPrecheck = `SELECT COUNT(*) FROM information_schema.innodb_trx WHERE trx_started < DATE_SUB(NOW(6), INTERVAL 5 SECOND)`

// longTrxSkipWarning is the operator text when the account cannot read
// information_schema.innodb_trx. Keep "skipped innodb_trx precheck" and
// "PROCESS" stable: the least-privilege e2e matches both.
const longTrxSkipWarning = "skipped innodb_trx precheck: this account cannot read information_schema.innodb_trx (PROCESS is missing). An open transaction older than 5s will not block this migration. The following DDL waits at most lock_wait_timeout (10s) and can leave schema_migrations dirty if a metadata lock is still held; stop writers and run migrate up again. Granting PROCESS makes this precheck fail closed instead of skipping."

// CheckLongTransactions is the PROCESS-gated half of the 000011 precheck.
// A visible transaction older than 5s aborts before DDL so the schema is
// not left dirty. Missing PROCESS (1227 or 1142) returns a warning and a
// nil error. The live-registration precheck stays in the SQL file.
func CheckLongTransactions(ctx context.Context, db *sql.DB) (string, error) {
	if db == nil {
		return "", fmt.Errorf("innodb_trx precheck: nil db")
	}
	var n int
	err := db.QueryRowContext(ctx, longTrxPrecheck).Scan(&n)
	if err != nil {
		if accessDenied(err) {
			return longTrxSkipWarning, nil
		}
		return "", fmt.Errorf("innodb_trx precheck: %w", err)
	}
	if n > 0 {
		return "", fmt.Errorf("innodb_trx precheck: %d transaction(s) older than 5s; stop them before migrate up", n)
	}
	return "", nil
}
