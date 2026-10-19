// Package meta provides module-level functionality for meta.
// input: sqlmock for the binlog_files upsert and the seal-facts update
// output: assertions that a ResetSealFacts row clears the facts, that a sealed row with facts writes them after the upsert (UTC times, NULL for an unknown span), that a row without facts or not sealed writes nothing more, that a catalog without the 000008 columns (1054) is skipped, that the next seal probes those columns and writes again when they return (#353), and that ProbeSealFacts opens only when the columns are absent and the schema is at least 8 or this process has seen them
// pos: persistence coverage for ADR 0009 phase 1, P1-4
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql"
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
	mock.ExpectQuery(regexp.QuoteMeta(probeSealColumnsSQL)).
		WillReturnError(&mysqlDriver.MySQLError{Number: 1054, Message: "Unknown column 'first_event_at'"})
	if err := store.UpsertBinlogFile(ctx, sealed); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// After a 1054, the next seal probes the 000008 columns. When they are back,
// the same process writes the facts again (#353, design 0009 §26.5).
func TestSealFacts_ProbeResumesAfterColumnsReturn(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	ctx := context.Background()
	first := time.Date(2026, 10, 9, 7, 48, 11, 0, time.UTC)
	sealed := tasks.BinlogFile{TaskID: "1", FileName: "mysql-bin.000003", FilePath: "/d/1/mysql-bin.000003", Epoch: 1,
		State: "SEALED", SizeBytes: 10, StartPos: 4, EndPos: 10, CreatedAt: first, SealedAt: first,
		FirstEventAt: &first, LastEventAt: &first, SHA256: "ab"}

	mock.ExpectExec(regexp.QuoteMeta(upsertBinlogFileSQL)).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta(upsertSealFactsSQL)).
		WillReturnError(&mysqlDriver.MySQLError{Number: 1054, Message: "Unknown column 'first_event_at'"})
	if err := store.UpsertBinlogFile(ctx, sealed); err != nil {
		t.Fatal(err)
	}

	mock.ExpectExec(regexp.QuoteMeta(upsertBinlogFileSQL)).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT first_event_at FROM binlog_files LIMIT 0")).
		WillReturnRows(sqlmock.NewRows([]string{"first_event_at"}))
	mock.ExpectExec(regexp.QuoteMeta(upsertSealFactsSQL)).
		WithArgs(first.UTC(), first.UTC(), "ab", "1", "mysql-bin.000003", int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.UpsertBinlogFile(ctx, sealed); err != nil {
		t.Fatalf("columns returned: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProbeSealFacts_SeenColumnsReopenAfterDown(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	ctx := context.Background()
	unknown := &mysqlDriver.MySQLError{Number: 1054, Message: "Unknown column 'first_event_at'"}

	probe := func(version int64, missing bool) (int, bool) {
		t.Helper()
		mock.ExpectQuery(regexp.QuoteMeta(currentSchemaVersionSQL)).
			WillReturnRows(sqlmock.NewRows([]string{"version", "dirty"}).AddRow(version, false))
		q := mock.ExpectQuery(regexp.QuoteMeta(probeSealColumnsSQL))
		if missing {
			q.WillReturnError(unknown)
		} else {
			q.WillReturnError(sql.ErrNoRows)
		}
		ver, absent, err := store.ProbeSealFacts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return ver, absent
	}

	if ver, absent := probe(7, true); ver != 7 || absent {
		t.Fatalf("schema 7 never seen: version=%d absent=%v", ver, absent)
	}
	if ver, absent := probe(8, true); ver != 8 || !absent {
		t.Fatalf("schema 8 missing columns: version=%d absent=%v", ver, absent)
	}
	if ver, absent := probe(8, false); ver != 8 || absent {
		t.Fatalf("columns returned: version=%d absent=%v", ver, absent)
	}
	if ver, absent := probe(7, true); ver != 7 || !absent {
		t.Fatalf("down to 7 after the columns were seen: version=%d absent=%v", ver, absent)
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
