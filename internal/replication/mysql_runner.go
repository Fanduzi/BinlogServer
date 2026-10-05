// Package replication provides module-level functionality for replication.
// input: source replication config, flavor-aware identity, checkpoint/file metadata store dependencies, and the upload client wired as object deleter and object reader
// output: replication run control, observable OPEN/SEALED artifacts, at-tip as soon as dump file/pos matches master (fresh LATEST or FILE_POS already there), a mid-file format description written once ahead of the first copied event without moving the checkpoint or the delay sample, a later description of a segment that already has events left out of the file and the delay, idle at-tip only when dump matches master file/pos, stop/start and kill-then-adopt resume from the last durable event in the local open segment (not SHOW MASTER STATUS and not position 4) while keeping those bytes, sealed-file handoff for upload, retention purge that deletes the bucket object for a sealed uploaded segment whose checksum is match and returns OBJECT_PURGE_FAILED without removing the local file when that delete fails, a longer bucket retention that deletes only the local file of a checksum-matched uploaded segment still inside that window, retention that keeps an expired sealed UPLOAD_FAILED or LOCAL_ONLY file and an on-disk uploaded file whose checksum is not match, recording that file as UPLOAD_FAILED, and its catalog row when upload and a catalog are configured and records one RETENTION_SKIPPED_NOT_UPLOADED event per file plus the binlog_server_retention_blocked_files count until a later pass purges the uploaded copy, a rotate checkpoint on the next file before that file is opened so a failed purge resumes there instead of resealing the file just sealed, permanent source errors including the MariaDB flavor hint when @@server_uuid is missing, adopted leftover directories that keep unrelated segments while continuing an open segment that already ends at the adopted FILE_POS, and lease takeover that continues in a readable catalog file_path directory from its last complete event, resumes a checkpoint already inside a sealed UPLOADED object from that object, or returns permanent SEGMENT_NOT_ON_WORKER naming the missing segment without creating a new directory when that unuploaded tail is not readable, records a sealed file whose upload did not finish as UPLOAD_FAILED so the existing retry uploads and verifies it, returns permanent SEALED_FILE_EXISTS when that sealed file is already on disk, returns permanent CHECKPOINT_WRITE_FAILED for a checkpoint write that is not a transient metadata error, returns a lease handoff when the seal-time epoch no longer matches, and bounds the post-seal upload with the same upload timeout the retry path uses, and keeps one catalog row per durable epoch so a later open segment does not erase an earlier sealed path, upload state, checksum, or object key, and drops an OPEN catalog row in this segment directory when that file is no longer there, keeps the executed GTID on every flushed checkpoint including the rotate onto the next file by decoding raw-mode GTID and query bodies, and continues a file/pos resume with that GTID when MySQL 1236 is returned by StartSync or by the first stream read, closing that syncer and opening StartSyncGTID, and returns an already-open dump to the scheduler as SOURCE_UNREACHABLE after 5 library reconnects (about 5s) so a longer source outage leaves RUNNING, and saves a resolved LATEST file and position with an empty gtid_set before the dump so a retry continues from that anchor instead of resolving LATEST again
// pos: data-plane runtime that consumes MySQL/MariaDB binlog stream and emits durable outputs
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/meta"
	"binlog_server/internal/tasks"

	sqlclient "github.com/go-mysql-org/go-mysql/client"
	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
)

var binlogMagic = []byte{0xfe, 'b', 'i', 'n'}

// segmentHasEvents reports whether the open segment already holds an event
// after the 4-byte magic header. A missing path is treated as empty so a
// test double that only captures writes still receives the format description.
func segmentHasEvents(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Size() > int64(len(binlogMagic))
}

var ErrLeaseEpochMismatch = errors.New("lease/epoch mismatch")

const (
	defaultServerIDBase uint32 = 200000
	defaultServerIDMod  uint32 = 1000000
)

// MySQLRunner 负责执行复制协议拉流、文件落盘、checkpoint 与上传流程。
type MySQLRunner struct {
	dataDir         string
	fetcher         sourceMetaFetcher
	checkpointStore CheckpointStore
	fileMetaStore   FileMetaStore
	uploadPrefix    string
	objectDeleter   objectDeleter
	objectOpener    objectOpener
	leaseVerifier   LeaseVerifier
	sealedHandler   func(context.Context, tasks.BinlogFile) error
	// uploadTimeout bounds the post-seal put. It is the same meta.timeout.upload_sec
	// value Scheduler.withUploadTimeout uses for background retry. Zero means no extra deadline.
	uploadTimeout    time.Duration
	progressReporter ProgressReporter
	newSyncer        func(replication.BinlogSyncerConfig) binlogSyncer
	writerOpener     func(task tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error)

	retentionMu        sync.Mutex
	retentionSkipNoted map[string]struct{}
	retentionBlocked   map[string]int

	// anchorMu guards anchor, the LATEST file/pos resolved by this process.
	// A retry with no metadata store reads it so it does not resolve LATEST again.
	anchorMu sync.Mutex
	anchor   map[string]resolvedPos
}

// resolvedPos is the file and position a LATEST start already chose.
type resolvedPos struct {
	file string
	pos  uint32
}

type sourceMetaFetcher interface {
	MasterStatusFetcher
	FetchServerUUID(ctx context.Context, source tasks.SourceConfig) (string, error)
}

type eventHandlerFunc func(*replication.BinlogEvent) error

type binlogStreamer interface {
	GetEvent(context.Context) (*replication.BinlogEvent, error)
}

type binlogSyncer interface {
	StartSync(gomysql.Position) (binlogStreamer, error)
	StartSyncGTID(gomysql.GTIDSet) (binlogStreamer, error)
	Close()
}

type liveBinlogSyncer struct {
	inner *replication.BinlogSyncer
}

func (s *liveBinlogSyncer) StartSync(pos gomysql.Position) (binlogStreamer, error) {
	return s.inner.StartSync(pos)
}

func (s *liveBinlogSyncer) StartSyncGTID(set gomysql.GTIDSet) (binlogStreamer, error) {
	return s.inner.StartSyncGTID(set)
}

func (s *liveBinlogSyncer) Close() {
	s.inner.Close()
}

// HandleEvent 让函数类型实现 go-mysql 的事件处理接口。
func (f eventHandlerFunc) HandleEvent(e *replication.BinlogEvent) error {
	return f(e)
}

// CheckpointStore 定义 checkpoint 持久化接口。
type CheckpointStore interface {
	// UpsertCheckpoint 持久化 checkpoint。
	UpsertCheckpoint(ctx context.Context, taskID string, checkpoint binlog.Checkpoint) error
	// LoadCheckpoint 读取最近 checkpoint。
	LoadCheckpoint(ctx context.Context, taskID string) (binlog.Checkpoint, bool, error)
}

// RunnerOption 用于可选注入 runner 的扩展能力。
type RunnerOption func(*MySQLRunner)

// WithCheckpointStore 注入 checkpoint 存储。
func WithCheckpointStore(store CheckpointStore) RunnerOption {
	return func(r *MySQLRunner) {
		r.checkpointStore = store
	}
}

// FileMetaStore 定义文件元数据存储接口。
type FileMetaStore interface {
	// UpsertBinlogFile 持久化文件元数据。
	UpsertBinlogFile(ctx context.Context, meta tasks.BinlogFile) error
}

// WithFileMetaStore 注入文件元数据存储。
func WithFileMetaStore(store FileMetaStore) RunnerOption {
	return func(r *MySQLRunner) {
		r.fileMetaStore = store
	}
}

// ProgressReporter 定义复制进度上报接口。
type ProgressReporter interface {
	// ReportReplicationProgress 上报复制进度。atTip 表示 dump 已在源库当前 file/pos。
	ReportReplicationProgress(taskID string, sourceEventAt time.Time, file string, pos uint32, atTip bool)
}

// LeaseVerifier 封文件前问租约是否仍在。tasks.LeaseManager 直接满足，不必经 App 转一层。
type LeaseVerifier interface {
	Verify(ctx context.Context, taskID, workerID string, epoch int64) (bool, error)
}

type leaseVerifierFunc func(context.Context, string, string, int64) (bool, error)

func (f leaseVerifierFunc) Verify(ctx context.Context, taskID, workerID string, epoch int64) (bool, error) {
	return f(ctx, taskID, workerID, epoch)
}

// WithUploader 注入对象存储上传器及 object key prefix。
func WithUploader(uploader tasks.FileUploader, prefix string) RunnerOption {
	return func(r *MySQLRunner) {
		r.uploadPrefix = prefix
		if deleter, ok := uploader.(objectDeleter); ok {
			r.objectDeleter = deleter
		}
		if opener, ok := uploader.(objectOpener); ok {
			r.objectOpener = opener
		}
		r.sealedHandler = func(ctx context.Context, file tasks.BinlogFile) error {
			_, err := tasks.ApplySealedUpload(ctx, uploader, r.fileMetaStore, file)
			return err
		}
	}
}

// WithUploadTimeout bounds the post-seal upload. The duration is meta.timeout.upload_sec,
// the same value the background retry passes to withUploadTimeout. A timeout becomes
// UPLOAD_FAILED and the dump continues.
func WithUploadTimeout(timeout time.Duration) RunnerOption {
	return func(r *MySQLRunner) {
		if timeout > 0 {
			r.uploadTimeout = timeout
		}
	}
}

// WithObjectDeleter 注入保留清理时使用的对象删除器。生产启动把上传客户端传进来。
// 同一个客户端实现了 OpenObject 时，接管用它把已上传的封存对象读回来。
func WithObjectDeleter(deleter objectDeleter) RunnerOption {
	return func(r *MySQLRunner) {
		r.objectDeleter = deleter
		if opener, ok := deleter.(objectOpener); ok {
			r.objectOpener = opener
		}
	}
}

// WithSealedHandler 在封文件后把已 seal 文件交给调用方上传（生产由 App 注入 ApplySealedUpload）。
func WithSealedHandler(handler func(context.Context, tasks.BinlogFile) error, prefix string) RunnerOption {
	return func(r *MySQLRunner) {
		r.sealedHandler = handler
		r.uploadPrefix = prefix
	}
}

// WithLeaseVerifier 注入封文件前的租约校验（LeaseManager.Verify 可直接传入）。
func WithLeaseVerifier(verifier LeaseVerifier) RunnerOption {
	return func(r *MySQLRunner) {
		r.leaseVerifier = verifier
	}
}

