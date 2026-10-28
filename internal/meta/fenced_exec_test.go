// Package meta provides module-level functionality for meta.
// input: sqlmock lease rows and backup_tasks upserts under FencedExec / UpsertTaskFenced
// output: P2-6a coverage for ADR 0009 §8.4 / #360: a stale owner's STOPPED write is refused when task_leases names a newer holder; a matching lease or one released at the writer's epoch allows the write; a release at a newer epoch refuses it (#421); with no lease row the task row decides
// pos: unit tests for meta.fencedExec (P2-6a); no dockerd
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"binlog_server/internal/healthstore"
	"binlog_server/internal/tasks"

	"github.com/DATA-DOG/go-sqlmock"
)

const selectLeaseForUpdateSQL = `SELECT owner_worker_id, epoch FROM task_leases WHERE task_id = ? FOR UPDATE`

func stoppedTask() tasks.Task {
	return tasks.Task{
		ID: "1", Name: "t", ClusterKey: "k", State: tasks.StateStopped,
		OwnerWorkerID: "", Epoch: 0, DesiredRun: tasks.TaskDesiredStop,
		Source: tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "u", Flavor: "mysql", ServerID: 1},
		Start:  tasks.StartConfig{Mode: tasks.StartModeLatest},
	}
}

// TestUpsertTaskFenced_StaleOwnerRefused is #355 run 3 / #360: worker B@epoch 1
// must not write STOPPED after task_leases already names A@epoch 2.
func TestUpsertTaskFenced_StaleOwnerRefused(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, 5*time.Second)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectLeaseForUpdateSQL)).
		WithArgs("1").
		WillReturnRows(sqlmock.NewRows([]string{"owner_worker_id", "epoch"}).AddRow("worker-a", int64(2)))
	mock.ExpectRollback()

	err = store.UpsertTaskFenced(context.Background(), stoppedTask(), "worker-b", 1)
	if !errors.Is(err, healthstore.ErrFenced) {
		t.Fatalf("UpsertTaskFenced: %v, want ErrFenced", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql: %v", err)
	}
}

func TestUpsertTaskFenced_MatchingOwnerWrites(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, 5*time.Second)

	task := tasks.Task{
		ID: "1", Name: "t", ClusterKey: "k", State: tasks.StateStopping,
		OwnerWorkerID: "worker-b", Epoch: 1, LastError: "lease lost",
		DesiredRun: tasks.TaskDesiredStop,
		Source:     tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "u", Flavor: "mysql", ServerID: 1},
		Start:      tasks.StartConfig{Mode: tasks.StartModeLatest},
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectLeaseForUpdateSQL)).
		WithArgs("1").
		WillReturnRows(sqlmock.NewRows([]string{"owner_worker_id", "epoch"}).AddRow("worker-b", int64(1)))
	mock.ExpectQuery(regexp.QuoteMeta(loadTaskRunStateSQL)).
		WithArgs("1").
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}))
	mock.ExpectExec(regexp.QuoteMeta(upsertTaskSQL)).
		WithArgs("1", "t", "k", "STOPPING", "lease lost", "worker-b", int64(1), "", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), "STOP", int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := store.UpsertTaskFenced(context.Background(), task, "worker-b", 1); err != nil {
		t.Fatalf("UpsertTaskFenced: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql: %v", err)
	}
}

func TestUpsertTaskFenced_ReleasedLeaseAllowsStopped(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, 5*time.Second)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectLeaseForUpdateSQL)).
		WithArgs("1").
		WillReturnRows(sqlmock.NewRows([]string{"owner_worker_id", "epoch"}).AddRow("", int64(1)))
	mock.ExpectQuery(regexp.QuoteMeta(loadTaskRunStateSQL)).
		WithArgs("1").
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow("run-1"))
	mock.ExpectExec(regexp.QuoteMeta(upsertTaskSQL)).
		WithArgs("1", "t", "k", "STOPPED", "", "", int64(0), "", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), "STOP", int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta(finishTaskRunSQL)).
		WithArgs(sqlmock.AnyArg(), "NORMAL_STOP", "run-1").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := store.UpsertTaskFenced(context.Background(), stoppedTask(), "worker-b", 1); err != nil {
		t.Fatalf("UpsertTaskFenced: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql: %v", err)
	}
}

