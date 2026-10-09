// Package tasks provides module-level functionality for tasks.
// input: task JSON payloads, runner callbacks, file lifecycle state, store/lease/uploader dependencies
// output: task/start/source/file models including gtid alias decoding, optional local and bucket retention days, OPEN/SEALED observability, checksum match on UPLOADED or mismatch and an unfinished check on UPLOAD_FAILED, files-list location and source_identity, at-tip replication progress, the process-local KeepLocalSegments flag for adopted leftover directories, the persisted desired-run and retry-budget fields omitted from the API JSON, pending_dump_cleanup when Stop could not KILL a Binlog Dump, with process_local when that column is absent, FilePos as JSON null and SQL NULL when a binlog position is unknown, SourceSwitchNotice for a VIP source switch event, and storage_alert when a checkpoint or segment disagrees with the stored transactions; storage_alert also carries segment, detail, missing_gtids, valid_segments, and restart_gtid_set
// pos: core domain orchestration layer governing backup task lifecycle and policies
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

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

const (
	// TaskDesiredRun is operator intent to keep the task running.
	TaskDesiredRun = "RUN"
	// TaskDesiredStop is operator intent to leave the task stopped.
	TaskDesiredStop = "STOP"
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
	// KeepLocalSegments is set when standalone adopt attaches a leftover directory.
	// Start then opens the next .open.e<epoch> and does not delete segments already there.
	// It is process-local and omitted from API and metadata JSON.
	KeepLocalSegments bool `json:"-"`
	// DesiredRun is RUN or STOP. Metadata mode persists it on backup_tasks.
	// Standalone keeps it on this struct until the process exits.
	DesiredRun string `json:"-"`
	// SpecRevision increases when the operator Starts, Stops, or edits the dump spec.
	// The control loop restarts one dump when this is ahead of the open dump.
	SpecRevision int64 `json:"-"`
	// AppliedSpecRevision is the revision whose dump this worker opened.
	AppliedSpecRevision int64 `json:"-"`
	// FailedSpecRevision is the revision that reached FAILED. 0 means the
	// current ask has not failed.
	FailedSpecRevision int64 `json:"-"`
	// RetryAttempt is the exponential backoff step. The next delay uses this
	// value plus one, so a restart does not start again at the base delay.
	RetryAttempt int64 `json:"-"`
	// ConsecutiveSourceFailures is the SOURCE_UNREACHABLE streak. The cap is 10.
	ConsecutiveSourceFailures int64 `json:"-"`
	// PendingDumpCleanup is set when Stop could not KILL the old Binlog Dump.
	// Metadata mode stores it on backup_tasks when migration 000004 has been applied.
	// Without that column it stays in the process that held the dump, including after STOPPED.
	// Standalone keeps it on this struct. Empty means the source thread is gone.
	PendingDumpCleanup *DumpCleanup `json:"pending_dump_cleanup,omitempty"`
	// StorageAlert is computed when a task is listed or fetched.
	// It is not stored. A nil alert means this response did not find a
	// checkpoint or segment that disagrees with the files on disk.
	StorageAlert *StorageAlert `json:"storage_alert,omitempty"`
}

// StorageAlert is a checkpoint or segment that cannot be continued safely.
// Code is STORAGE_INCONSISTENT. Message tells the DBA what was found and
// how to recover without restoring from the damaged files.
type StorageAlert struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Segment is the first damaged segment on disk.
	Segment string `json:"segment,omitempty"`
	// Detail is what was found in Segment, in English.
	Detail string `json:"detail,omitempty"`
	// MissingGTIDs is what the checkpoint lists that no segment holds (MySQL).
	MissingGTIDs string `json:"missing_gtids,omitempty"`
	// ValidSegments were written before the damage and still restore.
	ValidSegments []string `json:"valid_segments,omitempty"`
	// RestartGTIDSet is the gtid_set a new task starts from to continue
	// right after ValidSegments. Empty when it cannot be computed.
	RestartGTIDSet string `json:"restart_gtid_set,omitempty"`
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
	Password string `json:"password,omitempty"` // 仅用于输入，API 响应前会清空
	Flavor   string `json:"flavor"`
	ServerID uint32 `json:"server_id"`
	// SemiSync=true 时尝试以 semi-sync 协议拉流；主库不支持时会自动降级为异步。
	SemiSync bool `json:"semi_sync,omitempty"`
}