// WithProgressReporter 注入复制进度上报器。
func WithProgressReporter(reporter ProgressReporter) RunnerOption {
	return func(r *MySQLRunner) {
		r.progressReporter = reporter
	}
}

// NewMySQLRunner 创建 MySQL binlog 拉流执行器。
func NewMySQLRunner(dataDir string, opts ...RunnerOption) *MySQLRunner {
	if dataDir == "" {
		dataDir = "./data"
	}
	r := &MySQLRunner{
		dataDir: dataDir,
		fetcher: &mysqlStatusFetcher{},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Run 兼容 Runner 基础接口，不包含 ready 回调。
func (r *MySQLRunner) Run(ctx context.Context, task tasks.Task) error {
	// 兼容 Runner 基础接口：不携带 ready 回调。
	return finishRun(r.run(ctx, task, nil))
}

// RunWithNotify 在内部 ready 后触发回调，供 Scheduler 精准切 RUNNING。
func (r *MySQLRunner) RunWithNotify(ctx context.Context, task tasks.Task, onReady func()) error {
	// 提供给 Scheduler 的增强接口：runner ready 时主动回调。
	return finishRun(r.run(ctx, task, onReady))
}

// finishRun tells the scheduler to stop when this epoch no longer owns the task.
func finishRun(err error) error {
	if err == nil || !errors.Is(err, ErrLeaseEpochMismatch) {
		return err
	}
	return tasks.NewLeaseHandoff(err)
}

// latestAnchor is the LATEST file/pos this process already resolved for taskID.
func (r *MySQLRunner) latestAnchor(taskID string) (string, uint32, bool) {
	r.anchorMu.Lock()
	defer r.anchorMu.Unlock()
	saved, ok := r.anchor[taskID]
	if !ok || saved.file == "" || saved.pos == 0 {
		return "", 0, false
	}
	return saved.file, saved.pos, true
}

// persistLatestAnchor records a resolved LATEST file/pos before the dump.
// gtid_set is empty: a position in the middle of a file is not an executed set.
func (r *MySQLRunner) persistLatestAnchor(ctx context.Context, taskID, file string, pos uint32) error {
	if taskID == "" || file == "" || pos == 0 {
		return nil
	}
	r.anchorMu.Lock()
	if r.anchor == nil {
		r.anchor = make(map[string]resolvedPos)
	}
	r.anchor[taskID] = resolvedPos{file: file, pos: pos}
	r.anchorMu.Unlock()
	if r.checkpointStore == nil {
		return nil
	}
	err := r.checkpointStore.UpsertCheckpoint(ctx, taskID, binlog.Checkpoint{File: file, Pos: pos})
	if err != nil {
		return checkpointWriteError(err)
	}
	return nil
}

// checkpointWriteError keeps a metadata blip retryable and fails a write that will not clear.
func checkpointWriteError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if meta.IsTransientMySQLError(err) {
		return err
	}
	return tasks.NewPermanentError(tasks.CodeCheckpointWriteFailed, err.Error())
}

// run 执行一次任务复制会话，包含 checkpoint 恢复、拉流、落盘与收尾流程。
func (r *MySQLRunner) run(ctx context.Context, task tasks.Task, onReady func()) error {
	// Step 1: 解析源库标识与复制起点（含 checkpoint 接管修正）。
	// 常见误解：
	// onReady 只表示“复制连接+writer 已就绪”，不代表已经收到第一条业务事件。
	// FILE_POS/GTID 追旧事件时 RUNNING 仍可 DELAYED；fresh LATEST，以及 FILE_POS 起点已经不落后于 master 时，StartSync 成功即在源 tip。
	// dump 在请求位点之前下发的 format description 不是延迟。
	// source_server_uuid 作为 object key 的稳定维度，避免 cluster_key 相同但源实例切换时冲突。
	sourceServerUUID, err := r.fetcher.FetchServerUUID(ctx, task.Source)
	if err != nil {
		return classifySourceError(err)
	}
	sourceServerUUID = strings.TrimSpace(sourceServerUUID)
	if sourceServerUUID == "" {
		return tasks.NewPermanentError(tasks.CodeSourceIdentityUnavailable, "empty source server_uuid")
	}

	// 先解析请求的 start strategy（LATEST/FILE_POS/GTID）。
	requestedLatest := task.Start.Mode == tasks.StartModeLatest || task.Start.Mode == ""
	start, err := ResolveStart(ctx, task, r.fetcher)
	if err != nil {
		return classifySourceError(err)
	}
	var stored binlog.Checkpoint
	checkpointExists := false
	if r.checkpointStore != nil {
		// 持久化 checkpoint 优先级更高，保证重启后的 resumability。
		var ok bool
		stored, ok, err = r.checkpointStore.LoadCheckpoint(ctx, task.ID)
		if err != nil {
			return err
		}
		checkpointExists = ok
	}
	// A crash after the rename and before the upload-state write leaves a sealed
	// file that retry cannot see. Record it as UPLOAD_FAILED first, and if the
	// checkpoint is still on that sealed rotate, continue on the next file.
	if r.sealedHandler != nil {
		if err := r.enrollSealedUploads(ctx, task, sourceServerUUID); err != nil {
			return err
		}
		if checkpointExists && r.checkpointStore != nil {
			if next, advanced := r.checkpointPastSealedRotate(ctx, task.ID, stored); advanced {
				if err := r.checkpointStore.UpsertCheckpoint(ctx, task.ID, next); err != nil {
					return checkpointWriteError(err)
				}
				stored = next
			}
		}
	}
	// Fresh LATEST has already resolved to SHOW MASTER STATUS, so StartSync is at tip.
	// That file/pos is saved below when nothing older exists, with an empty
	// gtid_set. A later retry must not resolve LATEST again (#213). A GTID
	// checkpoint still keeps its set (#175, #199).
	// NextResumePosition is the resume choice shared with GET /api/tasks/{id}/checkpoint.
	// A local complete event wins. Adopt keeps its FILE_POS. Epoch > 1 with no
	// local event used to rewind to position 4. Takeover now follows the catalog
	// file_path: a readable segment directory continues there, an unreadable
	// open segment fails, and a checkpoint already in an UPLOADED object resumes
	// from that object.
	atTip := requestedLatest && !checkpointExists
	// anchorLatest is a first resolution: no stored checkpoint and no local event.
	// Resume and takeover clear it so this attempt does not overwrite them.
	anchorLatest := atTip
	segmentDir := ""
	var carried *tasks.BinlogFile
	flavor := task.Source.Flavor
	if flavor == "" {
		flavor = gomysql.MySQLFlavor
	}
	// gtidFallback is the executed set from the last flush. File/pos is tried
	// first. MySQL 1236 means that file is gone, and this set is how the dump
	// continues. A rewound position does not carry the set; the stored row does.
	gtidFallback := ""
	if checkpointExists {
		gtidFallback = strings.TrimSpace(stored.GTIDSet)
	}
	if resume, ok := tasks.NextResumePosition(r.dataDir, task, stored, checkpointExists); ok {
		start = tasks.StartConfig{
			Mode: tasks.StartModeFilePos,
			File: resume.File,
			Pos:  resume.Pos,
		}
		if g := strings.TrimSpace(resume.GTIDSet); g != "" {
			gtidFallback = g
		}
		atTip = false
		anchorLatest = false
	}
	if task.Epoch > 1 && !task.KeepLocalSegments {
		if _, _, local := binlog.DurableResume(r.dataDir, task.ID); !local {
			files, err := r.listCatalog(ctx, task.ID)
			if err != nil {
				return err
			}
			decision := tasks.ResolveTakeover(r.dataDir, task, stored, checkpointExists, files)
			if decision.Missing != "" {
				return segmentNotOnWorker(decision.Missing)
			}
			if decision.Covering != nil {
				carried = decision.Covering
				dir, err := r.materializeUploaded(ctx, task, *decision.Covering)
				if err != nil {
					return err
				}
				file, pos, ok := binlog.DurableResumeDir(dir)
				if !ok {
					return segmentNotOnWorker(decision.Covering.FileName)
				}
				start = tasks.StartConfig{Mode: tasks.StartModeFilePos, File: file, Pos: pos}
				segmentDir = dir
				atTip = false
				anchorLatest = false
			} else if decision.Apply {
				start = tasks.StartConfig{
					Mode: tasks.StartModeFilePos,
					File: decision.Checkpoint.File,
					Pos:  decision.Checkpoint.Pos,
				}
				if g := strings.TrimSpace(decision.Checkpoint.GTIDSet); g != "" {
					gtidFallback = g
				}
				if decision.Dir != "" {
					segmentDir = decision.Dir
				}
				atTip = false
				anchorLatest = false
			}
		}
	}
	if anchorLatest && start.File != "" && start.Pos != 0 {
		// This process already resolved LATEST and the metadata row was not
		// written, or there is no metadata store. Keep that file/pos.
		if file, pos, ok := r.latestAnchor(task.ID); ok {
			start = tasks.StartConfig{Mode: tasks.StartModeFilePos, File: file, Pos: pos}
			atTip = false
		}
		if err := r.persistLatestAnchor(ctx, task.ID, start.File, start.Pos); err != nil {
			return err
		}
	}

	// Step 2: 打开当前 open 文件并构造 writer。
	currentFile := start.File
	if currentFile == "" {
		currentFile = fmt.Sprintf("task-%s.binlog", task.ID)
	}
	currentPos := start.Pos
	currentStartPos := start.Pos
	currentCreatedAt := time.Now()

	currentPath := ""
	writerOpener := r.writerOpener
	if writerOpener == nil {
		writerOpener = func(task tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			var f *os.File
			var w *binlog.Writer
			var path string
			var err error
			if segmentDir != "" {
				f, w, path, err = r.openBinlogWriterIn(ctx, segmentDir, task, fileName, initialPos, sourceServerUUID, carried)
			} else {
				f, w, path, err = r.openBinlogWriter(ctx, task, fileName, initialPos, sourceServerUUID)
			}
			// A nil *os.File becomes a non-nil io.Closer. The deferred close
			// would then log a failure for a file that was never opened.
			if f == nil {
				return nil, w, path, err
			}
			return f, w, path, err
		}
	}

	file, writer, currentPath, err := writerOpener(task, currentFile, currentPos)
	if err != nil {
		return err
	}
	defer func() {
		if file != nil {
			if err := file.Close(); err != nil {
				log.Printf("close binlog file failed path=%s err=%v", currentPath, err)
			}
		}
	}()

	appendAndPersist := func(raw []byte, next binlog.Checkpoint) error {
		if err := writer.Append(raw, next); err != nil {
			return err
		}
		// 只有 writer flush 成功后才推进 checkpoint：
		// “已推进 checkpoint” 在语义上等价于“数据已安全落盘”。
		if err := writer.FlushAndCheckpoint(); err != nil {
			return err
		}

		checkpoint := writer.CurrentCheckpoint()
		currentPos = checkpoint.Pos
		if r.checkpointStore != nil {
			if err := r.checkpointStore.UpsertCheckpoint(ctx, task.ID, checkpoint); err != nil {
				return checkpointWriteError(err)
			}
		}
		if r.fileMetaStore != nil {
			info, err := os.Stat(currentPath)
			if err != nil {
				return err
			}
			if err := r.fileMetaStore.UpsertBinlogFile(ctx, carryUpload(tasks.BinlogFile{
				TaskID:      task.ID,
				FileName:    currentFile,
				FilePath:    currentPath,
				Epoch:       catalogEpoch(currentPath, task.Epoch),
				State:       "OPEN",
				SizeBytes:   info.Size(),
				StartPos:    currentStartPos,
				EndPos:      checkpoint.Pos,
				CreatedAt:   currentCreatedAt,
				UploadState: "LOCAL_ONLY",
			}, carried)); err != nil {
				return err
			}
		}
		return nil
	}

	// pendingFormat is the format description MySQL sends before a mid-file
	// dump. Its end_log_pos is 0 or the original header position (126 on
	// MySQL 8), which is behind the dump cursor. It is written once, in
	// front of the first copied event, and does not move currentPos.
	// An idle dump that never copies an event leaves it unwritten so resume
	// does not rewind to that header position. A segment that already has
	// events does not gain a second copy on the next start.
	var pendingFormat []byte
	persistRaw := func(raw []byte, next binlog.Checkpoint) error {
		payload := raw
		if len(pendingFormat) > 0 {
			if !segmentHasEvents(currentPath) {
				payload = make([]byte, 0, len(pendingFormat)+len(raw))
				payload = append(payload, pendingFormat...)
				payload = append(payload, raw...)
			}
			pendingFormat = nil
		}
		return appendAndPersist(payload, next)
	}

	// executed is the GTID set of transactions fully flushed. The seed is the
	// stored checkpoint when one exists, otherwise the GTID the task was
	// created with. An empty checkpoint write used to replace that set.
	executed := newExecutedGTID(flavor, gtidSeed(task.Start.GTIDSet, stored.GTIDSet, checkpointExists))
	// A file/pos task has no seed. Events in the middle of a file are not an
	// executed set, and writing them would make the next 1236 resume too short.
	trackGTID := executed.current() != ""
	checkpointAt := func(file string, pos uint32) binlog.Checkpoint {
		if pos == 0 {
			pos = currentPos
		}
		return binlog.Checkpoint{File: file, Pos: pos, GTIDSet: executed.current()}
	}

	// Step 3: 定义统一事件处理逻辑（异步/半同步共用）。
	// 单条事件处理逻辑：异步模式（GetEvent）与半同步模式（SynchronousEventHandler）共用。
	handleEvent := func(event *replication.BinlogEvent) error {
		if event == nil || event.Header == nil {
			return nil
		}
		if trackGTID {
			executed.note(event)
		}
		sourceEventAt := sourceEventTime(event)

		if event.Header.EventType == replication.HEARTBEAT_EVENT || event.Header.EventType == replication.HEARTBEAT_LOG_EVENT_V2 {
			if r.progressReporter != nil {
				r.progressReporter.ReportReplicationProgress(task.ID, sourceEventAt, currentFile, currentPos, atTip)
			}
			return nil
		}

		if event.Header.EventType == replication.ROTATE_EVENT {
			rotate, ok := event.Event.(*replication.RotateEvent)
			if ok && len(rotate.NextLogName) > 0 {
				nextFile := string(rotate.NextLogName)
				nextPos := uint32(rotate.Position)

				// 复制流开头常见一个 synthetic rotate（log_pos=0，指向当前文件）。
				// 该事件不在真实 binlog 文件里，写入会破坏和源文件的一致性。
				if event.Header.LogPos == 0 && nextFile == currentFile {
					currentPos = nextPos
					return nil
				}

				// 真实 rotate 必须先写入旧文件，再封口旧文件并切到新文件。
				rotateCheckpoint := checkpointAt(currentFile, event.Header.LogPos)
				if err := persistRaw(event.RawData, rotateCheckpoint); err != nil {
					return err
				}
				pendingFormat = nil

				if err := file.Close(); err != nil {
					return err
				}
				file = nil
				// Record the sealed row before the put. A crash inside the put
				// leaves UPLOAD_FAILED, which the background retry already uploads.
				// The next-file checkpoint is written before the put so a hung
				// upload cannot pin the lease, and a timeout still continues here.
				sealed, err := r.sealLocalFile(
					ctx,
					task,
					sourceServerUUID,
					currentPath,
					currentStartPos,
					rotateCheckpoint.Pos,
					currentCreatedAt,
					time.Now(),
				)
				if err != nil {
					return err
				}

				currentFile = nextFile
				currentPos = nextPos

				// The rotate is already in the sealed file. Record the next file
				// before opening it. If that open fails, the retry must start
				// there. Resuming on the sealed name opens <name>.open.e<epoch>
				// beside it, and the next rotate stops on
				// "sealed file already exists".
				if r.checkpointStore != nil {
					if err := r.checkpointStore.UpsertCheckpoint(ctx, task.ID, checkpointAt(currentFile, currentPos)); err != nil {
						return checkpointWriteError(err)
					}
				}
				if err := r.uploadSealed(ctx, sealed); err != nil {
					return err
				}

				file, writer, currentPath, err = writerOpener(task, currentFile, currentPos)
				if err != nil {
					return err
				}
				currentStartPos = currentPos
				currentCreatedAt = time.Now()

				if r.progressReporter != nil {
					r.progressReporter.ReportReplicationProgress(task.ID, sourceEventAt, currentFile, currentPos, atTip)
				}
				return nil
			}
		}

		// 从文件中部 dump 时，源库仍会先下发文件头的 format description（log_pos 置 0，
		// 或保留原始 end_log_pos，例如 MySQL 8 的 126）。该事件不在当前位点之后。
		// 单独落盘会把续传位点退回 126，并用文件创建时间报 DELAYED。
		// 这里只记住原文，等第一个真正复制的事件一起写入，位点停在那个事件上。
		// synthetic rotate 已在上面处理，这里不能抢在它前面把 log_pos=0 丢掉。
		if event.Header.EventType == replication.FORMAT_DESCRIPTION_EVENT && event.Header.LogPos <= currentPos {
			if !segmentHasEvents(currentPath) && len(event.RawData) > 0 {
				pendingFormat = append([]byte(nil), event.RawData...)
			}
			return nil
		}
		if event.Header.LogPos <= currentPos {
			return nil
		}

		next := checkpointAt(currentFile, event.Header.LogPos)
		if err := persistRaw(event.RawData, next); err != nil {
			return err
		}
		if r.progressReporter != nil {
			r.progressReporter.ReportReplicationProgress(task.ID, sourceEventAt, currentFile, currentPos, atTip)
		}
		return nil
	}

	// Step 4: 建立复制连接并启动拉流循环。
	semiSyncRequested := task.Source.SemiSync
	cfg := buildSyncerConfig(task)
	if semiSyncRequested {
		// 半同步模式使用同步处理器：只有 HandleEvent 成功返回后才会 ACK。
		// 这样可保证 ACK 发生在本地 fsync/checkpoint 成功之后（防止 ACK 早于持久化）。
		cfg.SynchronousEventHandler = eventHandlerFunc(handleEvent)
	}
	newSyncer := r.newSyncer
	if newSyncer == nil {
		newSyncer = func(cfg replication.BinlogSyncerConfig) binlogSyncer {
			return &liveBinlogSyncer{inner: replication.NewBinlogSyncer(cfg)}
		}
	}
	syncer := newSyncer(cfg)
	// Close is a method value. Reassigning syncer after a 1236 fallback must
	// not leave the new syncer unclosed, and must not close it twice via the
	// original method value.
	closeSyncer := syncer.Close
	defer func() { closeSyncer() }()

	streamer, start, err := openDump(syncer, flavor, start, gtidFallback)
	if err != nil {
		return classifySourceError(err)
	}

	// Adopt/resume is FILE_POS at a saved position. If that position is already the
	// master tip, report at-tip now. Waiting for the 2s idle poll lets the format
	// description header (binlog create time) cross the delay threshold first.
	if !atTip && start.Mode == tasks.StartModeFilePos && r.fetcher != nil {
		status, statusErr := r.fetcher.FetchMasterStatus(ctx, task.Source)
		if statusErr == nil && dumpAtOrBeyondMaster(currentFile, currentPos, status) {
			atTip = true
		}
	}

	if atTip && r.progressReporter != nil {
		// StartSync is already at master file/pos; do not wait for idle or the next event.
		r.progressReporter.ReportReplicationProgress(task.ID, time.Now().UTC(), currentFile, currentPos, true)
	}

	if onReady != nil {
		// 到这里说明“连接 + 起点 + writer”都已 ready，Scheduler 可安全切 RUNNING。
		onReady()
	}

	// sawDumpEvent is the first event from this dump. MySQL 1236 for a purged
	// file arrives on GetEvent after StartSync has already returned nil.
	// Falling back later would skip events already written.
	sawDumpEvent := false
	gtidFallbackUsed := false
	readEvent := func() (*replication.BinlogEvent, error) {
		for {
			event, err := r.nextEvent(ctx, streamer, task.ID, task.Source, currentFile, currentPos, &atTip)
			if err != nil {
				if ctx.Err() != nil || errors.Is(err, context.Canceled) {
					return nil, err
				}
				if !sawDumpEvent && !gtidFallbackUsed && start.Mode == tasks.StartModeFilePos && mysqlError1236(err) && strings.TrimSpace(gtidFallback) != "" {
					gtidFallbackUsed = true
					log.Printf("file/pos resume hit MySQL 1236 before any event; opening GTID dump task=%s file=%s pos=%d", task.ID, start.File, start.Pos)
					syncer.Close()
					syncer = newSyncer(cfg)
					closeSyncer = syncer.Close
					gtidStart := tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: strings.TrimSpace(gtidFallback)}
					var dumpErr error
					streamer, start, dumpErr = openDump(syncer, flavor, gtidStart, "")
					if dumpErr != nil {
						return nil, classifySourceError(dumpErr)
					}
					continue
				}
				return nil, err
			}
			if event != nil {
				sawDumpEvent = true
			}
			return event, nil
		}
	}

	if semiSyncRequested {
		// SynchronousEventHandler 模式下，事件由 syncer 内部 goroutine 推送到 handler。
		// 这里阻塞等待错误或取消，保持任务生命周期。
		for {
			event, err := readEvent()
			if err != nil {
				if ctx.Err() != nil || errors.Is(err, context.Canceled) {
					return nil
				}
				return err
			}
			if event == nil {
				continue
			}
		}
	}

	// Step 5: 异步模式主循环（逐条拉取并处理事件）。
	for {
		event, err := readEvent()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		if event == nil {
			continue
		}
		if err := handleEvent(event); err != nil {
			return err
		}
	}
}

