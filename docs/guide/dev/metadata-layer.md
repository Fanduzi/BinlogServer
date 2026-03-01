# 元数据层

本文档详细分析元数据存储的设计和实现。

## 1. 元数据概览

```
┌─────────────────────────────────────────────────────────────┐
│                       元数据层                               │
│                                                             │
│  ┌─────────┐ ┌─────────┐ ┌─────────┐ ┌─────────┐          │
│  │  Store  │ │Checkpoint│ │ Event  │ │  File  │           │
│  │ (任务)  │ │  Store   │ │ Store  │ │ Store  │           │
│  └────┬────┘ └────┬────┘ └────┬────┘ └────┬────┘          │
│       │           │           │           │                │
│       └───────────┴───────────┴───────────┘                │
│                       │                                     │
│                       ▼                                     │
│              ┌───────────────┐                             │
│              │  MySQLStore   │                             │
│              │   (统一实现)   │                             │
│              └───────────────┘                             │
└─────────────────────────────────────────────────────────────┘
```

## 2. 接口设计

### 2.1 Store（任务存储）

```go
type Store interface {
    // UpsertTask 创建或更新任务
    UpsertTask(ctx context.Context, task Task) error

    // GetTask 获取单个任务
    GetTask(ctx context.Context, id string) (Task, bool, error)

    // ListTasks 列出所有任务
    ListTasks(ctx context.Context) ([]Task, error)

    // ListTasksByState 列出指定状态的任务
    ListTasksByState(ctx context.Context, state TaskState) ([]Task, error)

    // DeleteTask 删除任务
    DeleteTask(ctx context.Context, id string) error
}
```

### 2.2 CheckpointStore（位点存储）

```go
type CheckpointStore interface {
    // SaveCheckpoint 保存位点
    SaveCheckpoint(ctx context.Context, taskID string, cp Checkpoint) error

    // GetCheckpoint 获取位点
    GetCheckpoint(ctx context.Context, taskID string) (Checkpoint, bool, error)
}
```

### 2.3 EventStore（事件存储）

```go
type EventStore interface {
    // AppendEvent 追加事件
    AppendEvent(ctx context.Context, taskID, eventType, message, detail string) error

    // ListEvents 列出事件
    ListEvents(ctx context.Context, taskID string, limit int) ([]TaskEvent, error)
}
```

### 2.4 FileStore（文件元数据存储）

```go
type FileStore interface {
    // UpsertFile 创建或更新文件元数据
    UpsertFile(ctx context.Context, f FileMeta) error

    // ListFiles 列出文件
    ListFiles(ctx context.Context, taskID string) ([]FileMeta, error)

    // UpdateUploadStatus 更新上传状态
    UpdateUploadStatus(ctx context.Context, taskID, fileName, status, reason string) error
}
```

### 2.5 LeaseManager（租约管理）

```go
type LeaseManager interface {
    // Acquire 获取租约
    Acquire(ctx context.Context, taskID, workerID string, ttl time.Duration) (epoch int64, acquired bool, err error)

    // Renew 续租
    Renew(ctx context.Context, taskID, workerID string, epoch int64, ttl time.Duration) (bool, error)

    // Release 释放租约
    Release(ctx context.Context, taskID, workerID string, epoch int64) (bool, error)
}
```

## 3. 数据库表结构

### 3.1 tasks 表

```sql
CREATE TABLE tasks (
    id              VARCHAR(64) PRIMARY KEY,
    name            VARCHAR(255) NOT NULL,
    cluster_key     VARCHAR(255) NOT NULL,
    state           VARCHAR(32) NOT NULL,
    source_host     VARCHAR(255) NOT NULL,
    source_port     INT NOT NULL,
    source_user     VARCHAR(64) NOT NULL,
    source_password VARCHAR(255) NOT NULL,
    start_mode      VARCHAR(32) NOT NULL,
    start_file      VARCHAR(255),
    start_pos       INT,
    start_gtid      TEXT,
    storage_retention_days INT NOT NULL DEFAULT 7,
    owner_worker_id VARCHAR(64),
    epoch           BIGINT NOT NULL DEFAULT 0,
    last_error      TEXT,
    created_at      DATETIME(3) NOT NULL,
    updated_at      DATETIME(3) NOT NULL,
    INDEX idx_state (state),
    INDEX idx_cluster_key (cluster_key),
    INDEX idx_updated_at (updated_at)
);
```

