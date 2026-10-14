// Package meta provides module-level functionality for meta.
// input: sqlmock for the binlog_files upsert and the seal-facts update
// output: assertions that a ResetSealFacts row clears the facts, that a sealed row with facts writes them after the upsert (UTC times, NULL for an unknown span), that a row without facts or not sealed writes nothing more, and that a catalog without the 000008 columns (1054) is skipped once and then never asked again
// pos: persistence coverage for ADR 0009 phase 1, P1-4
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"regexp"
	"testing"
	"time"

	"binlog_server/internal/tasks"

	"github.com/DATA-DOG/go-sqlmock"
	mysqlDriver "github.com/go-sql-driver/mysql"
)

func TestUpsertBinlogFile_WritesSealFacts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	ctx := context.Background()
	first := time.Date(2026, 10, 9, 7, 48, 11, 0, time.FixedZone("x", 8*3600))
	last := first.Add(40 * time.Second)
	sealed := tasks.BinlogFile{TaskID: "1", FileName: "mysql-bin.000003", FilePath: "/d/1/mysql-bin.000003.sealed.e1", Epoch: 1,
		State: "SEALED", SizeBytes: 10, StartPos: 4, EndPos: 10, CreatedAt: first, SealedAt: last,
		FirstEventAt: &first, LastEventAt: &last, SHA256: "ab"}

	mock.ExpectExec(regexp.QuoteMeta(upsertBinlogFileSQL)).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta(upsertSealFactsSQL)).
		WithArgs(first.UTC(), last.UTC(), "ab", "1", "mysql-bin.000003", int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.UpsertBinlogFile(ctx, sealed); err != nil {
		t.Fatal(err)
	}

	noSpan := sealed
	noSpan.FirstEventAt, noSpan.LastEventAt = nil, nil
	mock.ExpectExec(regexp.QuoteMeta(upsertBinlogFileSQL)).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta(upsertSealFactsSQL)).
		WithArgs(nil, nil, "ab", "1", "mysql-bin.000003", int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.UpsertBinlogFile(ctx, noSpan); err != nil {
		t.Fatal(err)
	}

	bare := sealed
	bare.FirstEventAt, bare.LastEventAt, bare.SHA256 = nil, nil, ""
	open := sealed
	open.State, open.FilePath = "OPEN", "/d/1/mysql-bin.000004.open.e1"
	for _, f := range []tasks.BinlogFile{bare, open} {
		mock.ExpectExec(regexp.QuoteMeta(upsertBinlogFileSQL)).WillReturnResult(sqlmock.NewResult(1, 1))
		if err := store.UpsertBinlogFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	mock.ExpectExec(regexp.QuoteMeta(upsertBinlogFileSQL)).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta(upsertSealFactsSQL)).
		WillReturnError(&mysqlDriver.MySQLError{Number: 1054, Message: "Unknown column 'first_event_at' in 'field list'"})
	if err := store.UpsertBinlogFile(ctx, sealed); err != nil {
		t.Fatalf("schema 7 catalog: %v", err)
	}
	mock.ExpectExec(regexp.QuoteMeta(upsertBinlogFileSQL)).WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.UpsertBinlogFile(ctx, sealed); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpsertBinlogFile_ResetSealFactsClearsThem(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	row := tasks.BinlogFile{TaskID: "1", FileName: "mysql-bin.000003", FilePath: "/d/1/mysql-bin.000003", State: "SEALED",
		SizeBytes: 10, StartPos: 4, EndPos: 10, ResetSealFacts: true}
	mock.ExpectExec(regexp.QuoteMeta(upsertBinlogFileSQL)).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta(upsertSealFactsSQL)).
		WithArgs(nil, nil, nil, "1", "mysql-bin.000003", int64(0)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.UpsertBinlogFile(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