const idlePollInterval = 2 * time.Second

func (r *MySQLRunner) nextEvent(ctx context.Context, streamer binlogStreamer, taskID string, source tasks.SourceConfig, file string, pos uint32, atTip *bool) (*replication.BinlogEvent, error) {
	eventCtx, cancel := context.WithTimeout(ctx, idlePollInterval)
	event, err := streamer.GetEvent(eventCtx)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		if errors.Is(err, context.DeadlineExceeded) {
			// Quiet dump wait is not at-tip by itself: FILE_POS/GTID catch-up can
			// idle while still behind SHOW MASTER STATUS. Confirm file then pos.
			tip := r.confirmIdleAtTip(ctx, source, file, pos)
			if atTip != nil {
				*atTip = tip
			}
			if r.progressReporter != nil {
				eventAt := time.Time{}
				if tip {
					eventAt = time.Now().UTC()
				}
				r.progressReporter.ReportReplicationProgress(taskID, eventAt, file, pos, tip)
			}
			return nil, nil
		}
		return nil, classifySourceError(err)
	}
	return event, nil
}

func (r *MySQLRunner) confirmIdleAtTip(ctx context.Context, source tasks.SourceConfig, file string, pos uint32) bool {
	if r.fetcher == nil {
		return false
	}
	status, err := r.fetcher.FetchMasterStatus(ctx, source)
	if err != nil {
		return false
	}
	return dumpAtOrBeyondMaster(file, pos, status)
}

