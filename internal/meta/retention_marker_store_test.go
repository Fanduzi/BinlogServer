// Package meta provides module-level functionality for meta.
// input: sqlmock for retention_removed_at with and without a fence token
// output: proof that a token locks task_leases in the same transaction as the marker update, that no token is one UPDATE, and that MySQL 1054 returns ErrRetentionMarkerUnsupported and the next call probes again (design 0009 §26.2, #341)
// pos: persistence coverage for migration 000010
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"binlog_server/internal/tasks"

	"github.com/DATA-DOG/go-sqlmock"
	mysqlDriver "github.com/go-sql-driver/mysql"
)

func TestMarkRetentionRemoved_UnfencedIsOneStatement(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	at := time.Date(2026, 10, 19, 1, 2, 3, 0, time.FixedZone("x", 8*3600))
	mock.ExpectExec(regexp.QuoteMeta(markRetentionRemovedSQL)).
		WithArgs(at.UTC(), "1", "mysql-bin.000001", int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.MarkRetentionRemoved(context.Background(), "1", "mysql-bin.000001", 1, at); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMarkRetentionRemoved_FencedWithLease(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	at := time.Date(2026, 10, 19, 1, 2, 3, 0, time.UTC)
	ctx := tasks.WithFenceIdentity(context.Background(), tasks.FenceIdentity{TaskID: "7", Owner: "worker-a", Epoch: 2})
	mock.ExpectBegin()
	mock.ExpectQuery("FROM task_leases WHERE task_id = \\? FOR UPDATE").
		WithArgs("7").
		WillReturnRows(sqlmock.NewRows([]string{"owner_worker_id", "epoch"}).AddRow("worker-a", 2))
	mock.ExpectExec(regexp.QuoteMeta(markRetentionRemovedSQL)).
		WithArgs(at.UTC(), "7", "mysql-bin.000001", int64(2)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := store.MarkRetentionRemoved(ctx, "7", "mysql-bin.000001", 2, at); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMarkRetentionRemoved_MissingColumnProbesAgain(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	unknown := &mysqlDriver.MySQLError{Number: 1054, Message: "Unknown column 'retention_removed_at'"}
	at := time.Date(2026, 10, 19, 1, 2, 3, 0, time.UTC)
	mock.ExpectExec(regexp.QuoteMeta(markRetentionRemovedSQL)).WillReturnError(unknown)
	if err := store.MarkRetentionRemoved(context.Background(), "1", "mysql-bin.000001", 1, at); !errors.Is(err, tasks.ErrRetentionMarkerUnsupported) {
		t.Fatalf("first 1054: %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(probeRetentionColumnSQL)).WillReturnError(unknown)
	if err := store.MarkRetentionRemoved(context.Background(), "1", "mysql-bin.000001", 1, at); !errors.Is(err, tasks.ErrRetentionMarkerUnsupported) {
		t.Fatalf("probe still absent: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
