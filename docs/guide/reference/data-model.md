# 数据模型

本文档描述核心数据结构和数据库表。

## 1. 核心数据结构

### 1.1 Task

```go
type Task struct {
    // 基本信息
    ID            string `json:"id"`
    Name          string `json:"name"`
    ClusterKey    string `json:"cluster_key"`

    // 源 MySQL 配置
    Source SourceConfig `json:"source"`

    // 启动配置
    Start StartConfig `json:"start"`

    // 存储配置
    Storage Storage `json:"storage"`

    // 状态
    State         State  `json:"state"`
    LastError     string `json:"last_error,omitempty"`
    OwnerWorkerID string `json:"owner_worker_id,omitempty"`
    Epoch         int64  `json:"epoch,omitempty"`
    RunID         string `json:"run_id,omitempty"`

    // 时间戳
    UpdatedAt time.Time `json:"updated_at"`
}
```

### 1.2 State（任务状态）

```go
type State string

const (
    StateCreated        State = "CREATED"         // 任务已创建但尚未启动
    StateStarting       State = "STARTING"        // 任务已进入启动流程
    StateRunning        State = "RUNNING"         // 正在拉取并持久化 binlog
    StateLeaseDegraded  State = "LEASE_DEGRADED"  // lease 续约异常但在 grace 窗口内
    StateRebuildingFile State = "REBUILDING_FILE" // failover 后重建当前 binlog 文件
    StateRetryBackoff   State = "RETRY_BACKOFF"   // 因可重试错误进入退避等待
    StateFailed         State = "FAILED"          // 因不可恢复错误失败停止
    StateStopping       State = "STOPPING"        // 已收到停止请求，等待收敛
    StateStopped        State = "STOPPED"         // 执行路径已完全退出
)
```

### 1.3 SourceConfig

```go
type SourceConfig struct {
    Host     string `json:"host"`
    Port     uint16 `json:"port"`
    User     string `json:"user"`
    Password string `json:"password,omitempty"`  // 注意：API 响应中不返回
    Flavor   string `json:"flavor"`              // mysql / mariadb
    ServerID uint32 `json:"server_id"`           // 复制 server_id
    SemiSync bool   `json:"semi_sync,omitempty"` // 是否启用半同步
}
```

### 1.4 StartConfig

```go
type StartMode string

const (
    StartModeLatest  StartMode = "LATEST"    // 从主库当前最新位点开始
    StartModeFilePos StartMode = "FILE_POS"  // 从指定 file/pos 开始
    StartModeGTID    StartMode = "GTID"      // 从指定 GTID 集开始
)

type StartConfig struct {
    Mode    StartMode `json:"mode"`
    File    string    `json:"file,omitempty"`     // FILE_POS 模式
    Pos     uint32    `json:"pos,omitempty"`      // FILE_POS 模式
    GTIDSet string    `json:"gtid_set,omitempty"` // GTID 模式
}
```

### 1.5 Storage

```go
type Storage struct {
    Dir           string `json:"dir,omitempty"`            // 本地存储目录
    RetentionDays int    `json:"retention_days,omitempty"` // 保留天数
}
```

### 1.6 Checkpoint

```go
type Checkpoint struct {
    File      string    `json:"file"`
    Pos       uint32    `json:"pos"`
    GTIDSet   string    `json:"gtid_set,omitempty"`
    UpdatedAt time.Time `json:"updated_at,omitempty"`
}
```

### 1.7 TaskEvent

```go
type TaskEvent struct {
    TaskID   string    `json:"task_id"`
    Type     string    `json:"type"`
    Message  string    `json:"message,omitempty"`
    Detail   string    `json:"detail,omitempty"`
    Time     time.Time `json:"time"`
    Sequence int64     `json:"sequence"`
}
```

### 1.8 BinlogFile

