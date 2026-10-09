// Package tasks provides module-level functionality for tasks.
// input: a task directory and a checkpoint reader
// output: assertions that AttachStorageAlert reports a stray rotate with the segment and restart set, refreshes an older STORAGE_INCONSISTENT last_error, stays quiet for out-of-order replica commits, and leaves the stored task unchanged; that replay drops the damaged segment and every later one; and that the recoverable window stops at the same place and reports the damage
// pos: list and get surface a backup damaged by a stale GTID re-dump, and replay stops before the damaged segment
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"binlog_server/internal/binlog"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

func TestAttachStorageAlert_StrayRotate(t *testing.T) {
	dir := t.TempDir()
	reader := &schedulerCheckpointReader{checkpoints: map[string]binlog.Checkpoint{}}
	s := NewScheduler(WithDataDir(dir), WithCheckpointReader(reader))
	task, err := s.CreateTaskFromSpec("mysql", "mysql-key", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: "mysql"}, &StartConfig{Mode: StartModeGTID, GTIDSet: gtidPITRUUID + ":1-36"}, nil)
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
	if got.StorageAlert.Segment != "mysql-bin.000006" {
		t.Fatalf("segment %q", got.StorageAlert.Segment)
	}
	if got.StorageAlert.RestartGTIDSet == "" || !strings.Contains(got.StorageAlert.Message, "Create a new task") {
		t.Fatalf("alert %+v", got.StorageAlert)
	}
	if stored.StorageAlert != nil {
		t.Fatal("stored task was mutated")
	}
	old := task
	old.State = StateFailed
	old.LastError = "STORAGE_INCONSISTENT: Stop the task and do not start it again. Do not restore from these files."
	refreshed := s.AttachStorageAlert(context.Background(), old)
	if refreshed.LastError != CodeStorageInconsistent+": "+refreshed.StorageAlert.Message {
		t.Fatalf("stale last_error kept: %q", refreshed.LastError)
	}
	other := task
	other.State = StateFailed
	other.LastError = "SOURCE_UNREACHABLE: dial tcp: connection refused"
	if got := s.AttachStorageAlert(context.Background(), other); got.LastError != other.LastError {
		t.Fatalf("unrelated last_error rewritten: %q", got.LastError)
	}
}

func TestSelectReplayFilesFor_DropsDamagedAndLater(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(WithDataDir(dir))
	task, err := s.CreateTaskFromSpec("mysql", "mysql-key", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: "mysql"}, &StartConfig{Mode: StartModeGTID, GTIDSet: gtidPITRUUID + ":1-36"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(dir, task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAlertSegment(t, filepath.Join(taskDir, "mysql-bin.000001"), 37)
	writeAlertSegment(t, filepath.Join(taskDir, "mysql-bin.000002"), 38)
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000003"), strayRotateSegment("mysql-bin.000003"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeAlertSegment(t, filepath.Join(taskDir, "mysql-bin.000004"), 40)
	files, err := s.ListFiles(task.ID, 200)
	if err != nil {
		t.Fatal(err)
	}
	chosen, warning := s.SelectReplayFilesFor(task, files)
	if len(chosen) != 2 || !strings.HasSuffix(chosen[0].FilePath, "mysql-bin.000001") || !strings.HasSuffix(chosen[1].FilePath, "mysql-bin.000002") {
		t.Fatalf("chosen %+v", chosen)
	}
	if !strings.Contains(warning, "mysql-bin.000003") || !strings.Contains(warning, "STORAGE_INCONSISTENT") {
		t.Fatalf("warning %q", warning)
	}
	clean := SelectReplayFiles(files)
	if len(clean) < 3 {
		t.Fatalf("SelectReplayFiles should keep the damaged files too: %d", len(clean))
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
	if err := os.WriteFile(path, windowSegment(t, clock(t, "2024-01-01 09:00:00"), "", seqs...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryWindow_StopsBeforeDamagedSegment(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(WithDataDir(dir))
	task, err := s.CreateTaskFromSpec("mysql", "mysql-key", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: "mysql"}, &StartConfig{Mode: StartModeGTID, GTIDSet: gtidPITRUUID + ":1-36"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(dir, task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAlertSegment(t, filepath.Join(taskDir, "mysql-bin.000001"), 37)
	writeAlertSegment(t, filepath.Join(taskDir, "mysql-bin.000002"), 38)
	clean, err := s.RecoveryWindow(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !clean.Continuous {
		t.Fatalf("undamaged window %+v", clean)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000003"), strayRotateSegment("mysql-bin.000003"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeAlertSegment(t, filepath.Join(taskDir, "mysql-bin.000004"), 40)
	got, err := s.RecoveryWindow(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Continuous {
		t.Fatalf("damaged window reported continuous: %+v", got)
	}
	if !gtidContains(t, got.GTIDSet, gtidPITRUUID+":37-38") || gtidContains(t, got.GTIDSet, gtidPITRUUID+":40") {
		t.Fatalf("gtid_set %q includes segments after the damage", got.GTIDSet)
	}
	var damage *RecoveryBreak
	for i := range got.Breaks {
		if strings.Contains(got.Breaks[i].Reason, "STORAGE_INCONSISTENT") {
			damage = &got.Breaks[i]
		}
	}
	if damage == nil || !strings.Contains(damage.Reason, "mysql-bin.000003") {
		t.Fatalf("breaks %+v", got.Breaks)
	}
	if strings.Join(damage.Files, ",") != "mysql-bin.000003,mysql-bin.000004" {
		t.Fatalf("damage files %v", damage.Files)
	}
	chosen, _ := s.SelectReplayFilesFor(task, mustListFiles(t, s, task.ID))
	if len(chosen) != 2 {
		t.Fatalf("replay and window disagree: %+v", chosen)
	}
}

func mustListFiles(t *testing.T, s *Scheduler, id string) []BinlogFile {
	t.Helper()
	files, err := s.ListFiles(id, 200)
	if err != nil {
		t.Fatal(err)
	}
	return files
}
