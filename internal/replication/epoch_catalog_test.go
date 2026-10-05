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

// v0533Catalog is the shape left by v0.5.33: every binlog_files.epoch is 0,
// including the open segment whose path already ends in .open.eN.
func v0533Catalog(taskID, taskDir string) *epochCatalog {
	return &epochCatalog{rows: []tasks.BinlogFile{
		{
			TaskID: taskID, FileName: "mysql-bin.000001", FilePath: filepath.Join(taskDir, "mysql-bin.000001"),
			Epoch: 0, State: "SEALED", UploadState: "UPLOADED", ObjectKey: "e2e/legacy/mysql-bin.000001",
			Checksum: tasks.ChecksumMatch,
		},
		{
			TaskID: taskID, FileName: "mysql-bin.000002", FilePath: filepath.Join(taskDir, "mysql-bin.000002"),
			Epoch: 0, State: "SEALED", UploadState: "UPLOADED", ObjectKey: "e2e/legacy/mysql-bin.000002",
			Checksum: tasks.ChecksumMatch,
		},
		{
			TaskID: taskID, FileName: "mysql-bin.000003", FilePath: filepath.Join(taskDir, "mysql-bin.000003"),
			Epoch: 0, State: "SEALED", UploadState: "UPLOAD_FAILED", ObjectKey: "e2e/legacy/mysql-bin.000003",
			UploadError: "boom",
		},
		{
			TaskID: taskID, FileName: "mysql-bin.000004",
			FilePath: filepath.Join(taskDir, "mysql-bin.000004.open.e1"),
			Epoch:    0, State: "OPEN", UploadState: "LOCAL_ONLY",
		},
	}}
}

// backfillV0533Epoch applies the 000002 path rule: .open.eN and .sealed.eN
// take that epoch. A plain sealed name stays 0.
func backfillV0533Epoch(cat *epochCatalog) {
	for i := range cat.rows {
		base := filepath.Base(cat.rows[i].FilePath)
		if n, ok := epochSuffix(base, ".open.e"); ok {
			cat.rows[i].Epoch = n
		}
		if n, ok := epochSuffix(base, ".sealed.e"); ok {
			cat.rows[i].Epoch = n
		}
	}
}

func epochSuffix(name, mark string) (int64, bool) {
	idx := strings.LastIndex(name, mark)
	if idx <= 0 || idx+len(mark) >= len(name) {
		return 0, false
	}
	n := int64(0)
	for _, r := range name[idx+len(mark):] {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int64(r-'0')
	}
	return n, true
}

