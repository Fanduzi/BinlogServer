package tasks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type retryTestUploader struct {
	mu          sync.Mutex
	errByObject map[string]error
	calls       []string
	started     chan struct{}
	block       chan struct{}
}

func (u *retryTestUploader) UploadFile(_ context.Context, _ string, localPath, objectKey string) error {
	if u.started != nil {
		select {
		case u.started <- struct{}{}:
		default:
		}
	}
	if u.block != nil {
		<-u.block
	}
	u.mu.Lock()
	u.calls = append(u.calls, objectKey)
	err := u.errByObject[objectKey]
	u.mu.Unlock()
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(localPath); statErr != nil {
		return statErr
	}
	return nil
}

func (u *retryTestUploader) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.calls)
}

type retryTestFileStore struct {
	mu    sync.Mutex
	files map[string][]BinlogFile
}

func newRetryTestFileStore() *retryTestFileStore {
	return &retryTestFileStore{files: make(map[string][]BinlogFile)}
}

func (s *retryTestFileStore) UpsertBinlogFile(_ context.Context, meta BinlogFile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.files[meta.TaskID]
	for i := range items {
		if items[i].FileName == meta.FileName {
			items[i] = meta
			s.files[meta.TaskID] = items
			return nil
		}
	}
	s.files[meta.TaskID] = append(items, meta)
	return nil
}

func (s *retryTestFileStore) ListBinlogFiles(_ context.Context, taskID string, limit int) ([]BinlogFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.files[taskID]
	if limit <= 0 || limit >= len(items) {
		out := make([]BinlogFile, len(items))
		copy(out, items)
		return out, nil
	}
	out := make([]BinlogFile, limit)
	copy(out, items[:limit])
	return out, nil
}

func (s *retryTestFileStore) get(taskID, fileName string) (BinlogFile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.files[taskID] {
		if item.FileName == fileName {
			return item, true
		}
	}
	return BinlogFile{}, false
}

func writeRetryTestFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("retry-upload-test"), 0o644); err != nil {
		t.Fatalf("write test file failed: %v", err)
	}
	return path
}

func TestScheduler_RetryFailedUploadsOnlyFailedSealed(t *testing.T) {
	tmpDir := t.TempDir()
	store := newRetryTestFileStore()
	uploader := &retryTestUploader{}
	s := NewScheduler(WithFileStore(store), WithFileUploader(uploader))

	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}

	store.files[task.ID] = []BinlogFile{
		{
			TaskID:      task.ID,
			FileName:    "mysql-bin.000001",
			FilePath:    writeRetryTestFile(t, tmpDir, "mysql-bin.000001"),
			SealedAt:    time.Now(),
			UploadState: "UPLOAD_FAILED",
			ObjectKey:   "prefix/cluster-a/uuid/mysql-bin.000001",
		},
		{
			TaskID:      task.ID,
			FileName:    "mysql-bin.000002",
			FilePath:    writeRetryTestFile(t, tmpDir, "mysql-bin.000002"),
			SealedAt:    time.Now(),
			UploadState: "UPLOADED",
			ObjectKey:   "prefix/cluster-a/uuid/mysql-bin.000002",
		},
		{
			TaskID:      task.ID,
			FileName:    "mysql-bin.000003.open.e2",
			FilePath:    writeRetryTestFile(t, tmpDir, "mysql-bin.000003.open.e2"),
			SealedAt:    time.Now(),
			UploadState: "UPLOAD_FAILED",
			ObjectKey:   "prefix/cluster-a/uuid/mysql-bin.000003.open.e2",
		},
	}

	stats, err := s.RetryFailedUploads(task.ID, 100)
	if err != nil {
		t.Fatalf("RetryFailedUploads returned error: %v", err)
	}
	if stats.Succeeded != 1 {
		t.Fatalf("expected succeeded=1, got %d", stats.Succeeded)
	}
	if stats.Skipped < 1 {
		t.Fatalf("expected skipped>=1, got %d", stats.Skipped)
	}
	if uploader.callCount() != 1 {
		t.Fatalf("expected upload call count=1, got %d", uploader.callCount())
	}
	file1, ok := store.get(task.ID, "mysql-bin.000001")
	if !ok {
		t.Fatal("file mysql-bin.000001 not found")
	}
	if file1.UploadState != "UPLOADED" {
		t.Fatalf("expected file1 state UPLOADED, got %s", file1.UploadState)
	}
	file3, ok := store.get(task.ID, "mysql-bin.000003.open.e2")
	if !ok {
		t.Fatal("file mysql-bin.000003.open.e2 not found")
	}
	if file3.UploadState != "UPLOAD_FAILED" {
		t.Fatalf("expected open file keep UPLOAD_FAILED, got %s", file3.UploadState)
	}
}

func TestScheduler_RetryFailedUploadsStateTransitionOnError(t *testing.T) {
	tmpDir := t.TempDir()
	store := newRetryTestFileStore()
	uploader := &retryTestUploader{
		errByObject: map[string]error{
			"prefix/cluster-a/uuid/mysql-bin.000010": errors.New("upload failed"),
		},
	}
	s := NewScheduler(WithFileStore(store), WithFileUploader(uploader))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	store.files[task.ID] = []BinlogFile{
		{
			TaskID:      task.ID,
			FileName:    "mysql-bin.000010",
			FilePath:    writeRetryTestFile(t, tmpDir, "mysql-bin.000010"),
			SealedAt:    time.Now(),
			UploadState: "UPLOAD_FAILED",
			ObjectKey:   "prefix/cluster-a/uuid/mysql-bin.000010",
		},
	}

	stats, err := s.RetryFailedUploads(task.ID, 10)
	if err != nil {
		t.Fatalf("RetryFailedUploads returned error: %v", err)
	}
	if stats.Failed != 1 {
		t.Fatalf("expected failed=1, got %d", stats.Failed)
	}
	item, ok := store.get(task.ID, "mysql-bin.000010")
	if !ok {
		t.Fatal("file mysql-bin.000010 not found")
	}
	if item.UploadState != "UPLOAD_FAILED" {
		t.Fatalf("expected keep UPLOAD_FAILED, got %s", item.UploadState)
	}
	if item.UploadError == "" {
		t.Fatal("expected upload error recorded")
	}
}

