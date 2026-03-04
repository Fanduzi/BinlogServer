// Package tasks provides module-level functionality for tasks.
// input: scheduler store/lease/uploader dependencies and timeout option configuration
// output: timeout-boundary regression coverage for internal read/lease/upload calls
// pos: scheduler timeout governance tests for internal dependency interactions
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var errTimeoutTestMissingDeadline = errors.New("missing deadline on internal call")

type timeoutListStore struct {
	tasks []Task
}

func (s *timeoutListStore) UpsertTask(context.Context, Task) error { return nil }
func (s *timeoutListStore) DeleteTask(context.Context, string) error {
	return nil
}
func (s *timeoutListStore) ListTasks(ctx context.Context) ([]Task, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errTimeoutTestMissingDeadline
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

type timeoutLeaseManager struct{}

func (m *timeoutLeaseManager) Acquire(ctx context.Context, _ string, _ string, _ time.Duration) (int64, bool, error) {
	if _, ok := ctx.Deadline(); !ok {
		return 0, false, errTimeoutTestMissingDeadline
	}
	<-ctx.Done()
	return 0, false, ctx.Err()
}
func (m *timeoutLeaseManager) Renew(ctx context.Context, _ string, _ string, _ int64, _ time.Time, _ time.Duration) (bool, error) {
	if _, ok := ctx.Deadline(); !ok {
		return false, errTimeoutTestMissingDeadline
	}
	<-ctx.Done()
	return false, ctx.Err()
}
func (m *timeoutLeaseManager) Release(ctx context.Context, _ string, _ string, _ int64) (bool, error) {
	if _, ok := ctx.Deadline(); !ok {
		return false, errTimeoutTestMissingDeadline
	}
	<-ctx.Done()
	return false, ctx.Err()
}

type timeoutUploader struct{}

func (u *timeoutUploader) UploadFile(ctx context.Context, _ string, _ string, _ string) error {
	if _, ok := ctx.Deadline(); !ok {
		return errTimeoutTestMissingDeadline
	}
	<-ctx.Done()
	return ctx.Err()
}

// TestScheduler_GetTaskUsesReadTimeout 验证相关行为。
func TestScheduler_GetTaskUsesReadTimeout(t *testing.T) {
	s := NewScheduler(
		WithStore(&timeoutListStore{}),
		WithInternalCallTimeouts(InternalCallTimeouts{
			Read: 20 * time.Millisecond,
		}),
	)

	_, err := s.GetTask("1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded, got %v", err)
	}
}

// TestScheduler_StartTaskUsesLeaseTimeout 验证相关行为。
func TestScheduler_StartTaskUsesLeaseTimeout(t *testing.T) {
	s := NewScheduler(
		WithRunner(&fakeRunner{started: make(chan Task, 1)}),
		WithClusterLeaseManager(&timeoutLeaseManager{}),
		WithClusterWorkerID("worker-a"),
		WithInternalCallTimeouts(InternalCallTimeouts{
			Lease: 20 * time.Millisecond,
		}),
	)

	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{
		Host: "127.0.0.1",
		Port: 3306,
		User: "root",
	}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}

	err = s.StartTask(task.ID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded, got %v", err)
	}
}

// TestScheduler_RetryFailedUploadsUsesUploadTimeout 验证相关行为。
func TestScheduler_RetryFailedUploadsUsesUploadTimeout(t *testing.T) {
	tmpDir := t.TempDir()
	fileStore := newRetryTestFileStore()
	uploader := &timeoutUploader{}
	s := NewScheduler(
		WithFileStore(fileStore),
		WithFileUploader(uploader),
		WithInternalCallTimeouts(InternalCallTimeouts{
			Upload: 20 * time.Millisecond,
		}),
	)

	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	fileStore.files[task.ID] = []BinlogFile{
		{
			TaskID:      task.ID,
			FileName:    "mysql-bin.000001",
			FilePath:    writeRetryTestFile(t, tmpDir, "mysql-bin.000001"),
			SealedAt:    time.Now(),
			UploadState: "UPLOAD_FAILED",
			ObjectKey:   "prefix/cluster-a/uuid/mysql-bin.000001",
		},
	}

	stats, err := s.RetryFailedUploads(task.ID, 10)
	if err != nil {
		t.Fatalf("RetryFailedUploads returned error: %v", err)
	}
	if stats.Failed != 1 {
		t.Fatalf("expected failed=1, got %d", stats.Failed)
	}
	item, ok := fileStore.get(task.ID, "mysql-bin.000001")
	if !ok {
		t.Fatal("expected retry file metadata present")
	}
	if !strings.Contains(item.UploadError, context.DeadlineExceeded.Error()) {
		t.Fatalf("expected upload error captured, got %q", item.UploadError)
	}
}
