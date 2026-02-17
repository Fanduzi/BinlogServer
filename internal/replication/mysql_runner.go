package replication

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
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

type MySQLRunner struct {
	dataDir          string
	fetcher          MasterStatusFetcher
	checkpointStore  CheckpointStore
	fileMetaStore    FileMetaStore
	uploader         FileUploader
	uploadPrefix     string
	leaseVerifier    LeaseVerifier
	progressReporter ProgressReporter
}

type eventHandlerFunc func(*replication.BinlogEvent) error

func (f eventHandlerFunc) HandleEvent(e *replication.BinlogEvent) error {
	return f(e)
}

type CheckpointStore interface {
	UpsertCheckpoint(ctx context.Context, taskID string, checkpoint binlog.Checkpoint) error
	LoadCheckpoint(ctx context.Context, taskID string) (binlog.Checkpoint, bool, error)
}

type RunnerOption func(*MySQLRunner)

func WithCheckpointStore(store CheckpointStore) RunnerOption {
	return func(r *MySQLRunner) {
		r.checkpointStore = store
	}
}

type FileMetaStore interface {
	UpsertBinlogFile(ctx context.Context, meta tasks.BinlogFile) error
}

func WithFileMetaStore(store FileMetaStore) RunnerOption {
	return func(r *MySQLRunner) {
		r.fileMetaStore = store
	}
}

type FileUploader interface {
	UploadFile(ctx context.Context, taskID, localPath, objectKey string) error
}

type ProgressReporter interface {
	ReportReplicationProgress(taskID string, sourceEventAt time.Time, file string, pos uint32)
}

type LeaseVerifier interface {
	VerifyLease(ctx context.Context, task tasks.Task) (bool, error)
}

type leaseVerifierFunc func(context.Context, tasks.Task) (bool, error)

func (f leaseVerifierFunc) VerifyLease(ctx context.Context, task tasks.Task) (bool, error) {
	return f(ctx, task)
}

func WithUploader(uploader FileUploader, prefix string) RunnerOption {
	return func(r *MySQLRunner) {
		r.uploader = uploader
		r.uploadPrefix = prefix
	}
}

func WithLeaseVerifier(verifier LeaseVerifier) RunnerOption {
	return func(r *MySQLRunner) {
		r.leaseVerifier = verifier
	}
}

func WithProgressReporter(reporter ProgressReporter) RunnerOption {
	return func(r *MySQLRunner) {
		r.progressReporter = reporter
	}
}

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

func (r *MySQLRunner) Run(ctx context.Context, task tasks.Task) error {
	// 兼容 Runner 基础接口：不携带 ready 回调。
	return r.run(ctx, task, nil)
}

func (r *MySQLRunner) RunWithNotify(ctx context.Context, task tasks.Task, onReady func()) error {
	// 提供给 Scheduler 的增强接口：runner ready 时主动回调。
	return r.run(ctx, task, onReady)
}

func (r *MySQLRunner) run(ctx context.Context, task tasks.Task, onReady func()) error {
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
			_ = file.Close()
		}
	}()

	appendAndPersist := func(raw []byte, next binlog.Checkpoint) error {
		if err := writer.Append(raw, next); err != nil {
			return err
		}
		// 只有 writer flush 成功后才推进 checkpoint。
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

	semiSyncRequested := task.Source.SemiSync
	cfg := buildSyncerConfig(task)
	if semiSyncRequested {
		// 半同步模式使用同步处理器：只有 HandleEvent 成功返回后才会 ACK。
		// 这样可保证 ACK 发生在本地 fsync/checkpoint 成功之后。
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
		// 到这里说明复制连接和本地 writer 都已就绪，可切换为 RUNNING。
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

func (r *MySQLRunner) finalizeSealedFile(
	ctx context.Context,
	task tasks.Task,
	localPath string,
	startPos uint32,
	endPos uint32,
	createdAt time.Time,
	sealedAt time.Time,
) error {
	if task.Epoch > 0 && r.leaseVerifier != nil {
		ok, err := r.leaseVerifier.VerifyLease(ctx, task)
		if err != nil {
			return err
		}
		if !ok {
			return ErrLeaseEpochMismatch
		}
	}

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
	objectKey := buildObjectKey(r.uploadPrefix, task.ID, sourceFile)
	if err := r.uploader.UploadFile(ctx, task.ID, sealedPath, objectKey); err != nil {
		if fileMeta != nil {
			fileMeta.UploadState = "UPLOAD_FAILED"
			fileMeta.UploadError = err.Error()
			if saveErr := r.fileMetaStore.UpsertBinlogFile(ctx, *fileMeta); saveErr != nil {
				return saveErr
			}
		}
		// best-effort policy：upload 失败也继续拉 binlog。
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

func buildSyncerConfig(task tasks.Task) replication.BinlogSyncerConfig {
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

func (r *MySQLRunner) openBinlogWriter(task tasks.Task, fileName string, initialPos uint32) (*os.File, *binlog.Writer, string, error) {
	dir := filepath.Join(r.dataDir, task.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, "", err
	}
	localFileName := openFileName(fileName, task.Epoch)
	if err := cleanupExpiredBinlogs(dir, task.Storage.RetentionDays, time.Now(), localFileName); err != nil {
		return nil, nil, "", err
	}

	path := filepath.Join(dir, localFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, "", err
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, "", err
	}
	if info.Size() == 0 {
		if _, err := f.Write(binlogMagic); err != nil {
			_ = f.Close()
			return nil, nil, "", err
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return nil, nil, "", err
		}
	}

	writer := binlog.NewWriter(f, binlog.Checkpoint{
		File: fileName,
		Pos:  initialPos,
	})
	return f, writer, path, nil
}

func defaultServerID(taskID string) uint32 {
	if n, err := strconv.ParseUint(taskID, 10, 32); err == nil {
		return uint32(200000 + n)
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(taskID))
	return 200000 + (h.Sum32() % 1000000)
}

type mysqlStatusFetcher struct{}

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

func effectiveStartFromCheckpoint(start tasks.StartConfig, checkpoint binlog.Checkpoint, exists bool) tasks.StartConfig {
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

func effectiveStartForTakeover(task tasks.Task, start tasks.StartConfig, checkpoint binlog.Checkpoint, exists bool) (tasks.StartConfig, bool) {
	effective := effectiveStartFromCheckpoint(start, checkpoint, exists)
	if task.Epoch <= 1 {
		return effective, false
	}
	if effective.Mode != tasks.StartModeFilePos || effective.File == "" || effective.Pos <= 4 {
		return effective, false
	}
	return tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: effective.File,
		Pos:  4,
	}, true
}

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

func buildObjectKey(prefix, taskID, fileName string) string {
	base := filepath.ToSlash(filepath.Join(taskID, fileName))
	if prefix == "" {
		return base
	}
	return filepath.ToSlash(filepath.Join(prefix, base))
}

func openFileName(sourceFile string, epoch int64) string {
	if epoch <= 0 {
		return sourceFile
	}
	return fmt.Sprintf("%s.open.e%d", sourceFile, epoch)
}

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

func sourceEventTime(event *replication.BinlogEvent) time.Time {
	if event == nil || event.Header == nil || event.Header.Timestamp == 0 {
		return time.Time{}
	}
	return time.Unix(int64(event.Header.Timestamp), 0).UTC()
}
