// Package meta provides module-level functionality for meta.
// input: migration 000011 up and down SQL, the task-write sources, and a sqlmock connection for the innodb_trx precheck
// output: proof that the up file stays inside the schema grant, the backfill bumps row_generation, down drops a routine only when information_schema shows it, and a missing PROCESS privilege is a warning
// pos: static check for ADR 0011 migration 000011; a live server is not required
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	mysqlDriver "github.com/go-sql-driver/mysql"
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
	if _, err := os.Stat(filepath.Join(dir, "000011_cluster_ownership.guards.sql")); !os.IsNotExist(err) {
		t.Fatal("guards file must not be installed by migrate")
	}
	upText := string(up)
	downText := string(down)

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
	var upSQL strings.Builder
	for _, line := range strings.Split(upText, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue
		}
		upSQL.WriteString(line)
		upSQL.WriteByte('\n')
	}
	for _, banned := range []string{
		"innodb_trx",
		"log_bin_trust_function_creators",
		"PRIVILEGE_TYPE",
		"CREATE TRIGGER",
		"CREATE PROCEDURE",
		"INTERVAL 5 SECOND",
	} {
		if strings.Contains(upSQL.String(), banned) {
			t.Fatalf("up file still requires a privilege beyond the schema grant: %s", banned)
		}
	}
	for _, want := range []string{
		"lease_expire_at > NOW(6)",
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
	if !strings.Contains(upText, "WHERE state = 'FAILED'") || !strings.Contains(upText, "AND desired_run = 'STOP'") {
		t.Fatal("backfill must rewrite only FAILED rows whose desired run is STOP")
	}

	proc := strings.Index(downText, "binlog_server_kill_trx")
	trig := strings.Index(downText, "backup_tasks_bu_generation")
	col := strings.Index(downText, "DROP COLUMN row_generation")
	if proc < 0 || trig < 0 || col < 0 || proc >= trig || trig >= col {
		t.Fatal("down must name the procedure, then the trigger, then drop row_generation")
	}
	if !strings.Contains(downText, "information_schema.routines") || !strings.Contains(downText, "information_schema.triggers") {
		t.Fatal("down must drop a routine only when information_schema shows it")
	}
	if strings.Contains(downText, "DROP PROCEDURE IF EXISTS") || strings.Contains(downText, "DROP TRIGGER IF EXISTS") {
		t.Fatal("down must not issue an unconditional DROP of a routine")
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
}

func TestBackupTaskUpdatesShareGenerationBump(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Dir(file)
	store, err := os.ReadFile(filepath.Join(dir, "mysql_store.go"))
	if err != nil {
		t.Fatal(err)
	}
	gate, err := os.ReadFile(filepath.Join(dir, "cluster_gate.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(store) + "\n" + string(gate)
	if !strings.Contains(src, `const rowGenerationBump = "row_generation = row_generation + 1"`) {
		t.Fatal("bump constant drifted")
	}
	if strings.Count(src, "row_generation = row_generation + 1") != 1 {
		t.Fatal("bump literal must live only in rowGenerationBump")
	}
	for _, name := range []string{"savePendingDumpSQL", "upsertTaskSQL", "reconcileLegacyDesiredRunSQL", "commitYieldSQL"} {
		if !constUsesBump(src, name) {
			t.Fatalf("%s must use rowGenerationBump", name)
		}
	}
	if strings.Contains(string(gate), "OR ? = 0") {
		t.Fatal("commitYieldSQL must not treat generation 0 as unread")
	}
}

func constUsesBump(src, name string) bool {
	marker := "const " + name
	i := strings.Index(src, marker)
	if i < 0 {
		return false
	}
	rest := src[i+len(marker):]
	end := len(rest)
	if n := strings.Index(rest, "\nconst "); n >= 0 && n < end {
		end = n
	}
	if n := strings.Index(rest, "\nfunc "); n >= 0 && n < end {
		end = n
	}
	return strings.Contains(rest[:end], "rowGenerationBump")
}

func TestCheckLongTransactions(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		if _, err := CheckLongTransactions(context.Background(), nil); err == nil {
			t.Fatal("nil db")
		}
	})
	t.Run("skip process", func(t *testing.T) {
		for _, number := range []uint16{1227, 1142} {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			mock.ExpectQuery("innodb_trx").WillReturnError(&mysqlDriver.MySQLError{Number: number, Message: "denied"})
			warning, err := CheckLongTransactions(context.Background(), db)
			_ = db.Close()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(warning, "skipped innodb_trx precheck") || !strings.Contains(warning, "PROCESS") {
				t.Fatalf("warning %q", warning)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("long transaction", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		mock.ExpectQuery("innodb_trx").WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(2))
		if _, err := CheckLongTransactions(context.Background(), db); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("clear", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		mock.ExpectQuery("innodb_trx").WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(0))
		warning, err := CheckLongTransactions(context.Background(), db)
		if err != nil || warning != "" {
			t.Fatalf("warning %q err %v", warning, err)
		}
	})
}

func TestMySQLTaskStore_GetTaskReadsRowGeneration(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer func() { _ = db.Close() }()

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	store.hasOwnershipCols = true
	now := time.Now()
	rows := sqlmock.NewRows(append(taskRowColumns(), "pending_delete", "row_generation")).AddRow(
		"7", "gen", "gen-key", "RUNNING", "SOURCE_UNREACHABLE: dial", "worker-a", int64(2), "run",
		`{"host":"127.0.0.1","port":3306,"user":"repl","flavor":"mysql","server_id":200001}`,
		`{"mode":"LATEST"}`, `{"dir":"./data"}`, now,
		"RUN", int64(1), int64(1), int64(0), int64(3), int64(3),
		0, int64(4),
	)
	mock.ExpectQuery(regexp.QuoteMeta(store.withPendingColumn(getTaskSQL))).
		WithArgs("7").
		WillReturnRows(rows)
	got, err := store.GetTask(context.Background(), "7")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !got.RowGenerationKnown || got.RowGeneration != 4 {
		t.Fatalf("generation known=%v value=%d", got.RowGenerationKnown, got.RowGeneration)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}
