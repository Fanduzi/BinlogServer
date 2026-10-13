// Package tasks provides module-level functionality for tasks.
// input: background and manual upload retries against failing task, catalog, and uploader fakes
// output: proof that S7 (retry pass failures), S8 (upload results not recorded) and S9 (stored but unverified objects) end in findings, and that the retry loop's own heartbeat moves on every pass
// pos: fault-injection tests for the upload half of ADR 0009 phase 0
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"testing"
	"time"
)

type upsertFailFileStore struct {
	*retryTestFileStore
	fail error
}

func (s *upsertFailFileStore) UpsertBinlogFile(ctx context.Context, f BinlogFile) error {
	if s.fail != nil {
		return s.fail
	}
	return s.retryTestFileStore.UpsertBinlogFile(ctx, f)
}

func seedFailedUpload(t *testing.T, store *retryTestFileStore, taskID, name string) {
	t.Helper()
	store.files[taskID] = append(store.files[taskID], BinlogFile{
		TaskID: taskID, FileName: name, FilePath: writeRetryTestFile(t, t.TempDir(), name),
		SealedAt: time.Now(), UploadState: "UPLOAD_FAILED", ObjectKey: "p/c/u/" + name,
	})
}

// S7: a background pass that cannot read the task list leaves a finding, and
// the loop's own heartbeat still moves.
func TestS7_RetryPassFailureIsFinding(t *testing.T) {
	store := newFakeStore()
	s := NewScheduler(WithStore(store), WithFileStore(newRetryTestFileStore()), WithFileUploader(&retryTestUploader{}))
	store.listErr = errors.New("server has gone away")
	s.retryKnownFailedUploads()
	f, ok := openFinding(s, ClusterFindingTask, FindingRetryLoopFailed)
	if !ok || f.Subject != "sync" || f.Severity != FindingCritical {
		t.Fatalf("no RETRY_LOOP_FAILED sync: %+v", s.FindingStats().Open)
	}
	if s.UploadRetryPassTS() == 0 {
		t.Fatalf("retry pass heartbeat did not move")
	}
	store.listErr = nil
	s.retryKnownFailedUploads()
	if _, ok := openFinding(s, ClusterFindingTask, FindingRetryLoopFailed); ok {
		t.Fatalf("RETRY_LOOP_FAILED not resolved after a good pass")
	}
}

// S8: the retry uploaded the file but could not write the result.
func TestS8_UploadResultNotRecordedIsFinding(t *testing.T) {
	base := newRetryTestFileStore()
	store := &upsertFailFileStore{retryTestFileStore: base}
	s := NewScheduler(WithFileStore(store), WithFileUploader(&retryTestUploader{}))
	task, err := s.CreateTask("s8", "s8-key")
	if err != nil {
		t.Fatal(err)
	}
	seedFailedUpload(t, base, task.ID, "mysql-bin.000001")
	store.fail = errors.New("lock wait timeout")
	if _, err := s.RetryFailedUploads(task.ID, 10); err != nil {
		t.Fatalf("RetryFailedUploads: %v", err)
	}
	f, ok := openFinding(s, task.ID, FindingUploadRecordFailed)
	if !ok || f.Subject != "mysql-bin.000001" {
		t.Fatalf("no UPLOAD_RECORD_FAILED: %+v", s.FindingStats().Open)
	}
}

// S8 (skip rows): a row the retry cannot use, whose UPLOAD_FAILED record also fails.
func TestS8_RetrySkipRecordFailureIsFinding(t *testing.T) {
	base := newRetryTestFileStore()
	store := &upsertFailFileStore{retryTestFileStore: base}
	s := NewScheduler(WithFileStore(store), WithFileUploader(&retryTestUploader{}))
	task, err := s.CreateTask("s8b", "s8b-key")
	if err != nil {
		t.Fatal(err)
	}
	base.files[task.ID] = []BinlogFile{{TaskID: task.ID, FileName: "mysql-bin.000002", FilePath: "/x/mysql-bin.000002", SealedAt: time.Now(), UploadState: "UPLOAD_FAILED"}}
	store.fail = errors.New("read-only")
	if _, err := s.RetryFailedUploads(task.ID, 10); err != nil {
		t.Fatalf("RetryFailedUploads: %v", err)
	}
	if _, ok := openFinding(s, task.ID, FindingUploadRecordFailed); !ok {
		t.Fatalf("no UPLOAD_RECORD_FAILED for the skipped row: %+v", s.FindingStats().Open)
	}
}

// S9: an uploader that cannot read the object back leaves UPLOADED with an
// empty checksum; that is a WARNING, not a silent green.
func TestS9_UnverifiedUploadIsFinding(t *testing.T) {
	store := newRetryTestFileStore()
	s := NewScheduler(WithFileStore(store), WithFileUploader(&retryTestUploader{}))
	task, err := s.CreateTask("s9", "s9-key")
	if err != nil {
		t.Fatal(err)
	}
	seedFailedUpload(t, store, task.ID, "mysql-bin.000003")
	if _, err := s.RetryFailedUploads(task.ID, 10); err != nil {
		t.Fatalf("RetryFailedUploads: %v", err)
	}
	row, _ := store.get(task.ID, "mysql-bin.000003")
	if row.UploadState != "UPLOADED" || row.Checksum != "" {
		t.Fatalf("row %+v", row)
	}
	f, ok := openFinding(s, task.ID, FindingUploadUnverified)
	if !ok || f.Severity != FindingWarning || f.Subject != "mysql-bin.000003" {
		t.Fatalf("no UPLOAD_UNVERIFIED: %+v", s.FindingStats().Open)
	}
}
