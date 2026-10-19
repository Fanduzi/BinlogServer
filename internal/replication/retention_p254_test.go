// Package replication provides module-level functionality for replication.
// input: a task with upload configured, local retention shorter than bucket retention, and a catalog that can fail event appends
// output: proof that deleting the local file of an UPLOADED row still inside the bucket window writes one RETENTION_REMOVED and sets RetentionRemovedAt, and that a failed append opens RETENTION_RECORD_FAILED (design 0009 §26, #341)
// pos: P2-5.4 red tests for local retention with upload configured
// note: if this file changes, update this header and module README.md.
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

func TestP254SplitRetentionWritesRemovedEventAndMarker(t *testing.T) {
	dir := t.TempDir()
	taskID := "task-1"
	taskDir := filepath.Join(dir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	mid := now.Add(-2 * 24 * time.Hour)
	midPath := filepath.Join(taskDir, "mysql-bin.000001")
	if err := os.WriteFile(midPath, []byte("mid"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(midPath, mid, mid); err != nil {
		t.Fatal(err)
	}
	catalog := &purgeCatalog{rows: map[string]tasks.BinlogFile{
		"mysql-bin.000001": {
			TaskID: taskID, FileName: "mysql-bin.000001", FilePath: midPath,
			State: "SEALED", UploadState: "UPLOADED", ObjectKey: "mid-object",
			Checksum: tasks.ChecksumMatch, SealedAt: mid, Epoch: 1,
		},
		"mysql-bin.000007": {
			TaskID: taskID, FileName: "mysql-bin.000007",
			State: "SEALED", UploadState: "UPLOADED", ObjectKey: "young-object",
			Checksum: tasks.ChecksumMatch, SealedAt: mid,
		},
	}}
	runner := NewMySQLRunner(dir, WithFileMetaStore(catalog), WithObjectDeleter(&purgeDeleter{}))
	file, _, _, err := runner.openBinlogWriter(context.Background(), tasks.Task{
		ID: taskID, Epoch: 1, Storage: tasks.Storage{RetentionDays: 7, LocalRetentionDays: 1, BucketRetentionDays: 7},
	}, "mysql-bin.000009", 4, "")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, err := os.Stat(midPath); !os.IsNotExist(err) {
		t.Fatalf("local file still present: %v", err)
	}
	row, ok := catalog.rows["mysql-bin.000001"]
	if !ok {
		t.Fatal("catalog row removed inside bucket retention")
	}
	if n := countRetentionRemoved(catalog.events, "mysql-bin.000001"); n != 1 {
		t.Fatalf("RETENTION_REMOVED events=%d %+v", n, catalog.events)
	}
	if row.RetentionRemovedAt == nil {
		t.Fatal("retention marker was not set on the kept row")
	}
	if n := countRetentionRemoved(catalog.events, "mysql-bin.000007"); n != 0 {
		t.Fatalf("bucket-only row inside the bucket window was marked removed: %+v", catalog.events)
	}
}

func TestP254SplitRetentionEventLossIsFinding(t *testing.T) {
	dir := t.TempDir()
	taskID := "task-1"
	taskDir := filepath.Join(dir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mid := time.Now().Add(-2 * 24 * time.Hour)
	midPath := filepath.Join(taskDir, "mysql-bin.000001")
	if err := os.WriteFile(midPath, []byte("mid"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(midPath, mid, mid); err != nil {
		t.Fatal(err)
	}
	catalog := &purgeCatalog{
		appendErr: errors.New("insert task_events: timeout"),
		rows: map[string]tasks.BinlogFile{
			"mysql-bin.000001": {
				TaskID: taskID, FileName: "mysql-bin.000001", FilePath: midPath,
				State: "SEALED", UploadState: "UPLOADED", ObjectKey: "mid-object",
				Checksum: tasks.ChecksumMatch, SealedAt: mid, Epoch: 1,
			},
		},
	}
	runner := NewMySQLRunner(dir, WithFileMetaStore(catalog), WithObjectDeleter(&purgeDeleter{}))
	rec := bindRecorder(runner)
	file, _, _, err := runner.openBinlogWriter(context.Background(), tasks.Task{
		ID: taskID, Epoch: 1, Storage: tasks.Storage{RetentionDays: 7, LocalRetentionDays: 1, BucketRetentionDays: 7},
	}, "mysql-bin.000009", 4, "")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, ok := rec.has(taskID, tasks.FindingRetentionRecordFailed, "mysql-bin.000001"); !ok {
		t.Fatalf("no RETENTION_RECORD_FAILED: %+v", rec.open)
	}
	if catalog.rows["mysql-bin.000001"].RetentionRemovedAt != nil {
		t.Fatal("marker set even though the event was not written")
	}
}