```go
type BinlogFile struct {
    TaskID      string    `json:"task_id"`
    FileName    string    `json:"file_name"`
    FilePath    string    `json:"file_path"`
    SizeBytes   int64     `json:"size_bytes"`
    StartPos    uint32    `json:"start_pos"`
    EndPos      uint32    `json:"end_pos"`
    CreatedAt   time.Time `json:"created_at"`
    SealedAt    time.Time `json:"sealed_at"`
    ObjectKey   string    `json:"object_key,omitempty"`
    UploadState string    `json:"upload_state,omitempty"`
    UploadError string    `json:"upload_error,omitempty"`
    UploadedAt  time.Time `json:"uploaded_at"`
}
```

### 1.9 TaskRun

```go
type TaskRun struct {
    RunID     string    `json:"run_id"`
    TaskID    string    `json:"task_id"`
    WorkerID  string    `json:"worker_id,omitempty"`
    Epoch     int64     `json:"epoch"`
    StartedAt time.Time `json:"started_at"`
    EndedAt   time.Time `json:"ended_at,omitempty"`
    EndReason string    `json:"end_reason,omitempty"`
}
```

### 1.10 WorkerHeartbeat

```go
type WorkerHeartbeat struct {
    WorkerID   string    `json:"worker_id"`
    Host       string    `json:"host"`
    Version    string    `json:"version"`
    LastSeenAt time.Time `json:"last_seen_at"`
    Status     string    `json:"status"`
}
```

### 1.11 ReplicationProgress

```go
type ReplicationProgress struct {
    TaskID        string    `json:"task_id"`
    LastEventAt   time.Time `json:"last_event_at"`
    LastEventFile string    `json:"last_event_file"`
    LastEventPos  uint32    `json:"last_event_pos"`
    UpdatedAt     time.Time `json:"updated_at"`
}
```

### 1.12 Lease（内部结构）

