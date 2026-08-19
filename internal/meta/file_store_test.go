// Package meta provides module-level functionality for meta.
// input: temp data_dir fixtures and task/checkpoint/file snapshots
// output: assertions that standalone file store survives reload and lists open files
// pos: unit tests for file-backed metadata used without meta_dsn
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"
)

func TestFileTaskStore_PersistsTaskAndCheckpoint(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileTaskStore(dir)
	if err != nil {
		t.Fatalf("NewFileTaskStore: %v", err)
	}

	task := tasks.Task{
		ID:         "4",
		Name:       "dogfood",
		ClusterKey: "dogfood",
		State:      tasks.StateRunning,
		Start:      tasks.StartConfig{Mode: tasks.StartModeLatest},
		Source: tasks.SourceConfig{
			Host:     "127.0.0.1",
			Port:     3306,
			User:     "repl",
			Password: "secret",
		},
		UpdatedAt: time.Now(),
	}
	if err := store.UpsertTask(context.Background(), task); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	if err := store.UpsertCheckpoint(context.Background(), task.ID, binlog.Checkpoint{
		File: "binlog.000002",
		Pos:  1205,
	}); err != nil {
		t.Fatalf("UpsertCheckpoint: %v", err)
	}

	reloaded, err := NewFileTaskStore(dir)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	list, err := reloaded.ListTasks(context.Background())
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(list) != 1 || list[0].ID != "4" || list[0].Start.Mode != tasks.StartModeLatest {
		t.Fatalf("unexpected restored tasks: %+v", list)
	}
	checkpoint, ok, err := reloaded.LoadCheckpoint(context.Background(), "4")
	if err != nil || !ok {
		t.Fatalf("LoadCheckpoint ok=%v err=%v", ok, err)
	}
	if checkpoint.File != "binlog.000002" || checkpoint.Pos != 1205 {
		t.Fatalf("unexpected checkpoint: %+v", checkpoint)
	}
}

func TestFileTaskStore_ListFilesIncludesOpenSegment(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileTaskStore(dir)
	if err != nil {
		t.Fatalf("NewFileTaskStore: %v", err)
	}
	taskDir := filepath.Join(dir, "4")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "binlog.000002"), []byte("binlog"), 0o644); err != nil {
		t.Fatalf("write open segment: %v", err)
	}

	files, err := store.ListBinlogFiles(context.Background(), "4", 200)
	if err != nil {
		t.Fatalf("ListBinlogFiles: %v", err)
	}
	if len(files) != 1 || files[0].FileName != "binlog.000002" {
		t.Fatalf("expected open segment listed, got %+v", files)
	}
}
