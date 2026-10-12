// Package tasks provides module-level functionality for tasks.
// input: task directories built by the storage alert and legacy failback fixtures
// output: assertions that storageHealth returns ok, damaged and legacy failback verdicts, that damage wins over a legacy failback, that the checkpoint gtid_set never changes the kind, and that ReplayWarning and replayCut follow the kind
// pos: the interface test of the one storage detector (ADR 0007)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorageHealth_KindsAndPrecedence(t *testing.T) {
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
	ok := s.storageHealth(task, "")
	if ok.Found() || ok.Kind != StorageOK || ok.ReplayWarning() != "" {
		t.Fatalf("healthy dir: %+v", ok)
	}
	files := mustListFiles(t, s, task.ID)
	chosen := SelectReplayFiles(files)
	if got := ok.replayCut(files, chosen); len(got) != len(chosen) {
		t.Fatalf("ok verdict cut replay: %d of %d", len(got), len(chosen))
	}

	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000003"), strayRotateSegment("mysql-bin.000003"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeAlertSegment(t, filepath.Join(taskDir, "mysql-bin.000004"), 40)
	bare := s.storageHealth(task, "")
	claimed := s.storageHealth(task, gtidPITRUUID+":1-44")
	for _, h := range []StorageHealth{bare, claimed} {
		if h.Kind != StorageDamaged || h.Problem.Segment != "mysql-bin.000003" {
			t.Fatalf("damaged dir: %+v", h)
		}
		if w := h.ReplayWarning(); !strings.HasPrefix(w, "Segment mysql-bin.000003 is damaged (STORAGE_INCONSISTENT).") {
			t.Fatalf("warning %q", w)
		}
	}
	if bare.Problem.Missing != "" || claimed.Problem.Missing == "" {
		t.Fatalf("checkpoint gtid_set should only fill Missing: bare %q claimed %q", bare.Problem.Missing, claimed.Problem.Missing)
	}
	files = mustListFiles(t, s, task.ID)
	if got := replayNames(bare.replayCut(files, SelectReplayFiles(files))); got != "mysql-bin.000001,mysql-bin.000002" {
		t.Fatalf("replay cut %q", got)
	}
	if got := (StorageHealth{Kind: StorageDamaged}).replayCut(files, SelectReplayFiles(files)); len(got) != 0 {
		t.Fatalf("a verdict without a segment keeps %d files", len(got))
	}
}

func TestStorageHealth_LegacyFailbackAndDamageWins(t *testing.T) {
	f := legacyDir(t)
	h := f.s.storageHealth(f.task, "")
	if h.Kind != StorageLegacyFailback || h.Problem.Segment != "mysql-bin.000004" {
		t.Fatalf("legacy dir: %+v", h)
	}
	if w := h.ReplayWarning(); !strings.HasPrefix(w, "Segment mysql-bin.000004 is in a failback written by an earlier build (STORAGE_INCONSISTENT).") {
		t.Fatalf("warning %q", w)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "mysql-bin.000003"), strayRotateSegment("mysql-bin.000003"), 0o644); err != nil {
		t.Fatal(err)
	}
	if h := f.s.storageHealth(f.task, ""); h.Kind != StorageDamaged || h.Problem.Segment != "mysql-bin.000003" {
		t.Fatalf("damage should win over a legacy failback: %+v", h)
	}
}

func TestStorageHealth_StoredHoleWarning(t *testing.T) {
	h := StorageHealth{Kind: StorageStoredHole}
	h.Problem.Segment = "mysql-bin.000005"
	h.Problem.Missing = gtidPITRUUID + ":185-190"
	h.Problem.Message = "m"
	want := "Segment mysql-bin.000005 follows a GTID hole (STORAGE_INCONSISTENT): " + gtidPITRUUID + ":185-190 is stored in no segment. Replay stops before it: that segment and every later one are left out. m"
	if got := h.ReplayWarning(); got != want {
		t.Fatalf("warning %q", got)
	}
}