// StartConfig 描述复制起点策略。
// Canonical GTID field is gtid_set; gtid is accepted as an input alias.
type StartConfig struct {
	Mode    StartMode `json:"mode"`
	File    string    `json:"file,omitempty"`
	Pos     uint32    `json:"pos,omitempty"`
	GTIDSet string    `json:"gtid_set,omitempty"`
}

type startConfigJSON struct {
	Mode    StartMode `json:"mode"`
	File    string    `json:"file,omitempty"`
	Pos     uint32    `json:"pos,omitempty"`
	GTIDSet string    `json:"gtid_set,omitempty"`
	GTID    string    `json:"gtid,omitempty"`
}

// UnmarshalJSON accepts both gtid_set and the api.md alias gtid.
func (s *StartConfig) UnmarshalJSON(data []byte) error {
	var raw startConfigJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.Mode = raw.Mode
	s.File = raw.File
	s.Pos = raw.Pos
	s.GTIDSet = raw.GTIDSet
	if s.GTIDSet == "" {
		s.GTIDSet = raw.GTID
	}
	return nil
}

// Storage 描述本地存储策略。
// retention_days 仍是必填边界。local_retention_days 与 bucket_retention_days
// 省略或为 0 时都等于 retention_days，清理行为和只配 retention_days 时相同。
// 桶保留短于本地保留时，创建和更新拒绝。
type Storage struct {
	Dir                 string `json:"dir,omitempty"`
	RetentionDays       int    `json:"retention_days,omitempty"`
	LocalRetentionDays  int    `json:"local_retention_days,omitempty"`
	BucketRetentionDays int    `json:"bucket_retention_days,omitempty"`
}

// SourceSwitchNotice is the DBA-facing record of a VIP reaching a different server.
// Message names the old and new identity. Continued is false when the task stops.
type SourceSwitchNotice struct {
	Message   string
	Detail    string
	Continued bool
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

// FilePos is a binlog file position. Zero means the position is not known.
// JSON null and SQL NULL are that unknown value. A writer does not store 0.
type FilePos uint32

// MarshalJSON encodes an unknown position as null.
func (p FilePos) MarshalJSON() ([]byte, error) {
	if p == 0 {
		return []byte("null"), nil
	}
	return json.Marshal(uint32(p))
}

// UnmarshalJSON accepts null and a JSON number. Null is unknown.
func (p *FilePos) UnmarshalJSON(data []byte) error {
	if p == nil {
		return fmt.Errorf("nil file position")
	}
	if string(data) == "null" {
		*p = 0
		return nil
	}
	var n uint32
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*p = FilePos(n)
	return nil
}

// Value stores unknown as NULL. A known position is a positive integer.
func (p FilePos) Value() (driver.Value, error) {
	if p == 0 {
		return nil, nil
	}
	return int64(p), nil
}

// Scan reads a nullable catalog position. NULL and 0 are unknown.
func (p *FilePos) Scan(src any) error {
	if p == nil {
		return fmt.Errorf("nil file position")
	}
	if src == nil {
		*p = 0
		return nil
	}
	switch v := src.(type) {
	case int64:
		*p = filePosFromInt(v)
	case int32:
		*p = filePosFromInt(int64(v))
	case int:
		*p = filePosFromInt(int64(v))
	case uint32:
		*p = FilePos(v)
	case uint64:
		if v == 0 || v > uint64(^uint32(0)) {
			*p = 0
			return nil
		}
		*p = FilePos(v)
	case []byte:
		n, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			return err
		}
		*p = filePosFromInt(n)
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return err
		}
		*p = filePosFromInt(n)
	default:
		return fmt.Errorf("unsupported file position type %T", src)
	}
	return nil
}

