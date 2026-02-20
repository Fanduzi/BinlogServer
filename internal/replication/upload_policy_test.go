package replication

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"binlog_server/internal/tasks"
)

type fakeUploader struct {
	err   error
	calls int
}

func (f *fakeUploader) UploadFile(_ context.Context, _ string, _ string, _ string) error {
	f.calls++
	return f.err
}

type fakeMetaStore struct {
	metas []tasks.BinlogFile
}

func (f *fakeMetaStore) UpsertBinlogFile(_ context.Context, meta tasks.BinlogFile) error {
	f.metas = append(f.metas, meta)
	return nil
}

func TestFinalizeSealedFile_BestEffortOnUploadFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mysql-bin.000001")
	if err := os.WriteFile(path, []byte("binlog-data"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	uploader := &fakeUploader{err: errors.New("upload failed")}
	metaStore := &fakeMetaStore{}
	runner := &MySQLRunner{
		uploader:      uploader,
		fileMetaStore: metaStore,
		uploadPrefix:  "prefix",
	}

	err := runner.finalizeSealedFile(
		context.Background(),
		tasks.Task{ID: "1", ClusterKey: "cluster-a"},
		"srv-uuid-1",
		path,
		4,
		1024,
		time.Now().Add(-time.Minute),
		time.Now(),
	)
	if err != nil {
		t.Fatalf("expected best-effort upload failure ignored, got err=%v", err)
	}
	if uploader.calls != 1 {
		t.Fatalf("expected uploader called once, got %d", uploader.calls)
	}
	if len(metaStore.metas) < 2 {
		t.Fatalf("expected multiple meta updates, got %d", len(metaStore.metas))
	}
	last := metaStore.metas[len(metaStore.metas)-1]
	if last.UploadState != "UPLOAD_FAILED" {
		t.Fatalf("expected UPLOAD_FAILED, got %s", last.UploadState)
	}
	if last.ObjectKey == "" {
		t.Fatal("expected object key recorded on upload failure")
	}
}

func TestFinalizeSealedFile_UploadSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mysql-bin.000001")
	if err := os.WriteFile(path, []byte("binlog-data"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	uploader := &fakeUploader{}
	metaStore := &fakeMetaStore{}
	runner := &MySQLRunner{
		uploader:      uploader,
		fileMetaStore: metaStore,
		uploadPrefix:  "prefix",
	}

	err := runner.finalizeSealedFile(
		context.Background(),
		tasks.Task{ID: "1", ClusterKey: "cluster-a"},
		"srv-uuid-1",
		path,
		4,
		1024,
		time.Now().Add(-time.Minute),
		time.Now(),
	)
	if err != nil {
		t.Fatalf("expected upload success, got err=%v", err)
	}
	last := metaStore.metas[len(metaStore.metas)-1]
	if last.UploadState != "UPLOADED" {
		t.Fatalf("expected UPLOADED, got %s", last.UploadState)
	}
	if last.ObjectKey == "" {
		t.Fatal("expected object key set")
	}
}
