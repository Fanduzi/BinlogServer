package meta

import (
	"context"
	"regexp"
	"testing"
	"time"

	"binlog_server/internal/binlog"
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

func TestMySQLTaskStore_UpsertCheckpoint(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	cp := binlog.Checkpoint{
		File: "mysql-bin.000123",
		Pos:  456,
	}

	mock.ExpectExec(regexp.QuoteMeta(upsertCheckpointSQL)).
		WithArgs("task-1", "mysql-bin.000123", uint32(456), "", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := store.UpsertCheckpoint(context.Background(), "task-1", cp); err != nil {
		t.Fatalf("UpsertCheckpoint returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestMySQLTaskStore_LoadCheckpoint(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	now := time.Now()
	rows := sqlmock.NewRows([]string{"file_name", "pos", "gtid_set", "updated_at"}).
		AddRow("mysql-bin.000123", uint32(456), "", now)
	mock.ExpectQuery(regexp.QuoteMeta(loadCheckpointSQL)).
		WithArgs("task-1").
		WillReturnRows(rows)

	cp, ok, err := store.LoadCheckpoint(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("LoadCheckpoint returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected checkpoint exists")
	}
	if cp.File != "mysql-bin.000123" || cp.Pos != 456 {
		t.Fatalf("unexpected checkpoint: %+v", cp)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestMySQLTaskStore_DeleteTask(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	mock.ExpectExec(regexp.QuoteMeta(deleteTaskSQL)).
		WithArgs("1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.DeleteTask(context.Background(), "1"); err != nil {
		t.Fatalf("DeleteTask returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestMySQLTaskStore_AppendAndListEvents(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	event := tasks.TaskEvent{
		TaskID:  "1",
		Type:    "TASK_STARTED",
		Message: "task started",
		Detail:  "",
		Time:    time.Now(),
	}

	mock.ExpectExec(regexp.QuoteMeta(insertTaskEventSQL)).
		WithArgs("1", "TASK_STARTED", "task started", "", sqlmock.AnyArg(), int64(0)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.AppendEvent(context.Background(), event); err != nil {
		t.Fatalf("AppendEvent returned error: %v", err)
	}

	rows := sqlmock.NewRows([]string{"task_id", "event_type", "message", "detail", "event_time", "event_seq"}).
		AddRow("1", "TASK_STARTED", "task started", "", time.Now(), int64(12))
	mock.ExpectQuery(regexp.QuoteMeta(listTaskEventsSQL)).
		WithArgs("1", 10).
		WillReturnRows(rows)

	events, err := store.ListEvents(context.Background(), "1", 10)
	if err != nil {
		t.Fatalf("ListEvents returned error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != "TASK_STARTED" {
		t.Fatalf("unexpected event type: %s", events[0].Type)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestMySQLTaskStore_UpsertAndListBinlogFiles(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	fileMeta := tasks.BinlogFile{
		TaskID:    "1",
		FileName:  "mysql-bin.000001",
		FilePath:  "/tmp/mysql-bin.000001",
		SizeBytes: 1024,
		StartPos:  4,
		EndPos:    1200,
		CreatedAt: time.Now().Add(-time.Minute),
		SealedAt:  time.Now(),
	}

	mock.ExpectExec(regexp.QuoteMeta(upsertBinlogFileSQL)).
		WithArgs(
			"1",
			"mysql-bin.000001",
			"/tmp/mysql-bin.000001",
			int64(1024),
			uint32(4),
			uint32(1200),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.UpsertBinlogFile(context.Background(), fileMeta); err != nil {
		t.Fatalf("UpsertBinlogFile returned error: %v", err)
	}

	rows := sqlmock.NewRows([]string{
		"task_id", "file_name", "file_path", "size_bytes", "start_pos", "end_pos", "created_at", "sealed_at",
	}).AddRow("1", "mysql-bin.000001", "/tmp/mysql-bin.000001", int64(1024), uint32(4), uint32(1200), time.Now().Add(-time.Minute), time.Now())
	mock.ExpectQuery(regexp.QuoteMeta(listBinlogFilesSQL)).
		WithArgs("1", 10).
		WillReturnRows(rows)

	files, err := store.ListBinlogFiles(context.Background(), "1", 10)
	if err != nil {
		t.Fatalf("ListBinlogFiles returned error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].FileName != "mysql-bin.000001" {
		t.Fatalf("unexpected file name: %s", files[0].FileName)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}
