// Package meta provides module-level functionality for meta.
// input: migrations/000011_cluster_ownership.guards.sql and a MySQL connection opened without relying on golang-migrate's multi-statement file
// output: backup_tasks_bu_generation and binlog_server_kill_trx, skipped when information_schema already has them
// pos: post-step of migrate up; the up file itself cannot carry a BEGIN/END body
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
)

// InstallClusterOwnershipGuards creates the row_generation trigger and
// binlog_server_kill_trx. Each CREATE is one statement. A trigger or
// procedure that is already present is left in place, so a second up is safe.
// The caller runs this after migration 000011 has committed. The backfill in
// that file therefore does not pass through the trigger.
func InstallClusterOwnershipGuards(ctx context.Context, db *sql.DB, path string) error {
	if db == nil {
		return fmt.Errorf("install cluster guards: nil db")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("install cluster guards: %w", err)
	}
	stmts := splitGuardStatements(string(raw))
	if len(stmts) == 0 {
		return fmt.Errorf("install cluster guards: %s has no statements", path)
	}
	for _, stmt := range stmts {
		upper := strings.ToUpper(stmt)
		switch {
		case strings.Contains(upper, "CREATE") && strings.Contains(upper, "TRIGGER"):
			n, err := countSchemaObject(ctx, db, `SELECT COUNT(*) FROM information_schema.triggers WHERE trigger_schema = DATABASE() AND trigger_name = 'backup_tasks_bu_generation'`)
			if err != nil {
				return err
			}
			if n > 0 {
				continue
			}
		case strings.Contains(upper, "CREATE") && strings.Contains(upper, "PROCEDURE"):
			n, err := countSchemaObject(ctx, db, `SELECT COUNT(*) FROM information_schema.routines WHERE routine_schema = DATABASE() AND routine_name = 'binlog_server_kill_trx'`)
			if err != nil {
				return err
			}
			if n > 0 {
				continue
			}
		default:
			return fmt.Errorf("install cluster guards: statement is not a trigger or procedure")
		}
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("install cluster guards: %w", err)
		}
	}
	return nil
}

func countSchemaObject(ctx context.Context, db *sql.DB, query string) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx, query).Scan(&n); err != nil {
		return 0, fmt.Errorf("install cluster guards: %w", err)
	}
	return n, nil
}

// splitGuardStatements splits on a line whose trimmed text is `-- next`.
func splitGuardStatements(text string) []string {
	var out []string
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "-- next" {
			if s := strings.TrimSpace(b.String()); s != "" {
				out = append(out, s)
			}
			b.Reset()
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if s := strings.TrimSpace(b.String()); s != "" {
		out = append(out, s)
	}
	return out
}
