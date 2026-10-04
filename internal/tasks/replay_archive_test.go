// Package tasks provides module-level functionality for tasks.
// input: on-disk segments, catalog rows, task state, and an optional object opener
// output: proof OpenReplayArchive returns one complete ustar and leaves no archive when a selected segment cannot be read
// pos: scheduler tests for the replay archive
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenReplayArchive_RunningOpenEpochAndEmpty(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(WithDataDir(dir))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatal(err)
	}
	empty, err := s.CreateTask("cluster-b", "cluster-b-key")
	if err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(dir, task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000001"), []byte("sealed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000002.open.e3"), []byte("open"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, state := range []State{StateRunning, StateStopped} {
		setSegmentDownloadState(t, s, task.ID, state)
		before := countReplayTemps(t)
		rc, size, err := s.OpenReplayArchive(task.ID, 200)
		if err != nil {
			t.Fatalf("state %s: %v", state, err)
		}
		body, err := io.ReadAll(rc)
		closeErr := rc.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("state %s read %v close %v", state, err, closeErr)
		}
		if int64(len(body)) != size {
			t.Fatalf("state %s size %d len %d", state, size, len(body))
		}
		entries := readReplayTar(t, body)
		if string(entries["mysql-bin.000001"]) != "sealed" || string(entries["mysql-bin.000002.open.e3"]) != "open" || len(entries) != 2 {
			t.Fatalf("state %s entries %#v", state, entries)
		}
		if countReplayTemps(t) != before {
			t.Fatalf("state %s temp leaked", state)
		}
	}

	before := countReplayTemps(t)
	rc, _, err := s.OpenReplayArchive(empty.ID, 200)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if len(readReplayTar(t, body)) != 0 {
		t.Fatalf("empty archive %d bytes", len(body))
	}
	if _, _, err := s.OpenReplayArchive("missing", 200); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if countReplayTemps(t) != before {
		t.Fatal("empty archive temp leaked")
	}
}

func TestOpenReplayArchive_ReadErrorLeavesNoArchive(t *testing.T) {
	store := newFakeFileStore()
	store.files["1"] = []BinlogFile{
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000001",
			FilePath:    "/outside/mysql-bin.000001",
			State:       "SEALED",
			UploadState: "UPLOADED",
			ObjectKey:   "ok-key",
		},
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000002",
			FilePath:    "/outside/mysql-bin.000002",
			State:       "SEALED",
			UploadState: "UPLOADED",
			ObjectKey:   "bad-key",
		},
	}
	opener := &trackingObjectStore{bodies: map[string][]byte{"ok-key": []byte("first-bytes")}, failRead: map[string]struct{}{"bad-key": {}}}
	s := NewScheduler(WithFileStore(store), WithFileUploader(opener))
	if _, err := s.CreateTask("cluster-a", "cluster-a-key"); err != nil {
		t.Fatal(err)
	}
	before := countReplayTemps(t)
	rc, _, err := s.OpenReplayArchive("1", 200)
	if err == nil || rc != nil || !strings.Contains(err.Error(), "read broke") {
		t.Fatalf("err=%v rc=%v", err, rc)
	}
	if !opener.closed["ok-key"] || !opener.closed["bad-key"] {
		t.Fatalf("closed %#v", opener.closed)
	}
	if countReplayTemps(t) != before {
		t.Fatal("temp left behind")
	}
}

type trackingObjectStore struct {
	bodies   map[string][]byte
	failRead map[string]struct{}
	closed   map[string]bool
}

func (s *trackingObjectStore) UploadFile(context.Context, string, string, string) error { return nil }

func (s *trackingObjectStore) OpenObject(_ context.Context, key string) (io.ReadCloser, int64, error) {
	if s.closed == nil {
		s.closed = map[string]bool{}
	}
	body := s.bodies[key]
	if _, fail := s.failRead[key]; fail {
		return &trackClose{Reader: errAfterOpen{}, close: func() { s.closed[key] = true }}, int64(len(body) + 1), nil
	}
	return &trackClose{Reader: bytes.NewReader(body), close: func() { s.closed[key] = true }}, int64(len(body)), nil
}

type trackClose struct {
	io.Reader
	close func()
}

func (t *trackClose) Close() error {
	t.close()
	return nil
}

type errAfterOpen struct{}

func (errAfterOpen) Read([]byte) (int, error) { return 0, errors.New("read broke") }

func readReplayTar(t *testing.T, raw []byte) map[string][]byte {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(raw))
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Format != tar.FormatUSTAR {
			t.Fatalf("format %v", hdr.Format)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = body
	}
}

func countReplayTemps(t *testing.T) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "binlog-replay-*.tar"))
	if err != nil {
		t.Fatal(err)
	}
	return len(matches)
}