func TestUpgradeBackfillRotateKeepsOneRowAndLegacyKeys(t *testing.T) {
	dir := t.TempDir()
	taskID := "9"
	taskDir := filepath.Join(dir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mysql-bin.000001", "mysql-bin.000002", "mysql-bin.000003", "mysql-bin.000004.open.e1"} {
		if err := os.WriteFile(filepath.Join(taskDir, name), []byte{0xfe, 'b', 'i', 'n'}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cat := v0533Catalog(taskID, taskDir)
	backfillV0533Epoch(cat)
	if row, ok := cat.find("mysql-bin.000004", 1); !ok || row.State != "OPEN" {
		t.Fatalf("backfill open row %+v ok=%v", row, ok)
	}
	if _, ok := cat.find("mysql-bin.000004", 0); ok {
		t.Fatal("epoch 0 open row still present after backfill")
	}
	runner := NewMySQLRunner(dir, WithFileMetaStore(cat))
	task := tasks.Task{ID: taskID, Epoch: 1, Storage: tasks.Storage{RetentionDays: 7}}
	file, _, path, err := runner.openBinlogWriter(context.Background(), task, "mysql-bin.000004", 4, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if err := runner.finalizeSealedFile(context.Background(), task, "", path, 4, 4, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	var current []tasks.BinlogFile
	for _, row := range cat.rows {
		if row.FileName == "mysql-bin.000004" {
			current = append(current, row)
		}
	}
	if len(current) != 1 || current[0].State != "SEALED" || current[0].Epoch != 1 || filepath.Base(current[0].FilePath) != "mysql-bin.000004" {
		t.Fatalf("current segment %+v", current)
	}
	for _, name := range []string{"mysql-bin.000001", "mysql-bin.000002"} {
		row, ok := cat.find(name, 0)
		if !ok || row.UploadState != "UPLOADED" || row.Checksum != tasks.ChecksumMatch || row.ObjectKey != "e2e/legacy/"+name {
			t.Fatalf("legacy %s %+v", name, row)
		}
	}
	failed, ok := cat.find("mysql-bin.000003", 0)
	if !ok || failed.UploadState != "UPLOAD_FAILED" {
		t.Fatalf("failed row %+v", failed)
	}
	replay := tasks.SelectReplayFiles(cat.rows)
	for _, row := range replay {
		if strings.Contains(row.FilePath, ".open.e") {
			t.Fatalf("replay still names a removed open path: %+v", replay)
		}
	}
	if len(replay) != 4 {
		t.Fatalf("replay %+v", replay)
	}
}

func TestUpgradeDropsOpenRowWhoseFileWasRemoved(t *testing.T) {
	dir := t.TempDir()
	taskID := "9"
	taskDir := filepath.Join(dir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	openPath := filepath.Join(taskDir, "mysql-bin.000004.open.e1")
	if err := os.WriteFile(openPath, []byte{0xfe, 'b', 'i', 'n'}, 0o644); err != nil {
		t.Fatal(err)
	}
	cat := &epochCatalog{rows: []tasks.BinlogFile{{
		TaskID: taskID, FileName: "mysql-bin.000004", FilePath: openPath,
		Epoch: 0, State: "OPEN", UploadState: "LOCAL_ONLY",
	}}}
	backfillV0533Epoch(cat)
	runner := NewMySQLRunner(dir, WithFileMetaStore(cat))
	file, _, _, err := runner.openBinlogWriter(context.Background(), tasks.Task{
		ID: taskID, Epoch: 2, Storage: tasks.Storage{RetentionDays: 7},
	}, "mysql-bin.000005", 4, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err := os.Stat(openPath); !os.IsNotExist(err) {
		t.Fatalf("stale open file: %v", err)
	}
	for _, row := range cat.rows {
		if row.FileName == "mysql-bin.000004" {
			t.Fatalf("stale open catalog row %+v", row)
		}
	}
	replay := tasks.SelectReplayFiles(cat.rows)
	for _, row := range replay {
		if strings.Contains(row.FilePath, "mysql-bin.000004") {
			t.Fatalf("replay %+v", replay)
		}
	}
}

func TestUpgradeRetentionPurgesUploadedAndKeepsFailed(t *testing.T) {
	dir := t.TempDir()
	taskID := "9"
	taskDir := filepath.Join(dir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * 24 * time.Hour)
	for _, name := range []string{"mysql-bin.000001", "mysql-bin.000002", "mysql-bin.000003", "mysql-bin.000004.open.e1"} {
		path := filepath.Join(taskDir, name)
		if err := os.WriteFile(path, []byte{0xfe, 'b', 'i', 'n'}, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	cat := v0533Catalog(taskID, taskDir)
	backfillV0533Epoch(cat)
	deleter := &purgeDeleter{}
	runner := NewMySQLRunner(dir, WithFileMetaStore(cat), WithObjectDeleter(deleter))
	file, _, _, err := runner.openBinlogWriter(context.Background(), tasks.Task{
		ID: taskID, Epoch: 1, ClusterKey: "cluster", Storage: tasks.Storage{RetentionDays: 7},
	}, "mysql-bin.000004", 4, "uuid")
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	for _, name := range []string{"mysql-bin.000001", "mysql-bin.000002"} {
		if _, ok := cat.find(name, 0); ok {
			t.Fatalf("uploaded %s kept", name)
		}
		if _, err := os.Stat(filepath.Join(taskDir, name)); !os.IsNotExist(err) {
			t.Fatalf("uploaded file %s: %v", name, err)
		}
	}
	failed, ok := cat.find("mysql-bin.000003", 0)
	if !ok || failed.UploadState != "UPLOAD_FAILED" || failed.ObjectKey != "e2e/legacy/mysql-bin.000003" {
		t.Fatalf("failed row %+v", failed)
	}
	if _, err := os.Stat(filepath.Join(taskDir, "mysql-bin.000003")); err != nil {
		t.Fatalf("failed file: %v", err)
	}
	open, ok := cat.find("mysql-bin.000004", 1)
	if !ok || open.State != "OPEN" || !strings.HasSuffix(open.FilePath, "mysql-bin.000004.open.e1") {
		t.Fatalf("open row %+v", open)
	}
	if len(deleter.keys) != 2 {
		t.Fatalf("deleted objects %v", deleter.keys)
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