### 3.2 checkpoints 表

```sql
CREATE TABLE checkpoints (
    task_id     VARCHAR(64) PRIMARY KEY,
    file        VARCHAR(255) NOT NULL,
    pos         INT NOT NULL,
    gtid        TEXT,
    updated_at  DATETIME(3) NOT NULL,
    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
);
```

### 3.3 task_events 表

```sql
CREATE TABLE task_events (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    task_id     VARCHAR(64) NOT NULL,
    event_type  VARCHAR(64) NOT NULL,
    message     VARCHAR(255) NOT NULL,
    detail      TEXT,
    created_at  DATETIME(3) NOT NULL,
    INDEX idx_task_id (task_id),
    INDEX idx_created_at (created_at),
    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
);
```

### 3.4 files 表

```sql
CREATE TABLE files (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    task_id         VARCHAR(64) NOT NULL,
    file_name       VARCHAR(255) NOT NULL,
    size            BIGINT NOT NULL,
    upload_status   VARCHAR(32) NOT NULL DEFAULT 'LOCAL_ONLY',
    upload_reason   TEXT,
    created_at      DATETIME(3) NOT NULL,
    updated_at      DATETIME(3) NOT NULL,
    UNIQUE KEY uk_task_file (task_id, file_name),
    INDEX idx_upload_status (upload_status),
    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
);
```

### 3.5 leases 表

```sql
CREATE TABLE leases (
    task_id     VARCHAR(64) PRIMARY KEY,
    worker_id   VARCHAR(64) NOT NULL,
    epoch       BIGINT NOT NULL,
    expires_at  DATETIME(3) NOT NULL,
    updated_at  DATETIME(3) NOT NULL,
    INDEX idx_expires_at (expires_at),
    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
);
```

### 3.6 worker_registrations 表

```sql
CREATE TABLE worker_registrations (
    worker_id   VARCHAR(64) PRIMARY KEY,
    session_id  VARCHAR(64) NOT NULL,
    role        VARCHAR(32) NOT NULL,
    expires_at  DATETIME(3) NOT NULL,
    updated_at  DATETIME(3) NOT NULL,
    INDEX idx_expires_at (expires_at)
);
```

## 4. MySQLStore 实现

### 4.1 结构

```go
type MySQLStore struct {
    db *sql.DB
}

func NewMySQLStore(cfg MySQLConfig) (*MySQLStore, error) {
    db, err := sql.Open("mysql", cfg.DSN)
    if err != nil {
        return nil, err
    }

    // 连接池配置
    db.SetMaxOpenConns(cfg.MaxOpenConns)
    db.SetMaxIdleConns(cfg.MaxIdleConns)
    db.SetConnMaxLifetime(cfg.ConnMaxLifetime)

    // 验证连接
    if err := db.Ping(); err != nil {
        return nil, err
    }

    return &MySQLStore{db: db}, nil
}
```

### 4.2 UpsertTask 实现

