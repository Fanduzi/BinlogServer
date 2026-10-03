// Package tasks provides module-level functionality for tasks.
// input: temporary data directories with sealed and open binlog segment names
// output: assertions for disk listing order, catalog fallback, and checkpoint absence
// pos: regression coverage for standalone files listing when meta has no catalog rows
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"binlog_server/internal/binlog"
)

func TestListTaskBinlogFilesOnDisk_OrderAndOpenPath(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(filepath.Join(taskDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(taskDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("mysql-bin.000010", "ten")
	write("notes.txt", "skip")
	write("mysql-bin.000004.open.e9", "e9")
	write("mysql-bin.000003", "sealed")
	write("mysql-bin.000004.open.e1", "e1")
	write("mariadb-bin.000001", "maria")

	files, err := listTaskBinlogFilesOnDisk(dir, "1", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name  string
		base  string
		state string
	}{
		{"mariadb-bin.000001", "mariadb-bin.000001", "SEALED"},
		{"mysql-bin.000003", "mysql-bin.000003", "SEALED"},
		{"mysql-bin.000004", "mysql-bin.000004.open.e1", "OPEN"},
		{"mysql-bin.000004", "mysql-bin.000004.open.e9", "OPEN"},
		{"mysql-bin.000010", "mysql-bin.000010", "SEALED"},
	}
	if len(files) != len(want) {
		t.Fatalf("got %d files: %+v", len(files), files)
	}
	for i, item := range want {
		got := files[i]
		if got.FileName != item.name || got.State != item.state || filepath.Base(got.FilePath) != item.base {
			t.Fatalf("files[%d]=name %s state %s path %s, want name %s state %s base %s", i, got.FileName, got.State, got.FilePath, item.name, item.state, item.base)
		}
		if got.FilePath != filepath.Join(taskDir, item.base) {
			t.Fatalf("files[%d] path %s", i, got.FilePath)
		}
		if got.UploadState != "LOCAL_ONLY" {
			t.Fatalf("files[%d] upload state %s", i, got.UploadState)
		}
	}
	if files[2].SizeBytes != int64(len("e1")) {
		t.Fatalf("open size %d", files[2].SizeBytes)
	}
}

func TestListTaskBinlogFilesOnDisk_LimitKeepsHighestIndexes(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "9")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mysql-bin.000001", "mysql-bin.000002", "mysql-bin.000003.open.e1"} {
		if err := os.WriteFile(filepath.Join(taskDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := listTaskBinlogFilesOnDisk(dir, "9", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d", len(files))
	}
	if filepath.Base(files[0].FilePath) != "mysql-bin.000002" || filepath.Base(files[1].FilePath) != "mysql-bin.000003.open.e1" {
		t.Fatalf("window = %s, %s", files[0].FilePath, files[1].FilePath)
	}
}

func TestListTaskBinlogFilesOnDisk_MissingOrUnsafe(t *testing.T) {
	dir := t.TempDir()
	files, err := listTaskBinlogFilesOnDisk(dir, "absent", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("missing dir returned %#v", files)
	}
	if _, err := listTaskBinlogFilesOnDisk(dir, "../secret", 10); err != nil {
		t.Fatal(err)
	}
	escaped, err := listTaskBinlogFilesOnDisk(dir, "../secret", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(escaped) != 0 {
		t.Fatalf("escaped id returned %#v", escaped)
	}
}

func TestScheduler_ListFiles_EmptyCatalogUsesDisk(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000008"), []byte("sealed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000009.open.e2"), []byte("open"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := newFakeFileStore()
	s := NewScheduler(WithFileStore(store), WithDataDir(dir))
	if _, err := s.CreateTask("cluster-a", "cluster-a-key"); err != nil {
		t.Fatal(err)
	}
	files, err := s.ListFiles("1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d: %+v", len(files), files)
	}
	if files[0].State != "SEALED" || filepath.Base(files[0].FilePath) != "mysql-bin.000008" {
		t.Fatalf("first %+v", files[0])
	}
	if files[1].FileName != "mysql-bin.000009" || files[1].State != "OPEN" || filepath.Base(files[1].FilePath) != "mysql-bin.000009.open.e2" {
		t.Fatalf("second %+v", files[1])
	}
}

func TestScheduler_ListFiles_CatalogWinsOverDisk(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000099"), []byte("disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := newFakeFileStore()
	store.files["1"] = []BinlogFile{{
		TaskID:   "1",
		FileName: "mysql-bin.000001",
		FilePath: "/meta/mysql-bin.000001",
		State:    "SEALED",
	}}
	s := NewScheduler(WithFileStore(store), WithDataDir(dir))
	if _, err := s.CreateTask("cluster-a", "cluster-a-key"); err != nil {
		t.Fatal(err)
	}
	files, err := s.ListFiles("1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].FilePath != "/meta/mysql-bin.000001" {
		t.Fatalf("catalog result lost: %+v", files)
	}
}

type errListFileStore struct{}

func (errListFileStore) UpsertBinlogFile(context.Context, BinlogFile) error { return nil }
func (errListFileStore) ListBinlogFiles(context.Context, string, int) ([]BinlogFile, error) {
	return nil, errors.New("db down")
}

func TestScheduler_ListFiles_StoreErrorDoesNotScanDisk(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000001"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewScheduler(WithFileStore(errListFileStore{}), WithDataDir(dir))
	if _, err := s.CreateTask("cluster-a", "cluster-a-key"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListFiles("1", 10); err == nil || err.Error() != "db down" {
		t.Fatalf("err = %v", err)
	}
}

func TestGetCheckpoint_DiskFilesDoNotInventRow(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000001"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewScheduler(WithDataDir(dir))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatal(err)
	}
	cp, ok, err := s.GetCheckpoint(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok || cp.File != "" || cp.Pos != 0 {
		t.Fatalf("invented checkpoint ok=%v %+v", ok, cp)
	}

	reader := &schedulerCheckpointReader{checkpoints: map[string]binlog.Checkpoint{
		task.ID: {File: "mysql-bin.000001", Pos: 128},
	}}
	withReader := NewScheduler(WithDataDir(dir), WithCheckpointReader(reader))
	if _, err := withReader.CreateTask("cluster-a", "cluster-a-key"); err != nil {
		t.Fatal(err)
	}
	cp, ok, err = withReader.GetCheckpoint(context.Background(), "1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || cp.File != "mysql-bin.000001" || cp.Pos != 128 {
		t.Fatalf("stored checkpoint lost: ok=%v %+v", ok, cp)
	}
}
