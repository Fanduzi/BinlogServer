// Package meta provides module-level functionality for meta.
// input: mocked MySQL contracts including OPEN/SEALED file state, retry and lease timing policies, optional AES-256 source-password key
// output: persistence contract coverage for tasks, files, leases, runs, checkpoints, GetTask by id, SQL LIMIT/OFFSET pages, GROUP BY state and source rollups, SameSourceHost loopback SQL identity, expired-lease listing, catalog file list replay order and limit window, a bounded binlog_files page query, an unknown end_pos bound as NULL without assigning end_pos, DeleteBinlogFile by task id, source file name, and epoch, ListEvents newest-row window returned oldest-first, the legacy desired_run reconcile update, source_json password encryption, schema 5 refusal that names ./migrate up, schema 6 acceptance without uk_task_file_epoch, and the pre-step-9 index list refusing schema 6 with missing index uk_task_file_epoch
// pos: metadata persistence layer between domain scheduler and MySQL storage engine
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/config"
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

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
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
		WithArgs("1", "cluster-a", "cluster-a-key", "RUNNING", "", "worker-a", int64(7), "run-1", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), "STOP", int64(0), int64(0), int64(0), int64(0), int64(0)).
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

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
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
		WithArgs("1", "cluster-a", "cluster-a-key", "STOPPED", "", "", int64(0), "", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), "STOP", int64(0), int64(0), int64(0), int64(0), int64(0)).
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

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	now := time.Now()

	rows := sqlmock.NewRows(taskRowColumns()).AddRow(
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
		"STOP", int64(0), int64(0), int64(0), int64(0), int64(0),
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

// TestListTaskSQL_OrdersByNumericID 验证 ListTasks 按数字 id 排序，避免 VARCHAR 字典序。
func TestListTaskSQL_OrdersByNumericID(t *testing.T) {
	if !strings.Contains(listTaskSQL, "ORDER BY CAST(id AS UNSIGNED), id") {
		t.Fatalf("listTaskSQL must order by CAST(id AS UNSIGNED), id, got %q", listTaskSQL)
	}
}

func toDriverValues(args []any) []driver.Value {
	out := make([]driver.Value, len(args))
	for i, arg := range args {
		out[i] = arg
	}
	return out
}

func taskRowColumns() []string {
	return []string{
		"id", "name", "cluster_key", "state", "last_error", "owner_worker_id", "epoch", "run_id", "source_json", "start_json", "storage_json", "updated_at",
		"desired_run", "spec_revision", "applied_spec_revision", "failed_spec_revision", "retry_attempt", "consecutive_source_failures",
	}
}

func addTaskRow(rows *sqlmock.Rows, id, name, clusterKey, state, sourceJSON string, now time.Time) *sqlmock.Rows {
	return rows.AddRow(
		id,
		name,
		clusterKey,
		state,
		"",
		"",
		int64(0),
		"",
		sourceJSON,
		`{"mode":"LATEST"}`,
		`{"dir":"./data"}`,
		now,
		"STOP",
		int64(0),
		int64(0),
		int64(0),
		int64(0),
		int64(0),
	)
}

// TestMySQLTaskStore_GetTaskUsesPrimaryKey 验证 GetTask 走 WHERE id=?。
func TestMySQLTaskStore_GetTaskUsesPrimaryKey(t *testing.T) {
	if !strings.Contains(getTaskSQL, "WHERE id = ?") {
		t.Fatalf("getTaskSQL must filter by primary key, got %q", getTaskSQL)
	}
	if strings.Contains(getTaskSQL, "LIMIT") {
		t.Fatalf("getTaskSQL must not paginate, got %q", getTaskSQL)
	}

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	now := time.Now()
	rows := addTaskRow(sqlmock.NewRows(taskRowColumns()), "7", "cluster-restored", "cluster-restored-key", "STOPPED",
		`{"host":"127.0.0.1","port":3306,"user":"repl","flavor":"mysql","server_id":200001}`, now)
	mock.ExpectQuery(regexp.QuoteMeta(getTaskSQL)).WithArgs("7").WillReturnRows(rows)

	got, err := store.GetTask(context.Background(), "7")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.ID != "7" || got.Name != "cluster-restored" {
		t.Fatalf("unexpected task: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestMySQLTaskStore_GetTaskRetriesTransientEOF 验证 meta HA 瞬时 EOF 后 GetTask 会重试并成功。
func TestMySQLTaskStore_GetTaskRetriesTransientEOF(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	now := time.Now()
	rows := addTaskRow(sqlmock.NewRows(taskRowColumns()), "9", "after-failover", "after-failover-key", "RUNNING",
		`{"host":"127.0.0.1","port":3306,"user":"repl","flavor":"mysql","server_id":200009}`, now)

	mock.ExpectQuery(regexp.QuoteMeta(getTaskSQL)).WithArgs("9").WillReturnError(errors.New("unexpected EOF"))
	mock.ExpectQuery(regexp.QuoteMeta(getTaskSQL)).WithArgs("9").WillReturnRows(rows)

	got, err := store.GetTask(context.Background(), "9")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.ID != "9" || got.State != tasks.StateRunning {
		t.Fatalf("unexpected task: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestMySQLTaskStore_GetTaskNotFound 验证主键未命中映射为 ErrTaskNotFound。
func TestMySQLTaskStore_GetTaskNotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	mock.ExpectQuery(regexp.QuoteMeta(getTaskSQL)).WithArgs("missing").WillReturnRows(sqlmock.NewRows(taskRowColumns()))

	_, err = store.GetTask(context.Background(), "missing")
	if err != tasks.ErrTaskNotFound {
		t.Fatalf("expected ErrTaskNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestListTasksPageSQL_PushesFiltersAndLimit 验证分页 SQL 含 ORDER BY/LIMIT，JSON_EXTRACT 仅在 host/port 出现。
func TestListTasksPageSQL_PushesFiltersAndLimit(t *testing.T) {
	countSQL, selectSQL, _, selectArgs := listTasksPageSQL(tasks.TaskListFilter{Limit: 2, Offset: 2})
	if strings.Contains(countSQL, "JSON_EXTRACT") || strings.Contains(selectSQL, "JSON_EXTRACT") {
		t.Fatalf("JSON_EXTRACT must be omitted without host/port, count=%q select=%q", countSQL, selectSQL)
	}
	if !strings.Contains(selectSQL, "ORDER BY CAST(id AS UNSIGNED), id LIMIT ? OFFSET ?") {
		t.Fatalf("paged select must order then limit, got %q", selectSQL)
	}
	if len(selectArgs) != 2 || selectArgs[0] != 2 || selectArgs[1] != 2 {
		t.Fatalf("expected limit/offset args [2 2], got %#v", selectArgs)
	}

	state := tasks.StateFailed
	port := uint16(3307)
	_, filteredSQL, countArgs, pageArgs := listTasksPageSQL(tasks.TaskListFilter{
		Host:   "db-b",
		Port:   &port,
		State:  &state,
		Limit:  2,
		Offset: 2,
	})
	if !strings.Contains(filteredSQL, "state = ?") {
		t.Fatalf("state filter missing: %q", filteredSQL)
	}
	if !strings.Contains(filteredSQL, "JSON_UNQUOTE(JSON_EXTRACT(source_json, '$.host')) = ?") {
		t.Fatalf("host JSON_EXTRACT missing: %q", filteredSQL)
	}
	if !strings.Contains(filteredSQL, "CAST(JSON_UNQUOTE(JSON_EXTRACT(source_json, '$.port')) AS UNSIGNED) = ?") {
		t.Fatalf("port JSON_EXTRACT missing: %q", filteredSQL)
	}
	if len(countArgs) != 3 {
		t.Fatalf("expected 3 count args, got %#v", countArgs)
	}
	if len(pageArgs) != 5 {
		t.Fatalf("expected 5 page args (filters+limit+offset), got %#v", pageArgs)
	}

	_, loopbackSQL, loopbackCountArgs, _ := listTasksPageSQL(tasks.TaskListFilter{Host: "localhost"})
	if strings.Contains(loopbackSQL, "JSON_UNQUOTE(JSON_EXTRACT(source_json, '$.host')) = ?") {
		t.Fatalf("loopback host must not use exact-text match, got %q", loopbackSQL)
	}
	if !strings.Contains(loopbackSQL, "localhost") || !strings.Contains(loopbackSQL, "^127") || !strings.Contains(loopbackSQL, "::1") {
		t.Fatalf("loopback host SQL must name localhost / 127/8 / ::1, got %q", loopbackSQL)
	}
	if !strings.Contains(loopbackSQL, "REGEXP") || !strings.Contains(loopbackSQL, "TRIM(TRAILING '.'") {
		t.Fatalf("loopback host SQL must normalize spelling and require a dotted quad, got %q", loopbackSQL)
	}
	if len(loopbackCountArgs) != 0 {
		t.Fatalf("loopback host SQL should bind no host literal, got %#v", loopbackCountArgs)
	}

	_, exactSQL, exactCountArgs, _ := listTasksPageSQL(tasks.TaskListFilter{Host: "db-primary.example"})
	if !strings.Contains(exactSQL, "JSON_UNQUOTE(JSON_EXTRACT(source_json, '$.host')) = ?") {
		t.Fatalf("non-loopback host must stay exact, got %q", exactSQL)
	}
	if len(exactCountArgs) != 1 || exactCountArgs[0] != "db-primary.example" {
		t.Fatalf("expected exact host arg, got %#v", exactCountArgs)
	}
}

// TestMySQLTaskStore_ListTasksPageUsesCountAndLimit 验证 COUNT + LIMIT/OFFSET，handler 不必看到整表。
func TestMySQLTaskStore_ListTasksPageUsesCountAndLimit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	filter := tasks.TaskListFilter{Limit: 2, Offset: 2}
	countSQL, selectSQL, _, selectArgs := listTasksPageSQL(filter)
	mock.ExpectQuery(regexp.QuoteMeta(countSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(6))

	now := time.Now()
	rows := addTaskRow(sqlmock.NewRows(taskRowColumns()), "3", "task-3", "k-3", "CREATED",
		`{"host":"db-a","port":3306}`, now)
	rows = addTaskRow(rows, "4", "task-4", "k-4", "CREATED", `{"host":"db-a","port":3306}`, now)
	mock.ExpectQuery(regexp.QuoteMeta(selectSQL)).WithArgs(toDriverValues(selectArgs)...).WillReturnRows(rows)

	page, total, err := store.ListTasksPage(context.Background(), filter)
	if err != nil {
		t.Fatalf("ListTasksPage returned error: %v", err)
	}
	if total != 6 || len(page) != 2 || page[0].ID != "3" || page[1].ID != "4" {
		t.Fatalf("unexpected page total=%d items=%+v", total, page)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestTaskRollupSQL_GroupsWithoutLimit(t *testing.T) {
	stateSQL, _ := taskStateCountSQL(tasks.TaskListFilter{Limit: 50, Offset: 10})
	if strings.Contains(stateSQL, "LIMIT") || !strings.Contains(stateSQL, "GROUP BY state") {
		t.Fatalf("state count SQL = %q", stateSQL)
	}
	sourceSQL, _ := taskSourceCountSQL(tasks.TaskListFilter{})
	if !strings.Contains(sourceSQL, "GROUP BY") || !strings.Contains(sourceSQL, "SUM(state = 'RUNNING')") || strings.Contains(sourceSQL, "LIMIT") {
		t.Fatalf("source count SQL = %q", sourceSQL)
	}
	if strings.Contains(sourceSQL, taskSelectColumns) {
		t.Fatalf("source rollup must not select full task rows: %q", sourceSQL)
	}

	failed := tasks.StateFailed
	runningSQL, runningArgs := runningTaskRefSQL(tasks.TaskListFilter{State: &failed})
	if runningSQL != "" || runningArgs != nil {
		t.Fatalf("non-running filter must not query refs, sql=%q args=%#v", runningSQL, runningArgs)
	}
	running := tasks.StateRunning
	port := uint16(3306)
	runningSQL, runningArgs = runningTaskRefSQL(tasks.TaskListFilter{State: &running, Host: "db-a", Port: &port})
	if !strings.Contains(runningSQL, "SELECT id,") || strings.Contains(runningSQL, taskSelectColumns) {
		t.Fatalf("running ref SQL = %q", runningSQL)
	}
	if len(runningArgs) != 3 || runningArgs[0] != "RUNNING" || runningArgs[1] != "db-a" || runningArgs[2] != 3306 {
		t.Fatalf("running ref args = %#v", runningArgs)
	}

	loopbackSQL, loopbackArgs := taskStateCountSQL(tasks.TaskListFilter{Host: "localhost"})
	if len(loopbackArgs) != 0 || !strings.Contains(loopbackSQL, "localhost") || strings.Contains(loopbackSQL, sourceHostJSONExpr+" = ?") {
		t.Fatalf("loopback state SQL = %q args=%#v", loopbackSQL, loopbackArgs)
	}
}

func TestMySQLTaskStore_CountRollupsUseGroupBy(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	state := tasks.StateFailed
	filter := tasks.TaskListFilter{Host: "db-b", State: &state, Limit: 1, Offset: 1}

	stateSQL, stateArgs := taskStateCountSQL(filter)
	mock.ExpectQuery(regexp.QuoteMeta(stateSQL)).WithArgs(toDriverValues(stateArgs)...).
		WillReturnRows(sqlmock.NewRows([]string{"state", "count"}).AddRow("FAILED", int64(4)))

	counts, err := store.CountTaskStates(context.Background(), filter)
	if err != nil {
		t.Fatalf("CountTaskStates returned error: %v", err)
	}
	if counts.Total != 4 || counts.Failed != 4 || counts.Running != 0 {
		t.Fatalf("state counts = %+v", counts)
	}

	sourceSQL, sourceArgs := taskSourceCountSQL(filter)
	mock.ExpectQuery(regexp.QuoteMeta(sourceSQL)).WithArgs(toDriverValues(sourceArgs)...).
		WillReturnRows(sqlmock.NewRows([]string{"host", "port", "task_count", "running", "starting", "failed", "retry"}).
			AddRow("db-b", int64(3307), int64(4), int64(0), int64(0), int64(4), int64(0)))

	sources, err := store.CountTasksBySource(context.Background(), filter)
	if err != nil {
		t.Fatalf("CountTasksBySource returned error: %v", err)
	}
	if len(sources) != 1 || sources[0].Host != "db-b" || sources[0].Port != 3307 || sources[0].TaskCount != 4 || sources[0].Failed != 4 {
		t.Fatalf("sources = %+v", sources)
	}

	refs, err := store.ListRunningTaskRefs(context.Background(), filter)
	if err != nil {
		t.Fatalf("ListRunningTaskRefs returned error: %v", err)
	}
	if refs != nil {
		t.Fatalf("failed filter refs = %+v, want nil", refs)
	}

	open := tasks.TaskListFilter{Limit: 50}
	stateSQL, stateArgs = taskStateCountSQL(open)
	mock.ExpectQuery(regexp.QuoteMeta(stateSQL)).WithArgs(toDriverValues(stateArgs)...).
		WillReturnRows(sqlmock.NewRows([]string{"state", "count"}).AddRow("RUNNING", int64(2)).AddRow("CREATED", int64(8)))
	counts, err = store.CountTaskStates(context.Background(), open)
	if err != nil {
		t.Fatalf("open CountTaskStates returned error: %v", err)
	}
	if counts.Total != 10 || counts.Running != 2 {
		t.Fatalf("open counts = %+v", counts)
	}
	refSQL, refArgs := runningTaskRefSQL(open)
	mock.ExpectQuery(regexp.QuoteMeta(refSQL)).WithArgs(toDriverValues(refArgs)...).
		WillReturnRows(sqlmock.NewRows([]string{"id", "host", "port"}).
			AddRow("9", "db-a", int64(3306)).
			AddRow("10", "db-a", int64(3306)))
	refs, err = store.ListRunningTaskRefs(context.Background(), open)
	if err != nil {
		t.Fatalf("open ListRunningTaskRefs returned error: %v", err)
	}
	if len(refs) != 2 || refs[0].ID != "9" || refs[0].Host != "db-a" || refs[0].Port != 3306 {
		t.Fatalf("refs = %+v", refs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

type sourceIdentityCase struct {
	host            string
	sameAsLocalhost bool
}

// sameSourceHostCases is the accept/reject set shared by lookup (SameSourceHost)
// and ListTasksPage SQL. Extra loopback spellings that ParseIP rejects stay exact.
func sameSourceHostCases() []sourceIdentityCase {
	return []sourceIdentityCase{
		{host: "localhost", sameAsLocalhost: true},
		{host: "LOCALHOST", sameAsLocalhost: true},
		{host: "localhost.", sameAsLocalhost: true},
		{host: "127.0.0.1", sameAsLocalhost: true},
		{host: "127.0.0.0", sameAsLocalhost: true},
		{host: "127.255.255.255", sameAsLocalhost: true},
		{host: "::1", sameAsLocalhost: true},
		{host: "[::1]", sameAsLocalhost: true},
		{host: "0:0:0:0:0:0:0:1", sameAsLocalhost: true},
		{host: "0000:0000:0000:0000:0000:0000:0000:0001", sameAsLocalhost: true},
		{host: "::ffff:127.0.0.1", sameAsLocalhost: true},
		{host: "[::ffff:127.0.0.1]", sameAsLocalhost: true},
		{host: "::ffff:7f00:1", sameAsLocalhost: true},
		{host: "127.000.0.1", sameAsLocalhost: false},
		{host: "127.0.00.1", sameAsLocalhost: false},
		{host: "[127.0.0.1]", sameAsLocalhost: false},
		{host: "[localhost]", sameAsLocalhost: false},
		{host: "db-primary.example", sameAsLocalhost: false},
		{host: "[2001:db8::1]", sameAsLocalhost: false},
		{host: "::ffff:10.0.0.1", sameAsLocalhost: false},
	}
}

func assertLoopbackSQLMatchesSameSourceHost(t *testing.T, sqlText string) {
	t.Helper()
	if strings.Contains(sqlText, `^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$`) {
		t.Fatalf("loopback SQL must not treat padded IPv4 as 127/8, got %q", sqlText)
	}
	if strings.Contains(sqlText, "INET_ATON(") {
		t.Fatalf("INET_ATON accepts 127.000.0.1; use a no-leading-zero 127/8 regexp, got %q", sqlText)
	}
	if strings.Contains(sqlText, "TRIM(BOTH ']'") {
		t.Fatalf("loopback SQL must not unbracket IPv4 like [127.0.0.1], got %q", sqlText)
	}
	if !strings.Contains(sqlText, "INSTR(") || !strings.Contains(sqlText, "':'") {
		t.Fatalf("loopback SQL must unwrap brackets only when the host contains ':', got %q", sqlText)
	}
	if !strings.Contains(sqlText, "INET6_ATON('::1')") {
		t.Fatalf("loopback SQL must keep INET6_ATON(::1) so expanded IPv6 loopback matches, got %q", sqlText)
	}
	if !strings.Contains(sqlText, "::ffff:127.0.0.0") {
		t.Fatalf("loopback SQL must keep IPv4-mapped 127/8, got %q", sqlText)
	}
}

// TestMySQLTaskStore_ListTasksPageSQLMatchesSameSourceHost 用 store 路径核对
// ListTasksPage SQL/args 与 SameSourceHost 同一套源身份。
//
// Live MySQL is skipped: this package has no real MySQL test harness (go-sqlmock
// only; e2e MySQL is docker-compose, not a unit fixture). The generated SQL/args
// are asserted against tasks.SameSourceHost instead of executing INET6_ATON.
func TestMySQLTaskStore_ListTasksPageSQLMatchesSameSourceHost(t *testing.T) {
	loopbackSQL := loopbackSourceHostSQL(sourceHostJSONExpr)
	assertLoopbackSQLMatchesSameSourceHost(t, loopbackSQL)

	for _, tc := range sameSourceHostCases() {
		same := tasks.SameSourceHost(tc.host, "localhost")
		if same != tc.sameAsLocalhost {
			t.Fatalf("SameSourceHost(%q, localhost)=%v, want %v", tc.host, same, tc.sameAsLocalhost)
		}
		_, selectSQL, countArgs, _ := listTasksPageSQL(tasks.TaskListFilter{Host: tc.host})
		usesExact := strings.Contains(selectSQL, sourceHostJSONExpr+" = ?")
		if tc.sameAsLocalhost {
			if usesExact {
				t.Fatalf("loopback filter %q must use identity SQL, got %q", tc.host, selectSQL)
			}
			if len(countArgs) != 0 {
				t.Fatalf("loopback filter %q must bind no host arg, got %#v", tc.host, countArgs)
			}
			assertLoopbackSQLMatchesSameSourceHost(t, selectSQL)
			continue
		}
		if !usesExact {
			t.Fatalf("non-loopback filter %q must stay exact, got %q", tc.host, selectSQL)
		}
		if len(countArgs) != 1 || countArgs[0] != tc.host {
			t.Fatalf("exact filter %q args=%#v", tc.host, countArgs)
		}
	}

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	filter := tasks.TaskListFilter{Host: "localhost", Limit: 10, Offset: 0}
	countSQL, selectSQL, _, selectArgs := listTasksPageSQL(filter)
	assertLoopbackSQLMatchesSameSourceHost(t, selectSQL)
	mock.ExpectQuery(regexp.QuoteMeta(countSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	now := time.Now()
	rows := addTaskRow(sqlmock.NewRows(taskRowColumns()), "1", "stored-localhost", "k-1", "CREATED",
		`{"host":"localhost","port":3306}`, now)
	rows = addTaskRow(rows, "2", "stored-ipv4", "k-2", "CREATED", `{"host":"127.0.0.1","port":3306}`, now)
	mock.ExpectQuery(regexp.QuoteMeta(selectSQL)).WithArgs(toDriverValues(selectArgs)...).WillReturnRows(rows)

	page, total, err := store.ListTasksPage(context.Background(), filter)
	if err != nil {
		t.Fatalf("ListTasksPage returned error: %v", err)
	}
	if total != 2 || len(page) != 2 {
		t.Fatalf("unexpected page total=%d items=%+v", total, page)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestMySQLTaskStore_ListStartingUnownedTasks 验证只查询 STARTING 且 owner 为空。
func TestMySQLTaskStore_ListStartingUnownedTasks(t *testing.T) {
	if !strings.Contains(listStartingUnownedTaskSQL, "state = ?") {
		t.Fatalf("claim SQL must filter state, got %q", listStartingUnownedTaskSQL)
	}
	if !strings.Contains(listStartingUnownedTaskSQL, "owner_worker_id IS NULL OR owner_worker_id = ''") {
		t.Fatalf("claim SQL must filter empty owner, got %q", listStartingUnownedTaskSQL)
	}
	if strings.Contains(listStartingUnownedTaskSQL, "LIMIT") {
		t.Fatalf("claim SQL must not paginate, got %q", listStartingUnownedTaskSQL)
	}

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	now := time.Now()
	rows := addTaskRow(sqlmock.NewRows(taskRowColumns()), "1", "dispatch", "k-1", "STARTING",
		`{"host":"127.0.0.1","port":3306}`, now)
	mock.ExpectQuery(regexp.QuoteMeta(listStartingUnownedTaskSQL)).
		WithArgs(string(tasks.StateStarting)).
		WillReturnRows(rows)

	list, err := store.ListStartingUnownedTasks(context.Background())
	if err != nil {
		t.Fatalf("ListStartingUnownedTasks returned error: %v", err)
	}
	if len(list) != 1 || list[0].ID != "1" {
		t.Fatalf("unexpected claim list: %+v", list)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestListTasksWithExpiredLeaseSQL_JoinsExpiredActiveStates(t *testing.T) {
	if !strings.Contains(listTasksWithExpiredLeaseSQL, "INNER JOIN task_leases") {
		t.Fatalf("listTasksWithExpiredLeaseSQL must join task_leases, got %q", listTasksWithExpiredLeaseSQL)
	}
	if !strings.Contains(listTasksWithExpiredLeaseSQL, "lease_expire_at <= NOW(6)") {
		t.Fatalf("listTasksWithExpiredLeaseSQL must filter expired leases, got %q", listTasksWithExpiredLeaseSQL)
	}
	for _, state := range []string{"RUNNING", "LEASE_DEGRADED", "RETRY_BACKOFF", "STOPPING"} {
		if !strings.Contains(listTasksWithExpiredLeaseSQL, state) {
			t.Fatalf("listTasksWithExpiredLeaseSQL must include state %s, got %q", state, listTasksWithExpiredLeaseSQL)
		}
	}
	if !strings.Contains(listTasksWithExpiredLeaseSQL, "ORDER BY CAST(t.id AS UNSIGNED), t.id") {
		t.Fatalf("listTasksWithExpiredLeaseSQL must order by numeric id, got %q", listTasksWithExpiredLeaseSQL)
	}
}

// TestMySQLTaskStore_ListTasksWithExpiredLease 验证过期租约任务列表扫描。
func TestMySQLTaskStore_ListTasksWithExpiredLease(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	now := time.Now()
	rows := sqlmock.NewRows(taskRowColumns()).AddRow(
		"7",
		"cluster-expired",
		"cluster-expired-key",
		"RUNNING",
		"",
		"worker-dead",
		int64(9),
		"run-9",
		`{"host":"127.0.0.1","port":3306,"user":"repl","flavor":"mysql","server_id":200001}`,
		`{"mode":"LATEST"}`,
		`{"dir":"./data"}`,
		now,
		"RUN", int64(0), int64(0), int64(0), int64(0), int64(0),
	)
	mock.ExpectQuery(regexp.QuoteMeta(listTasksWithExpiredLeaseSQL)).WillReturnRows(rows)

	list, err := store.ListTasksWithExpiredLease(context.Background())
	if err != nil {
		t.Fatalf("ListTasksWithExpiredLease returned error: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 task, got %d", len(list))
	}
	if list[0].ID != "7" || list[0].State != tasks.StateRunning {
		t.Fatalf("unexpected task loaded: %+v", list[0])
	}
	if list[0].OwnerWorkerID != "worker-dead" || list[0].Epoch != 9 || list[0].RunID != "run-9" {
		t.Fatalf("unexpected cluster run fields: owner=%q epoch=%d run_id=%q", list[0].OwnerWorkerID, list[0].Epoch, list[0].RunID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

const testSourcePasswordKey = "01234567890123456789012345678901"

type encryptedSourceJSONArg struct {
	key      string
	password string
	host     string
}

func (a encryptedSourceJSONArg) Match(v driver.Value) bool {
	raw, ok := v.(string)
	if !ok {
		b, ok := v.([]byte)
		if !ok {
			return false
		}
		raw = string(b)
	}
	var src tasks.SourceConfig
	if err := json.Unmarshal([]byte(raw), &src); err != nil {
		return false
	}
	if src.Host != a.host || src.User != "repl" || !strings.HasPrefix(src.Password, config.EncryptionPrefix) {
		return false
	}
	d, err := config.NewDecryptor(a.key)
	if err != nil {
		return false
	}
	plain, err := d.Decrypt(src.Password)
	return err == nil && plain == a.password
}

func TestMySQLTaskStore_EncryptsSourcePasswordWhenKeySet(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	if err := store.setSourcePasswordKey(testSourcePasswordKey); err != nil {
		t.Fatalf("setSourcePasswordKey: %v", err)
	}
	task := tasks.Task{
		ID:         "1",
		Name:       "cluster-a",
		ClusterKey: "cluster-a-key",
		State:      tasks.StateCreated,
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
		WithArgs("1", "cluster-a", "cluster-a-key", "CREATED", "", "", int64(0), "", encryptedSourceJSONArg{
			key:      testSourcePasswordKey,
			password: "secret",
			host:     "127.0.0.1",
		}, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), "STOP", int64(0), int64(0), int64(0), int64(0), int64(0)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := store.UpsertTask(context.Background(), task); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if task.Source.Password != "secret" {
		t.Fatalf("UpsertTask must not mutate in-memory password, got %q", task.Source.Password)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestMySQLTaskStore_ListTasksDecryptsSourcePassword(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	if err := store.setSourcePasswordKey(testSourcePasswordKey); err != nil {
		t.Fatalf("setSourcePasswordKey: %v", err)
	}
	d, err := config.NewDecryptor(testSourcePasswordKey)
	if err != nil {
		t.Fatalf("NewDecryptor: %v", err)
	}
	encrypted, err := d.Encrypt("secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	sourceJSON, err := json.Marshal(tasks.SourceConfig{
		Host:     "127.0.0.1",
		Port:     3306,
		User:     "repl",
		Password: encrypted,
		Flavor:   "mysql",
		ServerID: 200001,
	})
	if err != nil {
		t.Fatalf("marshal source: %v", err)
	}

	now := time.Now()
	rows := sqlmock.NewRows(taskRowColumns()).AddRow(
		"1", "cluster-a", "cluster-a-key", "STOPPED", "", "", int64(0), "",
		string(sourceJSON), `{"mode":"LATEST"}`, `{"dir":"./data"}`, now,
		"STOP", int64(0), int64(0), int64(0), int64(0), int64(0),
	)
	mock.ExpectQuery(regexp.QuoteMeta(listTaskSQL)).WillReturnRows(rows)

	list, err := store.ListTasks(context.Background())
	if err != nil {
		t.Fatalf("ListTasks returned error: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 task, got %d", len(list))
	}
	if list[0].Source.Password != "secret" {
		t.Fatalf("expected decrypted plaintext password on Task.Source, got %q", list[0].Source.Password)
	}
	if list[0].Source.Host != "127.0.0.1" {
		t.Fatalf("expected other source fields to stay plaintext, got %+v", list[0].Source)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestMySQLTaskStore_ListTasksLoadsPlaintextSourcePasswordWithoutPrefix(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	if err := store.setSourcePasswordKey(testSourcePasswordKey); err != nil {
		t.Fatalf("setSourcePasswordKey: %v", err)
	}
	now := time.Now()
	rows := sqlmock.NewRows(taskRowColumns()).AddRow(
		"1", "cluster-a", "cluster-a-key", "STOPPED", "", "", int64(0), "",
		`{"host":"127.0.0.1","port":3306,"user":"repl","password":"legacy-secret","flavor":"mysql","server_id":200001}`,
		`{"mode":"LATEST"}`, `{"dir":"./data"}`, now,
		"STOP", int64(0), int64(0), int64(0), int64(0), int64(0),
	)
	mock.ExpectQuery(regexp.QuoteMeta(listTaskSQL)).WillReturnRows(rows)

	list, err := store.ListTasks(context.Background())
	if err != nil {
		t.Fatalf("ListTasks returned error: %v", err)
	}
	if len(list) != 1 || list[0].Source.Password != "legacy-secret" {
		t.Fatalf("expected existing plaintext source_json to load, got %+v err=%v", list, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestMySQLTaskStore_ListTasksLoadsPlaintextWithoutKey(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	now := time.Now()
	rows := sqlmock.NewRows(taskRowColumns()).AddRow(
		"1", "cluster-a", "cluster-a-key", "STOPPED", "", "", int64(0), "",
		`{"host":"127.0.0.1","port":3306,"user":"repl","password":"legacy-secret","flavor":"mysql","server_id":200001}`,
		`{"mode":"LATEST"}`, `{"dir":"./data"}`, now,
		"STOP", int64(0), int64(0), int64(0), int64(0), int64(0),
	)
	mock.ExpectQuery(regexp.QuoteMeta(listTaskSQL)).WillReturnRows(rows)

	list, err := store.ListTasks(context.Background())
	if err != nil {
		t.Fatalf("ListTasks returned error: %v", err)
	}
	if len(list) != 1 || list[0].Source.Password != "legacy-secret" {
		t.Fatalf("expected plaintext persist without key, got %+v err=%v", list, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestNewMySQLTaskStoreWithSchemaTimeout_RejectsInvalidEncryptionKey(t *testing.T) {
	_, err := NewMySQLTaskStoreWithSchemaTimeout("user:pass@tcp(127.0.0.1:3306)/meta", time.Second, "short")
	if err == nil {
		t.Fatal("expected invalid encryption key to fail before schema checks")
	}
	if !strings.Contains(err.Error(), "32 bytes") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestMySQLTaskStore_UpsertCheckpoint 验证相关行为。

// TestMySQLTaskStore_UpsertCheckpoint 验证相关行为。
func TestMySQLTaskStore_UpsertCheckpoint(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
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

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
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

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
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

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
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

// TestMySQLTaskStore_ListEventsNewestWindowOldestFirst 验证事件窗口是最新行，返回旧的在前。
func TestMySQLTaskStore_ListEventsNewestWindowOldestFirst(t *testing.T) {
	if !strings.Contains(listTaskEventsSQL, "ORDER BY id DESC") || !strings.Contains(listTaskEventsSQL, "LIMIT") {
		t.Fatalf("event window must be the newest rows: %s", listTaskEventsSQL)
	}

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	now := time.Now()
	// Driver order is DESC: newest row first. ListEvents reverses to oldest-first.
	rows := sqlmock.NewRows([]string{"task_id", "event_type", "message", "detail", "event_time", "event_seq"}).
		AddRow("1", "TASK_STARTED", "task started", "", now, int64(3)).
		AddRow("1", "TASK_RUNNER_ERROR", "runner error", "SOURCE_UNREACHABLE: dial tcp: connection refused", now.Add(-time.Second), int64(2)).
		AddRow("1", "TASK_STARTED", "task started", "", now.Add(-2*time.Second), int64(1))
	mock.ExpectQuery(regexp.QuoteMeta(listTaskEventsSQL)).
		WithArgs("1", 200).
		WillReturnRows(rows)

	events, err := store.ListEvents(context.Background(), "1", 200)
	if err != nil {
		t.Fatalf("ListEvents returned error: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if events[0].Sequence != 1 || events[2].Sequence != 3 {
		t.Fatalf("want oldest-first sequences 1..3, got %d,%d,%d", events[0].Sequence, events[1].Sequence, events[2].Sequence)
	}
	if events[1].Type != "TASK_RUNNER_ERROR" {
		t.Fatalf("middle event = %s", events[1].Type)
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

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	fileMeta := tasks.BinlogFile{
		TaskID:      "1",
		FileName:    "mysql-bin.000001",
		FilePath:    "/tmp/mysql-bin.000001",
		State:       "SEALED",
		SizeBytes:   1024,
		StartPos:    4,
		EndPos:      1200,
		CreatedAt:   time.Now().Add(-time.Minute),
		SealedAt:    time.Now(),
		ObjectKey:   "prefix/1/mysql-bin.000001",
		UploadState: "UPLOADED",
		UploadError: "",
		UploadedAt:  time.Now(),
		Checksum:    tasks.ChecksumMatch,
	}

	mock.ExpectExec(regexp.QuoteMeta(upsertBinlogFileSQL)).
		WithArgs(
			"1",
			"mysql-bin.000001",
			"mysql-bin.000001",
			"/tmp/mysql-bin.000001",
			int64(0),
			"SEALED",
			int64(1024),
			int64(4),
			int64(1200),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			"prefix/1/mysql-bin.000001",
			"UPLOADED",
			"",
			sqlmock.AnyArg(),
			tasks.ChecksumMatch,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.UpsertBinlogFile(context.Background(), fileMeta); err != nil {
		t.Fatalf("UpsertBinlogFile returned error: %v", err)
	}

	rows := sqlmock.NewRows([]string{
		"task_id", "file_name", "file_path", "state", "size_bytes", "start_pos", "end_pos", "created_at", "sealed_at",
		"object_key", "upload_state", "upload_error", "uploaded_at", "checksum", "epoch",
	}).AddRow(
		"1", "mysql-bin.000001", "/tmp/mysql-bin.000001", "SEALED", int64(1024), uint32(4), uint32(1200), time.Now().Add(-time.Minute), time.Now(),
		"prefix/1/mysql-bin.000001", "UPLOADED", "", time.Now(), tasks.ChecksumMatch, int64(0),
	)
	mock.ExpectQuery(regexp.QuoteMeta(listBinlogFilesSQL)).
		WithArgs("1").
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
	if files[0].State != "SEALED" {
		t.Fatalf("unexpected file state: %s", files[0].State)
	}
	if files[0].Checksum != tasks.ChecksumMatch {
		t.Fatalf("checksum=%q, want %s", files[0].Checksum, tasks.ChecksumMatch)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestMySQLTaskStore_UpsertUnknownEndOmitsEndPos binds NULL and does not assign
// end_pos when the caller does not know it. A scanned NULL or 0 is unknown.
func TestMySQLTaskStore_UpsertUnknownEndOmitsEndPos(t *testing.T) {
	if strings.Contains(upsertBinlogFileUnknownEndSQL, "end_pos = VALUES(end_pos)") {
		t.Fatal("unknown-end upsert assigns end_pos")
	}
	if !strings.Contains(upsertBinlogFileSQL, "end_pos = VALUES(end_pos)") {
		t.Fatal("known-end upsert does not assign end_pos")
	}

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	unknown := tasks.BinlogFile{
		TaskID: "1", FileName: "mysql-bin.000008", FilePath: "/tmp/mysql-bin.000008",
		State: "SEALED", SizeBytes: 4, StartPos: 4,
	}
	mock.ExpectExec(regexp.QuoteMeta(upsertBinlogFileUnknownEndSQL)).
		WithArgs(
			"1", "mysql-bin.000008", "mysql-bin.000008", "/tmp/mysql-bin.000008",
			int64(0), "SEALED", int64(4), int64(4), nil,
			sqlmock.AnyArg(), sqlmock.AnyArg(), "", "LOCAL_ONLY", "", sqlmock.AnyArg(), "",
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.UpsertBinlogFile(context.Background(), unknown); err != nil {
		t.Fatal(err)
	}

	cols := []string{
		"task_id", "file_name", "file_path", "state", "size_bytes", "start_pos", "end_pos", "created_at", "sealed_at",
		"object_key", "upload_state", "upload_error", "uploaded_at", "checksum", "epoch",
	}
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(listBinlogFilesSQL)).
		WithArgs("1").
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("1", "mysql-bin.000008", "/tmp/mysql-bin.000008", "SEALED", int64(4), nil, nil, now, now, "", "LOCAL_ONLY", "", nil, nil, int64(0)).
			AddRow("1", "mysql-bin.000009", "/tmp/mysql-bin.000009", "SEALED", int64(4), int64(0), int64(0), now, now, "", "UPLOADED", "", nil, tasks.ChecksumMatch, int64(0)))
	files, err := store.ListBinlogFiles(context.Background(), "1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].StartPos != 0 || files[0].EndPos != 0 || files[1].StartPos != 0 || files[1].EndPos != 0 {
		t.Fatalf("scanned positions %+v", files)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestMySQLTaskStore_ListBinlogFilesReplayOrder 验证目录列表按源序号升序，同序号封存在 open 之前，limit 保留序号最大的窗口。
func TestMySQLTaskStore_ListBinlogFilesReplayOrder(t *testing.T) {
	if strings.Contains(listBinlogFilesSQL, "ORDER BY") || strings.Contains(listBinlogFilesSQL, "LIMIT") {
		t.Fatalf("listBinlogFilesSQL must return every row for the task so the replay window is not sealed_at: %s", listBinlogFilesSQL)
	}

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	newest := time.Now()
	older := newest.Add(-2 * time.Hour)
	oldest := newest.Add(-3 * time.Hour)
	cols := []string{
		"task_id", "file_name", "file_path", "state", "size_bytes", "start_pos", "end_pos", "created_at", "sealed_at",
		"object_key", "upload_state", "upload_error", "uploaded_at", "checksum", "epoch",
	}
	// Row order is newest sealed_at first, which used to be the list order.
	// 000002 sealed is newer than 000003. 000002's open epoch is newer than its seal.
	catalogRows := func() *sqlmock.Rows {
		return sqlmock.NewRows(cols).
			AddRow("1", "mysql-bin.000002", "/data/1/mysql-bin.000002", "SEALED", int64(20), uint32(4), uint32(20), older, newest, "", "LOCAL_ONLY", "", nil, nil, int64(0)).
			AddRow("1", "mysql-bin.000002.open.e1", "/data/1/mysql-bin.000002.open.e1", "OPEN", int64(8), uint32(4), uint32(8), newest, newest, "", "LOCAL_ONLY", "", nil, nil, int64(1)).
			AddRow("1", "mysql-bin.000001", "/data/1/mysql-bin.000001", "SEALED", int64(10), uint32(4), uint32(10), oldest, oldest, "", "UPLOADED", "", nil, tasks.ChecksumMatch, int64(0)).
			AddRow("1", "mysql-bin.000003", "/data/1/mysql-bin.000003.open.e4", "OPEN", int64(30), uint32(4), uint32(30), older, older, "", "LOCAL_ONLY", "", nil, nil, int64(4))
	}

	mock.ExpectQuery(regexp.QuoteMeta(listBinlogFilesSQL)).
		WithArgs("1").
		WillReturnRows(catalogRows())
	files, err := store.ListBinlogFiles(context.Background(), "1", 10)
	if err != nil {
		t.Fatalf("ListBinlogFiles returned error: %v", err)
	}
	want := []struct {
		name  string
		base  string
		state string
	}{
		{"mysql-bin.000001", "mysql-bin.000001", "SEALED"},
		{"mysql-bin.000002", "mysql-bin.000002", "SEALED"},
		{"mysql-bin.000002.open.e1", "mysql-bin.000002.open.e1", "OPEN"},
		{"mysql-bin.000003", "mysql-bin.000003.open.e4", "OPEN"},
	}
	if len(files) != len(want) {
		t.Fatalf("got %d files: %+v", len(files), files)
	}
	for i, item := range want {
		got := files[i]
		if got.FileName != item.name || got.State != item.state || got.FilePath != "/data/1/"+item.base {
			t.Fatalf("files[%d]=%s %s %s, want %s %s %s", i, got.FileName, got.State, got.FilePath, item.name, item.state, item.base)
		}
	}
	if files[0].Checksum != tasks.ChecksumMatch {
		t.Fatalf("checksum=%q, want %s", files[0].Checksum, tasks.ChecksumMatch)
	}
	if files[0].FileName >= files[len(files)-1].FileName {
		t.Fatalf("first source index is not lower than last: %s then %s", files[0].FileName, files[len(files)-1].FileName)
	}

	mock.ExpectQuery(regexp.QuoteMeta(listBinlogFilesSQL)).
		WithArgs("1").
		WillReturnRows(catalogRows())
	window, err := store.ListBinlogFiles(context.Background(), "1", 2)
	if err != nil {
		t.Fatalf("ListBinlogFiles window returned error: %v", err)
	}
	if len(window) != 2 || window[0].FilePath != "/data/1/mysql-bin.000002.open.e1" || window[1].FilePath != "/data/1/mysql-bin.000003.open.e4" {
		t.Fatalf("limit window = %+v, want highest indexes 000002.open.e1 then 000003.open.e4", window)
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

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	rows := sqlmock.NewRows([]string{
		"task_id", "file_name", "file_path", "size_bytes", "start_pos", "end_pos", "created_at", "sealed_at",
		"object_key", "upload_state", "upload_error", "uploaded_at", "epoch",
	}).AddRow(
		"1", "mysql-bin.000002", "/tmp/mysql-bin.000002", int64(2048), uint32(4), uint32(2200), time.Now().Add(-time.Minute), time.Now(),
		"prefix/cluster-a/uuid/mysql-bin.000002", "UPLOAD_FAILED", "network timeout", nil, int64(0),
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

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
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

func TestMySQLTaskStore_ReconcileLegacyDesiredRun(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db, time.Second)
	mock.ExpectExec(regexp.QuoteMeta(reconcileLegacyDesiredRunSQL)).
		WillReturnResult(sqlmock.NewResult(0, 2))

	n, err := store.ReconcileLegacyDesiredRun(context.Background())
	if err != nil {
		t.Fatalf("ReconcileLegacyDesiredRun: %v", err)
	}
	if n != 2 {
		t.Fatalf("rows=%d, want 2", n)
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

func TestMySQLTaskStore_EnsureSchemaTooOldTellsOperatorToMigrate(t *testing.T) {
	for _, version := range []int64{4, 5} {
		t.Run(fmt.Sprintf("schema%d", version), func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New returned error: %v", err)
			}
			defer db.Close()

			store := newMySQLTaskStoreFromDB(db, 5*time.Second)
			mock.ExpectQuery(regexp.QuoteMeta(currentSchemaVersionSQL)).
				WillReturnRows(sqlmock.NewRows([]string{"version", "dirty"}).AddRow(version, false))

			err = store.ensureSchema(context.Background())
			if err == nil {
				t.Fatal("expected schema version error")
			}
			if !strings.Contains(err.Error(), "./migrate up") {
				t.Fatalf("error should tell the operator to run ./migrate up, got %v", err)
			}
			if !strings.Contains(err.Error(), "schema version too old") {
				t.Fatalf("error should say schema version too old, got %v", err)
			}
			wantCurrent := fmt.Sprintf("current=%d", version)
			if !strings.Contains(err.Error(), wantCurrent) || !strings.Contains(err.Error(), "required>=6") {
				t.Fatalf("unexpected error: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet sql expectations: %v", err)
			}
		})
	}
}

func TestMySQLTaskStore_Schema6DropsFileEpochIndex(t *testing.T) {
	if minRequiredSchemaVersion != 6 {
		t.Fatalf("minRequiredSchemaVersion=%d, want 6", minRequiredSchemaVersion)
	}
	var indexes, columns []string
	for _, table := range requiredTableSchemas {
		if table.Name != "binlog_files" {
			continue
		}
		indexes = table.Indexes
		columns = table.Columns
	}
	if indexes == nil {
		t.Fatal("binlog_files is not a required table")
	}
	hasFileName := false
	for _, column := range columns {
		if column == "file_name" {
			hasFileName = true
		}
	}
	if !hasFileName {
		t.Fatal("file_name column must stay")
	}
	hasSourceEpoch := false
	for _, index := range indexes {
		if index == "uk_task_file_epoch" {
			t.Fatal("schema 6 must not require uk_task_file_epoch")
		}
		if index == "uk_task_source_epoch" {
			hasSourceEpoch = true
		}
	}
	if !hasSourceEpoch {
		t.Fatalf("required indexes %v missing uk_task_source_epoch", indexes)
	}
}

// TestMySQLTaskStore_LegacyFileEpochIndexRefusesSchema6 keeps the pre-step-9
// required-index list in the checker. Schema 6 has no uk_task_file_epoch, so
// that older list refuses with missing index uk_task_file_epoch. The production
// list no longer includes the index; this test does not put it back.
func TestMySQLTaskStore_LegacyFileEpochIndexRefusesSchema6(t *testing.T) {
	prev := requiredTableSchemas
	t.Cleanup(func() { requiredTableSchemas = prev })
	legacy := make([]tableSchemaSpec, len(prev))
	for i, spec := range prev {
		legacy[i] = spec
		legacy[i].Columns = append([]string(nil), spec.Columns...)
		legacy[i].Indexes = append([]string(nil), spec.Indexes...)
		if spec.Name == "binlog_files" {
			legacy[i].Indexes = append(legacy[i].Indexes, "uk_task_file_epoch")
		}
	}
	requiredTableSchemas = legacy

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	expectSchemaCheckQueries(mock, nil, nil, map[string]map[string]bool{
		"binlog_files": {"uk_task_file_epoch": true},
	})

	err = store.ensureSchema(context.Background())
	if err == nil {
		t.Fatal("expected missing uk_task_file_epoch")
	}
	if !strings.Contains(err.Error(), "missing index binlog_files.uk_task_file_epoch") {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestMySQLTaskStore_GetTaskReadsRetryBudget(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	now := time.Now()
	rows := sqlmock.NewRows(taskRowColumns()).AddRow(
		"7", "budget", "budget-key", "RETRY_BACKOFF", "SOURCE_UNREACHABLE: dial", "worker-a", int64(3), "run-3",
		`{"host":"127.0.0.1","port":3306,"user":"repl","flavor":"mysql","server_id":200001}`,
		`{"mode":"LATEST"}`, `{"dir":"./data"}`, now,
		"RUN", int64(4), int64(2), int64(0), int64(6), int64(6),
	)
	mock.ExpectQuery(regexp.QuoteMeta(getTaskSQL)).WithArgs("7").WillReturnRows(rows)

	got, err := store.GetTask(context.Background(), "7")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.DesiredRun != "RUN" || got.SpecRevision != 4 || got.AppliedSpecRevision != 2 || got.FailedSpecRevision != 0 {
		t.Fatalf("revisions: desired=%s spec=%d applied=%d failed=%d", got.DesiredRun, got.SpecRevision, got.AppliedSpecRevision, got.FailedSpecRevision)
	}
	if got.RetryAttempt != 6 || got.ConsecutiveSourceFailures != 6 {
		t.Fatalf("budget attempt=%d consecutive=%d", got.RetryAttempt, got.ConsecutiveSourceFailures)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestMySQLTaskStore_EnsureSchemaMissingMigrationTable 验证缺少 schema_migrations 时报错。
func TestMySQLTaskStore_EnsureSchemaMissingMigrationTable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
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

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
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

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	expectSchemaCheckQueries(mock, nil, nil, nil)
	mock.ExpectQuery(regexp.QuoteMeta(hasColumnSQL)).
		WithArgs("backup_tasks", "pending_dump_cleanup").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	if err := store.ensureSchema(context.Background()); err != nil {
		t.Fatalf("ensureSchema returned error: %v", err)
	}
	if store.PendingDumpColumn() {
		t.Fatal("a missing pending_dump_cleanup column stays optional")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestMySQLTaskStore_PendingDumpColumnRoundTrip(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	store.hasPendingDump = true
	now := time.Now()
	raw := `{"connection_id":42,"host":"10.0.0.8","port":3306}`
	rows := sqlmock.NewRows(append(taskRowColumns(), "pending_dump_cleanup")).AddRow(
		"7", "pending", "pending-key", "STOPPED", "", "", int64(0), "",
		`{"host":"10.0.0.8","port":3306,"user":"repl","password":"secret","flavor":"mysql"}`,
		`{"mode":"LATEST"}`, `{"retention_days":7}`, now,
		"STOP", int64(1), int64(1), int64(0), int64(0), int64(0),
		raw,
	)
	mock.ExpectQuery(regexp.QuoteMeta(store.withPendingColumn(getTaskSQL))).
		WithArgs("7").
		WillReturnRows(rows)
	got, err := store.GetTask(context.Background(), "7")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.PendingDumpCleanup == nil || got.PendingDumpCleanup.ConnectionID != 42 || got.PendingDumpCleanup.Warning == "" {
		t.Fatalf("pending %+v", got.PendingDumpCleanup)
	}

	mock.ExpectExec(regexp.QuoteMeta(savePendingDumpSQL)).
		WithArgs("", "7", raw).
		WillReturnResult(sqlmock.NewResult(0, 1))
	changed, err := store.SavePendingDumpCleanup(context.Background(), "7", raw, "")
	if err != nil || !changed {
		t.Fatalf("save changed=%v err=%v", changed, err)
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

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
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

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
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

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
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

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
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

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
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

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
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

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
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

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
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

func TestMySQLTaskStore_DeleteBinlogFile(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	if err := store.DeleteBinlogFile(context.Background(), " ", "mysql-bin.000001", 0); err == nil {
		t.Fatal("expected empty task id error")
	}
	if err := store.DeleteBinlogFile(context.Background(), "1", " ", 0); err == nil {
		t.Fatal("expected empty file name error")
	}
	mock.ExpectExec(regexp.QuoteMeta(deleteBinlogFileSQL)).
		WithArgs("1", "mysql-bin.000001", int64(0)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.DeleteBinlogFile(context.Background(), "1", "mysql-bin.000001", 0); err != nil {
		t.Fatalf("DeleteBinlogFile returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestMySQLTaskStore_ListBinlogFilesPageIsBounded(t *testing.T) {
	if !strings.Contains(listBinlogFilesPageFirstSQL, "LIMIT") || !strings.Contains(listBinlogFilesPageSQL, "LIMIT") {
		t.Fatal("binlog file page queries must LIMIT")
	}
	if strings.Contains(listBinlogFilesSQL, "LIMIT") {
		t.Fatal("replay window query must stay unlimited so Go can order by source index")
	}

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	cols := []string{
		"task_id", "file_name", "file_path", "state", "size_bytes", "start_pos", "end_pos", "created_at", "sealed_at",
		"object_key", "upload_state", "upload_error", "uploaded_at", "checksum", "epoch",
	}
	mock.ExpectQuery(regexp.QuoteMeta(listBinlogFilesPageFirstSQL)).
		WithArgs("1", 2).
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("1", "mysql-bin.000001", "/data/1/mysql-bin.000001", "SEALED", int64(10), uint32(4), uint32(10), time.Now(), time.Now(), "", "LOCAL_ONLY", "", nil, nil, int64(0)).
			AddRow("1", "mysql-bin.000002", "/data/1/mysql-bin.000002", "SEALED", int64(10), uint32(4), uint32(10), time.Now(), time.Now(), "", "UPLOADED", "", nil, tasks.ChecksumMatch, int64(1)))
	page, err := store.ListBinlogFilesPage(context.Background(), "1", "", 0, true, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].FileName != "mysql-bin.000001" || page[1].Epoch != 1 || page[1].Checksum != tasks.ChecksumMatch {
		t.Fatalf("first page = %+v", page)
	}

	mock.ExpectQuery(regexp.QuoteMeta(listBinlogFilesPageSQL)).
		WithArgs("1", "mysql-bin.000002", "mysql-bin.000002", int64(1), 2).
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("1", "mysql-bin.000003", "/data/1/mysql-bin.000003", "SEALED", int64(10), uint32(4), uint32(10), time.Now(), time.Now(), "", "LOCAL_ONLY", "", nil, nil, int64(0)))
	next, err := store.ListBinlogFilesPage(context.Background(), "1", page[1].FileName, page[1].Epoch, false, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 1 || next[0].FileName != "mysql-bin.000003" {
		t.Fatalf("second page = %+v", next)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
