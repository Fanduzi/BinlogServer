// input: MySQL connections, SQL schema/contracts, retry/lease timing policies
// output: persistent metadata operations for tasks, leases, runs, and checkpoints
// pos: metadata persistence layer between domain scheduler and MySQL storage engine
// note: if this file changes, update this header and module AGENTS.md.
package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

const minRequiredSchemaVersion int64 = 1

const currentSchemaVersionSQL = `
SELECT version, dirty
FROM schema_migrations
LIMIT 1;
`

const hasTableSQL = `
SELECT COUNT(*)
FROM information_schema.tables
WHERE table_schema = DATABASE()
  AND table_name = ?;
`

const hasColumnSQL = `
SELECT COUNT(*)
FROM information_schema.columns
WHERE table_schema = DATABASE()
  AND table_name = ?
  AND column_name = ?;
`

const hasIndexSQL = `
SELECT COUNT(*)
FROM information_schema.statistics
WHERE table_schema = DATABASE()
  AND table_name = ?
  AND index_name = ?;
`

type tableSchemaSpec struct {
	Name    string
	Columns []string
	Indexes []string
}

var requiredTableSchemas = []tableSchemaSpec{
	{
		Name: "backup_tasks",
		Columns: []string{
			"id", "name", "cluster_key", "state", "last_error", "owner_worker_id", "epoch", "run_id",
			"source_json", "start_json", "storage_json", "updated_at",
		},
		Indexes: []string{"PRIMARY", "uk_backup_tasks_cluster_key"},
	},
	{
		Name: "backup_checkpoints",
		Columns: []string{
			"task_id", "file_name", "pos", "gtid_set", "updated_at",
		},
		Indexes: []string{"PRIMARY"},
	},
	{
		Name: "task_events",
		Columns: []string{
			"id", "task_id", "event_type", "message", "detail", "event_time", "event_seq",
		},
		Indexes: []string{"PRIMARY", "idx_task_events_task_time"},
	},
	{
		Name: "binlog_files",
		Columns: []string{
			"id", "task_id", "file_name", "source_file", "file_path", "epoch", "state", "checksum",
			"size_bytes", "start_pos", "end_pos", "created_at", "sealed_at", "object_key",
			"upload_state", "upload_error", "uploaded_at",
		},
		Indexes: []string{"PRIMARY", "uk_task_file", "idx_task_sealed"},
	},
	{
		Name: "task_leases",
		Columns: []string{
			"task_id", "owner_worker_id", "epoch", "lease_expire_at", "renewed_at",
		},
		Indexes: []string{"PRIMARY"},
	},
	{
		Name: "task_runs",
		Columns: []string{
			"run_id", "task_id", "worker_id", "epoch", "started_at", "ended_at", "end_reason",
		},
		Indexes: []string{"PRIMARY", "idx_task_runs_task_started"},
	},
	{
		Name: "worker_heartbeats",
		Columns: []string{
			"worker_id", "host", "version", "last_seen_at", "status",
		},
		Indexes: []string{"PRIMARY", "idx_worker_heartbeats_seen"},
	},
	{
		Name: "worker_registrations",
		Columns: []string{
			"worker_id", "session_id", "lease_expire_at", "renewed_at",
		},
		Indexes: []string{"PRIMARY", "idx_worker_registrations_expire"},
	},
}

const upsertTaskSQL = `
INSERT INTO backup_tasks (id, name, cluster_key, state, last_error, owner_worker_id, epoch, run_id, source_json, start_json, storage_json, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
  name = VALUES(name),
  cluster_key = VALUES(cluster_key),
  state = VALUES(state),
  last_error = VALUES(last_error),
  owner_worker_id = VALUES(owner_worker_id),
  epoch = VALUES(epoch),
  run_id = VALUES(run_id),
  source_json = VALUES(source_json),
  start_json = VALUES(start_json),
  storage_json = VALUES(storage_json),
  updated_at = VALUES(updated_at);
`

const listTaskSQL = `
SELECT id, name, cluster_key, state, last_error, owner_worker_id, epoch, run_id, source_json, start_json, storage_json, updated_at
FROM backup_tasks
ORDER BY id;
`

const deleteTaskSQL = `
DELETE FROM backup_tasks
WHERE id = ?;
`

