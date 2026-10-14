// Package meta provides module-level functionality for meta.
// input: sqlmock for the task_findings statements; optionally BINLOG_TEST_META_DSN pointing at a schema 7 database
// output: assertions that a finding upsert binds every column (NULL resolved_at while open), a missing table maps to tasks.ErrFindingStoreAbsent, and on real MySQL a reopen keeps first_seen and a resolve keeps the row
// pos: persistence coverage for ADR 0009 findings
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/tasks"

	"github.com/DATA-DOG/go-sqlmock"
	mysqlDriver "github.com/go-sql-driver/mysql"
)

func TestUpsertFinding_BindsColumnsAndMapsMissingTable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	now := time.Date(2026, 10, 14, 1, 2, 3, 0, time.UTC)
	f := tasks.Finding{TaskID: "9", Kind: tasks.FindingTaskPersistFailed, Severity: tasks.FindingCritical, Detail: "d", FirstSeen: now, LastSeen: now, Count: 1}
	mock.ExpectExec(regexp.QuoteMeta(upsertFindingSQL)).
		WithArgs("9", "TASK_PERSIST_FAILED", "", "CRITICAL", "d", now, now, int64(1), nil).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.UpsertFinding(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec(regexp.QuoteMeta(upsertFindingSQL)).
		WillReturnError(&mysqlDriver.MySQLError{Number: 1146, Message: "Table 'meta.task_findings' doesn't exist"})
	if err := store.UpsertFinding(context.Background(), f); !errors.Is(err, tasks.ErrFindingStoreAbsent) {
		t.Fatalf("missing table err %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFindingsOnMySQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("BINLOG_TEST_META_DSN"))
	if dsn == "" {
		t.Skip("BINLOG_TEST_META_DSN is not set")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "DELETE FROM task_findings WHERE task_id = 'it-1'"); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	f := tasks.Finding{TaskID: "it-1", Kind: tasks.FindingEventPersistFailed, Subject: "TASK_CREATED", Severity: tasks.FindingWarning, Detail: "a", FirstSeen: t0, LastSeen: t0, Count: 1}
	if err := store.UpsertFinding(ctx, f); err != nil {
		t.Fatal(err)
	}
	f.FirstSeen, f.LastSeen, f.Count, f.Detail = t1, t1, 2, "b"
	if err := store.UpsertFinding(ctx, f); err != nil {
		t.Fatal(err)
	}
	open, err := store.ListFindings(ctx, "it-1", false, 10)
	if err != nil || len(open) != 1 || !open[0].FirstSeen.Equal(t0) || open[0].Count != 2 || open[0].Detail != "b" {
		t.Fatalf("open %+v err %v", open, err)
	}
	at := t1.Add(time.Minute)
	f.ResolvedAt = &at
	if err := store.UpsertFinding(ctx, f); err != nil {
		t.Fatal(err)
	}
	if open, _ := store.ListFindings(ctx, "it-1", false, 10); len(open) != 0 {
		t.Fatalf("resolved still open %+v", open)
	}
	all, err := store.ListFindings(ctx, "it-1", true, 10)
	if err != nil || len(all) != 1 || all[0].ResolvedAt == nil {
		t.Fatalf("history %+v err %v", all, err)
	}
	global, err := store.ListOpenFindings(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range global {
		if g.TaskID == "it-1" {
			t.Fatalf("resolved row in open list")
		}
	}
}
