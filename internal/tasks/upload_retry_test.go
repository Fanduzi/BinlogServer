// Package tasks provides module-level functionality for tasks.
// input: task commands/events, runner callbacks, store/lease/uploader dependencies
// output: task state transitions, scheduling decisions, execution coordination, background retry of sealed UPLOAD_FAILED rows without the manual API, checksum mismatch re-upload plus unfinished-checksum re-check becoming UPLOADED match, and the files list for a sealed file before and after that retry
// pos: core domain orchestration layer governing backup task lifecycle and policies
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// UploadFile 实现对应功能逻辑。
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

// callCount 实现对应功能逻辑。
func (u *retryTestUploader) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.calls)
}

type retryTestFileStore struct {
	mu    sync.Mutex
	files map[string][]BinlogFile
}

// newRetryTestFileStore 实现对应功能逻辑。
func newRetryTestFileStore() *retryTestFileStore {
	return &retryTestFileStore{files: make(map[string][]BinlogFile)}
}

// UpsertBinlogFile 实现对应功能逻辑。
func (s *retryTestFileStore) UpsertBinlogFile(_ context.Context, meta BinlogFile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.files[meta.TaskID]
	for i := range items {
		if items[i].FileName == meta.FileName && items[i].Epoch == meta.Epoch {
			items[i] = meta
			s.files[meta.TaskID] = items
			return nil
		}
	}
	s.files[meta.TaskID] = append(items, meta)
	return nil
}

// ListBinlogFiles 实现对应功能逻辑。
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

func (s *retryTestFileStore) ListFailedUploadBinlogFiles(_ context.Context, taskID string, limit int) ([]BinlogFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return FailedUploadFiles(s.files[taskID], limit), nil
}

// get 实现对应功能逻辑。
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

// writeRetryTestFile 实现对应功能逻辑。
func writeRetryTestFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("retry-upload-test"), 0o644); err != nil {
		t.Fatalf("write test file failed: %v", err)
	}
	return path
}

func TestScheduler_RetryFailedUploadsRequiresFailedUploadLookup(t *testing.T) {
	store := newFakeFileStore()
	s := NewScheduler(WithFileStore(store), WithFileUploader(&retryTestUploader{}))

	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	store.files[task.ID] = []BinlogFile{
		{
			TaskID:      task.ID,
			FileName:    "mysql-bin.000001",
			FilePath:    "/tmp/mysql-bin.000001",
			SealedAt:    time.Now(),
			UploadState: "UPLOAD_FAILED",
			ObjectKey:   "prefix/cluster-a/uuid/mysql-bin.000001",
		},
	}

	_, err = s.RetryFailedUploads(task.ID, 10)
	if !errors.Is(err, ErrFailedUploadLookupNotAvailable) {
		t.Fatalf("expected ErrFailedUploadLookupNotAvailable, got %v", err)
	}
}

func TestScheduler_RetryFailedUploadsFindsFailedFileOutsideListWindow(t *testing.T) {
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
			UploadState: "UPLOADED",
			ObjectKey:   "prefix/cluster-a/uuid/mysql-bin.000001",
		},
		{
			TaskID:      task.ID,
			FileName:    "mysql-bin.000002",
			FilePath:    writeRetryTestFile(t, tmpDir, "mysql-bin.000002"),
			SealedAt:    time.Now(),
			UploadState: "UPLOAD_FAILED",
			ObjectKey:   "prefix/cluster-a/uuid/mysql-bin.000002",
		},
	}

	stats, err := s.RetryFailedUploads(task.ID, 1)
	if err != nil {
		t.Fatalf("RetryFailedUploads returned error: %v", err)
	}
	if stats.Succeeded != 1 {
		t.Fatalf("expected succeeded=1, got %+v", stats)
	}
	if uploader.callCount() != 1 {
		t.Fatalf("expected 1 upload call, got %d", uploader.callCount())
	}
}

// TestScheduler_RetryFailedUploadsOnlyFailedSealed 验证相关行为。
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

// TestScheduler_RetryFailedUploadsStateTransitionOnError 验证相关行为。
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

