# 元数据层

本文档详细分析元数据存储的设计和实现。

## 1. 元数据概览

```
┌─────────────────────────────────────────────────────────────┐
│                       元数据层                               │
│                                                             │
│  ┌─────────┐ ┌─────────┐ ┌─────────┐ ┌─────────┐          │
│  │TaskStore│ │Checkpoint│ │ Event  │ │ Binlog │           │
│  │ (任务)  │ │  Store   │ │ Store  │ │ Store  │           │
│  └────┬────┘ └────┬────┘ └────┬────┘ └────┬────┘          │
│       │           │           │           │                │
│       └───────────┴───────────┴───────────┴───────────────┘
│                       │                                     │
│                       ▼                                     │
│              ┌───────────────┐                             │
│              │ MySQLTaskStore│                             │
│              │   (统一实现)   │                             │
│              └───────────────┘                             │
└─────────────────────────────────────────────────────────────┘
```

## 2. 接口设计

### 2.1 TaskStore（任务存储）

```go
type TaskStore interface {
    // UpsertTask 创建或更新任务
    UpsertTask(ctx context.Context, task tasks.Task) error

    // GetTask 获取单个任务
    GetTask(ctx context.Context, id string) (tasks.Task, bool, error)

    // ListTasks 列出所有任务
    ListTasks(ctx context.Context) ([]tasks.Task, error)
}
```

### 2.2 CheckpointStore（位点存储）

```go
type CheckpointStore interface {
    // UpsertCheckpoint 保存位点
    UpsertCheckpoint(ctx context.Context, taskID string, checkpoint binlog.Checkpoint) error

    // LoadCheckpoint 读取位点
    LoadCheckpoint(ctx context.Context, taskID string) (binlog.Checkpoint, bool, error)
}
```

### 2.3 EventStore（事件存储）

```go
type EventStore interface {
    // AppendEvent 追加事件
    AppendEvent(ctx context.Context, taskID string, eventType, message, detail string) error

    // ListEvents 列出事件
    ListEvents(ctx context.Context, taskID string, limit int) ([]tasks.TaskEvent, error)
}
```

### 2.4 FileMetaStore（文件元数据存储）

```go
type FileMetaStore interface {
    // UpsertBinlogFile 创建或更新文件元数据
    UpsertBinlogFile(ctx context.Context, meta tasks.BinlogFile) error

    // ListBinlogFiles 列出文件
    ListBinlogFiles(ctx context.Context, taskID string) ([]tasks.BinlogFile, error)
}
```

### 2.5 LeaseManager（租约管理）

```go
type LeaseManager interface {
    // Acquire 获取租约
    Acquire(ctx context.Context, taskID, workerID string, ttl time.Duration) (epoch int64, acquired bool, err error)

    // Renew 续租
    Renew(ctx context.Context, taskID, workerID string, epoch int64, now time.Time, ttl time.Duration) (bool, error)

    // Release 释放租约
    Release(ctx context.Context, taskID, workerID string, epoch int64) (bool, error)
}
```

### 2.6 WorkerStore（Worker 注册存储）

```go
type WorkerStore interface {
    // AcquireWorkerRegistration 获取 worker 注册
    AcquireWorkerRegistration(ctx context.Context, workerID, sessionID string, ttl time.Duration) error

    // RenewWorkerRegistration 续约 worker 注册
    RenewWorkerRegistration(ctx context.Context, workerID, sessionID string, now time.Time, ttl time.Duration) error

    // ReleaseWorkerRegistration 释放 worker 注册
    ReleaseWorkerRegistration(ctx context.Context, workerID, sessionID string) error
}
```

## 3. 数据库表结构

### 3.1 backup_tasks 表

