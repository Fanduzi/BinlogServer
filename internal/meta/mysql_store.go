package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"

	_ "github.com/go-sql-driver/mysql"
)

const createTaskTableSQL = `
CREATE TABLE IF NOT EXISTS backup_tasks (
  id VARCHAR(64) PRIMARY KEY,
  name VARCHAR(255) NOT NULL,
  state VARCHAR(32) NOT NULL,
  last_error TEXT NULL,
  source_json JSON NOT NULL,
  start_json JSON NOT NULL,
  storage_json JSON NOT NULL,
  updated_at DATETIME(6) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
`

const createCheckpointTableSQL = `
CREATE TABLE IF NOT EXISTS backup_checkpoints (
  task_id VARCHAR(64) PRIMARY KEY,
  file_name VARCHAR(255) NOT NULL,
  pos BIGINT UNSIGNED NOT NULL,
  gtid_set TEXT NULL,
  updated_at DATETIME(6) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
`

const createTaskEventsTableSQL = `
CREATE TABLE IF NOT EXISTS task_events (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  task_id VARCHAR(64) NOT NULL,
  event_type VARCHAR(64) NOT NULL,
  message TEXT NULL,
  detail TEXT NULL,
  event_time DATETIME(6) NOT NULL,
  event_seq BIGINT NOT NULL,
  INDEX idx_task_events_task_time (task_id, event_time)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
`

const createBinlogFilesTableSQL = `
CREATE TABLE IF NOT EXISTS binlog_files (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  task_id VARCHAR(64) NOT NULL,
  file_name VARCHAR(255) NOT NULL,
  source_file VARCHAR(255) NULL,
  file_path TEXT NOT NULL,
  epoch BIGINT NOT NULL DEFAULT 0,
  state VARCHAR(32) NOT NULL DEFAULT 'SEALED',
  checksum VARCHAR(128) NULL,
  size_bytes BIGINT NOT NULL,
  start_pos BIGINT UNSIGNED NOT NULL,
  end_pos BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(6) NOT NULL,
  sealed_at DATETIME(6) NOT NULL,
  object_key TEXT NULL,
  upload_state VARCHAR(32) NOT NULL DEFAULT 'LOCAL_ONLY',
  upload_error TEXT NULL,
  uploaded_at DATETIME(6) NULL,
  UNIQUE KEY uk_task_file (task_id, file_name),
  INDEX idx_task_sealed (task_id, sealed_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
`

const createTaskLeasesTableSQL = `
CREATE TABLE IF NOT EXISTS task_leases (
  task_id VARCHAR(64) PRIMARY KEY,
  owner_worker_id VARCHAR(128) NOT NULL,
  epoch BIGINT NOT NULL,
  lease_expire_at DATETIME(6) NOT NULL,
  renewed_at DATETIME(6) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
`

const createTaskRunsTableSQL = `
CREATE TABLE IF NOT EXISTS task_runs (
  run_id VARCHAR(64) PRIMARY KEY,
  task_id VARCHAR(64) NOT NULL,
  worker_id VARCHAR(128) NOT NULL,
  epoch BIGINT NOT NULL,
  started_at DATETIME(6) NOT NULL,
  ended_at DATETIME(6) NULL,
  end_reason VARCHAR(64) NULL,
  INDEX idx_task_runs_task_started (task_id, started_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
`

const upsertTaskSQL = `
INSERT INTO backup_tasks (id, name, state, last_error, source_json, start_json, storage_json, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
  name = VALUES(name),
  state = VALUES(state),
  last_error = VALUES(last_error),
  source_json = VALUES(source_json),
  start_json = VALUES(start_json),
  storage_json = VALUES(storage_json),
  updated_at = VALUES(updated_at);
`

const listTaskSQL = `
SELECT id, name, state, last_error, source_json, start_json, storage_json, updated_at
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

type MySQLTaskStore struct {
	db *sql.DB
}

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

func newMySQLTaskStoreFromDB(db *sql.DB) *MySQLTaskStore {
	return &MySQLTaskStore{db: db}
}

func (s *MySQLTaskStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *MySQLTaskStore) ensureSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, createTaskTableSQL)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, createCheckpointTableSQL)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, createTaskEventsTableSQL)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, createBinlogFilesTableSQL)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, createTaskLeasesTableSQL)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, createTaskRunsTableSQL)
	return err
}

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

	_, err = s.db.ExecContext(
		ctx,
		upsertTaskSQL,
		task.ID,
		task.Name,
		string(task.State),
		task.LastError,
		string(sourceJSON),
		string(startJSON),
		string(storageJSON),
		updatedAt,
	)
	return err
}

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
			sourceJSON  string
			startJSON   string
			storageJSON string
			lastError   sql.NullString
			updatedAt   time.Time
		)
		if err := rows.Scan(
			&task.ID,
			&task.Name,
			&state,
			&lastError,
			&sourceJSON,
			&startJSON,
			&storageJSON,
			&updatedAt,
		); err != nil {
			return nil, err
		}

		task.State = tasks.State(state)
		task.LastError = lastError.String
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

func (s *MySQLTaskStore) DeleteTask(ctx context.Context, taskID string) error {
	_, err := s.db.ExecContext(ctx, deleteTaskSQL, taskID)
	return err
}

func (s *MySQLTaskStore) UpsertCheckpoint(ctx context.Context, taskID string, checkpoint binlog.Checkpoint) error {
	updatedAt := checkpoint.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}

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
}

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
}

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
