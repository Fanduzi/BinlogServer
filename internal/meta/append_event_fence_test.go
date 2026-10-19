package meta

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"binlog_server/internal/tasks"
)

// Design 0009 §26.6: a RETENTION_REMOVED event from a run that lost its
// lease does not land. The insert shares the transaction that locks the
// lease row.
func TestAppendEvent_FencedByNewerEpoch(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	mock.ExpectBegin()
	mock.ExpectQuery("FROM task_leases WHERE task_id = \\? FOR UPDATE").
		WithArgs("7").WillReturnRows(sqlmock.NewRows([]string{"owner_worker_id", "epoch"}).AddRow("worker-b", 3))
	mock.ExpectRollback()
	err = store.AppendEvent(fencedCtx("worker-a", 2), tasks.TaskEvent{TaskID: "7", Type: "RETENTION_REMOVED", Message: "m removed by local retention", Time: time.Now()})
	if !errors.Is(err, tasks.ErrWriteFenced) {
		t.Fatalf("err = %v, want ErrWriteFenced", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAppendEvent_FenceMatchWritesInSameTx(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	mock.ExpectBegin()
	mock.ExpectQuery("FROM task_leases WHERE task_id = \\? FOR UPDATE").
		WithArgs("7").WillReturnRows(sqlmock.NewRows([]string{"owner_worker_id", "epoch"}).AddRow("worker-a", 2))
	mock.ExpectExec("INSERT INTO task_events").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	if err := store.AppendEvent(fencedCtx("worker-a", 2), tasks.TaskEvent{TaskID: "7", Type: "RETENTION_REMOVED", Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// §26.6: callers without a token (the scheduler's event lane, the finding
// sink) keep the single statement they had: one attempt, the store error
// returned as is, no retry loop holding the ordered lane during an outage.
func TestAppendEvent_NoTokenIsOneStatementWithoutRetry(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	mock.ExpectExec("INSERT INTO task_events").WillReturnError(errors.New("dial tcp 127.0.0.1:3306: connect: connection refused"))
	err = store.AppendEvent(context.Background(), tasks.TaskEvent{TaskID: "7", Type: "TASK_STARTED", Time: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v, want the store error from the one attempt", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A token for another task is not this task's fence: single statement.
func TestAppendEvent_OtherTaskTokenIsOneStatement(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	mock.ExpectExec("INSERT INTO task_events").WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.AppendEvent(fencedCtx("worker-a", 2), tasks.TaskEvent{TaskID: "8", Type: "TASK_STARTED", Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
