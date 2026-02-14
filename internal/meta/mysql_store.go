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
