// Package replication provides module-level functionality for replication.
// input: source replication config, flavor-aware identity, checkpoint/file metadata store dependencies
// output: replication run control, observable OPEN/SEALED artifacts, at-tip as soon as dump file/pos matches master (fresh LATEST or FILE_POS already there), dump preambles excluded from delay, idle at-tip only when dump matches master file/pos, stop/start resume from the last durable event in the local open segment (not SHOW MASTER STATUS and not position 4) while keeping those bytes, sealed-file handoff for upload, permanent source errors including the MariaDB flavor hint when @@server_uuid is missing, and adopted leftover directories that keep existing segments while opening the next epoch
// pos: data-plane runtime that consumes MySQL/MariaDB binlog stream and emits durable outputs
// note: if this file changes, update this header and module README.md.
package replication

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"

	sqlclient "github.com/go-mysql-org/go-mysql/client"
	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
)

var binlogMagic = []byte{0xfe, 'b', 'i', 'n'}
var ErrLeaseEpochMismatch = errors.New("lease/epoch mismatch")

const (
	defaultServerIDBase uint32 = 200000
	defaultServerIDMod  uint32 = 1000000
)

// MySQLRunner 负责执行复制协议拉流、文件落盘、checkpoint 与上传流程。
type MySQLRunner struct {
	dataDir          string
	fetcher          sourceMetaFetcher
	checkpointStore  CheckpointStore
	fileMetaStore    FileMetaStore
	uploadPrefix     string
	leaseVerifier    LeaseVerifier
	sealedHandler    func(context.Context, tasks.BinlogFile) error
	progressReporter ProgressReporter
	newSyncer        func(replication.BinlogSyncerConfig) binlogSyncer
	writerOpener     func(task tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error)
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
		r.sealedHandler = func(ctx context.Context, file tasks.BinlogFile) error {
			_, err := tasks.ApplySealedUpload(ctx, uploader, r.fileMetaStore, file)
			return err
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
	return r.run(ctx, task, nil)
}

// RunWithNotify 在内部 ready 后触发回调，供 Scheduler 精准切 RUNNING。
func (r *MySQLRunner) RunWithNotify(ctx context.Context, task tasks.Task, onReady func()) error {
	// 提供给 Scheduler 的增强接口：runner ready 时主动回调。
	return r.run(ctx, task, onReady)
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
	checkpointExists := false
	if r.checkpointStore != nil {
		// 持久化 checkpoint 优先级更高，保证重启后的 resumability。
		checkpoint, ok, err := r.checkpointStore.LoadCheckpoint(ctx, task.ID)
		if err != nil {
			return err
		}
		start, _ = effectiveStartForTakeover(task, start, checkpoint, ok)
		checkpointExists = ok
	}
	// Fresh LATEST has already resolved to SHOW MASTER STATUS, so StartSync is at tip.
	// A checkpoint means this run may still be catching up.
	atTip := requestedLatest && !checkpointExists
	// Stop leaves the open segment on disk and the next start gets a new epoch.
	// Without this, standalone LATEST jumps to the current master (a hole) and an
	// epoch above 1 rewinds to position 4 and deletes the segment (a re-dump).
	// Adopt keeps its own FILE_POS and must not take this path.
	if !task.KeepLocalSegments && strings.TrimSpace(r.dataDir) != "" {
		if resume, ok := localDurableResume(r.dataDir, task.ID); ok {
			start = resume
			atTip = false
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
			return r.openBinlogWriter(ctx, task, fileName, initialPos)
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
				return err
			}
		}
		if r.fileMetaStore != nil {
			info, err := os.Stat(currentPath)
			if err != nil {
				return err
			}
			if err := r.fileMetaStore.UpsertBinlogFile(ctx, tasks.BinlogFile{
				TaskID:      task.ID,
				FileName:    currentFile,
				FilePath:    currentPath,
				State:       "OPEN",
				SizeBytes:   info.Size(),
				StartPos:    currentStartPos,
				EndPos:      checkpoint.Pos,
				CreatedAt:   currentCreatedAt,
				UploadState: "LOCAL_ONLY",
			}); err != nil {
				return err
			}
		}
		return nil
	}

	// Step 3: 定义统一事件处理逻辑（异步/半同步共用）。
	// 单条事件处理逻辑：异步模式（GetEvent）与半同步模式（SynchronousEventHandler）共用。
	handleEvent := func(event *replication.BinlogEvent) error {
		if event == nil || event.Header == nil {
			return nil
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
				rotateCheckpoint := binlog.Checkpoint{
					File: currentFile,
					Pos:  event.Header.LogPos,
				}
				if rotateCheckpoint.Pos == 0 {
					rotateCheckpoint.Pos = currentPos
				}
				if err := appendAndPersist(event.RawData, rotateCheckpoint); err != nil {
					return err
				}

				if err := file.Close(); err != nil {
					return err
				}
				file = nil
				if err := r.finalizeSealedFile(
					ctx,
					task,
					sourceServerUUID,
					currentPath,
					currentStartPos,
					rotateCheckpoint.Pos,
					currentCreatedAt,
					time.Now(),
				); err != nil {
					return err
				}

				currentFile = nextFile
				currentPos = nextPos
				file, writer, currentPath, err = writerOpener(task, currentFile, currentPos)
				if err != nil {
					return err
				}
				currentStartPos = currentPos
				currentCreatedAt = time.Now()

				// rotate 后立即把 checkpoint 切到新文件起点，保证重启从新文件继续。
				if r.checkpointStore != nil {
					if err := r.checkpointStore.UpsertCheckpoint(ctx, task.ID, binlog.Checkpoint{
						File: currentFile,
						Pos:  currentPos,
					}); err != nil {
						return err
					}
				}
				if r.progressReporter != nil {
					r.progressReporter.ReportReplicationProgress(task.ID, sourceEventAt, currentFile, currentPos, atTip)
				}
				return nil
			}
		}

		// 从文件中部 dump 时，源库仍会先下发文件头的 format description（log_pos 置 0，
		// 或保留原始 end_log_pos，例如 MySQL 8 的 126）。该事件不在当前位点之后。
		// 写入会把 open 段撑成“只有文件头”，并用创建时间报 DELAYED；end_log_pos 还会把位点回拨。
		// synthetic rotate 已在上面处理，这里不能抢在它前面把 log_pos=0 丢掉。
		if event.Header.LogPos <= currentPos {
			return nil
		}

		next := binlog.Checkpoint{
			File: currentFile,
			Pos:  event.Header.LogPos,
		}
		if next.Pos == 0 {
			next.Pos = currentPos
		}

		if err := appendAndPersist(event.RawData, next); err != nil {
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
	defer syncer.Close()

	var streamer binlogStreamer
	switch start.Mode {
	case tasks.StartModeFilePos:
		streamer, err = syncer.StartSync(gomysql.Position{Name: start.File, Pos: start.Pos})
	case tasks.StartModeGTID:
		set, parseErr := gomysql.ParseGTIDSet(cfg.Flavor, start.GTIDSet)
		if parseErr != nil {
			return parseErr
		}
		streamer, err = syncer.StartSyncGTID(set)
	default:
		return fmt.Errorf("unsupported resolved start mode: %s", start.Mode)
	}
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

	if semiSyncRequested {
		// SynchronousEventHandler 模式下，事件由 syncer 内部 goroutine 推送到 handler。
		// 这里阻塞等待错误或取消，保持任务生命周期。
		for {
			event, err := r.nextEvent(ctx, streamer, task.ID, task.Source, currentFile, currentPos, &atTip)
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
		event, err := r.nextEvent(ctx, streamer, task.ID, task.Source, currentFile, currentPos, &atTip)
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

// finalizeSealedFile 负责 open 文件 seal、元数据落库以及 best-effort 上传。
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
	// 常见误解：
	// 1) “上传失败就应该报错退出”不符合本项目策略；这里是 best-effort，失败仅记元数据。
	// 2) “seal 只是改文件名”不完整；cluster 下 seal/upload 前必须再校验 lease ownership。
	// Step 1: cluster 下先做 ownership 校验，防止失租后继续发布文件。
	if task.Epoch > 0 && r.leaseVerifier != nil {
		ok, err := r.leaseVerifier.Verify(ctx, task.ID, task.OwnerWorkerID, task.Epoch)
		if err != nil {
			return err
		}
		if !ok {
			return ErrLeaseEpochMismatch
		}
	}

	// Step 2: open 文件改名为 sealed 文件（并防止覆盖已有 sealed 文件）。
	sealedPath, sourceFile, err := sealPath(localPath, task.Epoch)
	if err != nil {
		return err
	}
	if localPath != sealedPath {
		if _, err := os.Stat(sealedPath); err == nil {
			return fmt.Errorf("sealed file already exists: %s", sealedPath)
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.Rename(localPath, sealedPath); err != nil {
			return err
		}
	}

	var fileMeta *tasks.BinlogFile

	// Step 3: 先写 LOCAL_ONLY 元数据，再进行 upload。
	if r.fileMetaStore != nil {
		// 先落本地 file metadata；upload state 初始为 LOCAL_ONLY。
		info, err := os.Stat(sealedPath)
		if err != nil {
			return err
		}
		meta := tasks.BinlogFile{
			TaskID:      task.ID,
			FileName:    sourceFile,
			FilePath:    sealedPath,
			State:       "SEALED",
			SizeBytes:   info.Size(),
			StartPos:    startPos,
			EndPos:      endPos,
			CreatedAt:   createdAt,
			SealedAt:    sealedAt,
			UploadState: "LOCAL_ONLY",
		}
		if err := r.fileMetaStore.UpsertBinlogFile(ctx, meta); err != nil {
			return err
		}
		fileMeta = &meta
	}

	if r.sealedHandler == nil {
		return nil
	}
	if strings.TrimSpace(task.ClusterKey) == "" {
		return errors.New("cluster_key is required")
	}
	sourceServerUUID = strings.TrimSpace(sourceServerUUID)
	if sourceServerUUID == "" {
		return errors.New("source server_uuid is required")
	}
	objectKey := buildObjectKey(r.uploadPrefix, task.ClusterKey, sourceServerUUID, sourceFile)
	if fileMeta == nil {
		fileMeta = &tasks.BinlogFile{
			TaskID:      task.ID,
			FileName:    sourceFile,
			FilePath:    sealedPath,
			State:       "SEALED",
			StartPos:    startPos,
			EndPos:      endPos,
			CreatedAt:   createdAt,
			SealedAt:    sealedAt,
			ObjectKey:   objectKey,
			UploadState: "LOCAL_ONLY",
		}
	} else {
		fileMeta.ObjectKey = objectKey
	}
	return r.sealedHandler(ctx, *fileMeta)
}

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
		ServerID:        serverID,
		Flavor:          flavor,
		Host:            task.Source.Host,
		Port:            task.Source.Port,
		User:            task.Source.User,
		Password:        task.Source.Password,
		RawModeEnabled:  true,
		SemiSyncEnabled: task.Source.SemiSync,
	}
}

// openBinlogWriter 打开（或创建）本地 open 文件并返回带初始 checkpoint 的 writer。
func (r *MySQLRunner) openBinlogWriter(ctx context.Context, task tasks.Task, fileName string, initialPos uint32) (*os.File, *binlog.Writer, string, error) {
	// Step 1: 准备目录并清理 stale open / 过期文件。
	dir := filepath.Join(r.dataDir, task.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, "", err
	}
	// An adopted leftover directory keeps sealed and .open.e* segments.
	// A normal resume renames the open segment onto this epoch when its last
	// event ends at initialPos, then drops any other epoch. A new worker with
	// no local segment still starts clean and does not rename anything.
	if !task.KeepLocalSegments {
		if err := continueDurableOpenSegment(dir, fileName, task.Epoch, initialPos); err != nil {
			return nil, nil, "", err
		}
		if err := cleanupStaleOpenFiles(dir, task.Epoch); err != nil {
			return nil, nil, "", err
		}
	}
	localFileName := openFileName(fileName, task.Epoch)
	if err := cleanupExpiredBinlogs(dir, task.Storage.RetentionDays, time.Now(), localFileName); err != nil {
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
		createdAt := time.Now()
		if err := r.fileMetaStore.UpsertBinlogFile(ctx, tasks.BinlogFile{
			TaskID:      task.ID,
			FileName:    fileName,
			FilePath:    path,
			State:       "OPEN",
			SizeBytes:   info.Size(),
			StartPos:    initialPos,
			EndPos:      initialPos,
			CreatedAt:   createdAt,
			UploadState: "LOCAL_ONLY",
		}); err != nil {
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

// localDurableResume is FILE_POS at the last complete event in the highest
// open segment. File size is not that position when the dump started mid-file.
// A segment with no complete event is not a resume point.
func localDurableResume(dataDir, taskID string) (tasks.StartConfig, bool) {
	dir, ok := safeTaskDir(dataDir, taskID)
	if !ok {
		return tasks.StartConfig{}, false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return tasks.StartConfig{}, false
	}
	cands := make([]localSegment, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		seg, ok := classifyLocalSegment(entry.Name())
		if !ok || seg.epoch < 0 {
			continue
		}
		seg.path = filepath.Join(dir, entry.Name())
		cands = append(cands, seg)
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].seq != cands[j].seq {
			return cands[i].seq > cands[j].seq
		}
		return cands[i].epoch > cands[j].epoch
	})
	for _, seg := range cands {
		pos, _, _, ok := durableBinlogCursor(seg.path)
		if !ok {
			continue
		}
		return tasks.StartConfig{
			Mode: tasks.StartModeFilePos,
			File: seg.source,
			Pos:  pos,
		}, true
	}
	return tasks.StartConfig{}, false
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
		endPos, end, _, ok := durableBinlogCursor(currentPath)
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
		seg, ok := classifyLocalSegment(entry.Name())
		if !ok || seg.epoch < 0 || seg.source != fileName {
			continue
		}
		seg.path = filepath.Join(dir, entry.Name())
		endPos, end, _, ok := durableBinlogCursor(seg.path)
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

// durableBinlogCursor walks complete events. pos is the last event's end
// log_pos. end is the file offset of the first torn byte, or the file size
// when the segment ends on an event boundary.
func durableBinlogCursor(path string) (pos uint32, end int64, size int64, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, 0, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, 0, 0, false
	}
	size = info.Size()
	if size < 4 {
		return 0, 0, size, false
	}
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil || !bytes.Equal(magic, binlogMagic) {
		return 0, 0, size, false
	}
	offset := int64(4)
	hdr := make([]byte, replication.EventHeaderSize)
	var lastPos uint32
	var lastEnd int64
	found := false
	for offset+int64(replication.EventHeaderSize) <= size {
		if _, err := io.ReadFull(f, hdr); err != nil {
			break
		}
		eventSize := int64(binary.LittleEndian.Uint32(hdr[9:13]))
		logPos := binary.LittleEndian.Uint32(hdr[13:17])
		if eventSize < int64(replication.EventHeaderSize) || offset+eventSize > size {
			break
		}
		if _, err := f.Seek(eventSize-int64(replication.EventHeaderSize), io.SeekCurrent); err != nil {
			break
		}
		offset += eventSize
		lastPos = logPos
		lastEnd = offset
		found = true
	}
	if !found || lastPos == 0 {
		return 0, 0, size, false
	}
	return lastPos, lastEnd, size, true
}

func safeTaskDir(dataDir, taskID string) (string, bool) {
	dataDir = strings.TrimSpace(dataDir)
	taskID = strings.TrimSpace(taskID)
	if dataDir == "" || taskID == "" || taskID != filepath.Base(taskID) || strings.HasPrefix(taskID, ".") {
		return "", false
	}
	return filepath.Join(dataDir, taskID), true
}

// classifyLocalSegment matches tasks.classifyBinlogSegment. epoch -1 is a sealed name.
func classifyLocalSegment(name string) (localSegment, bool) {
	if name == "" || strings.HasPrefix(name, ".") {
		return localSegment{}, false
	}
	epoch := int64(-1)
	source := name
	const mark = ".open.e"
	if idx := strings.LastIndex(name, mark); idx > 0 {
		epochText := name[idx+len(mark):]
		if epochText == "" || strings.ContainsAny(epochText, "./\\") {
			return localSegment{}, false
		}
		n, err := strconv.ParseInt(epochText, 10, 64)
		if err != nil || n < 0 {
			return localSegment{}, false
		}
		source = name[:idx]
		epoch = n
	}
	dot := strings.LastIndex(source, ".")
	if dot <= 0 || dot == len(source)-1 {
		return localSegment{}, false
	}
	seq, err := strconv.ParseUint(source[dot+1:], 10, 64)
	if err != nil || source[:dot] == "" {
		return localSegment{}, false
	}
	return localSegment{source: source, seq: seq, epoch: epoch}, true
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

// effectiveStartForTakeover 在 worker 接管时把起点回拨到 file:4 并触发重建当前文件。
func effectiveStartForTakeover(task tasks.Task, start tasks.StartConfig, checkpoint binlog.Checkpoint, exists bool) (tasks.StartConfig, bool) {
	// 常见误解：
	// “接管后直接从 checkpoint.Pos 继续”可能导致当前文件缺头或断裂。
	// 接管场景回拨到 file:4 的目的，是在新 worker 上重建当前文件的完整单文件字节流。
	// Step 1: 先按 checkpoint 覆盖起点。
	effective := effectiveStartFromCheckpoint(start, checkpoint, exists)
	// Step 2: 非接管场景（epoch<=1）直接返回。
	if task.Epoch <= 1 {
		return effective, false
	}
	// Step 3: 接管场景仅在有效 FILE_POS 且 pos>4 时回拨到 4，重放当前文件保证单文件完整性。
	if effective.Mode != tasks.StartModeFilePos || effective.File == "" || effective.Pos <= 4 {
		return effective, false
	}
	return tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: effective.File,
		Pos:  4,
	}, true
}

// cleanupExpiredBinlogs 按保留天数清理过期本地文件（跳过当前活跃 open 文件）。
func cleanupExpiredBinlogs(dir string, retentionDays int, now time.Time, activeFileName string) error {
	if retentionDays <= 0 {
		retentionDays = 7
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	expireBefore := now.Add(-time.Duration(retentionDays) * 24 * time.Hour)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if entry.Name() == activeFileName {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().Before(expireBefore) {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
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