// TestScheduler_RetryFailedUploadsRejectsConcurrentJob 验证相关行为。
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

// TestScheduler_RetryFailedUploadsUpdatesMetrics 验证相关行为。
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
	if metrics.Success != 1 || metrics.Failed != 1 || metrics.Skipped != 0 {
		t.Fatalf("unexpected retry metrics: %+v", metrics)
	}
	if metrics.LastTs <= 0 {
		t.Fatalf("expected LastTs > 0, got %d", metrics.LastTs)
	}
}

// TestScheduler_ListUploadFailureReasons 验证相关行为。
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

// failThenUploader 前几次上传失败，之后成功。用来表示桶恢复后的后台重试。
type failThenUploader struct {
	mu        sync.Mutex
	failsLeft int
	calls     []string
}

func (u *failThenUploader) UploadFile(_ context.Context, _ string, localPath, objectKey string) error {
	u.mu.Lock()
	u.calls = append(u.calls, objectKey)
	fail := u.failsLeft > 0
	if fail {
		u.failsLeft--
	}
	u.mu.Unlock()
	if fail {
		return errors.New("bucket unavailable")
	}
	if _, err := os.Stat(localPath); err != nil {
		return err
	}
	return nil
}

func (u *failThenUploader) callObjectKeys() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]string, len(u.calls))
	copy(out, u.calls)
	return out
}

