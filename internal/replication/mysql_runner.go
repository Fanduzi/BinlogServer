package replication

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"

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

	file, writer, err := r.openBinlogWriter(task.ID, currentFile, currentPos)
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
				file, writer, err = r.openBinlogWriter(task.ID, currentFile, currentPos)
				if err != nil {
					return err
				}
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

func (r *MySQLRunner) openBinlogWriter(taskID, fileName string, initialPos uint32) (*os.File, *binlog.Writer, error) {
	dir := filepath.Join(r.dataDir, taskID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}

	path := filepath.Join(dir, fileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if info.Size() == 0 {
		if _, err := f.Write(binlogMagic); err != nil {
			_ = f.Close()
			return nil, nil, err
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return nil, nil, err
		}
	}

	writer := binlog.NewWriter(f, binlog.Checkpoint{
		File: fileName,
		Pos:  initialPos,
	})
	return f, writer, nil
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
