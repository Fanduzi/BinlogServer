# 数据模型

本文档描述核心数据结构和数据库表。

## 1. 核心数据结构

### 1.1 Task

```go
type Task struct {
    // 基本信息
    ID        string    `json:"id"`
    Name      string    `json:"name"`
    ClusterKey string   `json:"cluster_key"`

    // 源 MySQL 配置
    Source    SourceConfig `json:"source"`

    // 启动配置
    Start     StartConfig  `json:"start"`

    // 存储配置
    Storage   StorageConfig `json:"storage"`

    // 状态
    State       TaskState `json:"state"`
    OwnerWorkerID string  `json:"owner_worker_id"`
    Epoch       int64     `json:"epoch"`
    LastError   string    `json:"last_error"`

    // 时间戳
    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`
}
```

### 1.2 SourceConfig

```go
type SourceConfig struct {
    Host     string `json:"host"`
    Port     int    `json:"port"`
    User     string `json:"user"`
    Password string `json:"password"`  // 注意：API 响应中不返回
}
```

### 1.3 StartConfig

```go
type StartConfig struct {
    Mode string `json:"mode"`  // LATEST / FILE_POS / GTID

    // FILE_POS 模式
    File string `json:"file,omitempty"`
    Pos  uint32 `json:"pos,omitempty"`

    // GTID 模式
    GTID string `json:"gtid,omitempty"`
}
```

### 1.4 StorageConfig

```go
type StorageConfig struct {
    RetentionDays int `json:"retention_days"`
}
```

### 1.5 TaskState

```go
type TaskState string

const (
    StatePending       TaskState = "PENDING"
    StateStarting      TaskState = "STARTING"
    StateRunning       TaskState = "RUNNING"
    StateLeaseDegraded TaskState = "LEASE_DEGRADED"
    StateStopping      TaskState = "STOPPING"
    StateStopped       TaskState = "STOPPED"
    StateError         TaskState = "ERROR"
)
```

### 1.6 Checkpoint

```go
type Checkpoint struct {
    TaskID    string    `json:"task_id"`
    File      string    `json:"file"`
    Pos       uint32    `json:"pos"`
    GTID      string    `json:"gtid,omitempty"`
    UpdatedAt time.Time `json:"updated_at"`
}
```

### 1.7 TaskEvent

```go
type TaskEvent struct {
    ID        int64     `json:"id"`
    TaskID    string    `json:"task_id"`
    EventType string    `json:"event_type"`
    Message   string    `json:"message"`
    Detail    string    `json:"detail,omitempty"`
    CreatedAt time.Time `json:"created_at"`
}
```

### 1.8 FileMeta

```go
type FileMeta struct {
    ID           int64  `json:"id"`
    TaskID       string `json:"task_id"`
    FileName     string `json:"file_name"`
    Size         int64  `json:"size"`
    UploadStatus string `json:"upload_status"`
    UploadReason string `json:"upload_reason,omitempty"`
    CreatedAt    time.Time `json:"created_at"`
    UpdatedAt    time.Time `json:"updated_at"`
}

type UploadStatus string

