package tasks

import "time"

type State string

const (
	StateCreated        State = "CREATED"
	StateStarting       State = "STARTING"
	StateRunning        State = "RUNNING"
	StateLeaseDegraded  State = "LEASE_DEGRADED"
	StateRebuildingFile State = "REBUILDING_FILE"
	StateRetryBackoff   State = "RETRY_BACKOFF"
	StateFailed         State = "FAILED"
	StateStopping       State = "STOPPING"
	StateStopped        State = "STOPPED"
)

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

type TaskPatch struct {
	Name       *string       `json:"name,omitempty"`
	ClusterKey string        `json:"cluster_key"`
	Source     *SourceConfig `json:"source,omitempty"`
	Start      *StartConfig  `json:"start,omitempty"`
	Storage    *Storage      `json:"storage,omitempty"`
}

type UploadRetryStats struct {
	Scanned   int `json:"scanned"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

type StartMode string

const (
	StartModeLatest  StartMode = "LATEST"
	StartModeFilePos StartMode = "FILE_POS"
	StartModeGTID    StartMode = "GTID"
)

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

type StartConfig struct {
	Mode    StartMode `json:"mode"`
	File    string    `json:"file,omitempty"`
	Pos     uint32    `json:"pos,omitempty"`
	GTIDSet string    `json:"gtid_set,omitempty"`
}

type Storage struct {
	Dir           string `json:"dir,omitempty"`
	RetentionDays int    `json:"retention_days,omitempty"`
}

type TaskEvent struct {
	TaskID   string    `json:"task_id"`
	Type     string    `json:"type"`
	Message  string    `json:"message,omitempty"`
	Detail   string    `json:"detail,omitempty"`
	Time     time.Time `json:"time"`
	Sequence int64     `json:"sequence"`
}

type TaskRun struct {
	RunID     string    `json:"run_id"`
	TaskID    string    `json:"task_id"`
	WorkerID  string    `json:"worker_id,omitempty"`
	Epoch     int64     `json:"epoch"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	EndReason string    `json:"end_reason,omitempty"`
}

type WorkerHeartbeat struct {
	WorkerID   string    `json:"worker_id"`
	Host       string    `json:"host"`
	Version    string    `json:"version"`
	LastSeenAt time.Time `json:"last_seen_at"`
	Status     string    `json:"status"`
}

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

type ReplicationProgress struct {
	TaskID        string    `json:"task_id"`
	LastEventAt   time.Time `json:"last_event_at"`
	LastEventFile string    `json:"last_event_file"`
	LastEventPos  uint32    `json:"last_event_pos"`
	UpdatedAt     time.Time `json:"updated_at"`
}
