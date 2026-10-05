// Package replication provides module-level functionality for replication.
// input: a sealed uploaded catalog row and a later open epoch of the same source file
// output: proof the later epoch does not replace the sealed path, upload state, checksum, or object key, and that retention ages each epoch on its own state
// pos: regression coverage for one catalog row per durable epoch segment
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/tasks"
)

type epochCatalog struct {
	rows []tasks.BinlogFile
}

func (c *epochCatalog) UpsertBinlogFile(_ context.Context, meta tasks.BinlogFile) error {
	for i := range c.rows {
		if c.rows[i].FileName == meta.FileName && c.rows[i].Epoch == meta.Epoch && c.rows[i].TaskID == meta.TaskID {
			c.rows[i] = meta
			return nil
		}
	}
	c.rows = append(c.rows, meta)
	return nil
}

func (c *epochCatalog) ListBinlogFiles(_ context.Context, taskID string, limit int) ([]tasks.BinlogFile, error) {
	out := make([]tasks.BinlogFile, 0, len(c.rows))
	for _, row := range c.rows {
		if row.TaskID == taskID {
			out = append(out, row)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (c *epochCatalog) DeleteBinlogFile(_ context.Context, taskID, fileName string, epoch int64) error {
	kept := c.rows[:0]
	for _, row := range c.rows {
		if row.TaskID == taskID && row.FileName == fileName && row.Epoch == epoch {
			continue
		}
		kept = append(kept, row)
	}
	c.rows = kept
	return nil
}

func (c *epochCatalog) find(name string, epoch int64) (tasks.BinlogFile, bool) {
	for _, row := range c.rows {
		if row.FileName == name && row.Epoch == epoch {
			return row, true
		}
	}
	return tasks.BinlogFile{}, false
}

func TestOpenEpochDoesNotEraseSealedUpload(t *testing.T) {
	dir := t.TempDir()
	taskID := "9"
	taskDir := filepath.Join(dir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sealedPath := filepath.Join(taskDir, "mysql-bin.000176")
	if err := os.WriteFile(sealedPath, []byte("sealed-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	cat := &epochCatalog{rows: []tasks.BinlogFile{{
		TaskID:      taskID,
		FileName:    "mysql-bin.000176",
		FilePath:    sealedPath,
		Epoch:       0,
		State:       "SEALED",
		UploadState: "UPLOADED",
		ObjectKey:   "e2e/legacy/mysql-bin.000176",
		Checksum:    tasks.ChecksumMatch,
	}}}
	runner := NewMySQLRunner(dir, WithFileMetaStore(cat))
	file, _, path, err := runner.openBinlogWriter(context.Background(), tasks.Task{
		ID:      taskID,
		Epoch:   2,
		Storage: tasks.Storage{RetentionDays: 7},
	}, "mysql-bin.000176", 4, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if !strings.HasSuffix(path, "mysql-bin.000176.open.e2") {
		t.Fatalf("open path %s", path)
	}
	sealed, ok := cat.find("mysql-bin.000176", 0)
	if !ok {
		t.Fatal("sealed row missing")
	}
	if sealed.FilePath != sealedPath || sealed.State != "SEALED" || sealed.UploadState != "UPLOADED" || sealed.ObjectKey != "e2e/legacy/mysql-bin.000176" || sealed.Checksum != tasks.ChecksumMatch {
		t.Fatalf("sealed row changed: %+v", sealed)
	}
	open, ok := cat.find("mysql-bin.000176", 2)
	if !ok || open.State != "OPEN" || open.FilePath != path || open.UploadState != "LOCAL_ONLY" || open.ObjectKey != "" {
		t.Fatalf("open row %+v ok=%v", open, ok)
	}
	if _, err := os.Stat(sealedPath); err != nil {
		t.Fatalf("sealed bytes: %v", err)
	}
}

func TestSealLaterEpochUsesDistinctObjectKey(t *testing.T) {
	dir := t.TempDir()
	taskID := "9"
	taskDir := filepath.Join(dir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(taskDir, "mysql-bin.000176")
	openPath := filepath.Join(taskDir, "mysql-bin.000176.open.e3")
	if err := os.WriteFile(plain, []byte("first-seal"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(openPath, []byte("second-seal"), 0o644); err != nil {
		t.Fatal(err)
	}
	cat := &epochCatalog{rows: []tasks.BinlogFile{{
		TaskID:      taskID,
		FileName:    "mysql-bin.000176",
		FilePath:    plain,
		Epoch:       0,
		State:       "SEALED",
		UploadState: "UPLOADED",
		ObjectKey:   "prefix/cluster/uuid/mysql-bin.000176",
		Checksum:    tasks.ChecksumMatch,
	}}}
	uploader := &fakeUploader{}
	runner := NewMySQLRunner(dir, WithFileMetaStore(cat), WithUploader(uploader, "prefix"))
	err := runner.finalizeSealedFile(context.Background(), tasks.Task{
		ID: taskID, Epoch: 3, ClusterKey: "cluster", Storage: tasks.Storage{RetentionDays: 7},
	}, "uuid", openPath, 4, 40, time.Now(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sealedName := "mysql-bin.000176.sealed.e3"
	if _, err := os.Stat(filepath.Join(taskDir, sealedName)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(plain); err != nil {
		t.Fatalf("plain sealed file: %v", err)
	}
	old, ok := cat.find("mysql-bin.000176", 0)
	if !ok || old.ObjectKey != "prefix/cluster/uuid/mysql-bin.000176" || old.UploadState != "UPLOADED" || old.Checksum != tasks.ChecksumMatch || old.FilePath != plain {
		t.Fatalf("old row %+v", old)
	}
	next, ok := cat.find("mysql-bin.000176", 3)
	if !ok || !strings.HasSuffix(next.ObjectKey, "/"+sealedName) || next.State != "SEALED" {
		t.Fatalf("new row %+v", next)
	}
}

func TestFirstSealKeepsHistoricalObjectKey(t *testing.T) {
	dir := t.TempDir()
	taskID := "9"
	taskDir := filepath.Join(dir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	openPath := filepath.Join(taskDir, "mysql-bin.000176.open.e2")
	if err := os.WriteFile(openPath, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	cat := &epochCatalog{}
	runner := NewMySQLRunner(dir, WithFileMetaStore(cat), WithUploader(&fakeUploader{}, "prefix"))
	err := runner.finalizeSealedFile(context.Background(), tasks.Task{
		ID: taskID, Epoch: 2, ClusterKey: "cluster", Storage: tasks.Storage{RetentionDays: 7},
	}, "uuid", openPath, 4, 20, time.Now(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(taskDir, "mysql-bin.000176")); err != nil {
		t.Fatal(err)
	}
	row, ok := cat.find("mysql-bin.000176", 2)
	if !ok || !strings.HasSuffix(row.ObjectKey, "/mysql-bin.000176") || strings.Contains(row.ObjectKey, ".sealed.") {
		t.Fatalf("row %+v", row)
	}
}

func TestRetentionAgesEachEpoch(t *testing.T) {
	dir := t.TempDir()
	taskID := "9"
	taskDir := filepath.Join(dir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * 24 * time.Hour)
	plain := filepath.Join(taskDir, "mysql-bin.000176")
	failed := filepath.Join(taskDir, "mysql-bin.000176.sealed.e4")
	if err := os.WriteFile(plain, []byte("uploaded"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(failed, []byte("failed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(plain, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(failed, old, old); err != nil {
		t.Fatal(err)
	}
	cat := &epochCatalog{rows: []tasks.BinlogFile{
		{
			TaskID: taskID, FileName: "mysql-bin.000176", FilePath: plain, Epoch: 0,
			State: "SEALED", UploadState: "UPLOADED", ObjectKey: "legacy-key", Checksum: tasks.ChecksumMatch,
		},
		{
			TaskID: taskID, FileName: "mysql-bin.000176", FilePath: failed, Epoch: 4,
			State: "SEALED", UploadState: "UPLOAD_FAILED", ObjectKey: "failed-key", UploadError: "boom",
		},
	}}
	deleter := &purgeDeleter{}
	runner := NewMySQLRunner(dir, WithFileMetaStore(cat), WithObjectDeleter(deleter))
	file, _, _, err := runner.openBinlogWriter(context.Background(), tasks.Task{
		ID: taskID, Epoch: 5, Storage: tasks.Storage{RetentionDays: 7},
	}, "mysql-bin.000177", 4, "uuid")
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err := os.Stat(plain); err == nil {
		t.Fatal("uploaded epoch still on disk")
	}
	if _, err := os.Stat(failed); err != nil {
		t.Fatalf("failed epoch: %v", err)
	}
	if _, ok := cat.find("mysql-bin.000176", 0); ok {
		t.Fatal("uploaded catalog row kept")
	}
	kept, ok := cat.find("mysql-bin.000176", 4)
	if !ok || kept.UploadState != "UPLOAD_FAILED" {
		t.Fatalf("failed row %+v", kept)
	}
	if len(deleter.keys) != 1 || deleter.keys[0] != "legacy-key" {
		t.Fatalf("deleted %v", deleter.keys)
	}
}