// sealedUploadPending is the upload_error stored before the put.
// It is not a checksum verify, so the existing retry uploads the file.
const sealedUploadPending = "upload pending"

// finalizeSealedFile seals the open file, records upload intent, and uploads.
// Tests and any caller that seals without the rotate loop use this.
func (r *MySQLRunner) finalizeSealedFile(
	ctx context.Context,
	task tasks.Task,
	sourceServerUUID string,
	localPath string,
	startPos uint32,
	endPos uint32,
	createdAt time.Time,
	sealedAt time.Time,
) error {
	meta, err := r.sealLocalFile(ctx, task, sourceServerUUID, localPath, startPos, endPos, createdAt, sealedAt)
	if err != nil {
		return err
	}
	return r.uploadSealed(ctx, meta)
}

// sealLocalFile renames the open segment and writes the catalog row.
// With an uploader, that row is UPLOAD_FAILED before the put, so a crash
// during the put is already in the retry set. With no uploader, it stays LOCAL_ONLY.
func (r *MySQLRunner) sealLocalFile(
	ctx context.Context,
	task tasks.Task,
	sourceServerUUID string,
	localPath string,
	startPos uint32,
	endPos uint32,
	createdAt time.Time,
	sealedAt time.Time,
) (tasks.BinlogFile, error) {
	// 常见误解：
	// 1) “上传失败就应该报错退出”不符合本项目策略；这里是 best-effort，失败仅记元数据。
	// 2) “seal 只是改文件名”不完整；cluster 下 seal/upload 前必须再校验 lease ownership。
	// Step 1: cluster 下先做 ownership 校验，防止失租后继续发布文件。
	if task.Epoch > 0 && r.leaseVerifier != nil {
		ok, err := r.leaseVerifier.Verify(ctx, task.ID, task.OwnerWorkerID, task.Epoch)
		if err != nil {
			return tasks.BinlogFile{}, err
		}
		if !ok {
			return tasks.BinlogFile{}, ErrLeaseEpochMismatch
		}
	}

	// Step 2: open 文件改名为 sealed 文件（并防止覆盖已有 sealed 文件）。
	// A later epoch of a source name that already has a sealed segment uses
	// name.sealed.e<epoch> so the earlier path and object key stay put.
	sealedPath, sourceFile, err := sealPath(localPath, task.Epoch)
	if err != nil {
		return tasks.BinlogFile{}, err
	}
	if task.Epoch > 0 {
		chosen, err := r.sealTarget(ctx, task, filepath.Dir(localPath), sourceFile)
		if err != nil {
			return tasks.BinlogFile{}, err
		}
		sealedPath = chosen
	}
	if localPath != sealedPath {
		if _, err := os.Stat(sealedPath); err == nil {
			return tasks.BinlogFile{}, tasks.NewPermanentError(tasks.CodeSealedFileExists, fmt.Sprintf("sealed file already exists: %s", sealedPath))
		} else if !os.IsNotExist(err) {
			return tasks.BinlogFile{}, err
		}
		if err := os.Rename(localPath, sealedPath); err != nil {
			return tasks.BinlogFile{}, err
		}
	}

	info, err := os.Stat(sealedPath)
	if err != nil {
		return tasks.BinlogFile{}, err
	}
	meta := tasks.BinlogFile{
		TaskID:    task.ID,
		FileName:  sourceFile,
		FilePath:  sealedPath,
		Epoch:     catalogEpoch(sealedPath, task.Epoch),
		State:     "SEALED",
		SizeBytes: info.Size(),
		StartPos:  startPos,
		EndPos:    endPos,
		CreatedAt: createdAt,
		SealedAt:  sealedAt,
	}
	if r.sealedHandler == nil {
		meta.UploadState = "LOCAL_ONLY"
		if r.fileMetaStore != nil {
			if err := r.fileMetaStore.UpsertBinlogFile(ctx, meta); err != nil {
				return tasks.BinlogFile{}, err
			}
		}
		return meta, nil
	}
	if strings.TrimSpace(task.ClusterKey) == "" {
		return tasks.BinlogFile{}, errors.New("cluster_key is required")
	}
	sourceServerUUID = strings.TrimSpace(sourceServerUUID)
	if sourceServerUUID == "" {
		return tasks.BinlogFile{}, errors.New("source server_uuid is required")
	}
	meta.ObjectKey = buildObjectKey(r.uploadPrefix, task.ClusterKey, sourceServerUUID, filepath.Base(sealedPath))
	meta.UploadState = "UPLOAD_FAILED"
	meta.UploadError = sealedUploadPending
	if r.fileMetaStore != nil {
		if err := r.fileMetaStore.UpsertBinlogFile(ctx, meta); err != nil {
			return tasks.BinlogFile{}, err
		}
	}
	return meta, nil
}

// uploadSealed runs the post-seal put on a child context bounded by uploadTimeout.
// The deadline is the retry path's upload timeout. A timeout leaves the
// UPLOAD_FAILED row in place and does not stop the dump.
func (r *MySQLRunner) uploadSealed(ctx context.Context, file tasks.BinlogFile) error {
	if r == nil || r.sealedHandler == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	uploadCtx, cancel := r.boundedUpload(ctx)
	defer cancel()
	err := r.sealedHandler(uploadCtx, file)
	if err == nil || ctx.Err() != nil {
		return err
	}
	if uploadCtx.Err() != nil {
		return nil
	}
	return err
}

func (r *MySQLRunner) boundedUpload(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if r == nil || r.uploadTimeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, r.uploadTimeout)
}

// enrollSealedUploads records a sealed file the retry set does not yet name.
// That is a crash after rename, before the upload-state write, or a sealed
// LOCAL_ONLY row left while an uploader is configured. No uploader means this
// is not called, so LOCAL_ONLY stays unuploaded.
func (r *MySQLRunner) enrollSealedUploads(ctx context.Context, task tasks.Task, sourceServerUUID string) error {
	if r == nil || r.sealedHandler == nil || r.fileMetaStore == nil {
		return nil
	}
	if strings.TrimSpace(task.ClusterKey) == "" || strings.TrimSpace(sourceServerUUID) == "" {
		return nil
	}
	files, err := r.listCatalog(ctx, task.ID)
	if err != nil {
		return err
	}
	byName := make(map[string]tasks.BinlogFile, len(files))
	dirs := map[string]struct{}{}
	if dir := filepath.Join(r.dataDir, task.ID); strings.TrimSpace(r.dataDir) != "" && task.ID != "" {
		dirs[dir] = struct{}{}
	}
	for _, row := range files {
		byName[catalogSegmentName(row)] = row
		if path := strings.TrimSpace(row.FilePath); path != "" && path != "." {
			dirs[filepath.Dir(path)] = struct{}{}
		}
	}
	for dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			named, ok := binlog.ClassifySegment(entry.Name())
			if !ok || named.Open {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			row, exists := byName[entry.Name()]
			if !exists {
				row, exists = missingOpenRow(files, named)
			}
			if exists && isUploadedRow(row) {
				continue
			}
			if exists && sealedRetryReady(row, path) {
				continue
			}
			if exists && isOpenCatalogRow(row) && sameRegularFile(row.FilePath, path) {
				continue
			}
			if !exists && task.Epoch <= 0 {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			next := tasks.BinlogFile{
				TaskID:      task.ID,
				FileName:    named.Source,
				FilePath:    path,
				Epoch:       catalogEpoch(path, task.Epoch),
				State:       "SEALED",
				SizeBytes:   info.Size(),
				StartPos:    4,
				EndPos:      0,
				CreatedAt:   info.ModTime(),
				SealedAt:    info.ModTime(),
				ObjectKey:   buildObjectKey(r.uploadPrefix, task.ClusterKey, sourceServerUUID, entry.Name()),
				UploadState: "UPLOAD_FAILED",
				UploadError: sealedUploadPending,
			}
			if exists {
				next.Epoch = row.Epoch
				if strings.TrimSpace(row.ObjectKey) != "" {
					next.ObjectKey = row.ObjectKey
				}
				if row.StartPos != 0 {
					next.StartPos = row.StartPos
				}
				if row.EndPos != 0 {
					next.EndPos = row.EndPos
				}
				if !row.CreatedAt.IsZero() {
					next.CreatedAt = row.CreatedAt
				}
				if !row.SealedAt.IsZero() {
					next.SealedAt = row.SealedAt
				}
			}
			if next.SealedAt.IsZero() {
				next.SealedAt = time.Now()
			}
			if err := r.fileMetaStore.UpsertBinlogFile(ctx, next); err != nil {
				return err
			}
			byName[entry.Name()] = next
			log.Printf("sealed upload pending task=%s file=%s", task.ID, named.Source)
		}
	}
	return nil
}

