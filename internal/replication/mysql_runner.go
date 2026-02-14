package replication

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"

	sqlclient "github.com/go-mysql-org/go-mysql/client"
	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
)

var binlogMagic = []byte{0xfe, 'b', 'i', 'n'}

type MySQLRunner struct {
	dataDir         string
	fetcher         MasterStatusFetcher
	checkpointStore CheckpointStore
	fileMetaStore   FileMetaStore
	uploader        FileUploader
	uploadPrefix    string
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

func WithUploader(uploader FileUploader, prefix string) RunnerOption {
	return func(r *MySQLRunner) {
		r.uploader = uploader
		r.uploadPrefix = prefix
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
	start, err := ResolveStart(ctx, task, r.fetcher)
	if err != nil {
		return err
	}
	if r.checkpointStore != nil {
		checkpoint, ok, err := r.checkpointStore.LoadCheckpoint(ctx, task.ID)
		if err != nil {
			return err
		}
		start = effectiveStartFromCheckpoint(start, checkpoint, ok)
	}

	cfg := buildSyncerConfig(task)
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
	defer file.Close()

	for {
		event, err := streamer.GetEvent(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		if event == nil || event.Header == nil {
			continue
		}

		if event.Header.EventType == replication.ROTATE_EVENT {
			rotate, ok := event.Event.(*replication.RotateEvent)
			if ok && len(rotate.NextLogName) > 0 {
				currentFile = string(rotate.NextLogName)
				currentPos = uint32(rotate.Position)

				if err := file.Close(); err != nil {
					return err
				}
				var fileMeta *tasks.BinlogFile
				if r.fileMetaStore != nil {
					info, err := os.Stat(currentPath)
					if err != nil {
						return err
					}
					meta := tasks.BinlogFile{
						TaskID:      task.ID,
						FileName:    filepath.Base(currentPath),
						FilePath:    currentPath,
						SizeBytes:   info.Size(),
						StartPos:    currentStartPos,
						EndPos:      currentPos,
						CreatedAt:   currentCreatedAt,
						SealedAt:    time.Now(),
						UploadState: "LOCAL_ONLY",
					}
					if err := r.fileMetaStore.UpsertBinlogFile(ctx, meta); err != nil {
						return err
					}
					fileMeta = &meta
				}
				if r.uploader != nil {
					objectKey := buildObjectKey(r.uploadPrefix, task.ID, filepath.Base(currentPath))
					if err := r.uploader.UploadFile(ctx, task.ID, currentPath, objectKey); err != nil {
						if fileMeta != nil {
							fileMeta.UploadState = "UPLOAD_FAILED"
							fileMeta.UploadError = err.Error()
							if saveErr := r.fileMetaStore.UpsertBinlogFile(ctx, *fileMeta); saveErr != nil {
								return saveErr
							}
						}
						return err
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
				}
				file, writer, currentPath, err = r.openBinlogWriter(task, currentFile, currentPos)
				if err != nil {
					return err
				}
				currentStartPos = currentPos
				currentCreatedAt = time.Now()
			}
		}

		next := binlog.Checkpoint{
			File: currentFile,
			Pos:  event.Header.LogPos,
		}
		if next.Pos == 0 {
			next.Pos = currentPos
		}

		if err := writer.Append(event.RawData, next); err != nil {
			return err
		}
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
	}
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
		ServerID:       serverID,
		Flavor:         flavor,
		Host:           task.Source.Host,
		Port:           task.Source.Port,
		User:           task.Source.User,
		Password:       task.Source.Password,
		RawModeEnabled: true,
	}
}

func (r *MySQLRunner) openBinlogWriter(task tasks.Task, fileName string, initialPos uint32) (*os.File, *binlog.Writer, string, error) {
	dir := filepath.Join(r.dataDir, task.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, "", err
	}
	if err := cleanupExpiredBinlogs(dir, task.Storage.RetentionDays, time.Now(), fileName); err != nil {
		return nil, nil, "", err
	}

	path := filepath.Join(dir, fileName)
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
