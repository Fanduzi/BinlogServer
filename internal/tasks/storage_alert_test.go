// Package tasks provides module-level functionality for tasks.
// input: a task directory and a checkpoint reader
// output: assertions that AttachStorageAlert reports a checkpoint ahead of the files and leaves the stored task unchanged
// pos: list and get surface a backup damaged by a stale GTID re-dump
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"binlog_server/internal/binlog"
)

func TestAttachStorageAlert_CheckpointAhead(t *testing.T) {
	dir := t.TempDir()
	reader := &schedulerCheckpointReader{checkpoints: map[string]binlog.Checkpoint{}}
	s := NewScheduler(WithDataDir(dir), WithCheckpointReader(reader))
	task, err := s.CreateTaskFromSpec("mysql", "mysql-key", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: "mysql"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	reader.checkpoints[task.ID] = binlog.Checkpoint{File: "mysql-bin.000006", Pos: 1758, GTIDSet: gtidPITRUUID + ":1-44"}
	taskDir := filepath.Join(dir, task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAlertSegment(t, filepath.Join(taskDir, "mysql-bin.000006"), 37, 38, 39, 43, 44)
	stored := task
	got := s.AttachStorageAlert(context.Background(), task)
	if got.StorageAlert == nil || got.StorageAlert.Code != CodeStorageInconsistent {
		t.Fatalf("alert %+v", got.StorageAlert)
	}
	if stored.StorageAlert != nil {
		t.Fatal("stored task was mutated")
	}
	if _, found := binlog.DetectStorageProblem(taskDir, gtidPITRUUID+":1-39", "mysql"); found {
		t.Fatal("a checkpoint that does not claim the missing sequences is not an alert")
	}
}

func writeAlertSegment(t *testing.T, path string, seqs ...int64) {
	t.Helper()
	// Reuse the window fixture so the GTID events match what the scanner reads.
	if err := os.WriteFile(path, windowSegment(t, clock(t, "2024-01-01 09:00:00"), "", seqs...), 0o644); err != nil {
		t.Fatal(err)
	}
}