```go
type Lease struct {
    TaskID        string
    OwnerWorkerID string
    Epoch         int64
    LeaseExpireAt time.Time
    RenewedAt     time.Time
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
    source_flavor           VARCHAR(32) NOT NULL DEFAULT 'mysql',
    source_server_id        INT UNSIGNED NOT NULL,
    source_semi_sync        TINYINT(1) NOT NULL DEFAULT 0,
    start_mode              VARCHAR(32) NOT NULL DEFAULT 'LATEST',
    start_file              VARCHAR(255),
    start_pos               INT,
    start_gtid_set          TEXT,
    storage_dir             VARCHAR(512),
    storage_retention_days  INT NOT NULL DEFAULT 7,
    owner_worker_id         VARCHAR(64),
    epoch                   BIGINT NOT NULL DEFAULT 0,
    run_id                  VARCHAR(64),
    last_error              TEXT,
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
    gtid_set    TEXT,
    updated_at  DATETIME(3) NOT NULL,

    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 2.3 task_events

```sql
CREATE TABLE task_events (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    task_id     VARCHAR(64) NOT NULL,
    type        VARCHAR(64) NOT NULL,
    message     VARCHAR(255) NOT NULL,
    detail      TEXT,
    time        DATETIME(3) NOT NULL,
    sequence    BIGINT NOT NULL,

    INDEX idx_task_id (task_id),
    INDEX idx_time (time),
    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 2.4 binlog_files

```sql
CREATE TABLE binlog_files (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    task_id         VARCHAR(64) NOT NULL,
    file_name       VARCHAR(255) NOT NULL,
    file_path       VARCHAR(512) NOT NULL,
    size_bytes      BIGINT NOT NULL,
    start_pos       INT UNSIGNED NOT NULL,
    end_pos         INT UNSIGNED NOT NULL,
    created_at      DATETIME(3) NOT NULL,
    sealed_at       DATETIME(3),
    object_key      VARCHAR(512),
    upload_state    VARCHAR(32),
    upload_error    TEXT,
    uploaded_at     DATETIME(3),

    UNIQUE KEY uk_task_file (task_id, file_name),
    INDEX idx_upload_state (upload_state),
    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 2.5 task_runs

```sql
CREATE TABLE task_runs (
    run_id      VARCHAR(64) PRIMARY KEY,
    task_id     VARCHAR(64) NOT NULL,
    worker_id   VARCHAR(64),
    epoch       BIGINT NOT NULL,
    started_at  DATETIME(3) NOT NULL,
    ended_at    DATETIME(3),
    end_reason  VARCHAR(64),

    INDEX idx_task_id (task_id),
    INDEX idx_worker_id (worker_id),
    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 2.6 leases

```sql
CREATE TABLE leases (
    task_id         VARCHAR(64) PRIMARY KEY,
    owner_worker_id VARCHAR(64) NOT NULL,
    epoch           BIGINT NOT NULL,
    lease_expire_at DATETIME(3) NOT NULL,
    renewed_at      DATETIME(3) NOT NULL,

    INDEX idx_lease_expire_at (lease_expire_at),
    INDEX idx_owner_worker_id (owner_worker_id),
    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 2.7 worker_heartbeats

```sql
CREATE TABLE worker_heartbeats (
    worker_id   VARCHAR(64) PRIMARY KEY,
    host        VARCHAR(255) NOT NULL,
    version     VARCHAR(64) NOT NULL,
    last_seen_at DATETIME(3) NOT NULL,
    status      VARCHAR(32) NOT NULL,

    INDEX idx_last_seen_at (last_seen_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 2.8 worker_registrations

```sql
CREATE TABLE worker_registrations (
    worker_id   VARCHAR(64) PRIMARY KEY,
    session_id  VARCHAR(64) NOT NULL,
    expires_at  DATETIME(3) NOT NULL,
    updated_at  DATETIME(3) NOT NULL,

    INDEX idx_expires_at (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

## 3. ER 图

```
┌─────────────┐       ┌─────────────┐
│    tasks    │       │ checkpoints │
│─────────────│       │─────────────│
│ id (PK)     │◄──────│ task_id (PK)│
│ name        │       │ file        │
│ cluster_key │       │ pos         │
│ state       │       │ gtid_set    │
│ ...         │       └─────────────┘
└──────┬──────┘
       │
       │       ┌─────────────┐
       │       │ task_events │
       │       │─────────────│
       └──────►│ task_id (FK)│
               │ type        │
               │ message     │
               └─────────────┘

       │       ┌─────────────┐
       │       │binlog_files │
       │       │─────────────│
       └──────►│ task_id (FK)│
               │ file_name   │
               │ size_bytes  │
               └─────────────┘

       │       ┌─────────────┐
       │       │  task_runs  │
       │       │─────────────│
       └──────►│ task_id (FK)│
               │ run_id (PK) │
               │ worker_id   │
               └─────────────┘

       │       ┌─────────────┐
       │       │   leases    │
       │       │─────────────│
       └──────►│ task_id (PK)│
               │owner_worker │
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
| task_events | idx_time | 按时间排序 |
| binlog_files | uk_task_file | 唯一约束 |
| binlog_files | idx_upload_state | 按上传状态查询 |
| task_runs | idx_task_id | 查询任务运行记录 |
| leases | idx_lease_expire_at | 查询过期租约 |
| worker_heartbeats | idx_last_seen_at | 查询活跃 workers |
| worker_registrations | idx_expires_at | 查询活跃注册 |

## 5. 数据保留

| 数据类型 | 保留策略 |
|----------|----------|
| tasks | 任务删除时级联删除 |
| checkpoints | 任务删除时级联删除 |
| task_events | 任务删除时级联删除 |
| binlog_files | 任务删除时级联删除 |
| task_runs | 任务删除时级联删除 |
| leases | 任务删除时级联删除 |
| worker_heartbeats | 过期后可清理 |
| worker_registrations | 过期后可清理 |

## 6. 代码位置

| 模型 | 文件 |
|------|------|
| Task, State, TaskEvent, BinlogFile, TaskRun | `internal/tasks/model.go` |
| Checkpoint | `internal/binlog/checkpoint.go` |
| WorkerHeartbeat | `internal/tasks/model.go` |
| ReplicationProgress | `internal/tasks/model.go` |
| Lease（数据库模型） | `internal/meta/` |
| 表结构 SQL | `migrations/` |