const upsertCheckpointSQL = `
INSERT INTO backup_checkpoints (task_id, file_name, pos, gtid_set, updated_at)
VALUES (?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
  file_name = VALUES(file_name),
  pos = VALUES(pos),
  gtid_set = VALUES(gtid_set),
  updated_at = VALUES(updated_at);
`

const loadCheckpointSQL = `
SELECT file_name, pos, gtid_set, updated_at
FROM backup_checkpoints
WHERE task_id = ?;
`

const insertTaskEventSQL = `
INSERT INTO task_events (task_id, event_type, message, detail, event_time, event_seq)
VALUES (?, ?, ?, ?, ?, ?);
`

const listTaskEventsSQL = `
SELECT task_id, event_type, message, detail, event_time, event_seq
FROM task_events
WHERE task_id = ?
ORDER BY id DESC
LIMIT ?;
`

const upsertBinlogFileSQL = `
INSERT INTO binlog_files (
  task_id, file_name, file_path, size_bytes, start_pos, end_pos, created_at, sealed_at,
  object_key, upload_state, upload_error, uploaded_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
  file_path = VALUES(file_path),
  size_bytes = VALUES(size_bytes),
  start_pos = VALUES(start_pos),
  end_pos = VALUES(end_pos),
  created_at = VALUES(created_at),
  sealed_at = VALUES(sealed_at),
  object_key = VALUES(object_key),
  upload_state = VALUES(upload_state),
  upload_error = VALUES(upload_error),
  uploaded_at = VALUES(uploaded_at);
`

const listBinlogFilesSQL = `
SELECT task_id, file_name, file_path, size_bytes, start_pos, end_pos, created_at, sealed_at,
       object_key, upload_state, upload_error, uploaded_at
FROM binlog_files
WHERE task_id = ?
ORDER BY sealed_at DESC
LIMIT ?;
`

const listFailedSealedBinlogFilesSQL = `
SELECT task_id, file_name, file_path, size_bytes, start_pos, end_pos, created_at, sealed_at,
       object_key, upload_state, upload_error, uploaded_at
FROM binlog_files
WHERE task_id = ?
  AND upload_state = 'UPLOAD_FAILED'
  AND file_name NOT LIKE '%.open.e%'
ORDER BY sealed_at DESC
LIMIT ?;
`

const countUploadFailuresSQL = `
SELECT COUNT(*)
FROM binlog_files
WHERE upload_state = 'UPLOAD_FAILED';
`

const listUploadFailureReasonDetailsSQL = `
SELECT upload_error, uploaded_at, sealed_at, created_at
FROM binlog_files
WHERE task_id = ?
  AND upload_state = 'UPLOAD_FAILED';
`

const loadTaskRunStateSQL = `
SELECT run_id
FROM backup_tasks
WHERE id = ?;
`

const insertTaskRunSQL = `
INSERT INTO task_runs (run_id, task_id, worker_id, epoch, started_at)
VALUES (?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
  task_id = VALUES(task_id),
  worker_id = VALUES(worker_id),
  epoch = VALUES(epoch),
  started_at = VALUES(started_at);
`

const finishTaskRunSQL = `
UPDATE task_runs
SET ended_at = ?, end_reason = ?
WHERE run_id = ? AND ended_at IS NULL;
`

const listTaskRunsSQL = `
SELECT run_id, task_id, worker_id, epoch, started_at, ended_at, end_reason
FROM task_runs
WHERE task_id = ?
ORDER BY started_at DESC
LIMIT ?;
`

const upsertWorkerHeartbeatSQL = `
INSERT INTO worker_heartbeats (worker_id, host, version, last_seen_at, status)
VALUES (?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
  host = VALUES(host),
  version = VALUES(version),
  last_seen_at = VALUES(last_seen_at),
  status = VALUES(status);
`

const listWorkerHeartbeatsSQL = `
SELECT worker_id, host, version, last_seen_at, status
FROM worker_heartbeats
ORDER BY worker_id ASC
LIMIT ?;
`