// TestScheduler_BackgroundUploadRetryFlipsSealedFailureWithoutManualAPI 验证后台循环把已封存的 UPLOAD_FAILED 补成 UPLOADED。
// 测试本身不调用 RetryFailedUploads，也不走 HTTP。上传失败不改变任务状态。open 分段和本机不存在的文件不上传。
func TestScheduler_BackgroundUploadRetryFlipsSealedFailureWithoutManualAPI(t *testing.T) {
	tmpDir := t.TempDir()
	store := newRetryTestFileStore()
	uploader := &failThenUploader{failsLeft: 1}
	s := NewScheduler(WithFileStore(store), WithFileUploader(uploader))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	const (
		sealedKey  = "prefix/cluster-a/uuid/mysql-bin.000050"
		openKey    = "prefix/cluster-a/uuid/mysql-bin.000051.open.e1"
		missingKey = "prefix/cluster-a/uuid/mysql-bin.000052"
	)
	store.files[task.ID] = []BinlogFile{
		{
			TaskID:      task.ID,
			FileName:    "mysql-bin.000050",
			FilePath:    writeRetryTestFile(t, tmpDir, "mysql-bin.000050"),
			SealedAt:    time.Now(),
			UploadState: "UPLOAD_FAILED",
			ObjectKey:   sealedKey,
		},
		{
			TaskID:      task.ID,
			FileName:    "mysql-bin.000051",
			FilePath:    writeRetryTestFile(t, tmpDir, "mysql-bin.000051.open.e1"),
			State:       "OPEN",
			SealedAt:    time.Now(),
			UploadState: "UPLOAD_FAILED",
			ObjectKey:   openKey,
		},
		{
			TaskID:      task.ID,
			FileName:    "mysql-bin.000052",
			FilePath:    filepath.Join(tmpDir, "missing-mysql-bin.000052"),
			SealedAt:    time.Now(),
			UploadState: "UPLOAD_FAILED",
			UploadError: "previous failure",
			ObjectKey:   missingKey,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.RunBackgroundUploadRetry(ctx, 15*time.Millisecond)
	}()

	deadline := time.Now().Add(2 * time.Second)
	var sealed BinlogFile
	for {
		item, ok := store.get(task.ID, "mysql-bin.000050")
		if ok && item.UploadState == "UPLOADED" {
			sealed = item
			break
		}
		if time.Now().After(deadline) {
			state := ""
			if ok {
				state = item.UploadState
			}
			t.Fatalf("background retry did not reach UPLOADED, state=%q calls=%v", state, uploader.callObjectKeys())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("background upload retry did not stop")
	}

	if sealed.UploadError != "" {
		t.Fatalf("expected cleared upload error, got %q", sealed.UploadError)
	}
	calls := uploader.callObjectKeys()
	if len(calls) < 2 {
		t.Fatalf("expected a failed attempt before the bucket recovered, calls=%v", calls)
	}
	metricsDeadline := time.Now().Add(2 * time.Second)
	for {
		metrics := s.GetUploadRetryMetrics()
		if metrics.Success >= 1 && metrics.Failed >= 1 {
			break
		}
		if time.Now().After(metricsDeadline) {
			t.Fatalf("expected background retry metrics, got %+v", metrics)
		}
		time.Sleep(10 * time.Millisecond)
	}
	openFile, ok := store.get(task.ID, "mysql-bin.000051")
	if !ok || openFile.UploadState != "UPLOAD_FAILED" {
		t.Fatalf("open segment must stay UPLOAD_FAILED, ok=%v state=%s", ok, openFile.UploadState)
	}
	missing, ok := store.get(task.ID, "mysql-bin.000052")
	if !ok || missing.UploadState != "UPLOAD_FAILED" || missing.UploadError != "previous failure" {
		t.Fatalf("missing local file must be left unchanged, ok=%v file=%+v", ok, missing)
	}
	for _, key := range uploader.callObjectKeys() {
		if key == openKey || key == missingKey {
			t.Fatalf("background retry uploaded %s, calls=%v", key, uploader.callObjectKeys())
		}
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.State != StateCreated {
		t.Fatalf("background retry changed task state to %s", got.State)
	}
}

type countingFailedStore struct {
	*retryTestFileStore
	total int64
	err   error
}

func (s *countingFailedStore) CountUploadFailures(context.Context) (int64, error) {
	return s.total, s.err
}

// TestScheduler_BackgroundUploadRetryUsesFailureCount 验证全局失败数为 0 时后台这一轮不上传；计数失败或失败数大于 0 时仍上传。
func TestScheduler_BackgroundUploadRetryUsesFailureCount(t *testing.T) {
	tmpDir := t.TempDir()
	store := &countingFailedStore{retryTestFileStore: newRetryTestFileStore()}
	uploader := &retryTestUploader{}
	s := NewScheduler(WithFileStore(store), WithFileUploader(uploader))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	path := writeRetryTestFile(t, tmpDir, "mysql-bin.000070")
	store.files[task.ID] = []BinlogFile{{
		TaskID:      task.ID,
		FileName:    "mysql-bin.000070",
		FilePath:    path,
		SealedAt:    time.Now(),
		UploadState: "UPLOAD_FAILED",
		ObjectKey:   "prefix/cluster-a/uuid/mysql-bin.000070",
	}}

	s.retryKnownFailedUploads()
	if uploader.callCount() != 0 {
		t.Fatalf("expected no upload when the failure count is 0, calls=%d", uploader.callCount())
	}

	store.err = errors.New("count failed")
	s.retryKnownFailedUploads()
	if uploader.callCount() != 1 {
		t.Fatalf("expected upload when the failure count cannot be read, calls=%d", uploader.callCount())
	}
	item, ok := store.get(task.ID, "mysql-bin.000070")
	if !ok || item.UploadState != "UPLOADED" {
		t.Fatalf("expected UPLOADED after the count error, ok=%v state=%s", ok, item.UploadState)
	}

	item.UploadState = "UPLOAD_FAILED"
	item.UploadError = "again"
	if err := store.UpsertBinlogFile(context.Background(), item); err != nil {
		t.Fatalf("upsert failed: %v", err)
	}
	store.err = nil
	store.total = 1
	s.retryKnownFailedUploads()
	if uploader.callCount() != 2 {
		t.Fatalf("expected upload when the failure count is positive, calls=%d", uploader.callCount())
	}
}

// TestScheduler_RetryFailedUploadsStillAttemptsMissingLocalFile 锁定手动补传：本机没有文件时仍尝试上传并记下失败。
func TestScheduler_RetryFailedUploadsStillAttemptsMissingLocalFile(t *testing.T) {
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
			FileName:    "mysql-bin.000060",
			FilePath:    filepath.Join(t.TempDir(), "missing-mysql-bin.000060"),
			SealedAt:    time.Now(),
			UploadState: "UPLOAD_FAILED",
			ObjectKey:   "prefix/cluster-a/uuid/mysql-bin.000060",
		},
	}

	stats, err := s.RetryFailedUploads(task.ID, 10)
	if err != nil {
		t.Fatalf("RetryFailedUploads returned error: %v", err)
	}
	if stats.Failed != 1 || uploader.callCount() != 1 {
		t.Fatalf("expected manual retry to attempt the missing file, stats=%+v calls=%d", stats, uploader.callCount())
	}
	item, ok := store.get(task.ID, "mysql-bin.000060")
	if !ok || item.UploadState != "UPLOAD_FAILED" || item.UploadError == "" {
		t.Fatalf("expected UPLOAD_FAILED with an error, ok=%v file=%+v", ok, item)
	}
}

// checksumRetryUploader stores sealed bytes and can corrupt them or fail the object check.
type checksumRetryUploader struct {
	mu      sync.Mutex
	objects map[string][]byte
	corrupt bool
	headErr error
	uploads int
	checks  int
}

func (u *checksumRetryUploader) UploadFile(_ context.Context, _ string, localPath, objectKey string) error {
	body, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.uploads++
	if u.corrupt && len(body) > 0 {
		body[0] ^= 0xff
	}
	if u.objects == nil {
		u.objects = map[string][]byte{}
	}
	u.objects[objectKey] = append([]byte(nil), body...)
	return nil
}

func (u *checksumRetryUploader) SealedObjectMatches(_ context.Context, localPath, objectKey string) (bool, error) {
	local, err := os.ReadFile(localPath)
	if err != nil {
		return false, err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.checks++
	if u.headErr != nil {
		return false, u.headErr
	}
	return bytes.Equal(local, u.objects[objectKey]), nil
}

func (u *checksumRetryUploader) counts() (uploads, checks int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.uploads, u.checks
}

// TestBackgroundUploadRetry_ChecksumMismatchThenMatch re-uploads a mismatched object
// on the existing UPLOAD_FAILED path and records UPLOADED only after the bytes match.
func TestBackgroundUploadRetry_ChecksumMismatchThenMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mysql-bin.000080")
	if err := os.WriteFile(path, []byte("sealed-segment-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := newRetryTestFileStore()
	uploader := &checksumRetryUploader{corrupt: true}
	s := NewScheduler(WithFileStore(store), WithFileUploader(uploader))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatal(err)
	}
	file := BinlogFile{
		TaskID: task.ID, FileName: "mysql-bin.000080", FilePath: path, SealedAt: time.Now(),
		ObjectKey: "prefix/cluster-a/uuid/mysql-bin.000080", State: "SEALED",
	}
	updated, err := ApplySealedUpload(context.Background(), uploader, store, file)
	if err != nil {
		t.Fatal(err)
	}
	if updated.UploadState != "UPLOAD_FAILED" || updated.Checksum != ChecksumMismatch || updated.UploadError != ChecksumMismatchError {
		t.Fatalf("first upload: %+v", updated)
	}

	s.retryKnownFailedUploads()
	still, ok := store.get(task.ID, file.FileName)
	if !ok || still.UploadState != "UPLOAD_FAILED" || still.Checksum != ChecksumMismatch {
		t.Fatalf("corrupt object retried into %+v", still)
	}
	uploads, _ := uploader.counts()
	if uploads != 2 {
		t.Fatalf("uploads=%d, want the original put plus one re-upload", uploads)
	}

	uploader.mu.Lock()
	uploader.corrupt = false
	uploader.mu.Unlock()
	s.retryKnownFailedUploads()
	matched, ok := store.get(task.ID, file.FileName)
	if !ok || matched.UploadState != "UPLOADED" || matched.Checksum != ChecksumMatch || matched.UploadError != "" {
		t.Fatalf("re-verified row: %+v", matched)
	}
	uploads, checks := uploader.counts()
	if uploads != 3 || checks < 3 {
		t.Fatalf("uploads=%d checks=%d, want a third put that matches", uploads, checks)
	}
}

// TestBackgroundUploadRetry_HeadErrorThenMatch checks the stored object again
// and does not upload it again once the HEAD succeeds.
func TestBackgroundUploadRetry_HeadErrorThenMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mysql-bin.000081")
	if err := os.WriteFile(path, []byte("sealed-segment-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := newRetryTestFileStore()
	uploader := &checksumRetryUploader{headErr: errors.New("head object: connection reset")}
	s := NewScheduler(WithFileStore(store), WithFileUploader(uploader))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatal(err)
	}
	file := BinlogFile{
		TaskID: task.ID, FileName: "mysql-bin.000081", FilePath: path, SealedAt: time.Now(),
		ObjectKey: "prefix/cluster-a/uuid/mysql-bin.000081", State: "SEALED",
	}
	updated, err := ApplySealedUpload(context.Background(), uploader, store, file)
	if err != nil {
		t.Fatal(err)
	}
	if updated.UploadState != "UPLOAD_FAILED" || updated.Checksum != "" || !strings.HasPrefix(updated.UploadError, ChecksumVerifyPrefix) {
		t.Fatalf("head failure row: %+v", updated)
	}
	uploadsBefore, _ := uploader.counts()

	s.retryKnownFailedUploads()
	pending, ok := store.get(task.ID, file.FileName)
	if !ok || pending.UploadState != "UPLOAD_FAILED" || pending.Checksum != "" {
		t.Fatalf("unfinished check became %+v", pending)
	}
	uploads, _ := uploader.counts()
	if uploads != uploadsBefore {
		t.Fatalf("uploads=%d, want %d: a failed check must not upload again", uploads, uploadsBefore)
	}

	uploader.mu.Lock()
	uploader.headErr = nil
	uploader.mu.Unlock()
	s.retryKnownFailedUploads()
	matched, ok := store.get(task.ID, file.FileName)
	if !ok || matched.UploadState != "UPLOADED" || matched.Checksum != ChecksumMatch || matched.UploadError != "" {
		t.Fatalf("re-verified row: %+v", matched)
	}
	uploadsAfter, _ := uploader.counts()
	if uploadsAfter != uploadsBefore {
		t.Fatalf("uploads=%d, want %d after a successful check", uploadsAfter, uploadsBefore)
	}
}

// TestListFiles_PendingSealedUploadThenVerified is what the files list shows
// while a sealed file is waiting on the existing retry, and after that retry
// has checked the object.
func TestListFiles_PendingSealedUploadThenVerified(t *testing.T) {
	root := t.TempDir()
	store := newRetryTestFileStore()
	uploader := &checksumRetryUploader{}
	s := NewScheduler(WithFileStore(store), WithFileUploader(uploader), WithDataDir(root))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, task.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "mysql-bin.000003")
	if err := os.WriteFile(path, []byte("sealed-for-files-list"), 0o644); err != nil {
		t.Fatal(err)
	}
	store.files[task.ID] = []BinlogFile{{
		TaskID:      task.ID,
		FileName:    "mysql-bin.000003",
		FilePath:    path,
		State:       "SEALED",
		SealedAt:    time.Now(),
		UploadState: "UPLOAD_FAILED",
		UploadError: "upload pending",
		ObjectKey:   "prefix/cluster-a/uuid/mysql-bin.000003",
	}}

	files, err := s.ListFiles(task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].UploadState != "UPLOAD_FAILED" || files[0].Location != "local" || files[0].Checksum != "" {
		t.Fatalf("during recovery: %+v", files)
	}

	stats, err := s.RetryFailedUploads(task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Succeeded != 1 {
		t.Fatalf("retry stats: %+v", stats)
	}
	files, err = s.ListFiles(task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].UploadState != "UPLOADED" || files[0].Checksum != ChecksumMatch || files[0].Location != "both" {
		t.Fatalf("after verify: %+v", files)
	}
}