// missingOpenRow is the OPEN catalog row whose file was renamed to this sealed
// name before the sealed state was written. A later open epoch that is still
// on disk is not that row.
func missingOpenRow(files []tasks.BinlogFile, named binlog.SegmentName) (tasks.BinlogFile, bool) {
	if named.Open {
		return tasks.BinlogFile{}, false
	}
	for _, row := range files {
		if row.FileName != named.Source || !isOpenCatalogRow(row) {
			continue
		}
		if named.Epoch >= 0 && row.Epoch != named.Epoch {
			continue
		}
		path := strings.TrimSpace(row.FilePath)
		if path == "" {
			continue
		}
		if named.Epoch < 0 && filepath.Base(path) != fmt.Sprintf("%s.open.e%d", named.Source, row.Epoch) {
			continue
		}
		info, err := os.Stat(path)
		if err == nil && info.Mode().IsRegular() {
			continue
		}
		return row, true
	}
	return tasks.BinlogFile{}, false
}

func sealedRetryReady(row tasks.BinlogFile, path string) bool {
	if !strings.EqualFold(strings.TrimSpace(row.UploadState), "UPLOAD_FAILED") {
		return false
	}
	if strings.TrimSpace(row.ObjectKey) == "" || row.SealedAt.IsZero() {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(row.State), "SEALED") {
		return false
	}
	return sameRegularFile(row.FilePath, path)
}

func sameRegularFile(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if filepath.Clean(a) != filepath.Clean(b) {
		return false
	}
	info, err := os.Stat(b)
	return err == nil && info.Mode().IsRegular()
}

// checkpointPastSealedRotate moves a checkpoint that still names a sealed
// rotate onto the next file. Resuming on the sealed name would open a new
// segment beside it and stop on "sealed file already exists".
func (r *MySQLRunner) checkpointPastSealedRotate(ctx context.Context, taskID string, cp binlog.Checkpoint) (binlog.Checkpoint, bool) {
	if cp.File == "" || cp.Pos == 0 {
		return cp, false
	}
	path := r.sealedFilePath(ctx, taskID, cp.File)
	if path == "" {
		return cp, false
	}
	nextName, nextPos, endPos, ok := binlog.LastRotateTarget(path)
	if !ok || endPos == 0 || endPos != cp.Pos || nextName == "" || nextName == cp.File {
		return cp, false
	}
	out := cp
	out.File = nextName
	out.Pos = nextPos
	return out, true
}

func (r *MySQLRunner) sealedFilePath(ctx context.Context, taskID, name string) string {
	if name == "" || strings.ContainsAny(name, "/\\") {
		return ""
	}
	if strings.TrimSpace(r.dataDir) != "" && taskID != "" {
		direct := filepath.Join(r.dataDir, taskID, name)
		if info, err := os.Stat(direct); err == nil && info.Mode().IsRegular() {
			return direct
		}
	}
	files, err := r.listCatalog(ctx, taskID)
	if err != nil {
		return ""
	}
	for _, row := range files {
		if row.FileName != name {
			continue
		}
		path := strings.TrimSpace(row.FilePath)
		if filepath.Base(path) == name {
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
				return path
			}
		}
		if path != "" {
			sibling := filepath.Join(filepath.Dir(path), name)
			if info, err := os.Stat(sibling); err == nil && info.Mode().IsRegular() {
				return sibling
			}
		}
	}
	return ""
}

// maxDumpReconnectAttempts is how many times go-mysql may reconnect a dump
// that is already open before GetEvent returns the error.
// Each failed try waits 1s. A blip that reconnects inside this window stays
// RUNNING. Zero would retry forever inside the library, and the scheduler
// would keep showing RUNNING through a source outage.
const maxDumpReconnectAttempts = 5

// buildSyncerConfig 基于任务配置构造 go-mysql syncer 参数。
func buildSyncerConfig(task tasks.Task) replication.BinlogSyncerConfig {
	// 常见误解：
	// 不配置 server_id 并不等于 0 透传；这里会生成稳定默认值，避免与其他复制客户端冲突。
	flavor := task.Source.Flavor
	if flavor == "" {
		flavor = "mysql"
	}

	serverID := task.Source.ServerID
	if serverID == 0 {
		serverID = defaultServerID(task.ID)
	}

	return replication.BinlogSyncerConfig{
		ServerID:             serverID,
		Flavor:               flavor,
		Host:                 task.Source.Host,
		Port:                 task.Source.Port,
		User:                 task.Source.User,
		Password:             task.Source.Password,
		RawModeEnabled:       true,
		SemiSyncEnabled:      task.Source.SemiSync,
		MaxReconnectAttempts: maxDumpReconnectAttempts,
	}
}

func carryUpload(meta tasks.BinlogFile, carried *tasks.BinlogFile) tasks.BinlogFile {
	if carried == nil || carried.FileName != meta.FileName {
		return meta
	}
	if !strings.EqualFold(strings.TrimSpace(carried.UploadState), "UPLOADED") || strings.TrimSpace(carried.ObjectKey) == "" {
		return meta
	}
	meta.ObjectKey = carried.ObjectKey
	meta.UploadState = carried.UploadState
	return meta
}

func segmentNotOnWorker(label string) error {
	return tasks.NewPermanentError(tasks.CodeSegmentNotOnWorker, label+" is not on this worker. The lease moved; the segment directory did not. Make that file readable on this worker, then start the task.")
}

func (r *MySQLRunner) listCatalog(ctx context.Context, taskID string) ([]tasks.BinlogFile, error) {
	if r.fileMetaStore == nil {
		return nil, nil
	}
	lister, ok := r.fileMetaStore.(interface {
		ListBinlogFiles(context.Context, string, int) ([]tasks.BinlogFile, error)
	})
	if !ok {
		return nil, nil
	}
	return lister.ListBinlogFiles(ctx, taskID, retentionCatalogLimit)
}

// materializeUploaded copies a sealed UPLOADED object into this worker's
// segment directory so takeover can append after its last complete event.
func (r *MySQLRunner) materializeUploaded(ctx context.Context, task tasks.Task, row tasks.BinlogFile) (string, error) {
	label := strings.TrimSpace(row.FileName)
	if path := strings.TrimSpace(row.FilePath); path != "" {
		label = path
	}
	if r.objectOpener == nil {
		return "", segmentNotOnWorker(label)
	}
	rc, _, err := r.objectOpener.OpenObject(ctx, row.ObjectKey)
	if err != nil {
		return "", segmentNotOnWorker(label)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return "", segmentNotOnWorker(label)
	}
	dir := filepath.Join(r.dataDir, task.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, openFileName(row.FileName, task.Epoch))
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return "", err
	}
	return dir, nil
}

// openBinlogWriter 打开（或创建）本地 open 文件并返回带初始 checkpoint 的 writer。
func (r *MySQLRunner) openBinlogWriter(ctx context.Context, task tasks.Task, fileName string, initialPos uint32, sourceServerUUID string) (*os.File, *binlog.Writer, string, error) {
	return r.openBinlogWriterIn(ctx, filepath.Join(r.dataDir, task.ID), task, fileName, initialPos, sourceServerUUID, nil)
}

func (r *MySQLRunner) openBinlogWriterIn(ctx context.Context, dir string, task tasks.Task, fileName string, initialPos uint32, sourceServerUUID string, carried *tasks.BinlogFile) (*os.File, *binlog.Writer, string, error) {
	// Step 1: 准备目录并清理 stale open / 过期文件。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, "", err
	}
	// Rename the open segment whose last complete event already ends at
	// initialPos onto this epoch, then append. Adopt sets KeepLocalSegments
	// and must still do that rename: a new file would be magic plus the next
	// source event, with no format description and none of the bytes already
	// on disk. Unrelated epochs and sealed files stay. A normal resume also
	// drops other epochs. No matching segment still starts clean.
	if err := continueDurableOpenSegment(dir, fileName, task.Epoch, initialPos); err != nil {
		return nil, nil, "", err
	}
	if !task.KeepLocalSegments {
		if err := cleanupStaleOpenFiles(dir, task.Epoch); err != nil {
			return nil, nil, "", err
		}
	}
	localFileName := openFileName(fileName, task.Epoch)
	if err := r.cleanupTaskBinlogs(ctx, task, dir, localFileName, sourceServerUUID, time.Now()); err != nil {
		return nil, nil, "", err
	}

	// Step 2: 打开当前 open 文件，空文件时写入 binlog magic header。
	path := filepath.Join(dir, localFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, "", err
	}

	info, err := f.Stat()
	if err != nil {
		if closeErr := f.Close(); closeErr != nil {
			log.Printf("close binlog file after stat failure path=%s err=%v", path, closeErr)
		}
		return nil, nil, "", err
	}
	if info.Size() == 0 {
		if _, err := f.Write(binlogMagic); err != nil {
			if closeErr := f.Close(); closeErr != nil {
				log.Printf("close binlog file after write failure path=%s err=%v", path, closeErr)
			}
			return nil, nil, "", err
		}
		if err := f.Sync(); err != nil {
			if closeErr := f.Close(); closeErr != nil {
				log.Printf("close binlog file after sync failure path=%s err=%v", path, closeErr)
			}
			return nil, nil, "", err
		}
	}

	// Step 3: 持久化当前 open segment，供运行中 /files 查询。
	info, err = f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, "", err
	}
	if r.fileMetaStore != nil {
		if err := r.retireOtherOpenRows(ctx, task.ID, fileName, catalogEpoch(path, task.Epoch)); err != nil {
			_ = f.Close()
			return nil, nil, "", err
		}
		createdAt := time.Now()
		if err := r.fileMetaStore.UpsertBinlogFile(ctx, carryUpload(tasks.BinlogFile{
			TaskID:      task.ID,
			FileName:    fileName,
			FilePath:    path,
			Epoch:       catalogEpoch(path, task.Epoch),
			State:       "OPEN",
			SizeBytes:   info.Size(),
			StartPos:    initialPos,
			EndPos:      initialPos,
			CreatedAt:   createdAt,
			UploadState: "LOCAL_ONLY",
		}, carried)); err != nil {
			_ = f.Close()
			return nil, nil, "", err
		}
		// An older catalog row can still say OPEN for a segment this process
		// just removed, or for a path that is already gone. Retention does not
		// delete OPEN rows, so drop the ones in this directory that have no file.
		if err := r.retireMissingLocalOpenRows(ctx, task.ID, dir); err != nil {
			_ = f.Close()
			return nil, nil, "", err
		}
	}

	// Step 4: 返回带初始 checkpoint 的 writer。
	writer := binlog.NewWriter(f, binlog.Checkpoint{
		File: fileName,
		Pos:  initialPos,
	})
	return f, writer, path, nil
}