// acquireWorkerRegistrationSQL 以单条 UPSERT 表达“同 session 续租 + 过期接管”语义。
const acquireWorkerRegistrationSQL = `
INSERT INTO worker_registrations (worker_id, session_id, lease_expire_at, renewed_at)
VALUES (?, ?, DATE_ADD(NOW(6), INTERVAL ? MICROSECOND), NOW(6))
ON DUPLICATE KEY UPDATE
  session_id = IF(session_id = VALUES(session_id) OR lease_expire_at <= NOW(6), VALUES(session_id), session_id),
  lease_expire_at = IF(session_id = VALUES(session_id) OR lease_expire_at <= NOW(6), VALUES(lease_expire_at), lease_expire_at),
  renewed_at = IF(session_id = VALUES(session_id) OR lease_expire_at <= NOW(6), NOW(6), renewed_at);
`

// renewWorkerRegistrationSQL 只允许当前 session 续租（条件更新）。
const renewWorkerRegistrationSQL = `
UPDATE worker_registrations
SET lease_expire_at = DATE_ADD(NOW(6), INTERVAL ? MICROSECOND), renewed_at = NOW(6)
WHERE worker_id = ? AND session_id = ?;
`

// releaseWorkerRegistrationSQL 只删除当前 session 的注册记录，防止误删他人会话。
const releaseWorkerRegistrationSQL = `
DELETE FROM worker_registrations
WHERE worker_id = ? AND session_id = ?;
`

const getWorkerRegistrationSQL = `
SELECT session_id, lease_expire_at
FROM worker_registrations
WHERE worker_id = ?;
`

type MySQLTaskStore struct {
	db *sql.DB
}

// NewMySQLTaskStore 创建 MySQL 元数据存储并校验 schema 就绪。
func NewMySQLTaskStore(dsn string) (*MySQLTaskStore, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}

	store := newMySQLTaskStoreFromDB(db)
	if err := store.ensureSchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// newMySQLTaskStoreFromDB 基于现有 DB 句柄创建存储实例（用于测试注入）。
func newMySQLTaskStoreFromDB(db *sql.DB) *MySQLTaskStore {
	return &MySQLTaskStore{db: db}
}

