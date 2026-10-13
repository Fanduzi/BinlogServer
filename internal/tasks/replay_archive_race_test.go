// Package tasks provides module-level functionality for tasks.
// input: a task directory whose open segment is sealed (renamed) between listing steps
// output: proof the disk listing and the replay archive read the directory again instead of failing with lstat ENOENT (#322),
// and that an OPEN member missing for another reason still fails as ErrSegmentNotOnProcess
// pos: scheduler tests for the open-segment seal race
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sealOnce renames mysql-bin.000002.open.e1 to mysql-bin.000002 the first
// time it runs, which is what the runner does when it seals that segment.
func sealOnce(t *testing.T, dir string) func() {
	t.Helper()
	done := false
	return func() {
		if done {
			return
		}
		done = true
		if err := os.Rename(filepath.Join(dir, "mysql-bin.000002.open.e1"), filepath.Join(dir, "mysql-bin.000002")); err != nil {
			t.Errorf("seal: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "mysql-bin.000003.open.e1"), []byte("next"), 0o644); err != nil {
			t.Errorf("next open: %v", err)
		}
	}
}

func newSealRaceTask(t *testing.T) (*Scheduler, string, string) {
	t.Helper()
	dataDir := t.TempDir()
	s := NewScheduler(WithDataDir(dataDir))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(dataDir, task.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mysql-bin.000001"), []byte("sealed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mysql-bin.000002.open.e1"), []byte("open"), 0o644); err != nil {
		t.Fatal(err)
	}
	setSegmentDownloadState(t, s, task.ID, StateRunning)
	return s, task.ID, dir
}

func TestListTaskBinlogFilesOnDisk_SegmentSealedDuringListing(t *testing.T) {
	s, taskID, dir := newSealRaceTask(t)
	seal := sealOnce(t, dir)
	diskListAfterReadDir = func(string) { seal() }
	t.Cleanup(func() { diskListAfterReadDir = nil })

	files, err := listTaskBinlogFilesOnDisk(s.dataDir, taskID, segmentInventoryLimit)
	if err != nil {
		t.Fatalf("listing failed instead of reading again: %v", err)
	}
	var names []string
	for _, f := range files {
		names = append(names, filepath.Base(f.FilePath))
	}
	want := []string{"mysql-bin.000001", "mysql-bin.000002", "mysql-bin.000003.open.e1"}
	if len(names) != len(want) {
		t.Fatalf("names %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names %v, want %v", names, want)
		}
	}
}

