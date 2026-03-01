// input: source replication config, task state, checkpoint/file store dependencies
// output: replication run control, local binlog artifacts, and upload/recovery signals
// pos: data-plane runtime that consumes MySQL binlog stream and emits durable outputs
// note: if this file changes, update this header and module AGENTS.md.
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

	file, _, path, err := runner.openBinlogWriter(tasks.Task{ID: "1", Epoch: 42}, "mysql-bin.000123", 4)
	if err != nil {
		t.Fatalf("openBinlogWriter returned error: %v", err)
	}
	defer file.Close()

	if filepath.Base(path) != "mysql-bin.000123.open.e42" {
		t.Fatalf("expected epoch open suffix, got %s", filepath.Base(path))
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

	if _, err := os.Stat(openPath); !os.IsNotExist(err) {
		t.Fatalf("expected open file renamed away, stat err=%v", err)
	}
}
