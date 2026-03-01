// input: source replication config, task state, checkpoint/file store dependencies
// output: replication run control, local binlog artifacts, and upload/recovery signals
// pos: data-plane runtime that consumes MySQL binlog stream and emits durable outputs
// note: if this file changes, update this header and module AGENTS.md.
package replication

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
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
	uploader         FileUploader
	uploadPrefix     string
	leaseVerifier    LeaseVerifier
	progressReporter ProgressReporter
}

type sourceMetaFetcher interface {
	MasterStatusFetcher
	FetchServerUUID(ctx context.Context, source tasks.SourceConfig) (string, error)
}

type eventHandlerFunc func(*replication.BinlogEvent) error

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

// FileUploader 定义对象存储上传接口。
type FileUploader interface {
	// UploadFile 上传 sealed 文件。
	UploadFile(ctx context.Context, taskID, localPath, objectKey string) error
}

// ProgressReporter 定义复制进度上报接口。
type ProgressReporter interface {
	// ReportReplicationProgress 上报复制进度。
	ReportReplicationProgress(taskID string, sourceEventAt time.Time, file string, pos uint32)
}

// LeaseVerifier 定义 cluster 下 lease ownership 校验接口。
type LeaseVerifier interface {
	// VerifyLease 校验当前任务 lease/epoch 是否仍有效。
	VerifyLease(ctx context.Context, task tasks.Task) (bool, error)
}

type leaseVerifierFunc func(context.Context, tasks.Task) (bool, error)

// VerifyLease 让函数类型实现 LeaseVerifier 接口。
func (f leaseVerifierFunc) VerifyLease(ctx context.Context, task tasks.Task) (bool, error) {
	return f(ctx, task)
}

// WithUploader 注入对象存储上传器及 object key prefix。
func WithUploader(uploader FileUploader, prefix string) RunnerOption {
	return func(r *MySQLRunner) {
		r.uploader = uploader
		r.uploadPrefix = prefix
	}
}