const (
    UploadStatusLocalOnly  UploadStatus = "LOCAL_ONLY"
    UploadStatusUploaded   UploadStatus = "UPLOADED"
    UploadStatusFailed     UploadStatus = "UPLOAD_FAILED"
)
```

### 1.9 Lease

```go
type Lease struct {
    TaskID    string    `json:"task_id"`
    WorkerID  string    `json:"worker_id"`
    Epoch     int64     `json:"epoch"`
    ExpiresAt time.Time `json:"expires_at"`
    UpdatedAt time.Time `json:"updated_at"`
}
```

### 1.10 WorkerRegistration

```go
type WorkerRegistration struct {
    WorkerID  string    `json:"worker_id"`
    SessionID string    `json:"session_id"`
    Role      string    `json:"role"`
    ExpiresAt time.Time `json:"expires_at"`
    UpdatedAt time.Time `json:"updated_at"`
}
```

## 2. 数据库表结构

### 2.1 tasks

```sql
CREATE TABLE tasks (
    id                      VARCHAR(64) PRIMARY KEY,
    name                    VARCHAR(255) NOT NULL,
    cluster_key             VARCHAR(255) NOT NULL,
    state                   VARCHAR(32) NOT NULL,
    source_host             VARCHAR(255) NOT NULL,
    source_port             INT NOT NULL,
    source_user             VARCHAR(64) NOT NULL,
    source_password         VARCHAR(255) NOT NULL,
    start_mode              VARCHAR(32) NOT NULL DEFAULT 'LATEST',
    start_file              VARCHAR(255),
    start_pos               INT,
    start_gtid              TEXT,
    storage_retention_days  INT NOT NULL DEFAULT 7,
    owner_worker_id         VARCHAR(64),
    epoch                   BIGINT NOT NULL DEFAULT 0,
    last_error              TEXT,
    created_at              DATETIME(3) NOT NULL,
    updated_at              DATETIME(3) NOT NULL,

    INDEX idx_state (state),
    INDEX idx_cluster_key (cluster_key),
    INDEX idx_owner_worker (owner_worker_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 2.2 checkpoints

```sql
CREATE TABLE checkpoints (
    task_id     VARCHAR(64) PRIMARY KEY,
    file        VARCHAR(255) NOT NULL,
    pos         INT NOT NULL,
    gtid        TEXT,
    updated_at  DATETIME(3) NOT NULL,

    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 2.3 task_events

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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 2.4 files

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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 2.5 leases

```sql
CREATE TABLE leases (
    task_id     VARCHAR(64) PRIMARY KEY,
    worker_id   VARCHAR(64) NOT NULL,
    epoch       BIGINT NOT NULL,
    expires_at  DATETIME(3) NOT NULL,
    updated_at  DATETIME(3) NOT NULL,

    INDEX idx_expires_at (expires_at),
    INDEX idx_worker_id (worker_id),
    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 2.6 worker_registrations

```sql
CREATE TABLE worker_registrations (
    worker_id   VARCHAR(64) PRIMARY KEY,
    session_id  VARCHAR(64) NOT NULL,
    role        VARCHAR(32) NOT NULL,
    expires_at  DATETIME(3) NOT NULL,
    updated_at  DATETIME(3) NOT NULL,

    INDEX idx_expires_at (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

## 3. ER 图

```
┌─────────────┐       ┌─────────────┐
│    tasks    │       │  checkpoints│
│─────────────│       │─────────────│
│ id (PK)     │◄──────│ task_id (PK)│
│ name        │       │ file        │
│ cluster_key │       │ pos         │
│ state       │       │ gtid        │
│ ...         │       └─────────────┘
└──────┬──────┘
       │
       │       ┌─────────────┐
       │       │task_events  │
       │       │─────────────│
       └──────►│ task_id (FK)│
               │ event_type  │
               │ message     │
               └─────────────┘

       │       ┌─────────────┐
       │       │   files     │
       │       │─────────────│
       └──────►│ task_id (FK)│
               │ file_name   │
               │ size        │
               └─────────────┘

       │       ┌─────────────┐
       │       │   leases    │
       │       │─────────────│
       └──────►│ task_id (PK)│
               │ worker_id   │
               │ epoch       │
               └─────────────┘
```

## 4. 索引策略

| 表 | 索引 | 用途 |
|------|------|------|
| tasks | idx_state | 按状态查询任务 |
| tasks | idx_cluster_key | 按集群查询 |
| tasks | idx_owner_worker | 按执行者查询 |
| task_events | idx_task_id | 查询任务事件 |
| task_events | idx_created_at | 按时间排序 |
| leases | idx_expires_at | 查询过期租约 |
| worker_registrations | idx_expires_at | 查询活跃 workers |

## 5. 数据保留

| 数据类型 | 保留策略 |
|----------|----------|
| tasks | 任务删除时级联删除 |
| checkpoints | 任务删除时级联删除 |
| task_events | 任务删除时级联删除 |
| files | 任务删除时级联删除 |
| leases | 任务删除时级联删除 |
| worker_registrations | 过期后可清理 |

## 6. 代码位置

| 模型 | 文件 |
|------|------|
| Task | `internal/tasks/model.go` |
| Checkpoint | `internal/meta/model.go` |
| TaskEvent | `internal/tasks/model.go` |
| FileMeta | `internal/tasks/model.go` |
| 表结构 SQL | `migrations/` |
