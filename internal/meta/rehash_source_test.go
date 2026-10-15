// Package meta provides module-level functionality for meta.
// input: sqlmock for listSealedChecksumsSQL; optionally BINLOG_TEST_MYSQL_ROOT_DSNS for a real catalog with real MySQL 8.0 segments on disk
// output: the health rehasher's catalog source (#380 QA P1): ListSealedChecksums returns the 000008 sha256 of sealed rows and nothing on schema 6/7, and on a real catalog an in-place flip that keeps size and mtime is a rehash mismatch after reading the sealed bytes
// pos: ADR 0009 phase 2 P2-3 QA round 1
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/healthstore"
	"binlog_server/internal/tasks"

	"github.com/DATA-DOG/go-sqlmock"
	mysqlDriver "github.com/go-sql-driver/mysql"
)

func TestListSealedChecksums(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	mock.ExpectQuery(regexp.QuoteMeta(listSealedChecksumsSQL)).WithArgs("1").
		WillReturnRows(sqlmock.NewRows([]string{"file_name", "epoch", "size_bytes", "sha256"}).AddRow("mysql-bin.000004", int64(0), int64(2354), "ab"))
	rows, err := store.ListSealedChecksums(context.Background(), "1")
	if err != nil || len(rows) != 1 || rows[0].SHA256 != "ab" || rows[0].SizeBytes != 2354 || rows[0].FileName != "mysql-bin.000004" {
		t.Fatalf("rows %+v err %v", rows, err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(listSealedChecksumsSQL)).WithArgs("1").
		WillReturnError(&mysqlDriver.MySQLError{Number: 1054, Message: "Unknown column 'sha256'"})
	if rows, err := store.ListSealedChecksums(context.Background(), "1"); err != nil || len(rows) != 0 {
		t.Fatalf("schema 7: rows %+v err %v", rows, err)
	}
	// Remembered: no second query.
	if rows, err := store.ListSealedChecksums(context.Background(), "1"); err != nil || len(rows) != 0 {
		t.Fatalf("remembered: rows %+v err %v", rows, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// realCatalogTask is a scheduler on a real catalog with the real MySQL 8.0
// segments on disk and the sealed rows the runner writes (no ledger file).
func realCatalogTask(t *testing.T, root, name string) (*tasks.Scheduler, tasks.Task, string, string, int64) {
	t.Helper()
	ctx := context.Background()
	src := filepath.Join("..", "tasks", "testdata", "real-mysql80")
	db, m := scratchDB(t, root, name)
	if err := m.Up(); err != nil {
		t.Fatal(err)
	}
	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	data := t.TempDir()
	s := tasks.NewScheduler(tasks.WithDataDir(data), tasks.WithFileStore(store))
	task, err := s.CreateTaskFromSpec("mysql", name, &tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: "mysql"}, &tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: "2887ee3f-c849-11f1-8ab4-0242ac11000c:1-5"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(data, task.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, n := range []string{"mysql-bin.000004", "mysql-bin.000005"} {
		b, err := os.ReadFile(filepath.Join(src, n))
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		facts, err := binlog.ReadSealFacts(p)
		if err != nil {
			t.Fatal(err)
		}
		total += facts.Size
		now := time.Now()
		row := tasks.BinlogFile{TaskID: task.ID, FileName: n, FilePath: p, State: "SEALED", SizeBytes: facts.Size, StartPos: 4, EndPos: tasks.FilePos(facts.Size),
			CreatedAt: now, SealedAt: now, UploadState: "LOCAL_ONLY", SHA256: facts.SHA256}
		if err := store.UpsertBinlogFile(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	return s, task, data, dir, total
}

func realEval(s *tasks.Scheduler, data string) (*tasks.Evaluator, *healthstore.FileStore) {
	hs := healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))
	return tasks.NewEvaluator(s, tasks.EvaluatorConfig{Mode: tasks.EvaluatorOn, Store: hs, NodeID: "n", Instance: "i", Sweep: time.Minute, Debounce: time.Millisecond}), hs
}

func TestRehasherReadsCatalogChecksumOnMySQL(t *testing.T) {
	ctx := context.Background()
	for i, root := range rootDSNs(t) {
		s, _, data, dir, total := realCatalogTask(t, root, fmt.Sprintf("p23_rehash_src_%d", i))
		e, _ := realEval(s, data)
		r := tasks.NewRehasher(e, 64<<20)
		target := filepath.Join(dir, "mysql-bin.000005")
		info, _ := os.Stat(target)
		raw, _ := os.ReadFile(target)
		for j := len(raw) - 4; j < len(raw); j++ {
			raw[j] ^= 0xff
		}
		if err := os.WriteFile(target, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(target, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
		for j := 0; j < 2; j++ {
			if _, err := r.Step(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if st := r.Stats(); st.Mismatches != 1 || st.Bytes != total || st.Unavailable != 0 {
			t.Fatalf("stats %+v, want one mismatch after reading %d bytes", st, total)
		}
	}
}

func TestEvaluatorSeesHandAddedCopyOnMySQLCatalog(t *testing.T) {
	ctx := context.Background()
	for i, root := range rootDSNs(t) {
		s, task, data, dir, _ := realCatalogTask(t, root, fmt.Sprintf("p23_handcopy_%d", i))
		e, hs := realEval(s, data)
		src := filepath.Join(dir, "mysql-bin.000004")
		b, _ := os.ReadFile(src)
		if err := os.WriteFile(src+".sealed.e3", b, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := e.SweepNow(ctx); err != nil {
			t.Fatal(err)
		}
		open, err := hs.ListOpenFindingsForTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(open) != 1 || open[0].Kind != "SEGMENT_OVERLAP" || open[0].Subject != "mysql-bin.000004.sealed.e3" {
			t.Fatalf("open %+v, want one SEGMENT_OVERLAP for the hand-added copy", open)
		}
	}
}
