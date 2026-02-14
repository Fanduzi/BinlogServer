package binlog

import (
	"bytes"
	"errors"
	"testing"
)

type fakeFile struct {
	buf       bytes.Buffer
	syncErr   error
	syncCalls int
}

func (f *fakeFile) Write(p []byte) (int, error) {
	return f.buf.Write(p)
}

func (f *fakeFile) Sync() error {
	f.syncCalls++
	return f.syncErr
}

func TestWriter_AdvanceCheckpointAfterSync(t *testing.T) {
	file := &fakeFile{}
	initial := Checkpoint{File: "mysql-bin.000001", Pos: 4}
	next := Checkpoint{File: "mysql-bin.000001", Pos: 120}

	w := NewWriter(file, initial)
	if err := w.Append([]byte{0x01, 0x02}, next); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	if got := w.CurrentCheckpoint(); got.Pos != initial.Pos {
		t.Fatalf("checkpoint should not advance before sync, got pos=%d", got.Pos)
	}

	if err := w.FlushAndCheckpoint(); err != nil {
		t.Fatalf("FlushAndCheckpoint returned error: %v", err)
	}

	got := w.CurrentCheckpoint()
	if got.Pos != next.Pos || got.File != next.File {
		t.Fatalf("checkpoint not advanced, got %+v", got)
	}
	if file.syncCalls != 1 {
		t.Fatalf("expected sync called once, got %d", file.syncCalls)
	}
}

func TestWriter_NoCheckpointAdvanceWhenSyncFails(t *testing.T) {
	file := &fakeFile{syncErr: errors.New("sync failed")}
	initial := Checkpoint{File: "mysql-bin.000001", Pos: 4}
	next := Checkpoint{File: "mysql-bin.000001", Pos: 120}

	w := NewWriter(file, initial)
	if err := w.Append([]byte{0x01, 0x02}, next); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	if err := w.FlushAndCheckpoint(); err == nil {
		t.Fatal("expected sync error, got nil")
	}

	got := w.CurrentCheckpoint()
	if got.Pos != initial.Pos || got.File != initial.File {
		t.Fatalf("checkpoint advanced on sync failure, got %+v", got)
	}
}
