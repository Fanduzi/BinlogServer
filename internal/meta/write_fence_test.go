package meta

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"
)

func fencedCtx(owner string, epoch int64) context.Context {
	return tasks.WithFenceIdentity(context.Background(), tasks.FenceIdentity{TaskID: "7", Owner: owner, Epoch: epoch})
}

// #417: a checkpoint from an owner whose epoch was taken over is refused
// inside the transaction that would have written it.
func TestUpsertCheckpoint_FencedByNewerEpoch(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT owner_worker_id, epoch FROM task_leases WHERE task_id = \\? FOR UPDATE").
		WithArgs("7").WillReturnRows(sqlmock.NewRows([]string{"owner_worker_id", "epoch"}).AddRow("worker-b", 3))
	mock.ExpectRollback()
	err = store.UpsertCheckpoint(fencedCtx("worker-a", 2), "7", binlog.Checkpoint{File: "mysql-bin.000003", Pos: 120})
	if !errors.Is(err, tasks.ErrWriteFenced) {
		t.Fatalf("err = %v, want ErrWriteFenced", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpsertCheckpoint_FenceMatchWritesInSameTx(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	mock.ExpectBegin()
	mock.ExpectQuery("FROM task_leases WHERE task_id = \\? FOR UPDATE").
		WithArgs("7").WillReturnRows(sqlmock.NewRows([]string{"owner_worker_id", "epoch"}).AddRow("worker-a", 2))
	mock.ExpectExec("INSERT INTO backup_checkpoints").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	if err := store.UpsertCheckpoint(fencedCtx("worker-a", 2), "7", binlog.Checkpoint{File: "mysql-bin.000003", Pos: 120}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A released or missing lease row is not ownership: a binlog_files row is
// refused too (unlike the STOPPED convergence write of P2-6a).
func TestUpsertBinlogFile_FencedWhenLeaseReleasedOrMissing(t *testing.T) {
	for name, rows := range map[string]*sqlmock.Rows{
		"released": sqlmock.NewRows([]string{"owner_worker_id", "epoch"}).AddRow("", 2),
		"missing":  sqlmock.NewRows([]string{"owner_worker_id", "epoch"}),
	} {
		t.Run(name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			store := newMySQLTaskStoreFromDB(db, time.Second)
			mock.ExpectBegin()
			mock.ExpectQuery("FROM task_leases").WithArgs("7").WillReturnRows(rows)
			mock.ExpectRollback()
			err = store.UpsertBinlogFile(fencedCtx("worker-a", 2), tasks.BinlogFile{TaskID: "7", FileName: "mysql-bin.000003", Epoch: 2, State: "OPEN", EndPos: 120})
			if !errors.Is(err, tasks.ErrWriteFenced) {
				t.Fatalf("err = %v, want ErrWriteFenced", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Without a fence identity (standalone, background upload retry) the write
// is the old single statement.
func TestUpsertCheckpoint_NoIdentityUnchanged(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	mock.ExpectExec("INSERT INTO backup_checkpoints").WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.UpsertCheckpoint(context.Background(), "7", binlog.Checkpoint{File: "f", Pos: 4}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
