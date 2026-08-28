// Package replication provides module-level functionality for replication.
// input: task epoch, file metadata store, local open files, uploader and lease verification
// output: regression coverage for OPEN visibility/progress, sealing, upload, and lease-safe file publication
// pos: data-plane runtime that consumes MySQL binlog stream and emits durable outputs
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/tasks"

	"github.com/go-mysql-org/go-mysql/replication"
)

type fileStateUploader struct {
	calls     int
	lastTask  string
	lastPath  string
	lastObj   string
	uploadErr error
}

// UploadFile 实现对应功能逻辑。
func (u *fileStateUploader) UploadFile(_ context.Context, taskID, localPath, objectKey string) error {
	u.calls++
	u.lastTask = taskID
	u.lastPath = localPath
	u.lastObj = objectKey
	return u.uploadErr
}

type fileStateMetaStore struct {
	metas []tasks.BinlogFile
}

// UpsertBinlogFile 实现对应功能逻辑。
func (s *fileStateMetaStore) UpsertBinlogFile(_ context.Context, meta tasks.BinlogFile) error {
	s.metas = append(s.metas, meta)
	return nil
}

// TestFileState_OpenFileUsesEpochSuffix 验证相关行为。
func TestFileState_OpenFileUsesEpochSuffix(t *testing.T) {
	runner := NewMySQLRunner(t.TempDir())

	file, _, path, err := runner.openBinlogWriter(context.Background(), tasks.Task{ID: "1", Epoch: 42}, "mysql-bin.000123", 4)
	if err != nil {
		t.Fatalf("openBinlogWriter returned error: %v", err)
	}
	defer file.Close()

	if filepath.Base(path) != "mysql-bin.000123.open.e42" {
		t.Fatalf("expected epoch open suffix, got %s", filepath.Base(path))
	}
}

func TestFileState_OpenFilePublishesCurrentSegmentMetadata(t *testing.T) {
	metaStore := &fileStateMetaStore{}
	runner := NewMySQLRunner(t.TempDir(), WithFileMetaStore(metaStore))

	file, _, path, err := runner.openBinlogWriter(context.Background(), tasks.Task{ID: "19", Epoch: 8}, "mysql-bin.000123", 21790)
	if err != nil {
		t.Fatalf("openBinlogWriter returned error: %v", err)
	}
	defer file.Close()

	if len(metaStore.metas) != 1 {
		t.Fatalf("expected current open segment metadata, got %d records", len(metaStore.metas))
	}
	meta := metaStore.metas[0]
	if meta.TaskID != "19" || meta.FileName != "mysql-bin.000123" || meta.FilePath != path {
		t.Fatalf("unexpected current segment metadata: %+v", meta)
	}
	if meta.State != "OPEN" {
		t.Fatalf("expected OPEN state, got %q", meta.State)
	}
}

func TestFileState_OpenFileMetadataAdvancesAfterFlush(t *testing.T) {
	metaStore := &fileStateMetaStore{}
	streamer := &fakeStreamer{results: []streamResult{
		{event: newRunnerEvent(120)},
		{err: context.Canceled},
	}}
	runner := NewMySQLRunner(t.TempDir(), WithFileMetaStore(metaStore))
	runner.fetcher = &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"}
	runner.newSyncer = func(_ replication.BinlogSyncerConfig) binlogSyncer {
		return &fakeSyncer{streamer: streamer}
	}

	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: "mysql-bin.000123",
		Pos:  4,
	}))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(metaStore.metas) != 2 {
		t.Fatalf("expected open and advanced metadata, got %d records", len(metaStore.metas))
	}
	initial, advanced := metaStore.metas[0], metaStore.metas[1]
	if advanced.State != "OPEN" || advanced.EndPos != 120 {
		t.Fatalf("expected advanced OPEN metadata at pos 120, got %+v", advanced)
	}
	if advanced.SizeBytes <= initial.SizeBytes {
		t.Fatalf("expected file size to advance from %d, got %d", initial.SizeBytes, advanced.SizeBytes)
	}
}

