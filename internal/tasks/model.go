// input: task commands/events, runner callbacks, store/lease/uploader dependencies
// output: task state transitions, scheduling decisions, and execution coordination
// pos: core domain orchestration layer governing backup task lifecycle and policies
// note: if this file changes, update this header and module AGENTS.md.
package tasks

import "time"

// State 表示任务生命周期状态。
type State string

const (
	// StateCreated 表示任务已创建但尚未启动。
	StateCreated State = "CREATED"
	// StateStarting 表示任务已进入启动流程，等待 runner ready 或 worker claim。
	StateStarting State = "STARTING"
	// StateRunning 表示任务正在拉取并持久化 binlog。
	StateRunning State = "RUNNING"
	// StateLeaseDegraded 表示 lease 续约出现异常但仍在 grace 窗口内。
	StateLeaseDegraded State = "LEASE_DEGRADED"
	// StateRebuildingFile 表示 failover 后正在重建当前 binlog 文件。
	StateRebuildingFile State = "REBUILDING_FILE"
	// StateRetryBackoff 表示任务因可重试错误进入退避等待。
	StateRetryBackoff State = "RETRY_BACKOFF"
	// StateFailed 表示任务因不可恢复错误失败停止。
	StateFailed State = "FAILED"
	// StateStopping 表示已收到停止请求，等待执行 goroutine 收敛。
	StateStopping State = "STOPPING"
	// StateStopped 表示执行路径已经完全退出。
	StateStopped State = "STOPPED"
)

// Task 是任务的核心元数据模型（配置 + 运行时状态）。
type Task struct {
	ID            string       `json:"id"`
	Name          string       `json:"name"`
	ClusterKey    string       `json:"cluster_key"`
	State         State        `json:"state"`
	LastError     string       `json:"last_error,omitempty"`
	OwnerWorkerID string       `json:"owner_worker_id,omitempty"`
	Epoch         int64        `json:"epoch,omitempty"`
	RunID         string       `json:"run_id,omitempty"`
	Source        SourceConfig `json:"source"`
	Start         StartConfig  `json:"start"`
	Storage       Storage      `json:"storage"`
	UpdatedAt     time.Time    `json:"updated_at"`
}

// TaskPatch 是任务更新接口使用的部分字段 patch。
type TaskPatch struct {
	Name       *string       `json:"name,omitempty"`
	ClusterKey string        `json:"cluster_key"`
	Source     *SourceConfig `json:"source,omitempty"`
	Start      *StartConfig  `json:"start,omitempty"`
	Storage    *Storage      `json:"storage,omitempty"`
}

// UploadRetryStats 是一次 retry-upload 调用的执行统计。
type UploadRetryStats struct {
	Scanned   int `json:"scanned"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

// UploadRetryMetrics 是 retry-upload 的累计指标快照（用于 /metrics）。
type UploadRetryMetrics struct {
	Success int64 `json:"success"`
	Failed  int64 `json:"failed"`
	Skipped int64 `json:"skipped"`
	LastTs  int64 `json:"last_ts"`
}

// UploadFailureReason 是上传失败原因聚合项。
type UploadFailureReason struct {
	Reason     string    `json:"reason"`
	Count      int64     `json:"count"`
	LatestTime time.Time `json:"latest_time,omitempty"`
}

// StartMode 表示任务起点策略类型。
type StartMode string

const (
	// StartModeLatest 表示从主库当前最新位点开始。
	StartModeLatest StartMode = "LATEST"
	// StartModeFilePos 表示从指定 file/pos 开始。
	StartModeFilePos StartMode = "FILE_POS"
	// StartModeGTID 表示从指定 GTID 集开始。
	StartModeGTID StartMode = "GTID"
)

// SourceConfig 描述源 MySQL 连接与复制参数。
type SourceConfig struct {
	Host     string `json:"host"`
	Port     uint16 `json:"port"`
	User     string `json:"user"`
	Password string `json:"password,omitempty"`
	Flavor   string `json:"flavor"`
	ServerID uint32 `json:"server_id"`
	// SemiSync=true 时尝试以 semi-sync 协议拉流；主库不支持时会自动降级为异步。
	SemiSync bool `json:"semi_sync,omitempty"`
}

// StartConfig 描述复制起点策略。
type StartConfig struct {
	Mode    StartMode `json:"mode"`
	File    string    `json:"file,omitempty"`
	Pos     uint32    `json:"pos,omitempty"`
	GTIDSet string    `json:"gtid_set,omitempty"`
}

// Storage 描述本地存储策略。
type Storage struct {
	Dir           string `json:"dir,omitempty"`
	RetentionDays int    `json:"retention_days,omitempty"`
}

// TaskEvent 是任务事件流中的一条记录。
type TaskEvent struct {
	TaskID   string    `json:"task_id"`
	Type     string    `json:"type"`
	Message  string    `json:"message,omitempty"`
	Detail   string    `json:"detail,omitempty"`
	Time     time.Time `json:"time"`
	Sequence int64     `json:"sequence"`
}

// TaskRun 表示一次独立运行周期（run）的生命周期记录。
type TaskRun struct {
	RunID     string    `json:"run_id"`
	TaskID    string    `json:"task_id"`
	WorkerID  string    `json:"worker_id,omitempty"`
	Epoch     int64     `json:"epoch"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	EndReason string    `json:"end_reason,omitempty"`
}

// WorkerHeartbeat 描述 worker 的心跳状态快照。
type WorkerHeartbeat struct {
	WorkerID   string    `json:"worker_id"`
	Host       string    `json:"host"`
	Version    string    `json:"version"`
	LastSeenAt time.Time `json:"last_seen_at"`
	Status     string    `json:"status"`
}

// BinlogFile 描述单个 binlog 文件在本地和上传阶段的元数据。
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

// ReplicationProgress 描述任务最近一次复制进度观测值。
type ReplicationProgress struct {
	TaskID        string    `json:"task_id"`
	LastEventAt   time.Time `json:"last_event_at"`
	LastEventFile string    `json:"last_event_file"`
	LastEventPos  uint32    `json:"last_event_pos"`
	UpdatedAt     time.Time `json:"updated_at"`
}