// WithLeaseVerifier 注入 lease 校验器（cluster 安全边界）。
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
	// RUNNING 的含义是执行链路 ready，而不是延迟一定为 0。
	// source_server_uuid 作为 object key 的稳定维度，避免 cluster_key 相同但源实例切换时冲突。
	sourceServerUUID, err := r.fetcher.FetchServerUUID(ctx, task.Source)
	if err != nil {
		return err
	}
	sourceServerUUID = strings.TrimSpace(sourceServerUUID)
	if sourceServerUUID == "" {
		return errors.New("empty source server_uuid")
	}

	// 先解析请求的 start strategy（LATEST/FILE_POS/GTID）。
	start, err := ResolveStart(ctx, task, r.fetcher)
	if err != nil {
		return err
	}
	if r.checkpointStore != nil {
		// 持久化 checkpoint 优先级更高，保证重启后的 resumability。
		checkpoint, ok, err := r.checkpointStore.LoadCheckpoint(ctx, task.ID)
		if err != nil {
			return err
		}
		start, _ = effectiveStartForTakeover(task, start, checkpoint, ok)
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
	file, writer, currentPath, err := r.openBinlogWriter(task, currentFile, currentPos)
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
		return nil
	}

	// Step 3: 定义统一事件处理逻辑（异步/半同步共用）。
	// 单条事件处理逻辑：异步模式（GetEvent）与半同步模式（SynchronousEventHandler）共用。
	handleEvent := func(event *replication.BinlogEvent) error {
		if event == nil || event.Header == nil {
			return nil
		}
		sourceEventAt := sourceEventTime(event)

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
				file, writer, currentPath, err = r.openBinlogWriter(task, currentFile, currentPos)
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
					r.progressReporter.ReportReplicationProgress(task.ID, sourceEventAt, currentFile, currentPos)
				}
				return nil
			}
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
			r.progressReporter.ReportReplicationProgress(task.ID, sourceEventAt, currentFile, currentPos)
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
	syncer := replication.NewBinlogSyncer(cfg)
	defer syncer.Close()

	var streamer *replication.BinlogStreamer
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
		return err
	}

	if onReady != nil {
		// 到这里说明“连接 + 起点 + writer”都已 ready，Scheduler 可安全切 RUNNING。
		onReady()
	}

	if semiSyncRequested {
		// SynchronousEventHandler 模式下，事件由 syncer 内部 goroutine 推送到 handler。
		// 这里阻塞等待错误或取消，保持任务生命周期。
		for {
			_, err := streamer.GetEvent(ctx)
			if err != nil {
				if ctx.Err() != nil || errors.Is(err, context.Canceled) {
					return nil
				}
				return err
			}
		}
	}

	// Step 5: 异步模式主循环（逐条拉取并处理事件）。
	for {
		event, err := streamer.GetEvent(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		if err := handleEvent(event); err != nil {
			return err
		}
	}
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
		// cluster 下 seal/upload 前再次校验 lease/epoch，防止失租后继续发布文件。
		ok, err := r.leaseVerifier.VerifyLease(ctx, task)
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

	if r.uploader == nil {
		return nil
	}
	if strings.TrimSpace(task.ClusterKey) == "" {
		return errors.New("cluster_key is required")
	}
	sourceServerUUID = strings.TrimSpace(sourceServerUUID)
	if sourceServerUUID == "" {
		return errors.New("source server_uuid is required")
	}
	// Step 4: best-effort 上传；失败仅记录 UPLOAD_FAILED，不中断主链路。
	objectKey := buildObjectKey(r.uploadPrefix, task.ClusterKey, sourceServerUUID, sourceFile)
	if err := r.uploader.UploadFile(ctx, task.ID, sealedPath, objectKey); err != nil {
		if fileMeta != nil {
			fileMeta.ObjectKey = objectKey
			fileMeta.UploadState = "UPLOAD_FAILED"
			fileMeta.UploadError = err.Error()
			if saveErr := r.fileMetaStore.UpsertBinlogFile(ctx, *fileMeta); saveErr != nil {
				return saveErr
			}
		}
		// best-effort policy：upload 失败只落失败元数据，不打断拉流主链路。
		return nil
	}

	if fileMeta != nil {
		fileMeta.UploadState = "UPLOADED"
		fileMeta.ObjectKey = objectKey
		fileMeta.UploadError = ""
		fileMeta.UploadedAt = time.Now()
		if err := r.fileMetaStore.UpsertBinlogFile(ctx, *fileMeta); err != nil {
			return err
		}
	}
	return nil
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
func (r *MySQLRunner) openBinlogWriter(task tasks.Task, fileName string, initialPos uint32) (*os.File, *binlog.Writer, string, error) {
	// Step 1: 准备目录并清理 stale open / 过期文件。
	dir := filepath.Join(r.dataDir, task.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, "", err
	}
	if err := cleanupStaleOpenFiles(dir, task.Epoch); err != nil {
		return nil, nil, "", err
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

	// Step 3: 返回带初始 checkpoint 的 writer。
	writer := binlog.NewWriter(f, binlog.Checkpoint{
		File: fileName,
		Pos:  initialPos,
	})
	return f, writer, path, nil
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
		return MasterStatus{}, err
	}
	defer conn.Close()

	result, err := conn.Execute("SHOW MASTER STATUS")
	if err != nil {
		return MasterStatus{}, err
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

// FetchServerUUID 读取源库 server_uuid（用于 object key 维度）。
func (f *mysqlStatusFetcher) FetchServerUUID(_ context.Context, source tasks.SourceConfig) (string, error) {
	addr := fmt.Sprintf("%s:%d", source.Host, source.Port)
	conn, err := sqlclient.Connect(addr, source.User, source.Password, "")
	if err != nil {
		return "", err
	}
	defer conn.Close()

	result, err := conn.Execute("SHOW VARIABLES LIKE 'server_uuid'")
	if err != nil {
		return "", err
	}
	if result == nil || result.Resultset == nil || result.Resultset.RowNumber() == 0 {
		return "", errors.New("empty server_uuid")
	}

	value, err := result.GetString(0, 1)
	if err != nil {
		return "", err
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("empty server_uuid")
	}
	return value, nil
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
	// 注意不要用 filepath.Join：
	// object key 是对象存储逻辑路径，不应被本地路径规则（clean/绝对路径）影响。
	parts := make([]string, 0, 4)
	if p := strings.Trim(strings.TrimSpace(prefix), "/"); p != "" {
		parts = append(parts, p)
	}
	parts = append(parts,
		strings.Trim(strings.TrimSpace(clusterKey), "/"),
		strings.Trim(strings.TrimSpace(sourceServerUUID), "/"),
		strings.Trim(strings.TrimSpace(fileName), "/"),
	)
	return strings.Join(parts, "/")
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