```sql
CREATE TABLE backup_tasks (
    id VARCHAR(64) PRIMARY KEY,
    name VARCHAR(255) NOT NULL,
    cluster_key VARCHAR(255) NOT NULL,
    state VARCHAR(32) NOT NULL,
    last_error TEXT NULL,
    owner_worker_id VARCHAR(128) NULL,
    epoch BIGINT NOT NULL DEFAULT 1,
    run_id VARCHAR(128) NULL,
    source_json JSON NOT NULL,
    start_json JSON NOT NULL,
    storage_json JSON NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    UNIQUE KEY uk_backup_tasks_cluster_key (cluster_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

**JSON 字段说明：**

| 字段 | 内容 |
|------|------|
| `source_json` | `{"host":"...","port":3306,"user":"...","password":"...","flavor":"mysql","server_id":12345,"semi_sync":false}` |
| `start_json` | `{"mode":"LATEST"}` 或 `{"mode":"FILE_POS","file":"...","pos":123}` 或 `{"mode":"GTID","gtid_set":"..."}` |
| `storage_json` | `{"dir":"...","retention_days":30}` |

### 3.2 backup_checkpoints 表

```sql
CREATE TABLE backup_checkpoints (
    task_id VARCHAR(64) PRIMARY KEY,
    file_name VARCHAR(255) NOT NULL,
    pos BIGINT UNSIGNED NOT NULL,
    gtid_set TEXT NULL,
    updated_at DATETIME(6) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 3.3 task_events 表

```sql
CREATE TABLE task_events (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    task_id VARCHAR(64) NOT NULL,
    event_type VARCHAR(64) NOT NULL,
    message TEXT NULL,
    detail TEXT NULL,
    event_time DATETIME(6) NOT NULL,
    event_seq BIGINT NOT NULL,
    INDEX idx_task_events_task_time (task_id, event_time)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 3.4 binlog_files 表

```sql
CREATE TABLE binlog_files (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    task_id VARCHAR(64) NOT NULL,
    file_name VARCHAR(255) NOT NULL,
    file_path TEXT NOT NULL,
    size_bytes BIGINT NOT NULL,
    start_pos BIGINT UNSIGNED NOT NULL,
    end_pos BIGINT UNSIGNED NOT NULL,
    created_at DATETIME(6) NOT NULL,
    sealed_at DATETIME(6) NOT NULL,
    object_key TEXT NULL,
    upload_state VARCHAR(32) NULL,
    upload_error TEXT NULL,
    uploaded_at DATETIME(6) NULL,
    UNIQUE KEY uk_task_file (task_id, file_name),
    INDEX idx_task_sealed (task_id, sealed_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 3.5 task_leases 表

```sql
CREATE TABLE task_leases (
    task_id VARCHAR(64) PRIMARY KEY,
    owner_worker_id VARCHAR(128) NOT NULL,
    epoch BIGINT NOT NULL,
    lease_expire_at DATETIME(6) NOT NULL,
    renewed_at DATETIME(6) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 3.6 task_runs 表（任务运行历史）

```sql
CREATE TABLE task_runs (
    run_id VARCHAR(64) PRIMARY KEY,
    task_id VARCHAR(64) NOT NULL,
    worker_id VARCHAR(128) NOT NULL,
    epoch BIGINT NOT NULL,
    started_at DATETIME(6) NOT NULL,
    ended_at DATETIME(6) NULL,
    end_reason VARCHAR(64) NULL,
    INDEX idx_task_runs_task_started (task_id, started_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 3.7 worker_heartbeats 表

```sql
CREATE TABLE worker_heartbeats (
    worker_id VARCHAR(128) PRIMARY KEY,
    host VARCHAR(255) NOT NULL,
    version VARCHAR(64) NOT NULL,
    last_seen_at DATETIME(6) NOT NULL,
    status VARCHAR(32) NOT NULL,
    INDEX idx_worker_heartbeats_seen (last_seen_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 3.8 worker_registrations 表

```sql
CREATE TABLE worker_registrations (
    worker_id VARCHAR(128) PRIMARY KEY,
    session_id VARCHAR(128) NOT NULL,
    lease_expire_at DATETIME(6) NOT NULL,
    renewed_at DATETIME(6) NOT NULL,
    INDEX idx_worker_registrations_expire (lease_expire_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

## 4. MySQLTaskStore 实现

### 4.1 结构

```go
type MySQLTaskStore struct {
    db      *sql.DB
    queries *sqlcgen.Queries  // sqlc 生成的类型安全查询
}

func NewMySQLTaskStore(dsn string, opts ...Option) (*MySQLTaskStore, error) {
    db, err := sql.Open("mysql", dsn)
    if err != nil {
        return nil, err
    }

    // 连接池配置
    db.SetMaxOpenConns(25)
    db.SetMaxIdleConns(10)
    db.SetConnMaxLifetime(5 * time.Minute)

    // 验证 schema 版本
    if err := validateSchemaVersion(ctx, db); err != nil {
        return nil, err
    }

    return &MySQLTaskStore{
        db:      db,
        queries: sqlcgen.New(db),
    }, nil
}
```

### 4.2 UpsertTask 实现

```go
func (s *MySQLTaskStore) UpsertTask(ctx context.Context, task tasks.Task) error {
    sourceJSON, _ := json.Marshal(task.Source)
    startJSON, _ := json.Marshal(task.Start)
    storageJSON, _ := json.Marshal(task.Storage)

    query := `
        INSERT INTO backup_tasks (
            id, name, cluster_key, state, last_error, owner_worker_id,
            epoch, run_id, source_json, start_json, storage_json, updated_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON DUPLICATE KEY UPDATE
            name = VALUES(name),
            state = VALUES(state),
            last_error = VALUES(last_error),
            owner_worker_id = VALUES(owner_worker_id),
            epoch = VALUES(epoch),
            run_id = VALUES(run_id),
            updated_at = VALUES(updated_at)
    `

    _, err := s.db.ExecContext(ctx, query,
        task.ID, task.Name, task.ClusterKey, task.State, task.LastError,
        task.OwnerWorkerID, task.Epoch, task.RunID,
        sourceJSON, startJSON, storageJSON, task.UpdatedAt,
    )

    return err
}
```

### 4.3 Acquire Lease 实现

```go
func (s *MySQLTaskStore) Acquire(ctx context.Context, taskID, workerID string, ttl time.Duration) (int64, bool, error) {
    now := time.Now()
    expiresAt := now.Add(ttl)

    // 原子操作：检查并更新
    query := `
        INSERT INTO task_leases (task_id, owner_worker_id, epoch, lease_expire_at, renewed_at)
        VALUES (?, ?, 1, ?, ?)
        ON DUPLICATE KEY UPDATE
            owner_worker_id = IF(lease_expire_at < ?, VALUES(owner_worker_id), owner_worker_id),
            epoch = IF(lease_expire_at < ?, epoch + 1, epoch),
            lease_expire_at = IF(lease_expire_at < ?, VALUES(lease_expire_at), lease_expire_at),
            renewed_at = VALUES(renewed_at)
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
        OwnerWorkerID  string
        Epoch          int64
        LeaseExpireAt  time.Time
    }

    err = s.db.QueryRowContext(ctx,
        "SELECT owner_worker_id, epoch, lease_expire_at FROM task_leases WHERE task_id = ?",
        taskID,
    ).Scan(&lease.OwnerWorkerID, &lease.Epoch, &lease.LeaseExpireAt)

    if err != nil {
        return 0, false, err
    }

    // 判断是否获取成功
    acquired := lease.OwnerWorkerID == workerID && lease.LeaseExpireAt.After(now)
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
func withRetry(ctx context.Context, fn func() error) error {
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
    // MySQL 连接错误
    var mysqlErr *mysqlDriver.MySQLError
    if errors.As(err, &mysqlErr) {
        switch mysqlErr.Number {
        case 1040, 1154, 1155, 1156, 1157, 1158, 1159, 1184, 1185, 1199, 1203, 1205, 1213, 1220:
            return true
        }
    }

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

## 6. 代码位置

| 组件 | 文件 |
|------|------|
| TaskStore 接口 | `internal/tasks/scheduler.go` |
| MySQLTaskStore | `internal/meta/mysql_store.go` |
| LeaseManager 接口 | `internal/tasks/scheduler.go` |
| LeaseStore 实现 | `internal/meta/lease_store.go` |
| 重试逻辑 | `internal/meta/retry.go` |
| Tracing 包装 | `internal/meta/tracing.go` |
| SQL 迁移 | `migrations/000001_init_schema.up.sql` |

## 7. 本章小结

1. **接口设计**：按职责拆分为多个小接口（TaskStore, CheckpointStore, EventStore, FileMetaStore, LeaseManager, WorkerStore）
2. **JSON 字段**：复杂配置使用 JSON 字段存储（source_json, start_json, storage_json）
3. **原子操作**：使用 INSERT ... ON DUPLICATE KEY UPDATE 实现原子租约获取
4. **重试机制**：指数退避，区分可重试错误
5. **sqlc 生成**：使用 sqlc 生成类型安全的 SQL 查询