// TestFileState_SealRequiresLeaseAndEpochMatch 验证相关行为。
func TestFileState_SealRequiresLeaseAndEpochMatch(t *testing.T) {
	dir := t.TempDir()
	openPath := filepath.Join(dir, "mysql-bin.000123.open.e7")
	if err := os.WriteFile(openPath, []byte("binlog-data"), 0o644); err != nil {
		t.Fatalf("write open file: %v", err)
	}

	uploader := &fileStateUploader{}
	metaStore := &fileStateMetaStore{}
	runner := &MySQLRunner{
		uploader:      uploader,
		fileMetaStore: metaStore,
		uploadPrefix:  "prefix",
		leaseVerifier: leaseVerifierFunc(func(context.Context, tasks.Task) (bool, error) {
			return false, nil
		}),
	}

	err := runner.finalizeSealedFile(
		context.Background(),
		tasks.Task{ID: "1", Epoch: 7, OwnerWorkerID: "worker-a"},
		"srv-uuid-1",
		openPath,
		4,
		1024,
		time.Now().Add(-time.Minute),
		time.Now(),
	)
	if !errors.Is(err, ErrLeaseEpochMismatch) {
		t.Fatalf("expected ErrLeaseEpochMismatch, got %v", err)
	}
	if uploader.calls != 0 {
		t.Fatalf("expected no upload when lease mismatch, got %d", uploader.calls)
	}
	if len(metaStore.metas) != 0 {
		t.Fatalf("expected no meta upsert when lease mismatch, got %d", len(metaStore.metas))
	}

	if _, err := os.Stat(openPath); err != nil {
		t.Fatalf("expected open file retained on rejected seal, stat err=%v", err)
	}
	sealedPath := filepath.Join(dir, "mysql-bin.000123")
	if _, err := os.Stat(sealedPath); !os.IsNotExist(err) {
		t.Fatalf("expected sealed file not created, stat err=%v", err)
	}
}

// TestFileState_NeverPublishOpenFile 验证相关行为。
func TestFileState_NeverPublishOpenFile(t *testing.T) {
	dir := t.TempDir()
	openPath := filepath.Join(dir, "mysql-bin.000888.open.e12")
	if err := os.WriteFile(openPath, []byte("binlog-data"), 0o644); err != nil {
		t.Fatalf("write open file: %v", err)
	}

	uploader := &fileStateUploader{}
	metaStore := &fileStateMetaStore{}
	runner := &MySQLRunner{
		uploader:      uploader,
		fileMetaStore: metaStore,
		uploadPrefix:  "prefix",
		leaseVerifier: leaseVerifierFunc(func(context.Context, tasks.Task) (bool, error) {
			return true, nil
		}),
	}

	err := runner.finalizeSealedFile(
		context.Background(),
		tasks.Task{ID: "1", ClusterKey: "cluster-a", Epoch: 12, OwnerWorkerID: "worker-a"},
		"srv-uuid-1",
		openPath,
		4,
		1024,
		time.Now().Add(-time.Minute),
		time.Now(),
	)
	if err != nil {
		t.Fatalf("finalizeSealedFile returned error: %v", err)
	}

	if uploader.calls != 1 {
		t.Fatalf("expected upload called once, got %d", uploader.calls)
	}
	if strings.Contains(uploader.lastObj, ".open.e") {
		t.Fatalf("expected published object key without open suffix, got %s", uploader.lastObj)
	}
	if filepath.Base(uploader.lastPath) != "mysql-bin.000888" {
		t.Fatalf("expected upload sealed file name, got %s", filepath.Base(uploader.lastPath))
	}
	if len(metaStore.metas) == 0 {
		t.Fatal("expected binlog file metadata upsert")
	}
	last := metaStore.metas[len(metaStore.metas)-1]
	if last.FileName != "mysql-bin.000888" {
		t.Fatalf("expected sealed file name, got %s", last.FileName)
	}
	if last.State != "SEALED" {
		t.Fatalf("expected SEALED state, got %q", last.State)
	}

	if _, err := os.Stat(openPath); !os.IsNotExist(err) {
		t.Fatalf("expected open file renamed away, stat err=%v", err)
	}
}
