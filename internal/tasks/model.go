package tasks

import "time"

type State string

const (
	StateCreated      State = "CREATED"
	StateStarting     State = "STARTING"
	StateRunning      State = "RUNNING"
	StateRetryBackoff State = "RETRY_BACKOFF"
	StateFailed       State = "FAILED"
	StateStopping     State = "STOPPING"
	StateStopped      State = "STOPPED"
)

type Task struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	State     State        `json:"state"`
	LastError string       `json:"last_error,omitempty"`
	Source    SourceConfig `json:"source"`
	Start     StartConfig  `json:"start"`
	Storage   Storage      `json:"storage"`
	UpdatedAt time.Time    `json:"updated_at"`
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

type BinlogFile struct {
	TaskID    string    `json:"task_id"`
	FileName  string    `json:"file_name"`
	FilePath  string    `json:"file_path"`
	SizeBytes int64     `json:"size_bytes"`
	StartPos  uint32    `json:"start_pos"`
	EndPos    uint32    `json:"end_pos"`
	CreatedAt time.Time `json:"created_at"`
	SealedAt  time.Time `json:"sealed_at"`
}
