// Package tasks provides module-level functionality for tasks.
// input: a task directory and a checkpoint reader
// output: assertions that AttachStorageAlert reports a stray rotate, stays quiet for out-of-order replica commits, and leaves the stored task unchanged
// pos: list and get surface a backup damaged by a stale GTID re-dump
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"binlog_server/internal/binlog"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

func TestAttachStorageAlert_StrayRotate(t *testing.T) {
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
	writeAlertSegment(t, filepath.Join(taskDir, "mysql-bin.000001"), 39, 41)
	writeAlertSegment(t, filepath.Join(taskDir, "mysql-bin.000002"), 40)
	stored := task
	if got := s.AttachStorageAlert(context.Background(), task); got.StorageAlert != nil {
		t.Fatalf("out-of-order commits set an alert: %+v", got.StorageAlert)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000006"), strayRotateSegment("mysql-bin.000006"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := s.AttachStorageAlert(context.Background(), task)
	if got.StorageAlert == nil || got.StorageAlert.Code != CodeStorageInconsistent {
		t.Fatalf("alert %+v", got.StorageAlert)
	}
	if stored.StorageAlert != nil {
		t.Fatal("stored task was mutated")
	}
}

func strayRotateSegment(next string) []byte {
	body := make([]byte, 8+len(next))
	binary.LittleEndian.PutUint64(body[:8], 4)
	copy(body[8:], next)
	hdr := make([]byte, goreplication.EventHeaderSize)
	hdr[4] = byte(goreplication.ROTATE_EVENT)
	binary.LittleEndian.PutUint32(hdr[9:13], uint32(len(hdr)+len(body)))
	binary.LittleEndian.PutUint32(hdr[13:17], 1758)
	buf := append([]byte{0xfe, 'b', 'i', 'n'}, hdr...)
	return append(buf, body...)
}

func writeAlertSegment(t *testing.T, path string, seqs ...int64) {
	t.Helper()
	// Reuse the window fixture so the GTID events match what the scanner reads.
	if err := os.WriteFile(path, windowSegment(t, clock(t, "2024-01-01 09:00:00"), "", seqs...), 0o644); err != nil {
		t.Fatal(err)
	}
}