// continueDurableOpenSegment moves the open segment that already ends at
// initialPos onto this epoch so the next append keeps those bytes.
// A different position is left for cleanup (a new worker rebuilds from pos 4).
func continueDurableOpenSegment(dir, fileName string, epoch int64, initialPos uint32) error {
	if fileName == "" || initialPos == 0 {
		return nil
	}
	currentPath := filepath.Join(dir, openFileName(fileName, epoch))
	if info, err := os.Stat(currentPath); err == nil && info.Size() > 0 {
		endPos, end, _, ok := binlog.DurableCursor(currentPath)
		if ok && endPos == initialPos && end < info.Size() {
			return os.Truncate(currentPath, end)
		}
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	prev, ok := openSegmentEndingAt(dir, fileName, initialPos)
	if !ok || prev.path == currentPath {
		return nil
	}
	info, err := os.Stat(prev.path)
	if err != nil {
		return err
	}
	if prev.end < info.Size() {
		if err := os.Truncate(prev.path, prev.end); err != nil {
			return err
		}
	}
	return os.Rename(prev.path, currentPath)
}

type localSegment struct {
	source string
	seq    uint64
	epoch  int64
	path   string
	end    int64
}

func openSegmentEndingAt(dir, fileName string, pos uint32) (localSegment, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return localSegment{}, false
	}
	var best localSegment
	found := false
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		named, ok := binlog.ClassifySegment(entry.Name())
		if !ok || !named.Open || named.Source != fileName {
			continue
		}
		seg := localSegment{source: named.Source, seq: named.Seq, epoch: named.Epoch}
		seg.path = filepath.Join(dir, entry.Name())
		endPos, end, _, ok := binlog.DurableCursor(seg.path)
		if !ok || endPos != pos {
			continue
		}
		seg.end = end
		if !found || seg.epoch > best.epoch {
			best = seg
			found = true
		}
	}
	return best, found
}

// defaultServerID 为未显式配置 server_id 的任务生成稳定默认值。
func defaultServerID(taskID string) uint32 {
	if n, err := strconv.ParseUint(taskID, 10, 32); err == nil {
		return defaultServerIDBase + uint32(n)
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(taskID))
	return defaultServerIDBase + (h.Sum32() % defaultServerIDMod)
}

type mysqlStatusFetcher struct{}

// FetchMasterStatus 读取主库当前 binlog file/pos。
func (f *mysqlStatusFetcher) FetchMasterStatus(_ context.Context, source tasks.SourceConfig) (MasterStatus, error) {
	addr := fmt.Sprintf("%s:%d", source.Host, source.Port)
	conn, err := sqlclient.Connect(addr, source.User, source.Password, "")
	if err != nil {
		return MasterStatus{}, classifySourceError(err)
	}
	defer conn.Close()

	result, err := conn.Execute("SHOW MASTER STATUS")
	if err != nil || result == nil || result.Resultset == nil || result.Resultset.RowNumber() == 0 {
		result, err = conn.Execute("SHOW BINLOG STATUS")
		if err != nil {
			return MasterStatus{}, classifySourceError(err)
		}
	}
	if result == nil || result.Resultset == nil || result.Resultset.RowNumber() == 0 {
		return MasterStatus{}, errors.New("empty master status")
	}

	file, err := result.GetString(0, 0)
	if err != nil {
		return MasterStatus{}, err
	}
	pos, err := result.GetUint(0, 1)
	if err != nil {
		return MasterStatus{}, err
	}

	return MasterStatus{
		File: file,
		Pos:  uint32(pos),
	}, nil
}

// FetchServerUUID 读取源库身份（MySQL server_uuid；MariaDB 使用 server_id+gtid_domain_id）。
// server_uuid 为空或 unknown system variable 时，非 mariadb flavor 返回要求改 flavor 的永久错误。
func (f *mysqlStatusFetcher) FetchServerUUID(_ context.Context, source tasks.SourceConfig) (string, error) {
	addr := fmt.Sprintf("%s:%d", source.Host, source.Port)
	conn, err := sqlclient.Connect(addr, source.User, source.Password, "")
	if err != nil {
		return "", classifySourceError(err)
	}
	defer conn.Close()

	logBin, err := queryVariable(conn, "log_bin")
	if err != nil {
		return "", classifySourceError(err)
	}
	serverUUID, uuidErr := queryVariable(conn, "server_uuid")
	serverID, _ := queryVariable(conn, "server_id")
	domainID, _ := queryVariable(conn, "gtid_domain_id")
	identity, err := identityFromProbe(source.Flavor, logBin, serverUUID, uuidErr, serverID, domainID)
	if err != nil {
		return "", classifySourceError(err)
	}
	return identity, nil
}

func queryVariable(conn *sqlclient.Conn, name string) (string, error) {
	switch name {
	case "log_bin", "server_uuid", "server_id", "gtid_domain_id":
	default:
		return "", fmt.Errorf("unsupported variable %q", name)
	}
	result, err := conn.Execute("SHOW VARIABLES LIKE '" + name + "'")
	if err != nil {
		return "", err
	}
	if result == nil || result.Resultset == nil || result.Resultset.RowNumber() == 0 {
		return "", nil
	}
	value, err := result.GetString(0, 1)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

// openDump starts a file/pos dump, or a GTID dump.
// When StartSync itself returns MySQL 1236 and gtidFallback is non-empty, the
// second call is StartSyncGTID on this same syncer and start.Mode becomes GTID
// so at-tip is not taken from the file the source just said is gone.
// go-mysql v1.16 returns (streamer, nil) from StartSync before the server
// answers. That 1236 arrives later from GetEvent; run() closes this syncer
// and opens a new one, because a running syncer rejects StartSyncGTID.
func openDump(syncer binlogSyncer, flavor string, start tasks.StartConfig, gtidFallback string) (binlogStreamer, tasks.StartConfig, error) {
	if flavor == "" {
		flavor = gomysql.MySQLFlavor
	}
	switch start.Mode {
	case tasks.StartModeFilePos:
		streamer, err := syncer.StartSync(gomysql.Position{Name: start.File, Pos: start.Pos})
		if err == nil || !mysqlError1236(err) || strings.TrimSpace(gtidFallback) == "" {
			return streamer, start, err
		}
		set, parseErr := gomysql.ParseGTIDSet(flavor, strings.TrimSpace(gtidFallback))
		if parseErr != nil {
			return nil, start, err
		}
		streamer, err = syncer.StartSyncGTID(set)
		if err != nil {
			return nil, start, err
		}
		start.Mode = tasks.StartModeGTID
		return streamer, start, nil
	case tasks.StartModeGTID:
		set, parseErr := gomysql.ParseGTIDSet(flavor, start.GTIDSet)
		if parseErr != nil {
			return nil, start, parseErr
		}
		streamer, err := syncer.StartSyncGTID(set)
		return streamer, start, err
	default:
		return nil, start, fmt.Errorf("unsupported resolved start mode: %s", start.Mode)
	}
}

// effectiveStartFromCheckpoint 在 checkpoint 可用时覆盖请求起点。
func effectiveStartFromCheckpoint(start tasks.StartConfig, checkpoint binlog.Checkpoint, exists bool) tasks.StartConfig {
	// 常见误解：
	// 一旦 checkpoint 有效，恢复优先级高于请求 start 配置；这是为了保证可恢复性与连续性。
	if !exists {
		return start
	}
	if checkpoint.File == "" || checkpoint.Pos == 0 {
		return start
	}
	return tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: checkpoint.File,
		Pos:  checkpoint.Pos,
	}
}

// objectOpener reads one uploaded object back. S3Uploader implements it.
// Takeover uses it when the checkpoint is already inside a sealed object and
// the local file is gone. Download of one segment stays on the tasks API.
type objectOpener interface {
	OpenObject(ctx context.Context, objectKey string) (io.ReadCloser, int64, error)
}

// objectDeleter removes one uploaded object. S3Uploader implements it.
// A missing object is success so a retry after a partial purge can finish.
type objectDeleter interface {
	DeleteObject(ctx context.Context, objectKey string) error
}

// expiredFileCatalog is the binlog_files lookup retention uses.
// ListBinlogFiles must be called with retentionCatalogLimit: the replay window
// keeps the newest source indexes, and retention needs the oldest rows.
type expiredFileCatalog interface {
	ListBinlogFiles(ctx context.Context, taskID string, limit int) ([]tasks.BinlogFile, error)
	DeleteBinlogFile(ctx context.Context, taskID, fileName string, epoch int64) error
}

// retentionCatalogLimit keeps every catalog row. A replay-sized limit would
// drop the oldest uploaded segments and purge them locally only.
const retentionCatalogLimit = int(^uint(0) >> 1)

// objectPurgeFailed is the operator-facing prefix when retention cannot delete
// the bucket object. The task last_error starts with it, and the next file
// open retries because the local segment is still there.
const objectPurgeFailed = "OBJECT_PURGE_FAILED"

// retentionSkippedNotUploaded is the task event written when age retention
// leaves an expired sealed file that has not been uploaded. The message names
// the file and its upload_state. One event per file; later opens do not append
// another for that same file.
const retentionSkippedNotUploaded = "RETENTION_SKIPPED_NOT_UPLOADED"

// retentionObjects decides which expired sealed file has a bucket object.
// A local file is deleted only when that object was verified (checksum match).
// A mismatch or an unfinished check keeps the local file. A bucket-only
// UPLOADED row whose local file is already gone is still aged as before.
type retentionObjects struct {
	byName        map[string]tasks.BinlogFile
	deleter       objectDeleter
	standaloneKey func(fileName string) string
	drop          func(row tasks.BinlogFile) error
	note          func(row tasks.BinlogFile) error
	noteKept      func(name string)
	announce      func(name, uploadState string)
}

