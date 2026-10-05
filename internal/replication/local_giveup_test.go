// Package replication provides module-level functionality for replication.
// input: a sealed file that is already on disk, and checkpoint upsert errors
// output: proof that an existing sealed file and a non-transient checkpoint write are permanent, a transient checkpoint write is not, and a lease epoch mismatch leaves Run as a handoff
// pos: runner-level regression coverage for local errors that must not retry forever
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

func TestFinishRunLeaseMismatchIsHandoff(t *testing.T) {
	err := finishRun(ErrLeaseEpochMismatch)
	if !tasks.IsLeaseHandoff(err) || !errors.Is(err, ErrLeaseEpochMismatch) {
		t.Fatalf("expected lease handoff, got %v", err)
	}
}

func TestSealExistingFileIsPermanent(t *testing.T) {
	dir := t.TempDir()
	openPath := filepath.Join(dir, "mysql-bin.000003.open.e1")
	if err := os.WriteFile(openPath, []byte("binlog-data"), 0o644); err != nil {
		t.Fatalf("write open file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mysql-bin.000003"), []byte("already-sealed"), 0o644); err != nil {
		t.Fatalf("write plain sealed file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mysql-bin.000003.sealed.e1"), []byte("epoch-sealed"), 0o644); err != nil {
		t.Fatalf("write epoch sealed file: %v", err)
	}

	runner := &MySQLRunner{}
	WithLeaseVerifier(leaseVerifierFunc(func(context.Context, string, string, int64) (bool, error) {
		return true, nil
	}))(runner)

	err := runner.finalizeSealedFile(
		context.Background(),
		tasks.Task{ID: "1", Epoch: 1, OwnerWorkerID: "worker-a", ClusterKey: "cluster-a"},
		"srv-uuid-1",
		openPath,
		4,
		1024,
		time.Now().Add(-time.Minute),
		time.Now(),
	)
	if err == nil || !tasks.IsPermanent(err) {
		t.Fatalf("expected permanent sealed-file error, got %v", err)
	}
	if !strings.Contains(err.Error(), "SEALED_FILE_EXISTS") || !strings.Contains(err.Error(), "sealed file already exists:") {
		t.Fatalf("error=%v", err)
	}
}

func TestMySQLRunner_NonTransientCheckpointWriteIsPermanent(t *testing.T) {
	err := runCheckpointUpsert(t, errors.New("Error 1146: Table 'binlog_server.checkpoints' doesn't exist"))
	if err == nil || !tasks.IsPermanent(err) || !strings.Contains(err.Error(), "CHECKPOINT_WRITE_FAILED") {
		t.Fatalf("expected permanent checkpoint write, got %v", err)
	}
}

func TestMySQLRunner_TransientCheckpointWriteStaysRetryable(t *testing.T) {
	err := runCheckpointUpsert(t, errors.New("connection reset by peer"))
	if err == nil || tasks.IsPermanent(err) || !strings.Contains(err.Error(), "connection reset by peer") {
		t.Fatalf("transient checkpoint write must stay retryable, got %v", err)
	}
}

func runCheckpointUpsert(t *testing.T, upsertErr error) error {
	t.Helper()
	store := &fakeRunnerCheckpointStore{upsertErr: upsertErr}
	streamer := &fakeStreamer{results: []streamResult{{event: newRunnerEvent(120)}}}
	syncer := &fakeSyncer{streamer: streamer}
	runner := &MySQLRunner{
		fetcher:         &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		checkpointStore: store,
		newSyncer: func(goreplication.BinlogSyncerConfig) binlogSyncer {
			return syncer
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			file := &fakeSyncFile{}
			return &fakeCloser{}, binlog.NewWriter(file, binlog.Checkpoint{File: fileName, Pos: initialPos}), t.TempDir() + "/" + fileName, nil
		},
	}
	return runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: "mysql-bin.000010",
		Pos:  4,
	}))
}
