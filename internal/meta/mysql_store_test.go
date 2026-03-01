// Package meta provides module-level functionality for meta.
// input: MySQL connections, SQL schema/contracts, retry/lease timing policies
// output: persistent metadata operations for tasks, leases, runs, and checkpoints
// pos: metadata persistence layer between domain scheduler and MySQL storage engine
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"

	"github.com/DATA-DOG/go-sqlmock"
	mysqlDriver "github.com/go-sql-driver/mysql"
)

// TestMySQLTaskStore_UpsertTask 验证相关行为。
func TestMySQLTaskStore_UpsertTask(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	task := tasks.Task{
		ID:            "1",
		Name:          "cluster-a",
		ClusterKey:    "cluster-a-key",
		State:         tasks.StateRunning,
		OwnerWorkerID: "worker-a",
		Epoch:         7,
		RunID:         "run-1",
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

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(loadTaskRunStateSQL)).
		WithArgs("1").
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}))
	mock.ExpectExec(regexp.QuoteMeta(upsertTaskSQL)).
		WithArgs("1", "cluster-a", "cluster-a-key", "RUNNING", "", "worker-a", int64(7), "run-1", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta(insertTaskRunSQL)).
		WithArgs("run-1", "1", "worker-a", int64(7), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := store.UpsertTask(context.Background(), task); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestMySQLTaskStore_UpsertTask_FinishPreviousRunOnStop 验证相关行为。
func TestMySQLTaskStore_UpsertTask_FinishPreviousRunOnStop(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	task := tasks.Task{
		ID:            "1",
		Name:          "cluster-a",
		ClusterKey:    "cluster-a-key",
		State:         tasks.StateStopped,
		OwnerWorkerID: "",
		Epoch:         0,
		RunID:         "",
		LastError:     "",
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

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(loadTaskRunStateSQL)).
		WithArgs("1").
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow("run-1"))
	mock.ExpectExec(regexp.QuoteMeta(upsertTaskSQL)).
		WithArgs("1", "cluster-a", "cluster-a-key", "STOPPED", "", "", int64(0), "", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta(finishTaskRunSQL)).
		WithArgs(sqlmock.AnyArg(), "NORMAL_STOP", "run-1").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := store.UpsertTask(context.Background(), task); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestMySQLTaskStore_ListTasks 验证相关行为。
func TestMySQLTaskStore_ListTasks(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	now := time.Now()

	rows := sqlmock.NewRows([]string{
		"id", "name", "cluster_key", "state", "last_error", "owner_worker_id", "epoch", "run_id", "source_json", "start_json", "storage_json", "updated_at",
	}).AddRow(
		"7",
		"cluster-restored",
		"cluster-restored-key",
		"STOPPED",
		"",
		"worker-a",
		int64(9),
		"run-9",
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
	if list[0].OwnerWorkerID != "worker-a" || list[0].Epoch != 9 || list[0].RunID != "run-9" {
		t.Fatalf("unexpected cluster run fields: owner=%q epoch=%d run_id=%q", list[0].OwnerWorkerID, list[0].Epoch, list[0].RunID)
	}
	if list[0].ClusterKey != "cluster-restored-key" {
		t.Fatalf("unexpected cluster key: %q", list[0].ClusterKey)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestMySQLTaskStore_UpsertCheckpoint 验证相关行为。
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

// TestMySQLTaskStore_LoadCheckpoint 验证相关行为。
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

// TestMySQLTaskStore_DeleteTask 验证相关行为。
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

// TestMySQLTaskStore_AppendAndListEvents 验证相关行为。
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

// TestMySQLTaskStore_UpsertAndListBinlogFiles 验证相关行为。
func TestMySQLTaskStore_UpsertAndListBinlogFiles(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	fileMeta := tasks.BinlogFile{
		TaskID:      "1",
		FileName:    "mysql-bin.000001",
		FilePath:    "/tmp/mysql-bin.000001",
		SizeBytes:   1024,
		StartPos:    4,
		EndPos:      1200,
		CreatedAt:   time.Now().Add(-time.Minute),
		SealedAt:    time.Now(),
		ObjectKey:   "prefix/1/mysql-bin.000001",
		UploadState: "UPLOADED",
		UploadError: "",
		UploadedAt:  time.Now(),
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
			"prefix/1/mysql-bin.000001",
			"UPLOADED",
			"",
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.UpsertBinlogFile(context.Background(), fileMeta); err != nil {
		t.Fatalf("UpsertBinlogFile returned error: %v", err)
	}

	rows := sqlmock.NewRows([]string{
		"task_id", "file_name", "file_path", "size_bytes", "start_pos", "end_pos", "created_at", "sealed_at",
		"object_key", "upload_state", "upload_error", "uploaded_at",
	}).AddRow(
		"1", "mysql-bin.000001", "/tmp/mysql-bin.000001", int64(1024), uint32(4), uint32(1200), time.Now().Add(-time.Minute), time.Now(),
		"prefix/1/mysql-bin.000001", "UPLOADED", "", time.Now(),
	)
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
	if files[0].UploadState != "UPLOADED" {
		t.Fatalf("unexpected upload state: %s", files[0].UploadState)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestMySQLTaskStore_ListFailedUploadBinlogFiles 验证相关行为。
func TestMySQLTaskStore_ListFailedUploadBinlogFiles(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	rows := sqlmock.NewRows([]string{
		"task_id", "file_name", "file_path", "size_bytes", "start_pos", "end_pos", "created_at", "sealed_at",
		"object_key", "upload_state", "upload_error", "uploaded_at",
	}).AddRow(
		"1", "mysql-bin.000002", "/tmp/mysql-bin.000002", int64(2048), uint32(4), uint32(2200), time.Now().Add(-time.Minute), time.Now(),
		"prefix/cluster-a/uuid/mysql-bin.000002", "UPLOAD_FAILED", "network timeout", nil,
	)
	mock.ExpectQuery(regexp.QuoteMeta(listFailedSealedBinlogFilesSQL)).
		WithArgs("1", 100).
		WillReturnRows(rows)

	files, err := store.ListFailedUploadBinlogFiles(context.Background(), "1", 0)
	if err != nil {
		t.Fatalf("ListFailedUploadBinlogFiles returned error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].UploadState != "UPLOAD_FAILED" {
		t.Fatalf("expected upload state UPLOAD_FAILED, got %s", files[0].UploadState)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestMySQLTaskStore_ListUploadFailureReasons 验证相关行为。
func TestMySQLTaskStore_ListUploadFailureReasons(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	now := time.Now()
	rows := sqlmock.NewRows([]string{"upload_error", "uploaded_at", "sealed_at", "created_at"}).
		AddRow(" network   timeout ", nil, now.Add(-2*time.Minute), now.Add(-4*time.Minute)).
		AddRow("network timeout", nil, now.Add(-1*time.Minute), now.Add(-3*time.Minute)).
		AddRow("network timeout", nil, now.Add(-90*time.Second), now.Add(-200*time.Second)).
		AddRow("permission denied", nil, now.Add(-3*time.Minute), now.Add(-5*time.Minute)).
		AddRow("permission denied", now.Add(-30*time.Second), now.Add(-10*time.Minute), now.Add(-11*time.Minute))
	mock.ExpectQuery(regexp.QuoteMeta(listUploadFailureReasonDetailsSQL)).
		WithArgs("1").
		WillReturnRows(rows)

	items, err := store.ListUploadFailureReasons(context.Background(), "1", 20)
	if err != nil {
		t.Fatalf("ListUploadFailureReasons returned error: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(items))
	}
	if items[0].Reason != "network timeout" || items[0].Count != 3 {
		t.Fatalf("unexpected first row: %+v", items[0])
	}
	if !items[0].LatestTime.Equal(now.Add(-1 * time.Minute)) {
		t.Fatalf("unexpected first row latest_time: %v", items[0].LatestTime)
	}
	if items[1].Reason != "permission denied" || items[1].Count != 2 {
		t.Fatalf("unexpected second row: %+v", items[1])
	}
	if !items[1].LatestTime.Equal(now.Add(-30 * time.Second)) {
		t.Fatalf("unexpected second row latest_time: %v", items[1].LatestTime)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func expectSchemaCheckQueries(
	mock sqlmock.Sqlmock,
	missingTables map[string]bool,
	missingColumns map[string]map[string]bool,
	missingIndexes map[string]map[string]bool,
) {
	mock.ExpectQuery(regexp.QuoteMeta(currentSchemaVersionSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"version", "dirty"}).AddRow(minRequiredSchemaVersion, false))

	for _, table := range requiredTableSchemas {
		tableCount := 1
		if missingTables != nil && missingTables[table.Name] {
			tableCount = 0
		}
		mock.ExpectQuery(regexp.QuoteMeta(hasTableSQL)).
			WithArgs(table.Name).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(tableCount))
		if tableCount == 0 {
			continue
		}
		for _, column := range table.Columns {
			columnCount := 1
			if missingColumns != nil && missingColumns[table.Name] != nil && missingColumns[table.Name][column] {
				columnCount = 0
			}
			mock.ExpectQuery(regexp.QuoteMeta(hasColumnSQL)).
				WithArgs(table.Name, column).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(columnCount))
		}
		for _, index := range table.Indexes {
			indexCount := 1
			if missingIndexes != nil && missingIndexes[table.Name] != nil && missingIndexes[table.Name][index] {
				indexCount = 0
			}
			mock.ExpectQuery(regexp.QuoteMeta(hasIndexSQL)).
				WithArgs(table.Name, index).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(indexCount))
		}
	}
}

// TestMySQLTaskStore_EnsureSchemaMissingMigrationTable 验证缺少 schema_migrations 时报错。
func TestMySQLTaskStore_EnsureSchemaMissingMigrationTable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	mock.ExpectQuery(regexp.QuoteMeta(currentSchemaVersionSQL)).
		WillReturnError(&mysqlDriver.MySQLError{Number: 1146, Message: "Table 'binlog.schema_migrations' doesn't exist"})

	err = store.ensureSchema(context.Background())
	if err == nil {
		t.Fatal("expected schema version validation error")
	}
	if !strings.Contains(err.Error(), "missing table schema_migrations") {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestMySQLTaskStore_EnsureSchemaDirtyVersion 验证 dirty 版本状态会阻止启动。
func TestMySQLTaskStore_EnsureSchemaDirtyVersion(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	mock.ExpectQuery(regexp.QuoteMeta(currentSchemaVersionSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"version", "dirty"}).AddRow(minRequiredSchemaVersion, true))

	err = store.ensureSchema(context.Background())
	if err == nil {
		t.Fatal("expected schema version dirty error")
	}
	if !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestMySQLTaskStore_EnsureSchemaValid 验证 schema 完整时校验通过。
func TestMySQLTaskStore_EnsureSchemaValid(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	expectSchemaCheckQueries(mock, nil, nil, nil)

	if err := store.ensureSchema(context.Background()); err != nil {
		t.Fatalf("ensureSchema returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestMySQLTaskStore_EnsureSchemaMissingColumn 验证缺列时会给出明确报错。
func TestMySQLTaskStore_EnsureSchemaMissingColumn(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	expectSchemaCheckQueries(mock, nil, map[string]map[string]bool{
		"backup_tasks": {"cluster_key": true},
	}, nil)

	err = store.ensureSchema(context.Background())
	if err == nil {
		t.Fatal("expected schema validation error")
	}
	if !strings.Contains(err.Error(), "missing column backup_tasks.cluster_key") {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestMySQLTaskStore_EnsureSchemaMissingTable 验证缺表时会给出明确报错。
func TestMySQLTaskStore_EnsureSchemaMissingTable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	expectSchemaCheckQueries(mock, map[string]bool{"binlog_files": true}, nil, nil)

	err = store.ensureSchema(context.Background())
	if err == nil {
		t.Fatal("expected schema validation error")
	}
	if !strings.Contains(err.Error(), "missing table binlog_files") {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestMySQLTaskStore_ListTaskRuns 验证相关行为。
func TestMySQLTaskStore_ListTaskRuns(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	now := time.Now()
	rows := sqlmock.NewRows([]string{
		"run_id", "task_id", "worker_id", "epoch", "started_at", "ended_at", "end_reason",
	}).
		AddRow("run-2", "task-1", "worker-b", int64(8), now, nil, nil).
		AddRow("run-1", "task-1", "worker-a", int64(7), now.Add(-time.Hour), now.Add(-30*time.Minute), "NORMAL_STOP")

	mock.ExpectQuery(regexp.QuoteMeta(listTaskRunsSQL)).
		WithArgs("task-1", 10).
		WillReturnRows(rows)

	runs, err := store.ListTaskRuns(context.Background(), "task-1", 0)
	if err != nil {
		t.Fatalf("ListTaskRuns returned error: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}
	if runs[0].RunID != "run-2" || !runs[0].EndedAt.IsZero() {
		t.Fatalf("unexpected latest run: %+v", runs[0])
	}
	if runs[1].RunID != "run-1" || runs[1].EndReason != "NORMAL_STOP" || runs[1].EndedAt.IsZero() {
		t.Fatalf("unexpected historical run: %+v", runs[1])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestMySQLTaskStore_ListTaskRuns_LimitCappedTo200 验证相关行为。
func TestMySQLTaskStore_ListTaskRuns_LimitCappedTo200(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	rows := sqlmock.NewRows([]string{
		"run_id", "task_id", "worker_id", "epoch", "started_at", "ended_at", "end_reason",
	})
	mock.ExpectQuery(regexp.QuoteMeta(listTaskRunsSQL)).
		WithArgs("task-1", 200).
		WillReturnRows(rows)

	runs, err := store.ListTaskRuns(context.Background(), "task-1", 999)
	if err != nil {
		t.Fatalf("ListTaskRuns returned error: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("expected empty runs, got %d", len(runs))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestMySQLTaskStore_UpsertAndListWorkerHeartbeats 验证相关行为。
func TestMySQLTaskStore_UpsertAndListWorkerHeartbeats(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	now := time.Now()
	hb := tasks.WorkerHeartbeat{
		WorkerID:   "worker-a",
		Host:       "host-a",
		Version:    "v1.0.0",
		LastSeenAt: now,
		Status:     "ONLINE",
	}

	mock.ExpectExec(regexp.QuoteMeta(upsertWorkerHeartbeatSQL)).
		WithArgs("worker-a", "host-a", "v1.0.0", sqlmock.AnyArg(), "ONLINE").
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := store.UpsertWorkerHeartbeat(context.Background(), hb); err != nil {
		t.Fatalf("UpsertWorkerHeartbeat returned error: %v", err)
	}

	rows := sqlmock.NewRows([]string{"worker_id", "host", "version", "last_seen_at", "status"}).
		AddRow("worker-a", "host-a", "v1.0.0", now, "ONLINE")
	mock.ExpectQuery(regexp.QuoteMeta(listWorkerHeartbeatsSQL)).
		WithArgs(200).
		WillReturnRows(rows)

	items, err := store.ListWorkerHeartbeats(context.Background(), 0)
	if err != nil {
		t.Fatalf("ListWorkerHeartbeats returned error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].WorkerID != "worker-a" || items[0].Status != "ONLINE" {
		t.Fatalf("unexpected heartbeat item: %+v", items[0])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestMySQLTaskStore_AcquireWorkerRegistrationHeldByOtherSession 验证相关行为。
func TestMySQLTaskStore_AcquireWorkerRegistrationHeldByOtherSession(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	mock.ExpectExec(regexp.QuoteMeta(acquireWorkerRegistrationSQL)).
		WithArgs("worker-a", "session-b", durationToMicroseconds(15*time.Second)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(getWorkerRegistrationSQL)).
		WithArgs("worker-a").
		WillReturnRows(sqlmock.NewRows([]string{"session_id", "lease_expire_at"}).
			AddRow("session-other", now.Add(10*time.Second)))
	mock.ExpectQuery(regexp.QuoteMeta(currentDBTimeSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"NOW(6)"}).AddRow(now))

	ok, err := store.AcquireWorkerRegistration(context.Background(), "worker-a", "session-b", 15*time.Second)
	if err != nil {
		t.Fatalf("AcquireWorkerRegistration returned error: %v", err)
	}
	if ok {
		t.Fatal("expected acquire=false when worker_id held by another active session")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestMySQLTaskStore_AcquireWorkerRegistrationSuccessAfterReadBack 验证相关行为。
func TestMySQLTaskStore_AcquireWorkerRegistrationSuccessAfterReadBack(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	mock.ExpectExec(regexp.QuoteMeta(acquireWorkerRegistrationSQL)).
		WithArgs("worker-a", "session-a", durationToMicroseconds(15*time.Second)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(getWorkerRegistrationSQL)).
		WithArgs("worker-a").
		WillReturnRows(sqlmock.NewRows([]string{"session_id", "lease_expire_at"}).
			AddRow("session-a", now.Add(10*time.Second)))
	mock.ExpectQuery(regexp.QuoteMeta(currentDBTimeSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"NOW(6)"}).AddRow(now))

	ok, err := store.AcquireWorkerRegistration(context.Background(), "worker-a", "session-a", 15*time.Second)
	if err != nil {
		t.Fatalf("AcquireWorkerRegistration returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected acquire=true when read-back owner/session is current and lease valid")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestMySQLTaskStore_RenewAndReleaseWorkerRegistration 验证相关行为。
func TestMySQLTaskStore_RenewAndReleaseWorkerRegistration(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db)
	mock.ExpectExec(regexp.QuoteMeta(renewWorkerRegistrationSQL)).
		WithArgs(durationToMicroseconds(12*time.Second), "worker-a", "session-a").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(releaseWorkerRegistrationSQL)).
		WithArgs("worker-a", "session-a").
		WillReturnResult(sqlmock.NewResult(0, 1))

	ok, err := store.RenewWorkerRegistration(context.Background(), "worker-a", "session-a", 12*time.Second)
	if err != nil {
		t.Fatalf("RenewWorkerRegistration returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected renew=true")
	}
	if err := store.ReleaseWorkerRegistration(context.Background(), "worker-a", "session-a"); err != nil {
		t.Fatalf("ReleaseWorkerRegistration returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}