func TestScheduler_RetryFailedUploadsRejectsConcurrentJob(t *testing.T) {
	tmpDir := t.TempDir()
	store := newRetryTestFileStore()
	uploader := &retryTestUploader{
		started: make(chan struct{}, 1),
		block:   make(chan struct{}),
	}
	s := NewScheduler(WithFileStore(store), WithFileUploader(uploader))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	store.files[task.ID] = []BinlogFile{
		{
			TaskID:      task.ID,
			FileName:    "mysql-bin.000020",
			FilePath:    writeRetryTestFile(t, tmpDir, "mysql-bin.000020"),
			SealedAt:    time.Now(),
			UploadState: "UPLOAD_FAILED",
			ObjectKey:   "prefix/cluster-a/uuid/mysql-bin.000020",
		},
	}

	done := make(chan error, 1)
	go func() {
		_, runErr := s.RetryFailedUploads(task.ID, 10)
		done <- runErr
	}()
	<-uploader.started

	if _, err := s.RetryFailedUploads(task.ID, 10); !errors.Is(err, ErrUploadRetryInProgress) {
		t.Fatalf("expected ErrUploadRetryInProgress, got %v", err)
	}
	close(uploader.block)
	if err := <-done; err != nil {
		t.Fatalf("first retry returned error: %v", err)
	}
}

func TestScheduler_RetryFailedUploadsUpdatesMetrics(t *testing.T) {
	tmpDir := t.TempDir()
	store := newRetryTestFileStore()
	uploader := &retryTestUploader{
		errByObject: map[string]error{
			"prefix/cluster-a/uuid/mysql-bin.000031": errors.New("upload failed"),
		},
	}
	s := NewScheduler(WithFileStore(store), WithFileUploader(uploader))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}

	store.files[task.ID] = []BinlogFile{
		{
			TaskID:      task.ID,
			FileName:    "mysql-bin.000030",
			FilePath:    writeRetryTestFile(t, tmpDir, "mysql-bin.000030"),
			SealedAt:    time.Now(),
			UploadState: "UPLOAD_FAILED",
			ObjectKey:   "prefix/cluster-a/uuid/mysql-bin.000030",
		},
		{
			TaskID:      task.ID,
			FileName:    "mysql-bin.000031",
			FilePath:    writeRetryTestFile(t, tmpDir, "mysql-bin.000031"),
			SealedAt:    time.Now(),
			UploadState: "UPLOAD_FAILED",
			ObjectKey:   "prefix/cluster-a/uuid/mysql-bin.000031",
		},
		{
			TaskID:      task.ID,
			FileName:    "mysql-bin.000032",
			FilePath:    writeRetryTestFile(t, tmpDir, "mysql-bin.000032"),
			SealedAt:    time.Now(),
			UploadState: "UPLOADED",
			ObjectKey:   "prefix/cluster-a/uuid/mysql-bin.000032",
		},
	}

	if _, err := s.RetryFailedUploads(task.ID, 100); err != nil {
		t.Fatalf("RetryFailedUploads returned error: %v", err)
	}

	metrics := s.GetUploadRetryMetrics()
	if metrics.Success != 1 || metrics.Failed != 1 || metrics.Skipped != 1 {
		t.Fatalf("unexpected retry metrics: %+v", metrics)
	}
	if metrics.LastTs <= 0 {
		t.Fatalf("expected LastTs > 0, got %d", metrics.LastTs)
	}
}

func TestScheduler_ListUploadFailureReasons(t *testing.T) {
	store := newRetryTestFileStore()
	s := NewScheduler(WithFileStore(store))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	now := time.Now()
	store.files[task.ID] = []BinlogFile{
		{
			TaskID:      task.ID,
			FileName:    "mysql-bin.000040",
			UploadState: "UPLOAD_FAILED",
			UploadError: "network timeout",
			SealedAt:    now.Add(-3 * time.Minute),
		},
		{
			TaskID:      task.ID,
			FileName:    "mysql-bin.000041",
			UploadState: "UPLOAD_FAILED",
			UploadError: " network timeout ",
			SealedAt:    now.Add(-1 * time.Minute),
		},
		{
			TaskID:      task.ID,
			FileName:    "mysql-bin.000042",
			UploadState: "UPLOAD_FAILED",
			UploadError: "permission denied",
			SealedAt:    now,
		},
		{
			TaskID:      task.ID,
			FileName:    "mysql-bin.000043",
			UploadState: "UPLOADED",
			UploadError: "",
			SealedAt:    now,
		},
	}

	reasons, err := s.ListUploadFailureReasons(task.ID, 20)
	if err != nil {
		t.Fatalf("ListUploadFailureReasons returned error: %v", err)
	}
	if len(reasons) != 2 {
		t.Fatalf("expected 2 reasons, got %d", len(reasons))
	}
	if reasons[0].Reason != "network timeout" || reasons[0].Count != 2 {
		t.Fatalf("unexpected first reason item: %+v", reasons[0])
	}
	if reasons[1].Reason != "permission denied" || reasons[1].Count != 1 {
		t.Fatalf("unexpected second reason item: %+v", reasons[1])
	}
}