// Close 关闭底层数据库连接。
func (s *MySQLTaskStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// ensureSchema 校验元数据表结构是否满足当前版本要求。
func (s *MySQLTaskStore) ensureSchema(ctx context.Context) error {
	if err := s.ensureSchemaVersion(ctx); err != nil {
		return err
	}

	var missing []string
	for _, table := range requiredTableSchemas {
		exists, err := s.hasTable(ctx, table.Name)
		if err != nil {
			return err
		}
		if !exists {
			missing = append(missing, fmt.Sprintf("missing table %s", table.Name))
			continue
		}
		for _, column := range table.Columns {
			hasColumn, err := s.hasColumn(ctx, table.Name, column)
			if err != nil {
				return err
			}
			if !hasColumn {
				missing = append(missing, fmt.Sprintf("missing column %s.%s", table.Name, column))
			}
		}
		for _, index := range table.Indexes {
			hasIndex, err := s.hasIndex(ctx, table.Name, index)
			if err != nil {
				return err
			}
			if !hasIndex {
				missing = append(missing, fmt.Sprintf("missing index %s.%s", table.Name, index))
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf(
		"metadata schema is not up to date; apply database migration before startup: %s",
		strings.Join(missing, "; "),
	)
}

func (s *MySQLTaskStore) ensureSchemaVersion(ctx context.Context) error {
	var (
		version int64
		dirty   bool
	)
	err := s.db.QueryRowContext(ctx, currentSchemaVersionSQL).Scan(&version, &dirty)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("schema_migrations is empty; run migrations to version >= %d", minRequiredSchemaVersion)
		}
		if isMySQLTableNotFoundError(err) {
			return fmt.Errorf("missing table schema_migrations; run golang-migrate before startup")
		}
		return err
	}
	if dirty {
		return fmt.Errorf("schema_migrations is dirty at version=%d; repair migration state before startup", version)
	}
	if version < minRequiredSchemaVersion {
		return fmt.Errorf("schema version too old: current=%d required>=%d", version, minRequiredSchemaVersion)
	}
	return nil
}

func isMySQLTableNotFoundError(err error) bool {
	var mysqlErr *mysqlDriver.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1146
}

func (s *MySQLTaskStore) hasTable(ctx context.Context, tableName string) (bool, error) {
	var count int
	row := s.db.QueryRowContext(ctx, hasTableSQL, tableName)
	if err := row.Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *MySQLTaskStore) hasColumn(ctx context.Context, tableName, columnName string) (bool, error) {
	var count int
	row := s.db.QueryRowContext(ctx, hasColumnSQL, tableName, columnName)
	if err := row.Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *MySQLTaskStore) hasIndex(ctx context.Context, tableName, indexName string) (bool, error) {
	var count int
	row := s.db.QueryRowContext(ctx, hasIndexSQL, tableName, indexName)
	if err := row.Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// UpsertTask 写入任务最新快照，并在状态收敛时补写 run 终态信息。
func (s *MySQLTaskStore) UpsertTask(ctx context.Context, task tasks.Task) error {
	sourceJSON, err := json.Marshal(task.Source)
	if err != nil {
		return err
	}
	startJSON, err := json.Marshal(task.Start)
	if err != nil {
		return err
	}
	storageJSON, err := json.Marshal(task.Storage)
	if err != nil {
		return err
	}

	updatedAt := task.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var previousRunID sql.NullString
	row := tx.QueryRowContext(ctx, loadTaskRunStateSQL, task.ID)
	if err := row.Scan(&previousRunID); err != nil && err != sql.ErrNoRows {
		return err
	}

	if _, err = tx.ExecContext(
		ctx,
		upsertTaskSQL,
		task.ID,
		task.Name,
		task.ClusterKey,
		string(task.State),
		task.LastError,
		task.OwnerWorkerID,
		task.Epoch,
		task.RunID,
		string(sourceJSON),
		string(startJSON),
		string(storageJSON),
		updatedAt,
	); err != nil {
		return err
	}

	currentRunID := strings.TrimSpace(task.RunID)
	previous := strings.TrimSpace(previousRunID.String)

	if currentRunID != "" && currentRunID != previous {
		if _, err = tx.ExecContext(
			ctx,
			insertTaskRunSQL,
			currentRunID,
			task.ID,
			task.OwnerWorkerID,
			task.Epoch,
			updatedAt,
		); err != nil {
			return err
		}
	}

	if previous != "" && previous != currentRunID {
		if _, err = tx.ExecContext(
			ctx,
			finishTaskRunSQL,
			updatedAt,
			inferRunEndReason(task),
			previous,
		); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// ListTasks 读取全部任务快照并反序列化配置字段。
func (s *MySQLTaskStore) ListTasks(ctx context.Context) ([]tasks.Task, error) {
	rows, err := s.db.QueryContext(ctx, listTaskSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []tasks.Task
	for rows.Next() {
		var (
			task        tasks.Task
			state       string
			ownerWorker sql.NullString
			epoch       int64
			runID       sql.NullString
			sourceJSON  string
			startJSON   string
			storageJSON string
			lastError   sql.NullString
			updatedAt   time.Time
		)
		if err := rows.Scan(
			&task.ID,
			&task.Name,
			&task.ClusterKey,
			&state,
			&lastError,
			&ownerWorker,
			&epoch,
			&runID,
			&sourceJSON,
			&startJSON,
			&storageJSON,
			&updatedAt,
		); err != nil {
			return nil, err
		}

		task.State = tasks.State(state)
		task.LastError = lastError.String
		task.OwnerWorkerID = ownerWorker.String
		task.Epoch = epoch
		task.RunID = runID.String
		task.UpdatedAt = updatedAt
		if err := json.Unmarshal([]byte(sourceJSON), &task.Source); err != nil {
			return nil, fmt.Errorf("decode source json for task %s: %w", task.ID, err)
		}
		if err := json.Unmarshal([]byte(startJSON), &task.Start); err != nil {
			return nil, fmt.Errorf("decode start json for task %s: %w", task.ID, err)
		}
		if err := json.Unmarshal([]byte(storageJSON), &task.Storage); err != nil {
			return nil, fmt.Errorf("decode storage json for task %s: %w", task.ID, err)
		}

		list = append(list, task)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

// DeleteTask 删除任务及其关联元数据。
func (s *MySQLTaskStore) DeleteTask(ctx context.Context, taskID string) error {
	_, err := s.db.ExecContext(ctx, deleteTaskSQL, taskID)
	return err
}

// UpsertCheckpoint 写入任务 checkpoint 快照。
func (s *MySQLTaskStore) UpsertCheckpoint(ctx context.Context, taskID string, checkpoint binlog.Checkpoint) error {
	updatedAt := checkpoint.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}

	return WithRetry(ctx, DefaultMySQLRetryPolicy(), func() error {
		_, err := s.db.ExecContext(
			ctx,
			upsertCheckpointSQL,
			taskID,
			checkpoint.File,
			checkpoint.Pos,
			checkpoint.GTIDSet,
			updatedAt,
		)
		return err
	})
}

// LoadCheckpoint 读取任务最近 checkpoint。
func (s *MySQLTaskStore) LoadCheckpoint(ctx context.Context, taskID string) (binlog.Checkpoint, bool, error) {
	var (
		cp      binlog.Checkpoint
		gtidSet sql.NullString
	)

	row := s.db.QueryRowContext(ctx, loadCheckpointSQL, taskID)
	if err := row.Scan(&cp.File, &cp.Pos, &gtidSet, &cp.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return binlog.Checkpoint{}, false, nil
		}
		return binlog.Checkpoint{}, false, err
	}
	cp.GTIDSet = gtidSet.String
	return cp, true, nil
}

// AppendEvent 追加任务事件记录。
func (s *MySQLTaskStore) AppendEvent(ctx context.Context, event tasks.TaskEvent) error {
	eventTime := event.Time
	if eventTime.IsZero() {
		eventTime = time.Now()
	}

	_, err := s.db.ExecContext(
		ctx,
		insertTaskEventSQL,
		event.TaskID,
		event.Type,
		event.Message,
		event.Detail,
		eventTime,
		event.Sequence,
	)
	return err
}

// ListEvents 按时间倒序读取任务事件，并限制返回条数。
func (s *MySQLTaskStore) ListEvents(ctx context.Context, taskID string, limit int) ([]tasks.TaskEvent, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, listTaskEventsSQL, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []tasks.TaskEvent
	for rows.Next() {
		var e tasks.TaskEvent
		if err := rows.Scan(&e.TaskID, &e.Type, &e.Message, &e.Detail, &e.Time, &e.Sequence); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Return events in ascending order for stable timeline.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// UpsertBinlogFile 写入/更新 binlog 文件元数据。
func (s *MySQLTaskStore) UpsertBinlogFile(ctx context.Context, meta tasks.BinlogFile) error {
	createdAt := meta.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	sealedAt := meta.SealedAt
	if sealedAt.IsZero() {
		sealedAt = createdAt
	}
	uploadedAt := sql.NullTime{}
	if !meta.UploadedAt.IsZero() {
		uploadedAt.Valid = true
		uploadedAt.Time = meta.UploadedAt
	}
	uploadState := meta.UploadState
	if uploadState == "" {
		uploadState = "LOCAL_ONLY"
	}

	return WithRetry(ctx, DefaultMySQLRetryPolicy(), func() error {
		_, err := s.db.ExecContext(
			ctx,
			upsertBinlogFileSQL,
			meta.TaskID,
			meta.FileName,
			meta.FilePath,
			meta.SizeBytes,
			meta.StartPos,
			meta.EndPos,
			createdAt,
			sealedAt,
			meta.ObjectKey,
			uploadState,
			meta.UploadError,
			uploadedAt,
		)
		return err
	})
}

// ListBinlogFiles 列出任务 binlog 文件元数据（按更新时间倒序）。
func (s *MySQLTaskStore) ListBinlogFiles(ctx context.Context, taskID string, limit int) ([]tasks.BinlogFile, error) {
	if limit <= 0 {
		limit = 200
	}

	rows, err := s.db.QueryContext(ctx, listBinlogFilesSQL, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []tasks.BinlogFile
	for rows.Next() {
		var item tasks.BinlogFile
		var uploadedAt sql.NullTime
		if err := rows.Scan(
			&item.TaskID,
			&item.FileName,
			&item.FilePath,
			&item.SizeBytes,
			&item.StartPos,
			&item.EndPos,
			&item.CreatedAt,
			&item.SealedAt,
			&item.ObjectKey,
			&item.UploadState,
			&item.UploadError,
			&uploadedAt,
		); err != nil {
			return nil, err
		}
		if uploadedAt.Valid {
			item.UploadedAt = uploadedAt.Time
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ListFailedUploadBinlogFiles 列出上传失败的 sealed 文件。
func (s *MySQLTaskStore) ListFailedUploadBinlogFiles(ctx context.Context, taskID string, limit int) ([]tasks.BinlogFile, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	rows, err := s.db.QueryContext(ctx, listFailedSealedBinlogFilesSQL, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []tasks.BinlogFile
	for rows.Next() {
		var item tasks.BinlogFile
		var uploadedAt sql.NullTime
		if err := rows.Scan(
			&item.TaskID,
			&item.FileName,
			&item.FilePath,
			&item.SizeBytes,
			&item.StartPos,
			&item.EndPos,
			&item.CreatedAt,
			&item.SealedAt,
			&item.ObjectKey,
			&item.UploadState,
			&item.UploadError,
			&uploadedAt,
		); err != nil {
			return nil, err
		}
		if uploadedAt.Valid {
			item.UploadedAt = uploadedAt.Time
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// CountUploadFailures 统计上传失败文件总数。
func (s *MySQLTaskStore) CountUploadFailures(ctx context.Context) (int64, error) {
	var count int64
	row := s.db.QueryRowContext(ctx, countUploadFailuresSQL)
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// ListUploadFailureReasons 聚合任务上传失败原因并按次数排序。
func (s *MySQLTaskStore) ListUploadFailureReasons(ctx context.Context, taskID string, limit int) ([]tasks.UploadFailureReason, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}

	rows, err := s.db.QueryContext(ctx, listUploadFailureReasonDetailsSQL, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	agg := make(map[string]tasks.UploadFailureReason)
	for rows.Next() {
		var (
			rawReason string
			uploaded  sql.NullTime
			sealedAt  time.Time
			createdAt time.Time
		)
		if err := rows.Scan(&rawReason, &uploaded, &sealedAt, &createdAt); err != nil {
			return nil, err
		}
		reason := tasks.NormalizeUploadFailureReason(rawReason)
		item := agg[reason]
		item.Reason = reason
		item.Count++
		latest := sealedAt
		if createdAt.After(latest) {
			latest = createdAt
		}
		if uploaded.Valid && uploaded.Time.After(latest) {
			latest = uploaded.Time
		}
		if latest.After(item.LatestTime) {
			item.LatestTime = latest
		}
		agg[reason] = item
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]tasks.UploadFailureReason, 0, len(agg))
	for _, item := range agg {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if !out[i].LatestTime.Equal(out[j].LatestTime) {
			return out[i].LatestTime.After(out[j].LatestTime)
		}
		return out[i].Reason < out[j].Reason
	})
	if limit < len(out) {
		out = out[:limit]
	}
	return out, nil
}

// ListTaskRuns 列出任务运行历史记录。
func (s *MySQLTaskStore) ListTaskRuns(ctx context.Context, taskID string, limit int) ([]tasks.TaskRun, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 200 {
		limit = 200
	}

	rows, err := s.db.QueryContext(ctx, listTaskRunsSQL, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []tasks.TaskRun
	for rows.Next() {
		var item tasks.TaskRun
		var endedAt sql.NullTime
		var endReason sql.NullString
		if err := rows.Scan(
			&item.RunID,
			&item.TaskID,
			&item.WorkerID,
			&item.Epoch,
			&item.StartedAt,
			&endedAt,
			&endReason,
		); err != nil {
			return nil, err
		}
		if endedAt.Valid {
			item.EndedAt = endedAt.Time
		}
		if endReason.Valid {
			item.EndReason = endReason.String
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// inferRunEndReason 根据任务终态字段推导 run 结束原因枚举。
func inferRunEndReason(task tasks.Task) string {
	if task.LastError == "" {
		return "NORMAL_STOP"
	}
	if strings.Contains(strings.ToLower(task.LastError), "lease") {
		if strings.Contains(strings.ToLower(task.LastError), "grace") {
			return "LEASE_GRACE_EXCEEDED"
		}
		return "LEASE_LOST"
	}
	return "STOP_WITH_ERROR"
}

// UpsertWorkerHeartbeat 写入 worker 心跳记录。
func (s *MySQLTaskStore) UpsertWorkerHeartbeat(ctx context.Context, hb tasks.WorkerHeartbeat) error {
	lastSeenAt := hb.LastSeenAt
	if lastSeenAt.IsZero() {
		lastSeenAt = time.Now()
	}
	_, err := s.db.ExecContext(
		ctx,
		upsertWorkerHeartbeatSQL,
		hb.WorkerID,
		hb.Host,
		hb.Version,
		lastSeenAt,
		hb.Status,
	)
	return err
}

// ListWorkerHeartbeats 列出 worker 心跳快照。
func (s *MySQLTaskStore) ListWorkerHeartbeats(ctx context.Context, limit int) ([]tasks.WorkerHeartbeat, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, listWorkerHeartbeatsSQL, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []tasks.WorkerHeartbeat
	for rows.Next() {
		var item tasks.WorkerHeartbeat
		if err := rows.Scan(
			&item.WorkerID,
			&item.Host,
			&item.Version,
			&item.LastSeenAt,
			&item.Status,
		); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// AcquireWorkerRegistration 获取 worker_id 注册所有权。
// 语义：
// 1) 若 worker_id 不存在，创建并占有；
// 2) 若已被同 session 占有，续租并保持占有；
// 3) 若被其他 session 占有但已过期，接管；
// 4) 若被其他 session 活跃占有，返回 ok=false。
func (s *MySQLTaskStore) AcquireWorkerRegistration(ctx context.Context, workerID, sessionID string, ttl time.Duration) (bool, error) {
	var ok bool
	err := WithRetry(ctx, DefaultMySQLRetryPolicy(), func() error {
		// 先执行“可接管条件更新”：
		// 仅当“同 session”或“已过期”时，才会改写 session_id 与过期时间。
		_, err := s.db.ExecContext(
			ctx,
			acquireWorkerRegistrationSQL,
			workerID,
			sessionID,
			durationToMicroseconds(ttl),
		)
		if err != nil {
			return err
		}

		// 再回读 + 对齐 DB 当前时间判定最终结果，避免应用机时钟偏差。
		reg, exists, err := s.getWorkerRegistrationNoRetry(ctx, workerID)
		if err != nil {
			return err
		}
		if !exists {
			ok = false
			return nil
		}
		var dbNow time.Time
		if err := s.db.QueryRowContext(ctx, currentDBTimeSQL).Scan(&dbNow); err != nil {
			return err
		}
		ok = reg.sessionID == sessionID && reg.leaseExpireAt.After(dbNow)
		return nil
	})
	if err != nil {
		return false, err
	}
	return ok, nil
}

// RenewWorkerRegistration 为当前 session 续约 worker_id 注册。
// 仅当 (worker_id, session_id) 精确匹配时更新成功；若返回 ok=false，表示所有权已不在当前 session。
func (s *MySQLTaskStore) RenewWorkerRegistration(ctx context.Context, workerID, sessionID string, ttl time.Duration) (bool, error) {
	var ok bool
	err := WithRetry(ctx, DefaultMySQLRetryPolicy(), func() error {
		result, err := s.db.ExecContext(
			ctx,
			renewWorkerRegistrationSQL,
			durationToMicroseconds(ttl),
			workerID,
			sessionID,
		)
		if err != nil {
			return err
		}
		ok, err = rowsAffectedGreaterThanZero(result)
		return err
	})
	if err != nil {
		return false, err
	}
	return ok, nil
}

// ReleaseWorkerRegistration 释放当前 session 的 worker_id 注册记录。
// 删除条件包含 session_id，避免误删其他实例刚接管的注册。
func (s *MySQLTaskStore) ReleaseWorkerRegistration(ctx context.Context, workerID, sessionID string) error {
	return WithRetry(ctx, DefaultMySQLRetryPolicy(), func() error {
		_, err := s.db.ExecContext(
			ctx,
			releaseWorkerRegistrationSQL,
			workerID,
			sessionID,
		)
		return err
	})
}

type workerRegistrationRecord struct {
	sessionID     string
	leaseExpireAt time.Time
}

// getWorkerRegistrationNoRetry 读取 worker 注册记录（不带重试封装）。
func (s *MySQLTaskStore) getWorkerRegistrationNoRetry(ctx context.Context, workerID string) (workerRegistrationRecord, bool, error) {
	var rec workerRegistrationRecord
	row := s.db.QueryRowContext(ctx, getWorkerRegistrationSQL, workerID)
	if err := row.Scan(&rec.sessionID, &rec.leaseExpireAt); err != nil {
		if err == sql.ErrNoRows {
			return workerRegistrationRecord{}, false, nil
		}
		return workerRegistrationRecord{}, false, err
	}
	return rec, true, nil
}
