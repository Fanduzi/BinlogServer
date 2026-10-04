// Package tasks provides module-level functionality for tasks.
// input: local open segments and stored checkpoints
// output: proof that NextResumePosition matches the position Start continues from
// pos: regression coverage for the operator-visible resume identity
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"binlog_server/internal/binlog"
)

func TestNextResumePosition_LocalEventAndCheckpointGTID(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// One complete event. The header layout matches binlog.DurableCursor.
	raw := []byte{0xfe, 'b', 'i', 'n'}
	hdr := make([]byte, 19)
	hdr[9] = 19
	hdr[13] = 154
	raw = append(raw, hdr...)
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000003.open.e2"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 10, 4, 1, 2, 3, 0, time.UTC)
	stored := binlog.Checkpoint{
		File:      "mysql-bin.000003",
		Pos:       154,
		GTIDSet:   "24bc785e-9a61-11e1-8a5d-080027635ef5:1-20",
		UpdatedAt: when,
	}
	task := Task{ID: "1", Epoch: 2, Start: StartConfig{Mode: StartModeLatest}}

	got, ok := NextResumePosition(dir, task, stored, true)
	if !ok || got.File != "mysql-bin.000003" || got.Pos != 154 || got.GTIDSet != stored.GTIDSet || !got.UpdatedAt.Equal(when) {
		t.Fatalf("matching checkpoint: ok=%v %+v", ok, got)
	}

	stored.Pos = 999
	got, ok = NextResumePosition(dir, task, stored, true)
	if !ok || got.File != "mysql-bin.000003" || got.Pos != 154 || got.GTIDSet != "" {
		t.Fatalf("local event wins: ok=%v %+v", ok, got)
	}

	adopted := task
	adopted.KeepLocalSegments = true
	got, ok = NextResumePosition(dir, adopted, binlog.Checkpoint{}, false)
	if ok {
		t.Fatalf("adopted task must keep its saved start, got %+v", got)
	}

	noLocal := Task{ID: "missing", Epoch: 2}
	cp := binlog.Checkpoint{File: "mysql-bin.000008", Pos: 400, GTIDSet: "uuid:1-2"}
	got, ok = NextResumePosition(dir, noLocal, cp, true)
	if !ok || got.File != "mysql-bin.000008" || got.Pos != 4 || got.GTIDSet != "uuid:1-2" {
		t.Fatalf("takeover rewind: ok=%v %+v", ok, got)
	}

	stopped := noLocal
	stopped.Epoch = 0
	got, ok = NextResumePosition(dir, stopped, cp, true)
	if !ok || got.Pos != 400 || got.GTIDSet != "uuid:1-2" {
		t.Fatalf("stopped epoch keeps the stored pos: ok=%v %+v", ok, got)
	}
}