func TestOpenReplayArchive_OpenSegmentSealedAfterSelection(t *testing.T) {
	s, taskID, dir := newSealRaceTask(t)
	seal := sealOnce(t, dir)
	replayArchiveAfterSelect = func(string, []BinlogFile) { seal() }
	t.Cleanup(func() { replayArchiveAfterSelect = nil })

	rc, size, err := s.OpenReplayArchive(taskID, segmentInventoryLimit)
	if err != nil {
		t.Fatalf("archive failed instead of selecting again: %v", err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || int64(len(body)) != size {
		t.Fatalf("read %v len %d size %d", err, len(body), size)
	}
	entries := readReplayTar(t, body)
	// The archive is the selection: the sealed file takes the open member's
	// place, and the next open segment, created after the selection, is not in it.
	if string(entries["mysql-bin.000001"]) != "sealed" || string(entries["mysql-bin.000002"]) != "open" || len(entries) != 2 {
		t.Fatalf("entries %#v", entries)
	}
}

func TestListFiles_StandaloneSegmentSealedDuringListing(t *testing.T) {
	s, taskID, dir := newSealRaceTask(t)
	seal := sealOnce(t, dir)
	diskListAfterReadDir = func(string) { seal() }
	t.Cleanup(func() { diskListAfterReadDir = nil })

	if _, err := s.ListFiles(taskID, segmentInventoryLimit); err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
}

func TestOpenReplayArchive_OpenSegmentGrowsAfterOpen(t *testing.T) {
	s, taskID, dir := newSealRaceTask(t)
	open := filepath.Join(dir, "mysql-bin.000002.open.e1")

	members, err := s.openSelectedReplayMembers(taskID, mustSelect(t, s, taskID))
	if err != nil {
		t.Fatal(err)
	}
	// The runner appends to the open segment after the member was opened.
	f, err := os.OpenFile(open, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("-more-events"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	rc, size, err := materializeReplayTar(members)
	if err != nil {
		t.Fatalf("tar of a grown open segment: %v", err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if int64(len(body)) != size {
		t.Fatalf("len %d size %d", len(body), size)
	}
	entries := readReplayTar(t, body)
	if string(entries["mysql-bin.000002.open.e1"]) != "open" {
		t.Fatalf("open member %q, want the bytes at open", entries["mysql-bin.000002.open.e1"])
	}
}

func TestOpenReplayArchive_OpenSegmentSealedAsSealedEpoch(t *testing.T) {
	s, taskID, dir := newSealRaceTask(t)
	// mysql-bin.000002 already exists from an earlier epoch, so the seal
	// renames the open file to mysql-bin.000002.sealed.e1.
	if err := os.WriteFile(filepath.Join(dir, "mysql-bin.000002"), []byte("earlier"), 0o644); err != nil {
		t.Fatal(err)
	}
	replayArchiveAfterSelect = func(string, []BinlogFile) {
		if err := os.Rename(filepath.Join(dir, "mysql-bin.000002.open.e1"), filepath.Join(dir, "mysql-bin.000002.sealed.e1")); err != nil {
			t.Errorf("seal: %v", err)
		}
	}
	t.Cleanup(func() { replayArchiveAfterSelect = nil })

	rc, _, err := s.OpenReplayArchive(taskID, segmentInventoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	entries := readReplayTar(t, body)
	if string(entries["mysql-bin.000002"]) != "earlier" || string(entries["mysql-bin.000002.sealed.e1"]) != "open" || len(entries) != 3 {
		t.Fatalf("entries %#v", entries)
	}
}

func mustSelect(t *testing.T, s *Scheduler, taskID string) []BinlogFile {
	t.Helper()
	selected, err := s.selectReplayFiles(taskID, segmentInventoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	return selected
}

// An OPEN catalog row whose file is not on this process (another worker's
// segment, or a file that is gone) is not a seal race: after the retries the
// archive fails with ErrSegmentNotOnProcess itself, which the API maps to
// 404, and no retry hint or temp file is left.
func TestOpenReplayArchive_MissingOpenSegmentIsNotOnProcess(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	store := newFakeFileStore()
	store.files["1"] = []BinlogFile{{
		TaskID:      "1",
		FileName:    "mysql-bin.000005",
		FilePath:    filepath.Join(outside, "mysql-bin.000005.open.e1"),
		State:       "OPEN",
		UploadState: "LOCAL_ONLY",
	}}
	s := NewScheduler(WithFileStore(store), WithDataDir(dir))
	if _, err := s.CreateTask("cluster-a", "cluster-a-key"); err != nil {
		t.Fatal(err)
	}
	selections := 0
	replayArchiveAfterSelect = func(string, []BinlogFile) { selections++ }
	t.Cleanup(func() { replayArchiveAfterSelect = nil })

	before := countReplayTemps(t)
	rc, _, err := s.OpenReplayArchive("1", segmentInventoryLimit)
	if rc != nil || !errors.Is(err, ErrSegmentNotOnProcess) {
		t.Fatalf("rc=%v err=%v, want ErrSegmentNotOnProcess", rc, err)
	}
	var sealed *replayMemberSealedError
	if errors.As(err, &sealed) || strings.Contains(err.Error(), "retry") || strings.Contains(err.Error(), "sealed") {
		t.Fatalf("missing open member surfaced as a seal race: %v", err)
	}
	if selections != replayArchiveAttempts {
		t.Fatalf("selections %d, want %d", selections, replayArchiveAttempts)
	}
	if countReplayTemps(t) != before {
		t.Fatal("temp left behind")
	}
}

// A plain sealed file of an earlier epoch that the selection left out is not
// the successor of a vanished open segment: it is older and shorter than the
// open file the listing saw, so it is not read in its place.
func TestOpenSelectedReplayMembers_EarlierPlainEpochIsNotSuccessor(t *testing.T) {
	s, taskID, dir := newSealRaceTask(t)
	earlier := filepath.Join(dir, "mysql-bin.000002")
	if err := os.WriteFile(earlier, []byte("e0"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(earlier, old, old); err != nil {
		t.Fatal(err)
	}
	var openRow BinlogFile
	for _, f := range mustSelect(t, s, taskID) {
		if filepath.Base(f.FilePath) == "mysql-bin.000002.open.e1" {
			openRow = f
		}
	}
	if openRow.FilePath == "" {
		t.Fatal("open segment not listed")
	}
	// The open file is removed without a seal.
	if err := os.Remove(filepath.Join(dir, "mysql-bin.000002.open.e1")); err != nil {
		t.Fatal(err)
	}
	members, err := s.openSelectedReplayMembers(taskID, []BinlogFile{openRow})
	if err == nil {
		closeReplayMembers(members)
		t.Fatalf("read %v in place of the vanished open segment", members[0].name)
	}
	if !errors.Is(err, ErrSegmentNotOnProcess) {
		t.Fatalf("err %v, want ErrSegmentNotOnProcess", err)
	}
}

// The plain sealed name the seal produced is read in place of the open name
// when it is selected directly (no fresh listing in between).
func TestOpenSelectedReplayMembers_PlainSealIsSuccessor(t *testing.T) {
	s, taskID, dir := newSealRaceTask(t)
	var openRow BinlogFile
	for _, f := range mustSelect(t, s, taskID) {
		if filepath.Base(f.FilePath) == "mysql-bin.000002.open.e1" {
			openRow = f
		}
	}
	sealOnce(t, dir)()
	members, err := s.openSelectedReplayMembers(taskID, []BinlogFile{openRow})
	if err != nil {
		t.Fatal(err)
	}
	defer closeReplayMembers(members)
	if len(members) != 1 || members[0].name != "mysql-bin.000002" {
		t.Fatalf("members %+v", members)
	}
}

// The point-in-time and GTID selections open each chosen segment against the
// listing they selected from. A vanished open member is reported with the
// plain not-on-process message, so the replay JSON routes still answer 404
// with the same body, and only the archive selects again.
func TestOpenSelectedSegment_VanishedOpenKeepsNotOnProcessMessage(t *testing.T) {
	s, taskID, dir := newSealRaceTask(t)
	files, err := s.ListFiles(taskID, segmentInventoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	var openRow BinlogFile
	for _, f := range files {
		if filepath.Base(f.FilePath) == "mysql-bin.000002.open.e1" {
			openRow = f
		}
	}
	if err := os.Remove(filepath.Join(dir, "mysql-bin.000002.open.e1")); err != nil {
		t.Fatal(err)
	}
	_, body, _, err := s.openSelectedSegment(taskID, files, openRow)
	if body != nil || !errors.Is(err, ErrSegmentNotOnProcess) || err.Error() != ErrSegmentNotOnProcess.Error() {
		t.Fatalf("body=%v err=%v", body, err)
	}
	var sealed *replayMemberSealedError
	if !errors.As(err, &sealed) {
		t.Fatalf("archive cannot tell the seal race: %v", err)
	}
}