func filePosFromInt(n int64) FilePos {
	if n <= 0 || n > int64(^uint32(0)) {
		return 0
	}
	return FilePos(n)
}

// BinlogFile 描述单个 binlog 文件在本地和上传阶段的元数据。
type BinlogFile struct {
	TaskID   string `json:"task_id"`
	FileName string `json:"file_name"`
	FilePath string `json:"file_path"`
	// Epoch is the durable segment generation. 0 is a plain sealed name from
	// this process or from an older release. A later .open.eN or .sealed.eN
	// uses N. file_name stays the source binlog name.
	Epoch     int64  `json:"epoch,omitempty"`
	State     string `json:"state"`
	SizeBytes int64  `json:"size_bytes"`
	// StartPos is the first event position in the segment. Zero is unknown:
	// JSON null, and the catalog writer stores NULL rather than 0.
	StartPos FilePos `json:"start_pos"`
	// EndPos is the last complete event's end log position. Zero is unknown:
	// JSON null. An upsert that does not know it does not assign end_pos.
	// The value 0 is never stored.
	EndPos      FilePos   `json:"end_pos"`
	CreatedAt   time.Time `json:"created_at"`
	SealedAt    time.Time `json:"sealed_at"`
	ObjectKey   string    `json:"object_key,omitempty"`
	UploadState string    `json:"upload_state,omitempty"`
	UploadError string    `json:"upload_error,omitempty"`
	// Checksum is match when the stored object is the same bytes as the sealed file.
	// mismatch means that comparison finished and the bytes differ.
	// Empty means the object HEAD did not finish. Empty is not verified.
	// Only match is stored on an UPLOADED row. A mismatch or an unfinished check is UPLOAD_FAILED.
	Checksum   string    `json:"checksum,omitempty"`
	UploadedAt time.Time `json:"uploaded_at"`
	// Location is local, bucket, or both. The files list fills it in.
	// It is not stored. bucket means this sealed UPLOADED object is the only
	// copy: file_path is the catalog path and is not on this process.
	Location string `json:"location,omitempty"`
	// SourceIdentity is the server that wrote this file. The files list fills
	// it in from the source chain. It is not stored. Empty when the chain is unknown.
	SourceIdentity string `json:"source_identity,omitempty"`
	// SourceServer is the 1-based stint in source_chain.servers that wrote
	// this file. A failback A, B, A names stint 3 for A's new files.
	// It is not stored. 0 (omitted) when the chain is unknown.
	SourceServer int `json:"source_server,omitempty"`
}

const (
	// ChecksumMatch means the object HEAD ETag equals the sealed local file.
	ChecksumMatch = "match"
	// ChecksumMismatch means the object was compared and is not the sealed bytes.
	ChecksumMismatch = "mismatch"
	// ChecksumMismatchError is the upload_error for a finished comparison that differed.
	// The row is UPLOAD_FAILED so the existing upload retry uploads it again.
	ChecksumMismatchError = "checksum mismatch"
	// ChecksumVerifyPrefix prefixes upload_error when the object was stored but the
	// comparison did not finish. The existing upload retry checks that object again.
	ChecksumVerifyPrefix = "checksum verify failed: "
)

// ReplicationProgress 描述任务最近一次复制进度观测值。
type ReplicationProgress struct {
	TaskID        string    `json:"task_id"`
	LastEventAt   time.Time `json:"last_event_at"`
	LastEventFile string    `json:"last_event_file"`
	LastEventPos  uint32    `json:"last_event_pos"`
	UpdatedAt     time.Time `json:"updated_at"`
	// AtTip is true when the dump is at the source's current file/pos.
	// Delay/status must not treat last-event header age as lag in that case.
	AtTip bool `json:"-"`
}
