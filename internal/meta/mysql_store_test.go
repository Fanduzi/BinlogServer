package meta

import (
	"context"
	"regexp"
	"testing"
	"time"

	"binlog_server/internal/tasks"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestMySQLTaskStore_UpsertTask(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	task := tasks.Task{
		ID:    "1",
		Name:  "cluster-a",
		State: tasks.StateRunning,
		Source: tasks.SourceConfig{
			Host:     "127.0.0.1",
			Port:     3306,
			User:     "repl",
			Password: "secret",
			Flavor:   "mysql",
			ServerID: 200001,
		},
		Start: tasks.StartConfig{Mode: tasks.StartModeLatest},
	}

	mock.ExpectExec(regexp.QuoteMeta(upsertTaskSQL)).
		WithArgs("1", "cluster-a", "RUNNING", "", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := store.UpsertTask(context.Background(), task); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestMySQLTaskStore_ListTasks(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	now := time.Now()

	rows := sqlmock.NewRows([]string{
		"id", "name", "state", "last_error", "source_json", "start_json", "storage_json", "updated_at",
	}).AddRow(
		"7",
		"cluster-restored",
		"STOPPED",
		"",
		`{"host":"127.0.0.1","port":3306,"user":"repl","flavor":"mysql","server_id":200001}`,
		`{"mode":"LATEST"}`,
		`{"dir":"./data"}`,
		now,
	)

	mock.ExpectQuery(regexp.QuoteMeta(listTaskSQL)).WillReturnRows(rows)

	list, err := store.ListTasks(context.Background())
	if err != nil {
		t.Fatalf("ListTasks returned error: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 task, got %d", len(list))
	}
	if list[0].ID != "7" || list[0].State != tasks.StateStopped {
		t.Fatalf("unexpected task loaded: %+v", list[0])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}