// cleanupExpiredBinlogs 按保留天数清理过期本地文件（跳过当前活跃 open 文件和其它 open 分段）。
func cleanupExpiredBinlogs(dir string, retentionDays int, now time.Time, activeFileName string) error {
	return purgeExpiredBinlogs(context.Background(), dir, retentionDays, retentionDays, now, activeFileName, func() (*retentionObjects, error) {
		return nil, nil
	})
}

// cleanupTaskBinlogs 在打开本地文件时清理过期分段。
// 只配 retention_days，或本地天数与桶天数相同，行为与 v0.5.28 相同：
// checksum 为 match 的已上传封存分段先删对象和目录行，再删本地文件。
// 配了上传和目录，且桶保留长于本地保留时，介于两者之间且 checksum 为 match 的 UPLOADED 封存文件只删本地，
// 对象和目录行留下。本地文件已经不在时，桶年龄用 sealed_at，没有则用 uploaded_at。
// 对象删除失败时本地文件留下，错误以 OBJECT_PURGE_FAILED 返回，下一次打开文件会再试。
// 配置了上传且有目录时，过期的 UPLOAD_FAILED / LOCAL_ONLY 封存文件和目录行留下，不返回错误。
// 磁盘上 checksum 不是 match 的 UPLOADED 封存文件同样留下，并记成 UPLOAD_FAILED，供已有的补传再校验。
// 没有目录时桶保留不生效，上传仍按本地天数把对象和本地文件一起删。
func (r *MySQLRunner) cleanupTaskBinlogs(ctx context.Context, task tasks.Task, dir, activeFileName, sourceServerUUID string, now time.Time) error {
	localDays := task.Storage.EffectiveLocalRetentionDays()
	bucketDays := localDays
	if r.objectDeleter != nil && r.fileMetaStore != nil {
		bucketDays = task.Storage.EffectiveBucketRetentionDays()
		if bucketDays < localDays {
			bucketDays = localDays
		}
	}
	var kept []string
	err := purgeExpiredBinlogs(ctx, dir, localDays, bucketDays, now, activeFileName, func() (*retentionObjects, error) {
		if r.objectDeleter == nil {
			return nil, nil
		}
		objects, err := r.retentionObjects(ctx, task, sourceServerUUID)
		if err != nil || objects == nil {
			return objects, err
		}
		objects.noteKept = func(name string) {
			kept = append(kept, name)
		}
		objects.announce = func(name, uploadState string) {
			r.announceRetentionSkip(ctx, task.ID, name, uploadState)
		}
		return objects, nil
	})
	if err != nil {
		return err
	}
	// Upload without a catalog still deletes the local file. Only the catalog
	// path can tell that the sealed file never reached the bucket.
	if r.objectDeleter != nil && r.fileMetaStore != nil {
		r.setRetentionBlocked(task.ID, kept)
	}
	return nil
}

func (r *MySQLRunner) retentionObjects(ctx context.Context, task tasks.Task, sourceServerUUID string) (*retentionObjects, error) {
	objects := &retentionObjects{deleter: r.objectDeleter}
	if r.fileMetaStore == nil {
		objects.standaloneKey = func(name string) string {
			if strings.TrimSpace(task.ClusterKey) == "" || strings.TrimSpace(sourceServerUUID) == "" {
				return ""
			}
			return buildObjectKey(r.uploadPrefix, task.ClusterKey, sourceServerUUID, name)
		}
		return objects, nil
	}
	catalog, ok := r.fileMetaStore.(expiredFileCatalog)
	if !ok {
		return nil, fmt.Errorf("%s: binlog catalog cannot delete retained objects", objectPurgeFailed)
	}
	files, err := catalog.ListBinlogFiles(ctx, task.ID, retentionCatalogLimit)
	if err != nil {
		return nil, fmt.Errorf("%s: list binlog files: %w", objectPurgeFailed, err)
	}
	objects.byName = make(map[string]tasks.BinlogFile, len(files))
	for _, file := range files {
		objects.byName[catalogSegmentName(file)] = file
	}
	objects.drop = func(row tasks.BinlogFile) error {
		return catalog.DeleteBinlogFile(ctx, task.ID, row.FileName, row.Epoch)
	}
	objects.note = func(row tasks.BinlogFile) error {
		return r.fileMetaStore.UpsertBinlogFile(ctx, row)
	}
	return objects, nil
}

// purgeExpiredBinlogs removes sealed files older than retention.
// The active open file and every other open segment stay.
// bucketDays equal to localDays is the single cutoff: load runs only after an
// expired sealed file is found, so a quiet directory does not read the catalog.
// bucketDays greater than localDays removes an uploaded local file that is
// still inside the bucket window without deleting the object or the catalog
// row, and also purges catalog rows whose local file is already gone once
// sealed_at (or uploaded_at) is past the bucket window.
// A nil retentionObjects deletes local files only.
func purgeExpiredBinlogs(ctx context.Context, dir string, localDays, bucketDays int, now time.Time, activeFileName string, load func() (*retentionObjects, error)) error {
	if localDays <= 0 {
		localDays = 7
	}
	if bucketDays <= 0 || bucketDays < localDays {
		bucketDays = localDays
	}
	if bucketDays == localDays {
		return purgeExpiredAt(ctx, dir, localDays, now, activeFileName, load)
	}
	return purgeSplitRetention(ctx, dir, localDays, bucketDays, now, activeFileName, load)
}

