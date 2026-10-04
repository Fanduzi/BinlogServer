// Package tasks provides module-level functionality for tasks.
// input: local data_dir segments, catalog rows, and task state
// output: proof that OpenTaskSegment serves sealed and current open-epoch bytes at the size captured at open, for RUNNING and STOPPED, and refuses names that are not on this process
// pos: scheduler tests for the local segment download open
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenTaskSegment_RunningAndStoppedOpenEpoch(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(WithDataDir(dir))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(dir, task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sealed := []byte("sealed-bytes")
	openBody := []byte("open-epoch-bytes")
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000003"), sealed, 0o644); err != nil {
		t.Fatal(err)
	}
	openPath := filepath.Join(taskDir, "mysql-bin.000004.open.e2")
	if err := os.WriteFile(openPath, openBody, 0o644); err != nil {
		t.Fatal(err)
	}

	for _, state := range []State{StateRunning, StateStopped} {
		setSegmentDownloadState(t, s, task.ID, state)
		gotState, err := s.GetTask(task.ID)
		if err != nil || gotState.State != state {
			t.Fatalf("state %s: %+v %v", state, gotState.State, err)
		}
		rc, size, err := s.OpenTaskSegment(task.ID, "mysql-bin.000004.open.e2")
		if err != nil {
			t.Fatalf("state %s open: %v", state, err)
		}
		if size != int64(len(openBody)) {
			t.Fatalf("state %s size %d", state, size)
		}
		grown, err := os.OpenFile(openPath, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := grown.Write([]byte("-grown")); err != nil {
			t.Fatal(err)
		}
		_ = grown.Close()
		body, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		onDisk, err := os.ReadFile(openPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != string(openBody) || int64(len(body)) != size || len(onDisk) <= len(body) {
			t.Fatalf("state %s body %q disk %q size %d", state, body, onDisk, size)
		}
		if err := os.WriteFile(openPath, openBody, 0o644); err != nil {
			t.Fatal(err)
		}

		sealedRC, sealedSize, err := s.OpenTaskSegment(task.ID, "mysql-bin.000003")
		if err != nil {
			t.Fatalf("state %s sealed: %v", state, err)
		}
		sealedGot, err := io.ReadAll(sealedRC)
		_ = sealedRC.Close()
		if err != nil || string(sealedGot) != string(sealed) || sealedSize != int64(len(sealed)) {
			t.Fatalf("state %s sealed body %q err %v", state, sealedGot, err)
		}
	}

	if _, _, err := s.OpenTaskSegment(task.ID, "mysql-bin.000004"); !errors.Is(err, ErrSegmentNotOnProcess) {
		t.Fatalf("source name of an open-only segment: %v", err)
	}
}

func TestOpenTaskSegment_CatalogDoesNotInventBytes(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	secret := []byte("catalog-path-bytes")
	if err := os.WriteFile(filepath.Join(outside, "mysql-bin.000001"), secret, 0o644); err != nil {
		t.Fatal(err)
	}
	store := newFakeFileStore()
	store.files["1"] = []BinlogFile{{
		FileName: "mysql-bin.000001",
		FilePath: filepath.Join(outside, "mysql-bin.000001"),
		State:    "SEALED",
	}, {
		FileName: "mysql-bin.000002",
		FilePath: filepath.Join(outside, "mysql-bin.000002.open.e3"),
		State:    "OPEN",
	}}
	s := NewScheduler(WithFileStore(store), WithDataDir(dir))
	if _, err := s.CreateTask("cluster-a", "cluster-a-key"); err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000099"), []byte("extra-on-disk"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.OpenTaskSegment("1", "mysql-bin.000001"); !errors.Is(err, ErrSegmentNotOnProcess) {
		t.Fatalf("missing local sealed: %v", err)
	}
	if _, _, err := s.OpenTaskSegment("1", "mysql-bin.000002.open.e3"); !errors.Is(err, ErrSegmentNotOnProcess) {
		t.Fatalf("missing local open: %v", err)
	}
	if _, _, err := s.OpenTaskSegment("1", "mysql-bin.000099"); !errors.Is(err, ErrSegmentNotOnProcess) {
		t.Fatalf("disk file outside catalog: %v", err)
	}

	local := []byte("local-open")
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000002.open.e3"), local, 0o644); err != nil {
		t.Fatal(err)
	}
	rc, size, err := s.OpenTaskSegment("1", "mysql-bin.000002.open.e3")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(body) != string(local) || size != int64(len(local)) {
		t.Fatalf("local open %q size %d err %v", body, size, err)
	}
}

func TestOpenTaskSegment_RejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(WithDataDir(dir))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "secret")
	if err := os.WriteFile(outside, []byte("secret-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(dir, task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000001"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(taskDir, "mysql-bin.000008")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", ".", "..", "../secret", `..\secret`, "mysql-bin.000001/../secret", "foo/bar", "mysql-bin.000001\x00"} {
		if _, _, err := s.OpenTaskSegment(task.ID, name); !errors.Is(err, ErrInvalidSegmentName) {
			t.Fatalf("name %q: %v", name, err)
		}
	}
	if _, _, err := s.OpenTaskSegment(task.ID, "mysql-bin.000008"); !errors.Is(err, ErrInvalidSegmentName) {
		t.Fatalf("symlink escape: %v", err)
	}
	if _, _, err := s.OpenTaskSegment("missing", "mysql-bin.000001"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task: %v", err)
	}
}

func setSegmentDownloadState(t *testing.T, s *Scheduler, id string, state State) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok {
		t.Fatalf("missing task %s", id)
	}
	task.State = state
	s.tasks[id] = task
}