```go
func (s *MySQLStore) UpsertTask(ctx context.Context, task Task) error {
    query := `
        INSERT INTO tasks (
            id, name, cluster_key, state, source_host, source_port,
            source_user, source_password, start_mode, start_file,
            start_pos, start_gtid, storage_retention_days,
            owner_worker_id, epoch, last_error, created_at, updated_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON DUPLICATE KEY UPDATE
            name = VALUES(name),
            state = VALUES(state),
            owner_worker_id = VALUES(owner_worker_id),
            epoch = VALUES(epoch),
            last_error = VALUES(last_error),
            updated_at = VALUES(updated_at)
    `

    _, err := s.db.ExecContext(ctx, query,
        task.ID, task.Name, task.ClusterKey, task.State,
        task.Source.Host, task.Source.Port, task.Source.User, task.Source.Password,
        task.Start.Mode, task.Start.File, task.Start.Pos, task.Start.GTID,
        task.Storage.RetentionDays,
        task.OwnerWorkerID, task.Epoch, task.LastError,
        task.CreatedAt, task.UpdatedAt,
    )

    return err
}
```

### 4.3 Acquire Lease 实现

```go
func (s *MySQLStore) Acquire(ctx context.Context, taskID, workerID string, ttl time.Duration) (int64, bool, error) {
    now := time.Now()
    expiresAt := now.Add(ttl)

    // 原子操作：检查并更新
    query := `
        INSERT INTO leases (task_id, worker_id, epoch, expires_at, updated_at)
        VALUES (?, ?, 1, ?, ?)
        ON DUPLICATE KEY UPDATE
            worker_id = IF(expires_at < ?, VALUES(worker_id), worker_id),
            epoch = IF(expires_at < ?, epoch + 1, epoch),
            expires_at = IF(expires_at < ?, VALUES(expires_at), expires_at),
            updated_at = VALUES(updated_at)
    `

    result, err := s.db.ExecContext(ctx, query,
        taskID, workerID, expiresAt, now,
        now, now, now,
    )
    if err != nil {
        return 0, false, err
    }

    // 查询结果
    var lease struct {
        WorkerID  string
        Epoch     int64
        ExpiresAt time.Time
    }

    err = s.db.QueryRowContext(ctx,
        "SELECT worker_id, epoch, expires_at FROM leases WHERE task_id = ?",
        taskID,
    ).Scan(&lease.WorkerID, &lease.Epoch, &lease.ExpiresAt)

    if err != nil {
        return 0, false, err
    }

    // 判断是否获取成功
    acquired := lease.WorkerID == workerID && lease.ExpiresAt.After(now)
    return lease.Epoch, acquired, nil
}
```

## 5. 重试机制

### 5.1 重试配置

```go
type RetryConfig struct {
    MaxAttempts int           // 最大重试次数
    InitialWait time.Duration // 初始等待时间
    MaxWait     time.Duration // 最大等待时间
    Multiplier  float64       // 退避倍数
}
```

### 5.2 重试实现

```go
func (s *MySQLStore) withRetry(ctx context.Context, fn func() error) error {
    cfg := RetryConfig{
        MaxAttempts: 3,
        InitialWait: 100 * time.Millisecond,
        MaxWait:     5 * time.Second,
        Multiplier:  2.0,
    }

    var lastErr error
    wait := cfg.InitialWait

    for i := 0; i < cfg.MaxAttempts; i++ {
        err := fn()
        if err == nil {
            return nil
        }

        lastErr = err

        // 判断是否可重试
        if !isRetriable(err) {
            return err
        }

        // 等待
        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-time.After(wait):
        }

        // 指数退避
        wait = time.Duration(float64(wait) * cfg.Multiplier)
        if wait > cfg.MaxWait {
            wait = cfg.MaxWait
        }
    }

    return lastErr
}
```

### 5.3 可重试错误

```go
func isRetriable(err error) bool {
    // 连接错误
    if strings.Contains(err.Error(), "connection") {
        return true
    }

    // 超时
    if strings.Contains(err.Error(), "timeout") {
        return true
    }

    // 死锁
    if strings.Contains(err.Error(), "deadlock") {
        return true
    }

    // 锁等待超时
    if strings.Contains(err.Error(), "lock wait timeout") {
        return true
    }

    return false
}
```

## 6. 事务使用

### 6.1 需要事务的场景

- 删除任务时同时删除相关数据
- 批量更新

### 6.2 事务示例

```go
func (s *MySQLStore) DeleteTask(ctx context.Context, id string) error {
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    // 删除相关数据（有外键级联，但显式删除更清晰）
    if _, err := tx.ExecContext(ctx, "DELETE FROM task_events WHERE task_id = ?", id); err != nil {
        return err
    }
    if _, err := tx.ExecContext(ctx, "DELETE FROM files WHERE task_id = ?", id); err != nil {
        return err
    }
    if _, err := tx.ExecContext(ctx, "DELETE FROM checkpoints WHERE task_id = ?", id); err != nil {
        return err
    }
    if _, err := tx.ExecContext(ctx, "DELETE FROM leases WHERE task_id = ?", id); err != nil {
        return err
    }

    // 删除任务
    if _, err := tx.ExecContext(ctx, "DELETE FROM tasks WHERE id = ?", id); err != nil {
        return err
    }

    return tx.Commit()
}
```

## 7. 代码位置

| 组件 | 文件 |
|------|------|
| Store 接口 | `internal/tasks/store.go` |
| MySQLStore | `internal/meta/mysql_store.go` |
| LeaseStore | `internal/meta/lease_store.go` |
| 重试逻辑 | `internal/meta/retry.go` |

## 8. 本章小结

1. **接口设计**：按职责拆分为多个小接口
2. **表结构**：外键级联保证数据一致性
3. **原子操作**：使用 INSERT ... ON DUPLICATE KEY UPDATE
4. **重试机制**：指数退避，区分可重试错误
