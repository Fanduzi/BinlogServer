// Package meta provides module-level functionality for meta.
// input: migration 000011 up, down, and guards SQL
// output: proof that the up file stays free of compound statements, the backfill bumps row_generation, down drops the procedure before the trigger and does not restore desired STOP, and the guards file carries the trigger signal text
// pos: static check for ADR 0011 migration 000011; a live server is not required
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func migrationDir(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "migrations")
}

func TestClusterOwnershipMigrationText(t *testing.T) {
	dir := migrationDir(t)
	up, err := os.ReadFile(filepath.Join(dir, "000011_cluster_ownership.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	down, err := os.ReadFile(filepath.Join(dir, "000011_cluster_ownership.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	guards, err := os.ReadFile(filepath.Join(dir, "000011_cluster_ownership.guards.sql"))
	if err != nil {
		t.Fatal(err)
	}
	upText := string(up)
	downText := string(down)
	guardText := string(guards)

	for _, line := range strings.Split(upText, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") || trimmed == "" {
			continue
		}
		upper := strings.ToUpper(trimmed)
		if strings.Contains(upper, "CREATE TRIGGER") || strings.Contains(upper, "CREATE PROCEDURE") || strings.Contains(upper, "BEGIN") {
			t.Fatalf("up file must not carry a compound trigger or procedure body: %s", trimmed)
		}
	}
	for _, want := range []string{
		"lease_expire_at > NOW(6)",
		"INTERVAL 5 SECOND",
		"log_bin_trust_function_creators",
		"PRIVILEGE_TYPE = 'SUPER'",
		"SET SESSION lock_wait_timeout = 10",
		"row_generation",
		"failed_reason",
		"tail_unreadable_since",
		"pending_delete",
		"CHANGE COLUMN version build_version",
		"cluster_protocol_version",
		"CREATE TABLE task_chain",
		"CREATE TABLE task_node_refusal",
		"CREATE TABLE meta_canary",
		"CREATE TABLE cluster_storage",
		"failed_reason = 'LEGACY_UNKNOWN'",
		"row_generation = row_generation + 1",
		"INSERT IGNORE INTO task_chain",
		"verified_at",
		"DEFAULT ''''",
	} {
		if !strings.Contains(upText, want) {
			t.Fatalf("up file missing %q", want)
		}
	}
	if strings.Index(upText, "lease_expire_at > NOW(6)") > strings.Index(upText, "ADD COLUMN row_generation") {
		t.Fatal("precheck must run before the row_generation column is added")
	}
	if strings.Index(upText, "SET SESSION lock_wait_timeout = 10") > strings.Index(upText, "ADD COLUMN row_generation") {
		t.Fatal("lock_wait_timeout must be set before DDL")
	}
	if strings.Index(upText, "failed_reason = 'LEGACY_UNKNOWN'") > strings.Index(guardText, "CREATE") {
		// backfill lives in the up file; the trigger lives in guards, installed after up
	}
	if !strings.Contains(upText, "WHERE state = 'FAILED'") || !strings.Contains(upText, "AND desired_run = 'STOP'") {
		t.Fatal("backfill must rewrite only FAILED rows whose desired run is STOP")
	}

	proc := strings.Index(downText, "DROP PROCEDURE IF EXISTS binlog_server_kill_trx")
	trig := strings.Index(downText, "DROP TRIGGER IF EXISTS backup_tasks_bu_generation")
	col := strings.Index(downText, "DROP COLUMN row_generation")
	if proc < 0 || trig < 0 || col < 0 || !(proc < trig && trig < col) {
		t.Fatal("down must drop the procedure, then the trigger, then row_generation")
	}
	var downSQL strings.Builder
	for _, line := range strings.Split(downText, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue
		}
		downSQL.WriteString(line)
		downSQL.WriteByte('\n')
	}
	if strings.Contains(downSQL.String(), "LEGACY_UNKNOWN") || strings.Contains(downSQL.String(), "desired_run") {
		t.Fatal("down must not write LEGACY_UNKNOWN back to desired STOP")
	}
	if !strings.Contains(downText, "cluster_protocol_version >= 11") {
		t.Fatal("down must refuse a live protocol 11 registration")
	}
	if !strings.Contains(downText, "pending_delete <> 0") {
		t.Fatal("down must refuse and then delete pending_delete rows")
	}
	if !strings.Contains(downText, "SET SESSION lock_wait_timeout = 10") {
		t.Fatal("down must set lock_wait_timeout")
	}

	if !strings.Contains(guardText, "backup_tasks update must bump row_generation") {
		t.Fatal("trigger signal text drifted")
	}
	if !strings.Contains(guardText, "SQL SECURITY DEFINER") {
		t.Fatal("procedure must be SQL SECURITY DEFINER")
	}
	if !strings.Contains(guardText, "IN p_trx_id VARCHAR(64)") {
		t.Fatal("procedure argument must be VARCHAR")
	}
	if !strings.Contains(guardText, "mysql.rds_kill") || !strings.Contains(guardText, "performance_schema.data_locks") {
		t.Fatal("procedure must name rds_kill and data_locks")
	}
	parts := splitGuardStatements(guardText)
	if len(parts) != 2 {
		t.Fatalf("guards split into %d statements, want 2", len(parts))
	}
	if !strings.Contains(strings.ToUpper(parts[0]), "TRIGGER") || !strings.Contains(strings.ToUpper(parts[1]), "PROCEDURE") {
		t.Fatal("guards order must be trigger then procedure")
	}
}

func guardsSQLPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(migrationDir(t), "000011_cluster_ownership.guards.sql")
}

func installOwnershipGuards(t *testing.T, db *sql.DB) {
	t.Helper()
	if err := InstallClusterOwnershipGuards(context.Background(), db, guardsSQLPath(t)); err != nil {
		t.Fatal(err)
	}
}