func TestUpsertTaskFenced_NoLeaseRowAllowsWrite(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, 5*time.Second)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectLeaseForUpdateSQL)).
		WithArgs("1").
		WillReturnRows(sqlmock.NewRows([]string{"owner_worker_id", "epoch"}))
	mock.ExpectQuery(regexp.QuoteMeta(selectTaskOwnerForUpdateSQL)).
		WithArgs("1").
		WillReturnRows(sqlmock.NewRows([]string{"owner_worker_id", "epoch"}).AddRow("worker-b", int64(1)))
	mock.ExpectQuery(regexp.QuoteMeta(loadTaskRunStateSQL)).
		WithArgs("1").
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}))
	mock.ExpectExec(regexp.QuoteMeta(upsertTaskSQL)).
		WithArgs("1", "t", "k", "STOPPED", "", "", int64(0), "", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), "STOP", int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := store.UpsertTaskFenced(context.Background(), stoppedTask(), "worker-b", 1); err != nil {
		t.Fatalf("UpsertTaskFenced: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql: %v", err)
	}
}

func TestFencedExec_CallbackSkippedWhenFenced(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, 5*time.Second)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectLeaseForUpdateSQL)).
		WithArgs("task-x").
		WillReturnRows(sqlmock.NewRows([]string{"owner_worker_id", "epoch"}).AddRow("other", int64(9)))
	mock.ExpectRollback()

	ran := false
	err = store.FencedExec(context.Background(), "task-x", "me", 1, func(*sql.Tx) error {
		ran = true
		return nil
	})
	if !errors.Is(err, healthstore.ErrFenced) {
		t.Fatalf("FencedExec: %v, want ErrFenced", err)
	}
	if ran {
		t.Fatal("callback ran despite fence")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql: %v", err)
	}
}

// #421: n2 claimed epoch 2, failed and released the lease (” / 2). The old
// owner (worker-b, epoch 1) wakes up; its STOPPED write must not land.
func TestUpsertTaskFenced_ReleasedAtNewerEpochRefused(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, 5*time.Second)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectLeaseForUpdateSQL)).
		WithArgs("1").
		WillReturnRows(sqlmock.NewRows([]string{"owner_worker_id", "epoch"}).AddRow("", int64(2)))
	mock.ExpectRollback()

	if err := store.UpsertTaskFenced(context.Background(), stoppedTask(), "worker-b", 1); !errors.Is(err, healthstore.ErrFenced) {
		t.Fatalf("UpsertTaskFenced: %v, want ErrFenced", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql: %v", err)
	}
}

// With no task_leases row the task row is the fence: another owner or a
// newer epoch on it refuses the write.
func TestUpsertTaskFenced_NoLeaseRowTaskRowNamesOtherOwner(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, 5*time.Second)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectLeaseForUpdateSQL)).
		WithArgs("1").
		WillReturnRows(sqlmock.NewRows([]string{"owner_worker_id", "epoch"}))
	mock.ExpectQuery(regexp.QuoteMeta(selectTaskOwnerForUpdateSQL)).
		WithArgs("1").
		WillReturnRows(sqlmock.NewRows([]string{"owner_worker_id", "epoch"}).AddRow("worker-a", int64(3)))
	mock.ExpectRollback()

	if err := store.UpsertTaskFenced(context.Background(), stoppedTask(), "worker-b", 1); !errors.Is(err, healthstore.ErrFenced) {
		t.Fatalf("UpsertTaskFenced: %v, want ErrFenced", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql: %v", err)
	}
}
