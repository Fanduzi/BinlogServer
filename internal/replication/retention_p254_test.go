// Package replication provides module-level functionality for replication.
// input: a task with upload configured, local retention shorter than bucket retention, and a catalog that can fail event appends or report the 000010 marker column absent
// output: proof that deleting the local file of an UPLOADED row still inside the bucket window writes one RETENTION_REMOVED and sets RetentionRemovedAt, that a failed append opens RETENTION_RECORD_FAILED (design 0009 §26, #341), and that after the marker column appears the next pass writes only the marker (#437)
// pos: P2-5.4 red tests for local retention with upload configured, including the online 000010 catch-up
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

// markerFlipCatalog is a purgeCatalog whose 000010 column can appear between
// retention passes. unsupported is what MySQL 1054 becomes
// (tasks.ErrRetentionMarkerUnsupported).
type markerFlipCatalog struct {
	*purgeCatalog
	unsupported bool
}

func (c *markerFlipCatalog) RetentionMarked(ctx context.Context, taskID, fileName string, epoch int64) (bool, error) {
	if c.unsupported {
		return false, tasks.ErrRetentionMarkerUnsupported
	}
	return c.purgeCatalog.RetentionMarked(ctx, taskID, fileName, epoch)
}

func (c *markerFlipCatalog) MarkRetentionRemoved(ctx context.Context, taskID, fileName string, epoch int64, at time.Time) error {
	if c.unsupported {
		return tasks.ErrRetentionMarkerUnsupported
	}
	return c.purgeCatalog.MarkRetentionRemoved(ctx, taskID, fileName, epoch, at)
}

// TestOnlineMigrateUpWritesRetentionMarkerWithoutSecondEvent is #437.
// A pass while the marker column is absent writes one RETENTION_REMOVED and
// remembers the key. Later passes still do not write the marker, and they
// must not write a second event. After an online migrate up the column is
// present; the next pass writes only the marker.
func TestOnlineMigrateUpWritesRetentionMarkerWithoutSecondEvent(t *testing.T) {
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
	catalog := &markerFlipCatalog{
		unsupported: true,
		purgeCatalog: &purgeCatalog{rows: map[string]tasks.BinlogFile{
			"mysql-bin.000001": {
				TaskID: taskID, FileName: "mysql-bin.000001", FilePath: midPath,
				State: "SEALED", UploadState: "UPLOADED", ObjectKey: "mid-object",
				Checksum: tasks.ChecksumMatch, SealedAt: mid, Epoch: 1,
			},
		}},
	}
	runner := NewMySQLRunner(dir, WithFileMetaStore(catalog), WithObjectDeleter(&purgeDeleter{}))
	task := tasks.Task{
		ID: taskID, Epoch: 1, Storage: tasks.Storage{RetentionDays: 7, LocalRetentionDays: 1, BucketRetentionDays: 7},
	}
	open := func(name string) {
		t.Helper()
		file, _, _, err := runner.openBinlogWriter(context.Background(), task, name, 4, "")
		if err != nil {
			t.Fatal(err)
		}
		file.Close()
	}

	open("mysql-bin.000009")
	if _, err := os.Stat(midPath); !os.IsNotExist(err) {
		t.Fatalf("local file still present: %v", err)
	}
	if n := countRetentionRemoved(catalog.events, "mysql-bin.000001"); n != 1 {
		t.Fatalf("first pass events=%d %+v", n, catalog.events)
	}
	if catalog.rows["mysql-bin.000001"].RetentionRemovedAt != nil {
		t.Fatal("marker set while the column was absent")
	}

	open("mysql-bin.000010")
	if n := countRetentionRemoved(catalog.events, "mysql-bin.000001"); n != 1 {
		t.Fatalf("column still absent, events=%d %+v", n, catalog.events)
	}
	if catalog.rows["mysql-bin.000001"].RetentionRemovedAt != nil {
		t.Fatal("marker set while the column was still absent")
	}

	catalog.unsupported = false
	open("mysql-bin.000011")
	if n := countRetentionRemoved(catalog.events, "mysql-bin.000001"); n != 1 {
		t.Fatalf("after the column appeared, events=%d %+v", n, catalog.events)
	}
	if catalog.rows["mysql-bin.000001"].RetentionRemovedAt == nil {
		t.Fatal("marker still NULL after the column appeared")
	}
}