func purgeExpiredAt(ctx context.Context, dir string, retentionDays int, now time.Time, activeFileName string, load func() (*retentionObjects, error)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	expireBefore := now.Add(-time.Duration(retentionDays) * 24 * time.Hour)
	var objects *retentionObjects
	loaded := false
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == activeFileName || isOpenSegmentName(name) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.ModTime().Before(expireBefore) {
			continue
		}
		if !loaded {
			loaded = true
			if load != nil {
				objects, err = load()
				if err != nil {
					return err
				}
			}
		}
		removeLocal := true
		if objects != nil && objects.deleter != nil {
			removeLocal, err = objects.release(ctx, name, false, true)
			if err != nil {
				return err
			}
		}
		if !removeLocal {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// purgeSplitRetention keeps an uploaded object that is still inside the bucket
// window and deletes only its local file. A file still on disk is aged by
// mtime for both cutoffs. A catalog row with no local file is aged by
// sealed_at, then uploaded_at. A row with neither timestamp stays.
func purgeSplitRetention(ctx context.Context, dir string, localDays, bucketDays int, now time.Time, activeFileName string, load func() (*retentionObjects, error)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var objects *retentionObjects
	if load != nil {
		objects, err = load()
		if err != nil {
			return err
		}
	}
	expireLocal := now.Add(-time.Duration(localDays) * 24 * time.Hour)
	expireBucket := now.Add(-time.Duration(bucketDays) * 24 * time.Hour)
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		seen[name] = struct{}{}
		if name == activeFileName || isOpenSegmentName(name) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mtime := info.ModTime()
		if !mtime.Before(expireLocal) {
			continue
		}
		removeLocal := true
		if objects != nil && objects.deleter != nil {
			removeLocal, err = objects.release(ctx, name, !mtime.Before(expireBucket), true)
			if err != nil {
				return err
			}
		}
		if !removeLocal {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if objects == nil {
		return nil
	}
	for name, row := range objects.byName {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, ok := seen[name]; ok || isOpenCatalogRow(row) || !isUploadedRow(row) {
			continue
		}
		age := row.SealedAt
		if age.IsZero() {
			age = row.UploadedAt
		}
		if age.IsZero() || !age.Before(expireBucket) {
			continue
		}
		if _, err := objects.release(ctx, name, false, false); err != nil {
			return err
		}
	}
	return nil
}

// release deletes the bucket object for one expired sealed file before the
// local file is removed. localOnly leaves a verified uploaded object and its
// catalog row in place and still removes the local file. onDisk is true when
// that local file is still in the task directory. removeLocal is false when
// the file must stay so the next purge can retry. An open catalog row is not
// deleted. A sealed UPLOAD_FAILED or LOCAL_ONLY catalog row stays too: that
// local file is the only copy, and keeping it is not an error. An on-disk
// UPLOADED row whose checksum is not match stays too, and is recorded as
// UPLOAD_FAILED so the existing upload retry can check or upload it again.
// A bucket-only UPLOADED row is still deleted on the old rule.
func (o *retentionObjects) release(ctx context.Context, name string, localOnly, onDisk bool) (bool, error) {
	if o == nil || o.deleter == nil {
		return true, nil
	}
	row, ok := o.byName[name]
	if ok && isOpenCatalogRow(row) {
		return false, nil
	}
	if ok && onDisk && unverifiedSealedUpload(row) {
		recorded := markUnverifiedUploadFailed(row)
		if o.note != nil {
			if err := o.note(recorded); err != nil {
				log.Printf("retention: record unverified upload %s: %v", name, err)
			} else {
				row = recorded
				o.byName[name] = recorded
			}
		} else {
			row = recorded
			o.byName[name] = recorded
		}
	}
	if ok {
		state, keep := unuploadedSealedState(row)
		if !keep && onDisk && unverifiedSealedUpload(row) {
			keep = true
			state = strings.TrimSpace(row.UploadState)
		}
		if keep {
			if o.noteKept != nil {
				o.noteKept(name)
			}
			if o.announce != nil {
				o.announce(name, state)
			}
			return false, nil
		}
	}
	if localOnly && ok && verifiedUploaded(row) {
		return true, nil
	}
	key := ""
	if ok && isUploadedRow(row) {
		key = strings.TrimSpace(row.ObjectKey)
	} else if !ok && o.standaloneKey != nil {
		key = strings.TrimSpace(o.standaloneKey(name))
	}
	if key == "" {
		return true, nil
	}
	if err := o.deleter.DeleteObject(ctx, key); err != nil {
		if ok && o.note != nil {
			recorded := row
			recorded.UploadError = err.Error()
			if recErr := o.note(recorded); recErr != nil {
				return false, fmt.Errorf("%s: %s: %w (also failed to record purge error: %v)", objectPurgeFailed, name, err, recErr)
			}
		}
		return false, fmt.Errorf("%s: %s: %w", objectPurgeFailed, name, err)
	}
	if ok && o.drop != nil {
		if err := o.drop(row); err != nil {
			return false, fmt.Errorf("%s: %s: remove catalog row: %w", objectPurgeFailed, name, err)
		}
	}
	return true, nil
}

func isOpenSegmentName(name string) bool {
	return strings.Contains(name, ".open.e")
}

func isOpenCatalogRow(row tasks.BinlogFile) bool {
	if strings.EqualFold(strings.TrimSpace(row.State), "OPEN") {
		return true
	}
	return isOpenSegmentName(row.FileName) || isOpenSegmentName(filepath.Base(row.FilePath))
}

func isUploadedRow(row tasks.BinlogFile) bool {
	return strings.EqualFold(strings.TrimSpace(row.UploadState), "UPLOADED") && strings.TrimSpace(row.ObjectKey) != ""
}

func verifiedUploaded(row tasks.BinlogFile) bool {
	return isUploadedRow(row) && strings.EqualFold(strings.TrimSpace(row.Checksum), tasks.ChecksumMatch)
}

// unverifiedSealedUpload is an on-disk UPLOADED object that was not compared
// equal to the sealed file. Retention must not delete that local file.
func unverifiedSealedUpload(row tasks.BinlogFile) bool {
	if isOpenCatalogRow(row) || !isUploadedRow(row) {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(row.Checksum), tasks.ChecksumMatch)
}

// markUnverifiedUploadFailed records the row so the existing UPLOAD_FAILED
// retry can upload a mismatch again or check an unfinished checksum again.
func markUnverifiedUploadFailed(row tasks.BinlogFile) tasks.BinlogFile {
	row.UploadState = "UPLOAD_FAILED"
	if strings.EqualFold(strings.TrimSpace(row.Checksum), tasks.ChecksumMismatch) {
		row.UploadError = tasks.ChecksumMismatchError
		return row
	}
	row.Checksum = ""
	if !strings.HasPrefix(strings.TrimSpace(row.UploadError), tasks.ChecksumVerifyPrefix) {
		row.UploadError = tasks.ChecksumVerifyPrefix + "not verified"
	}
	return row
}

// unuploadedSealedState reports the catalog upload_state when this sealed row
// must stay on disk. UPLOADED, including a row with an empty object key, is
// not in this set. Open rows are not in this set.
func unuploadedSealedState(row tasks.BinlogFile) (string, bool) {
	if isOpenCatalogRow(row) {
		return "", false
	}
	state := strings.TrimSpace(row.UploadState)
	switch strings.ToUpper(state) {
	case "UPLOAD_FAILED", "LOCAL_ONLY":
		return state, true
	default:
		return "", false
	}
}

func (r *MySQLRunner) announceRetentionSkip(ctx context.Context, taskID, name, uploadState string) {
	if r == nil || taskID == "" || name == "" {
		return
	}
	key := taskID + "\x00" + name
	r.retentionMu.Lock()
	if _, ok := r.retentionSkipNoted[key]; ok {
		r.retentionMu.Unlock()
		return
	}
	r.retentionMu.Unlock()

	writer, ok := r.fileMetaStore.(interface {
		AppendEvent(context.Context, tasks.TaskEvent) error
	})
	if !ok {
		return
	}
	if err := writer.AppendEvent(ctx, tasks.TaskEvent{
		TaskID:  taskID,
		Type:    retentionSkippedNotUploaded,
		Message: name + " upload_state=" + uploadState,
		Detail:  uploadState,
		Time:    time.Now(),
	}); err != nil {
		return
	}
	r.retentionMu.Lock()
	if r.retentionSkipNoted == nil {
		r.retentionSkipNoted = map[string]struct{}{}
	}
	r.retentionSkipNoted[key] = struct{}{}
	r.retentionMu.Unlock()
}

func (r *MySQLRunner) setRetentionBlocked(taskID string, names []string) {
	if r == nil || taskID == "" {
		return
	}
	r.retentionMu.Lock()
	defer r.retentionMu.Unlock()
	if r.retentionBlocked == nil {
		r.retentionBlocked = map[string]int{}
	}
	r.retentionBlocked[taskID] = len(names)
}

// RetentionBlockedFiles is the current count of expired sealed files this
// process is keeping because they are not uploaded. The count drops when a
// later retention pass purges them. A process that has not run that pass
// reports no entry for the task.
func (r *MySQLRunner) RetentionBlockedFiles() map[string]int {
	if r == nil {
		return map[string]int{}
	}
	r.retentionMu.Lock()
	defer r.retentionMu.Unlock()
	out := make(map[string]int, len(r.retentionBlocked))
	for id, n := range r.retentionBlocked {
		out[id] = n
	}
	return out
}

// cleanupStaleOpenFiles 清理旧 epoch 遗留的 .open.e* 文件。
func cleanupStaleOpenFiles(dir string, currentEpoch int64) error {
	if currentEpoch <= 0 {
		return nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		idx := strings.LastIndex(name, ".open.e")
		if idx <= 0 {
			continue
		}
		epochText := name[idx+len(".open.e"):]
		epoch, err := strconv.ParseInt(epochText, 10, 64)
		if err != nil {
			continue
		}
		if epoch == currentEpoch {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// buildObjectKey 生成上传对象路径（prefix/cluster_key/server_uuid/file_name）。
func buildObjectKey(prefix, clusterKey, sourceServerUUID, fileName string) string {
	return tasks.ObjectKey(prefix, clusterKey, sourceServerUUID, fileName)
}

// openFileName 为 open 状态文件添加 epoch 后缀。
func openFileName(sourceFile string, epoch int64) string {
	if epoch <= 0 {
		return sourceFile
	}
	return fmt.Sprintf("%s.open.e%d", sourceFile, epoch)
}

// catalogEpoch is the binlog_files.epoch for this path. A plain name sealed
// while the task epoch is positive keeps that epoch so it does not replace an
// older epoch-0 row. A .open.eN or .sealed.eN name uses N.
func catalogEpoch(path string, taskEpoch int64) int64 {
	named, ok := binlog.ClassifySegment(filepath.Base(path))
	if ok && named.Epoch >= 0 {
		return named.Epoch
	}
	if taskEpoch > 0 {
		return taskEpoch
	}
	return 0
}

// catalogSegmentName is the retention and enroll key. It is the on-disk
// basename, so a sealed epoch and a later open epoch of the same source name
// stay distinct. A row with no path falls back to file name plus epoch.
func catalogSegmentName(file tasks.BinlogFile) string {
	base := filepath.Base(strings.TrimSpace(file.FilePath))
	switch base {
	case "", ".", "..":
		if file.Epoch != 0 {
			return fmt.Sprintf("%s#%d", file.FileName, file.Epoch)
		}
		return file.FileName
	default:
		return base
	}
}

// sealTarget picks the sealed basename. The first seal of a source name keeps
// the plain name, which is the object key older releases already uploaded.
// A later epoch uses name.sealed.e<epoch> when that plain file is present or
// any other catalog row for the source name already exists.
func (r *MySQLRunner) sealTarget(ctx context.Context, task tasks.Task, dir, sourceFile string) (string, error) {
	plain := filepath.Join(dir, sourceFile)
	if task.Epoch <= 0 {
		return plain, nil
	}
	info, err := os.Stat(plain)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	distinct := err == nil && info.Mode().IsRegular()
	if !distinct {
		files, listErr := r.listCatalog(ctx, task.ID)
		if listErr != nil {
			return "", listErr
		}
		for _, row := range files {
			if row.FileName == sourceFile && row.Epoch != task.Epoch {
				distinct = true
				break
			}
		}
	}
	if !distinct {
		return plain, nil
	}
	return filepath.Join(dir, fmt.Sprintf("%s.sealed.e%d", sourceFile, task.Epoch)), nil
}

// retireMissingLocalOpenRows drops OPEN rows in dir whose file is gone.
// Rows outside dir stay: another worker's segment is not this directory.
// Sealed rows stay, including a plain name uploaded under the historical key.
func (r *MySQLRunner) retireMissingLocalOpenRows(ctx context.Context, taskID, dir string) error {
	if r == nil || r.fileMetaStore == nil {
		return nil
	}
	deleter, ok := r.fileMetaStore.(interface {
		DeleteBinlogFile(context.Context, string, string, int64) error
	})
	if !ok {
		return nil
	}
	files, err := r.listCatalog(ctx, taskID)
	if err != nil {
		return err
	}
	dir = filepath.Clean(dir)
	for _, row := range files {
		if !strings.EqualFold(strings.TrimSpace(row.State), "OPEN") {
			continue
		}
		path := strings.TrimSpace(row.FilePath)
		if path == "" || filepath.Clean(filepath.Dir(path)) != dir {
			continue
		}
		info, statErr := os.Stat(path)
		if statErr == nil && info.Mode().IsRegular() {
			continue
		}
		if statErr != nil && !os.IsNotExist(statErr) {
			return statErr
		}
		if err := deleter.DeleteBinlogFile(ctx, taskID, row.FileName, row.Epoch); err != nil {
			return err
		}
	}
	return nil
}

// retireOtherOpenRows drops OPEN catalog rows for this source name at a
// different epoch. Sealed rows stay. A store that cannot delete is left as-is.
func (r *MySQLRunner) retireOtherOpenRows(ctx context.Context, taskID, source string, epoch int64) error {
	if r == nil || r.fileMetaStore == nil {
		return nil
	}
	deleter, ok := r.fileMetaStore.(interface {
		DeleteBinlogFile(context.Context, string, string, int64) error
	})
	if !ok {
		return nil
	}
	files, err := r.listCatalog(ctx, taskID)
	if err != nil {
		return err
	}
	for _, row := range files {
		if row.FileName != source || row.Epoch == epoch || !isOpenCatalogRow(row) {
			continue
		}
		if err := deleter.DeleteBinlogFile(ctx, taskID, row.FileName, row.Epoch); err != nil {
			return err
		}
	}
	return nil
}

// sealPath 把 .open.e<epoch> 文件映射到 sealed 文件路径，并返回源文件名。
func sealPath(localPath string, epoch int64) (string, string, error) {
	base := filepath.Base(localPath)
	if epoch <= 0 {
		return localPath, base, nil
	}

	expectedSuffix := fmt.Sprintf(".open.e%d", epoch)
	sourceFile := strings.TrimSuffix(base, expectedSuffix)
	if sourceFile == base {
		return "", "", fmt.Errorf("open file %s does not match epoch %d", base, epoch)
	}
	return filepath.Join(filepath.Dir(localPath), sourceFile), sourceFile, nil
}

// sourceEventTime 提取 binlog 事件头时间戳（UTC）。
func sourceEventTime(event *replication.BinlogEvent) time.Time {
	if event == nil || event.Header == nil || event.Header.Timestamp == 0 {
		return time.Time{}
	}
	return time.Unix(int64(event.Header.Timestamp), 0).UTC()
}
