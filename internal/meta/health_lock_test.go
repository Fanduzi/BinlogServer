// Package meta provides module-level functionality for meta.
// input: sqlmock for the health insert, the task_leases lock, and a slow health update
// output: proof that a 1205 and a confirmed lock wait are ErrLockWait, a plain slow statement is not, and a 1213 is retried once
// pos: ADR 0011 PR-2 lock-stall classification
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"errors"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"

	"github.com/DATA-DOG/go-sqlmock"
	mysqlDriver "github.com/go-sql-driver/mysql"
)

func TestApplyTransitionsInsertLockWaitIsStall(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	store.session = &sessionState{bound: 5 * time.Second, expectSec: 5}
	store.lockWaitSeen = func(int64) bool { return false }
	mock.ExpectQuery("SELECT VERSION\\(\\)").WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("8.0.36"))
	mock.ExpectQuery("SELECT CONNECTION_ID\\(\\)").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(7)))
	mock.ExpectExec("INSERT IGNORE INTO task_health").
		WillReturnError(&mysqlDriver.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded; try restarting transaction"})
	_, err = store.ApplyTransitions(context.Background(), "t1", health.Fence{Mode: health.FenceStandalone, Node: "n", Instance: "i"}, nil, health.TaskHealth{TaskID: "t1"})
	if !errors.Is(err, healthstore.ErrLockWait) {
		t.Fatalf("insert 1205: %v, want ErrLockWait", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestApplyTransitionsSlowQueryIsNotStall(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	store.session = &sessionState{bound: 5 * time.Second, expectSec: 5}
	store.lockWaitSeen = func(int64) bool { return false }
	mock.ExpectQuery("SELECT VERSION\\(\\)").WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("8.0.36"))
	mock.ExpectQuery("SELECT CONNECTION_ID\\(\\)").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(7)))
	mock.ExpectExec("INSERT IGNORE INTO task_health").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT CONNECTION_ID\\(\\)").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(7)))
	mock.ExpectQuery("SELECT fence_mode").WillReturnRows(sqlmock.NewRows([]string{"m", "e", "n", "i", "g", "c"}).AddRow("", 0, "", "", 0, ""))
	mock.ExpectExec("UPDATE task_health").WillReturnError(context.DeadlineExceeded)
	mock.ExpectRollback()
	_, err = store.ApplyTransitions(context.Background(), "t1", health.Fence{Mode: health.FenceStandalone, Node: "n", Instance: "i"}, nil, health.TaskHealth{TaskID: "t1", Status: health.StatusHealthy})
	if errors.Is(err, healthstore.ErrLockWait) {
		t.Fatalf("slow update counted as a lock stall: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow update: %v, want DeadlineExceeded", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestApplyTransitionsDeadlineOnLeaseLock(t *testing.T) {
	for _, waiting := range []bool{true, false} {
		waiting := waiting
		t.Run(map[bool]string{true: "confirmed", false: "not-waiting"}[waiting], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			store := newMySQLTaskStoreFromDB(db, time.Second)
			store.session = &sessionState{bound: 5 * time.Second, expectSec: 5}
			store.lockWaitSeen = func(int64) bool { return waiting }
			mock.ExpectQuery("SELECT VERSION\\(\\)").WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("8.0.36"))
			mock.ExpectQuery("SELECT CONNECTION_ID\\(\\)").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(9)))
			mock.ExpectExec("INSERT IGNORE INTO task_health").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT CONNECTION_ID\\(\\)").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(9)))
			mock.ExpectQuery("FROM task_leases").WillReturnError(context.DeadlineExceeded)
			mock.ExpectRollback()
			_, err = store.ApplyTransitions(context.Background(), "t1", health.Fence{Mode: health.FenceCluster, Node: "n", Instance: "i", TaskEpoch: 1}, nil, health.TaskHealth{TaskID: "t1"})
			if waiting {
				if !errors.Is(err, healthstore.ErrLockWait) {
					t.Fatalf("confirmed lock deadline: %v, want ErrLockWait", err)
				}
			} else if errors.Is(err, healthstore.ErrLockWait) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("unconfirmed deadline: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestApplyTransitionsRetriesDeadlockOnce(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	deadlock := &mysqlDriver.MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock; try restarting transaction"}
	mock.ExpectQuery("SELECT VERSION\\(\\)").WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("8.0.36"))
	mock.ExpectExec("INSERT IGNORE INTO task_health").WillReturnError(deadlock)
	mock.ExpectExec("INSERT IGNORE INTO task_health").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT fence_mode").WillReturnRows(sqlmock.NewRows([]string{"m", "e", "n", "i", "g", "c"}).AddRow("", 0, "", "", 0, ""))
	mock.ExpectExec("UPDATE task_health").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	_, err = store.ApplyTransitions(context.Background(), "t1", health.Fence{Mode: health.FenceStandalone, Node: "n", Instance: "i"}, nil, health.TaskHealth{TaskID: "t1", Status: health.StatusHealthy})
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	mock.ExpectExec("INSERT IGNORE INTO task_health").WillReturnError(deadlock)
	mock.ExpectExec("INSERT IGNORE INTO task_health").WillReturnError(deadlock)
	_, err = store.ApplyTransitions(context.Background(), "t1", health.Fence{Mode: health.FenceStandalone, Node: "n", Instance: "i"}, nil, health.TaskHealth{TaskID: "t1"})
	if errors.Is(err, healthstore.ErrLockWait) || !isDeadlock(err) {
		t.Fatalf("second 1213: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
