// Package tasks provides module-level functionality for tasks.
// input: temporary data directories with sealed and open binlog segment names
// output: assertions for disk listing order, standalone positions from SegmentPositions, the files-list projection of an unknown end and a local event span, catalog replay window, replay selection of every sealed segment plus the highest open epoch, an earlier source ordered before a later source's lower index, catalog fallback, checkpoint absence, standalone restart discovery, and adopt-then-start of a leftover directory
// pos: regression coverage for standalone files listing when meta has no catalog rows
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"binlog_server/internal/binlog"
)

func TestListTaskBinlogFilesOnDisk_OrderAndOpenPath(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(filepath.Join(taskDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(taskDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("mysql-bin.000010", "ten")
	write("notes.txt", "skip")
	write("mysql-bin.000004.open.e9", "e9")
	write("mysql-bin.000004.sealed.e7", "e7")
	write("mysql-bin.000003", "sealed")
	write("mysql-bin.000004.open.e1", "e1")
	write("mariadb-bin.000001", "maria")
	// Two source prefixes order by their oldest file time. Give every file
	// the same time so the order is the prefix tie-break, not write timing.
	same := time.Unix(1700000000, 0)
	for _, name := range []string{"mysql-bin.000010", "mysql-bin.000004.open.e9", "mysql-bin.000004.sealed.e7", "mysql-bin.000003", "mysql-bin.000004.open.e1", "mariadb-bin.000001"} {
		if err := os.Chtimes(filepath.Join(taskDir, name), same, same); err != nil {
			t.Fatal(err)
		}
	}

	files, err := listTaskBinlogFilesOnDisk(dir, "1", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name  string
		base  string
		state string
	}{
		{"mariadb-bin.000001", "mariadb-bin.000001", "SEALED"},
		{"mysql-bin.000003", "mysql-bin.000003", "SEALED"},
		{"mysql-bin.000004", "mysql-bin.000004.open.e1", "OPEN"},
		{"mysql-bin.000004", "mysql-bin.000004.sealed.e7", "SEALED"},
		{"mysql-bin.000004", "mysql-bin.000004.open.e9", "OPEN"},
		{"mysql-bin.000010", "mysql-bin.000010", "SEALED"},
	}
	if len(files) != len(want) {
		t.Fatalf("got %d files: %+v", len(files), files)
	}
	for i, item := range want {
		got := files[i]
		if got.FileName != item.name || got.State != item.state || filepath.Base(got.FilePath) != item.base {
			t.Fatalf("files[%d]=name %s state %s path %s, want name %s state %s base %s", i, got.FileName, got.State, got.FilePath, item.name, item.state, item.base)
		}
		if got.FilePath != filepath.Join(taskDir, item.base) {
			t.Fatalf("files[%d] path %s", i, got.FilePath)
		}
		if got.UploadState != "LOCAL_ONLY" {
			t.Fatalf("files[%d] upload state %s", i, got.UploadState)
		}
	}
	if files[2].SizeBytes != int64(len("e1")) {
		t.Fatalf("open size %d", files[2].SizeBytes)
	}
	if files[3].Epoch != 7 || files[3].State != "SEALED" {
		t.Fatalf("sealed epoch %+v", files[3])
	}
}

func TestListTaskBinlogFilesOnDisk_LimitKeepsHighestIndexes(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "9")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mysql-bin.000001", "mysql-bin.000002", "mysql-bin.000003.open.e1"} {
		if err := os.WriteFile(filepath.Join(taskDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := listTaskBinlogFilesOnDisk(dir, "9", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d", len(files))
	}
	if filepath.Base(files[0].FilePath) != "mysql-bin.000002" || filepath.Base(files[1].FilePath) != "mysql-bin.000003.open.e1" {
		t.Fatalf("window = %s, %s", files[0].FilePath, files[1].FilePath)
	}
}

func TestWindowBinlogFilesForReplay_CatalogOrderAndLimit(t *testing.T) {
	newest := time.Now()
	older := newest.Add(-2 * time.Hour)
	oldest := newest.Add(-3 * time.Hour)
	files := []BinlogFile{
		{FileName: "mysql-bin.000002", FilePath: "/data/1/mysql-bin.000002", State: "SEALED", SealedAt: newest},
		{FileName: "mysql-bin.000002.open.e1", FilePath: "/data/1/mysql-bin.000002.open.e1", State: "OPEN", SealedAt: newest},
		{FileName: "mysql-bin.000001", FilePath: "/data/1/mysql-bin.000001", State: "SEALED", SealedAt: oldest},
		{FileName: "mysql-bin.000003", FilePath: "/data/1/mysql-bin.000003.open.e4", State: "OPEN", SealedAt: older},
	}
	original := files[0].FilePath
	got := WindowBinlogFilesForReplay(files, 10)
	if files[0].FilePath != original {
		t.Fatalf("input reordered: %s", files[0].FilePath)
	}
	want := []string{
		"/data/1/mysql-bin.000001",
		"/data/1/mysql-bin.000002",
		"/data/1/mysql-bin.000002.open.e1",
		"/data/1/mysql-bin.000003.open.e4",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d", len(got))
	}
	for i, path := range want {
		if got[i].FilePath != path {
			t.Fatalf("got[%d]=%s want %s", i, got[i].FilePath, path)
		}
	}
	if got[1].State != "SEALED" || got[2].State != "OPEN" {
		t.Fatalf("same index states = %s, %s", got[1].State, got[2].State)
	}
	window := WindowBinlogFilesForReplay(files, 2)
	if len(window) != 2 || window[0].FilePath != want[2] || window[1].FilePath != want[3] {
		t.Fatalf("window = %s, %s", window[0].FilePath, window[1].FilePath)
	}
}

func TestSelectReplayFiles_SwitchOrdersEarlierSourceFirst(t *testing.T) {
	oldAt := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	newAt := oldAt.Add(time.Hour)
	const newID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	files := []BinlogFile{
		{FileName: newID + ".mysql-bin.000001", FilePath: "/data/" + newID + ".mysql-bin.000001", State: "SEALED", CreatedAt: newAt},
		{FileName: "mysql-bin.000009", FilePath: "/data/mysql-bin.000009", State: "SEALED", CreatedAt: oldAt},
		{FileName: "mysql-bin.000003", FilePath: "/data/mysql-bin.000003", State: "SEALED", CreatedAt: oldAt},
		{FileName: newID + ".mysql-bin.000003", FilePath: "/data/" + newID + ".mysql-bin.000003", State: "OPEN", CreatedAt: newAt},
	}
	got := SelectReplayFiles(files)
	want := []string{"mysql-bin.000003", "mysql-bin.000009", newID + ".mysql-bin.000001", newID + ".mysql-bin.000003"}
	if len(got) != len(want) {
		t.Fatalf("len %d %+v", len(got), got)
	}
	for i, name := range want {
		if got[i].FileName != name {
			t.Fatalf("index %d got %s want %s", i, got[i].FileName, name)
		}
	}
}

func TestSelectReplayFiles_OnePathPerIndex(t *testing.T) {
	files := []BinlogFile{
		{FileName: "mysql-bin.000003", FilePath: "/data/1/mysql-bin.000003", State: "SEALED"},
		{FileName: "mysql-bin.000004", FilePath: "/data/1/mysql-bin.000004", State: "SEALED"},
		{FileName: "mysql-bin.000004", FilePath: "/data/1/mysql-bin.000004.open.e1", State: "OPEN"},
		{FileName: "mysql-bin.000004", FilePath: "/data/1/mysql-bin.000004.open.e9", State: "OPEN"},
		{FileName: "mysql-bin.000005", FilePath: "/data/1/mysql-bin.000005.open.e2", State: "OPEN"},
		{FileName: "notes", FilePath: "/data/1/notes.txt"},
		{FileName: "mysql-bin.000006", FilePath: "  "},
	}
	original := files[1].FilePath
	got := SelectReplayFiles(files)
	if files[1].FilePath != original {
		t.Fatalf("input replaced: %s", files[1].FilePath)
	}
	want := []string{
		"/data/1/mysql-bin.000003",
		"/data/1/mysql-bin.000004",
		"/data/1/mysql-bin.000004.open.e9",
		"/data/1/mysql-bin.000005.open.e2",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d: %+v", len(got), got)
	}
	for i, path := range want {
		if got[i].FilePath != path {
			t.Fatalf("got[%d]=%s want %s", i, got[i].FilePath, path)
		}
	}

	// A later lower epoch does not replace the highest epoch already chosen.
	reversed := []BinlogFile{
		{FilePath: "/data/1/mysql-bin.000004.open.e9"},
		{FilePath: "/data/1/mysql-bin.000004"},
		{FilePath: "/data/1/mysql-bin.000004.open.e1"},
	}
	kept := SelectReplayFiles(reversed)
	if len(kept) != 2 || kept[0].FilePath != "/data/1/mysql-bin.000004" || kept[1].FilePath != "/data/1/mysql-bin.000004.open.e9" {
		t.Fatalf("kept %+v", kept)
	}
	if len(SelectReplayFiles(nil)) != 0 {
		t.Fatal("nil input")
	}
}

func TestSelectReplayFiles_DropsCoveredTakeoverReseal(t *testing.T) {
	const end = uint32(266551305)
	files := []BinlogFile{
		{
			FileName: "mysql-bin.000436", FilePath: "/data/a/6/mysql-bin.000436",
			State: "SEALED", Epoch: 1, StartPos: 4, EndPos: FilePos(end),
			UploadState: "UPLOADED", ObjectKey: "bucket/mysql-bin.000436",
		},
		{
			FileName: "mysql-bin.000436", FilePath: "/data/b/6/mysql-bin.000436.sealed.e3",
			State: "SEALED", Epoch: 3, StartPos: FilePos(end), EndPos: FilePos(end),
			UploadState: "UPLOADED", ObjectKey: "bucket/mysql-bin.000436.sealed.e3",
		},
		{
			FileName: "mysql-bin.000437", FilePath: "/data/b/6/mysql-bin.000437.open.e3",
			State: "OPEN", Epoch: 3,
		},
	}
	got := SelectReplayFiles(files)
	if len(got) != 2 || got[0].FilePath != "/data/a/6/mysql-bin.000436" || got[1].FilePath != "/data/b/6/mysql-bin.000437.open.e3" {
		t.Fatalf("replay %+v", got)
	}
}

func TestSelectReplayFiles_KeepsUnrecordedEndPos(t *testing.T) {
	files := []BinlogFile{
		{FilePath: "/data/1/mysql-bin.000004", State: "SEALED", StartPos: 4, EndPos: 1000},
		{FilePath: "/data/1/mysql-bin.000004.sealed.e1", State: "SEALED", StartPos: 4, EndPos: 0},
	}
	got := SelectReplayFiles(files)
	if len(got) != 2 || got[0].FilePath != "/data/1/mysql-bin.000004" || got[1].FilePath != "/data/1/mysql-bin.000004.sealed.e1" {
		t.Fatalf("replay %+v", got)
	}
}

func TestSelectReplayFiles_KeepsSealedWhenOpenHasDifferentObject(t *testing.T) {
	files := []BinlogFile{
		{FilePath: "/data/1/mysql-bin.000004", State: "SEALED", ObjectKey: "prefix/mysql-bin.000004", UploadState: "UPLOADED"},
		{FilePath: "/data/1/mysql-bin.000004.open.e2", State: "OPEN", UploadState: "LOCAL_ONLY"},
	}
	got := SelectReplayFiles(files)
	if len(got) != 2 || got[0].FilePath != "/data/1/mysql-bin.000004" || got[1].FilePath != "/data/1/mysql-bin.000004.open.e2" {
		t.Fatalf("got %+v", got)
	}
	copied := []BinlogFile{
		{FilePath: "/data/1/mysql-bin.000004", State: "SEALED", ObjectKey: "prefix/mysql-bin.000004"},
		{FilePath: "/data/1/mysql-bin.000004.open.e2", State: "OPEN", ObjectKey: "prefix/mysql-bin.000004"},
	}
	got = SelectReplayFiles(copied)
	if len(got) != 1 || got[0].FilePath != "/data/1/mysql-bin.000004.open.e2" {
		t.Fatalf("copied %+v", got)
	}
}

func TestSelectReplayFiles_LimitWindowKeepsHighestIndexes(t *testing.T) {
	files := []BinlogFile{
		{FilePath: "/data/1/mysql-bin.000001"},
		{FilePath: "/data/1/mysql-bin.000002"},
		{FilePath: "/data/1/mysql-bin.000002.open.e1"},
		{FilePath: "/data/1/mysql-bin.000002.open.e8"},
		{FilePath: "/data/1/mysql-bin.000003"},
	}
	window := WindowBinlogFilesForReplay(files, 3)
	if len(window) != 3 || filepath.Base(window[0].FilePath) != "mysql-bin.000002.open.e1" {
		t.Fatalf("window %+v", window)
	}
	got := SelectReplayFiles(window)
	if len(got) != 2 || got[0].FilePath != "/data/1/mysql-bin.000002.open.e8" || got[1].FilePath != "/data/1/mysql-bin.000003" {
		t.Fatalf("replay %+v", got)
	}
	all := SelectReplayFiles(WindowBinlogFilesForReplay(files, 10))
	if len(all) != 4 || all[0].FilePath != "/data/1/mysql-bin.000001" || all[1].FilePath != "/data/1/mysql-bin.000002" || all[2].FilePath != "/data/1/mysql-bin.000002.open.e8" || all[3].FilePath != "/data/1/mysql-bin.000003" {
		t.Fatalf("full %+v", all)
	}
}

func TestReplayClient(t *testing.T) {
	client, hint := ReplayClient("mysql")
	if client != "mysqlbinlog" || hint != "MySQL mysqlbinlog" {
		t.Fatalf("mysql client=%q hint=%q", client, hint)
	}
	client, hint = ReplayClient(" MariaDB ")
	if client != "mariadb-binlog" || hint != "mariadb-binlog" {
		t.Fatalf("mariadb client=%q hint=%q", client, hint)
	}
	client, hint = ReplayClient("")
	if client != "" || hint != "" {
		t.Fatalf("empty client=%q hint=%q", client, hint)
	}
}

func TestListTaskBinlogFilesOnDisk_MissingOrUnsafe(t *testing.T) {
	dir := t.TempDir()
	files, err := listTaskBinlogFilesOnDisk(dir, "absent", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("missing dir returned %#v", files)
	}
	if _, err := listTaskBinlogFilesOnDisk(dir, "../secret", 10); err != nil {
		t.Fatal(err)
	}
	escaped, err := listTaskBinlogFilesOnDisk(dir, "../secret", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(escaped) != 0 {
		t.Fatalf("escaped id returned %#v", escaped)
	}
}

func TestAnnotateSegmentLocations(t *testing.T) {
	dir := t.TempDir()
	taskID := "1"
	taskDir := filepath.Join(dir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	localName := "mysql-bin.000001"
	if err := os.WriteFile(filepath.Join(taskDir, localName), []byte("both"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := annotateSegmentLocations(dir, []BinlogFile{
		{TaskID: taskID, FileName: localName, FilePath: filepath.Join(taskDir, localName), UploadState: "UPLOADED", ObjectKey: "obj-both"},
		{TaskID: taskID, FileName: "mysql-bin.000002", FilePath: filepath.Join(taskDir, "mysql-bin.000002"), State: "SEALED", UploadState: "UPLOADED", ObjectKey: "obj-bucket"},
		{TaskID: taskID, FileName: localName, FilePath: filepath.Join(taskDir, localName), UploadState: "LOCAL_ONLY"},
	})
	if files[0].Location != "both" || files[1].Location != "bucket" || files[2].Location != "local" {
		t.Fatalf("%s %s %s", files[0].Location, files[1].Location, files[2].Location)
	}
}

func TestScheduler_ListFiles_EmptyCatalogUsesDisk(t *testing.T) {
	dir := t.TempDir()
	store := newFakeFileStore()
	s := NewScheduler(WithFileStore(store), WithDataDir(dir))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(dir, task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000008"), []byte("sealed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000009.open.e2"), []byte("open"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := s.ListFiles(task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d: %+v", len(files), files)
	}
	if files[0].State != "SEALED" || filepath.Base(files[0].FilePath) != "mysql-bin.000008" {
		t.Fatalf("first %+v", files[0])
	}
	if files[1].FileName != "mysql-bin.000009" || files[1].State != "OPEN" || filepath.Base(files[1].FilePath) != "mysql-bin.000009.open.e2" {
		t.Fatalf("second %+v", files[1])
	}
}

func TestScheduler_ListFiles_CatalogWinsOverDisk(t *testing.T) {
	dir := t.TempDir()
	store := newFakeFileStore()
	s := NewScheduler(WithFileStore(store), WithDataDir(dir))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(dir, task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000099"), []byte("disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	store.files[task.ID] = []BinlogFile{{
		TaskID:   task.ID,
		FileName: "mysql-bin.000001",
		FilePath: "/meta/mysql-bin.000001",
		State:    "SEALED",
	}}
	files, err := s.ListFiles(task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].FilePath != "/meta/mysql-bin.000001" {
		t.Fatalf("catalog result lost: %+v", files)
	}
}

type errListFileStore struct{}

func (errListFileStore) UpsertBinlogFile(context.Context, BinlogFile) error { return nil }
func (errListFileStore) ListBinlogFiles(context.Context, string, int) ([]BinlogFile, error) {
	return nil, errors.New("db down")
}

func TestScheduler_ListFiles_StoreErrorDoesNotScanDisk(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(WithFileStore(errListFileStore{}), WithDataDir(dir))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(dir, task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000001"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListFiles(task.ID, 10); err == nil || err.Error() != "db down" {
		t.Fatalf("err = %v", err)
	}
}

func TestGetCheckpoint_DiskFilesDoNotInventRow(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000001"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cp, ok, err := s.GetCheckpoint(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok || cp.File != "" || cp.Pos != 0 {
		t.Fatalf("invented checkpoint ok=%v %+v", ok, cp)
	}

	reader := &schedulerCheckpointReader{checkpoints: map[string]binlog.Checkpoint{
		task.ID: {File: "mysql-bin.000001", Pos: 128},
	}}
	withReader := NewScheduler(WithDataDir(dir), WithCheckpointReader(reader))
	if _, err := withReader.CreateTask("cluster-a", "cluster-a-key"); err != nil {
		t.Fatal(err)
	}
	cp, ok, err = withReader.GetCheckpoint(context.Background(), "1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || cp.File != "mysql-bin.000001" || cp.Pos != 128 {
		t.Fatalf("stored checkpoint lost: ok=%v %+v", ok, cp)
	}
}

func TestStandaloneRestart_DiscoversLeftoverDirectories(t *testing.T) {
	dir := t.TempDir()
	first := NewScheduler(WithDataDir(dir))
	task, err := first.CreateTask("orders", "orders")
	if err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(dir, task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "notes.txt"), []byte("skip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000010"), []byte("ten"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000004.open.e1"), []byte("open"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000003"), []byte("sealed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "8"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "9"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "9", "notes.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if len(first.ListTasks()) != 1 {
		t.Fatalf("live task duplicated: %+v", first.ListTasks())
	}

	second := NewScheduler(WithDataDir(dir))
	listed := second.ListTasks()
	if len(listed) != 1 || listed[0].ID != task.ID {
		t.Fatalf("discovered %+v, want only %s", listed, task.ID)
	}
	if listed[0].State != StateStopped || listed[0].Name != task.ID || listed[0].Source.Host != "" || listed[0].Source.User != "" {
		t.Fatalf("identity %+v", listed[0])
	}
	got, err := second.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != task.ID || got.Source.Password != "" {
		t.Fatalf("get %+v", got)
	}
	files, err := second.ListFiles(task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("files %+v", files)
	}
	if files[0].FileName != "mysql-bin.000003" || files[0].State != "SEALED" || files[0].FilePath != filepath.Join(taskDir, "mysql-bin.000003") {
		t.Fatalf("sealed %+v", files[0])
	}
	if files[1].FileName != "mysql-bin.000004" || files[1].State != "OPEN" || files[1].FilePath != filepath.Join(taskDir, "mysql-bin.000004.open.e1") {
		t.Fatalf("open %+v", files[1])
	}
	if files[2].FileName != "mysql-bin.000010" || filepath.Base(files[2].FilePath) != "mysql-bin.000010" {
		t.Fatalf("last %+v", files[2])
	}
	cp, ok, err := second.GetCheckpoint(context.Background(), task.ID)
	if err != nil || ok || cp.File != "" || cp.Pos != 0 {
		t.Fatalf("checkpoint err=%v ok=%v %+v", err, ok, cp)
	}
	events, err := second.ListEvents(task.ID, 10)
	if err != nil || len(events) != 0 {
		t.Fatalf("events err=%v %+v", err, events)
	}
	if _, progressOK, err := second.GetReplicationProgress(task.ID); err != nil || progressOK {
		t.Fatalf("progress err=%v ok=%v", err, progressOK)
	}
	if err := second.StartTask(task.ID); !errors.Is(err, ErrDiskBackupReadOnly) {
		t.Fatalf("start err=%v", err)
	}
	if err := second.DeleteTask(task.ID); !errors.Is(err, ErrDiskBackupReadOnly) {
		t.Fatalf("delete err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(taskDir, "mysql-bin.000003")); err != nil {
		t.Fatal(err)
	}
	created, err := second.CreateTask("fresh", "fresh")
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == task.ID {
		t.Fatalf("reused leftover id %s", created.ID)
	}
	ids := map[string]bool{}
	for _, item := range second.ListTasks() {
		ids[item.ID] = true
	}
	if !ids[task.ID] || !ids[created.ID] || ids["8"] || ids["9"] {
		t.Fatalf("ids %+v", ids)
	}
	if _, err := second.GetTask("missing"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing err=%v", err)
	}

	meta := NewScheduler(WithStore(newFakeStore()), WithDataDir(dir))
	if got := meta.ListTasks(); len(got) != 0 {
		t.Fatalf("meta list discovered disk tasks: %+v", got)
	}
	if _, err := meta.ListFiles(task.ID, 10); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("meta files err=%v", err)
	}
	if _, err := meta.GetTask(task.ID); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("meta get err=%v", err)
	}
}

func TestAdoptDiskBackup_DefaultFilePosThenStart(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "4")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sealedBody := []byte("sealed-seg")
	openBody := []byte("open-seg")
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000003"), sealedBody, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000004.open.e2"), openBody, 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &adoptFileRunner{dir: dir, seen: make(chan Task, 1)}
	s := NewScheduler(
		WithDataDir(dir),
		WithClusterLeaseManager(NewMemoryLease()),
		WithClusterWorkerID("standalone"),
		WithRunner(runner),
	)
	if err := s.StartTask("4"); !errors.Is(err, ErrDiskBackupReadOnly) {
		t.Fatalf("start before adopt: %v", err)
	}
	if _, err := s.UpdateTask("4", TaskPatch{ClusterKey: "adopted-4"}); !errors.Is(err, ErrDiskBackupReadOnly) {
		t.Fatalf("update before adopt: %v", err)
	}
	if _, err := s.AdoptDiskBackup("4", TaskPatch{ClusterKey: "adopted-4"}); !errors.Is(err, ErrSourceRequired) {
		t.Fatalf("missing source: %v", err)
	}

	name := "restored"
	adopted, err := s.AdoptDiskBackup("4", TaskPatch{
		Name:       &name,
		ClusterKey: "adopted-4",
		Source: &SourceConfig{
			Host:     "127.0.0.1",
			Port:     3306,
			User:     "repl",
			Password: "s3cret-adopt",
			Flavor:   "mysql",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if adopted.ID != "4" || adopted.Name != "restored" || adopted.State != StateStopped {
		t.Fatalf("adopted %+v", adopted)
	}
	if adopted.Source.Host != "127.0.0.1" || adopted.Source.User != "repl" || adopted.Source.Password != "s3cret-adopt" || adopted.Source.Flavor != "mysql" {
		t.Fatalf("source %+v", adopted.Source)
	}
	if adopted.Start.Mode != StartModeFilePos || adopted.Start.File != "mysql-bin.000004" || adopted.Start.Pos != uint32(len(openBody)) {
		t.Fatalf("start %+v", adopted.Start)
	}
	if !adopted.KeepLocalSegments {
		t.Fatal("expected keep-local flag")
	}
	if _, err := s.AdoptDiskBackup("4", TaskPatch{
		ClusterKey: "other",
		Source:     &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "s3cret-adopt"},
	}); !errors.Is(err, ErrTaskAlreadyHasMetadata) {
		t.Fatalf("second adopt: %v", err)
	}

	if err := s.StartTask("4"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.StopTask("4") })
	select {
	case got := <-runner.seen:
		if got.ID != "4" || got.Epoch != 3 || !got.KeepLocalSegments {
			t.Fatalf("runner task %+v", got)
		}
		if got.Start.File != "mysql-bin.000004" || got.Start.Pos != uint32(len(openBody)) {
			t.Fatalf("runner start %+v", got.Start)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner was not called")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := s.GetTask("4")
		if err != nil {
			t.Fatal(err)
		}
		if got.State == StateRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state %s", got.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got, err := os.ReadFile(filepath.Join(taskDir, "mysql-bin.000003")); err != nil || string(got) != string(sealedBody) {
		t.Fatalf("sealed changed: %v %q", err, got)
	}
	if got, err := os.ReadFile(filepath.Join(taskDir, "mysql-bin.000004.open.e2")); err != nil || string(got) != string(openBody) {
		t.Fatalf("open changed: %v %q", err, got)
	}
	if got, err := os.ReadFile(filepath.Join(taskDir, "mysql-bin.000004.open.e3")); err != nil || string(got) != "new-bytes" {
		t.Fatalf("new epoch: %v %q", err, got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "4" {
		t.Fatalf("data_dir entries: %v", entries)
	}
}

func TestAdoptDiskBackup_ExplicitStartAndMetaStore(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "4")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000003"), []byte("sealed"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewScheduler(WithDataDir(dir))
	latest := StartModeLatest
	adopted, err := s.AdoptDiskBackup("4", TaskPatch{
		ClusterKey: "adopted-4",
		Source:     &SourceConfig{Host: "10.0.0.8", Port: 3306, User: "repl", Password: "pw", Flavor: ""},
		Start:      &StartConfig{Mode: latest},
	})
	if err != nil {
		t.Fatal(err)
	}
	if adopted.Start.Mode != StartModeLatest || adopted.Start.File != "" || adopted.Start.Pos != 0 || adopted.Source.Flavor != "mysql" {
		t.Fatalf("override %+v source %+v", adopted.Start, adopted.Source)
	}
	if adopted.State != StateStopped || adopted.Name != "4" {
		t.Fatalf("identity %+v", adopted)
	}

	meta := NewScheduler(WithStore(newFakeStore()), WithDataDir(dir))
	if _, err := meta.AdoptDiskBackup("4", TaskPatch{
		ClusterKey: "adopted-4",
		Source:     &SourceConfig{Host: "10.0.0.8", Port: 3306, User: "repl", Password: "pw"},
	}); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("meta adopt: %v", err)
	}
	if got := meta.ListTasks(); len(got) != 0 {
		t.Fatalf("meta discovered %+v", got)
	}
}

type adoptFileRunner struct {
	dir  string
	seen chan Task
}

func (r *adoptFileRunner) Run(ctx context.Context, task Task) error {
	if task.KeepLocalSegments && task.Epoch > 0 && task.Start.File != "" {
		name := task.Start.File + ".open.e" + strconv.FormatInt(task.Epoch, 10)
		if err := os.WriteFile(filepath.Join(r.dir, task.ID, name), []byte("new-bytes"), 0o644); err != nil {
			return err
		}
	}
	select {
	case r.seen <- task:
	default:
	}
	<-ctx.Done()
	return context.Canceled
}

func writeDiskSegment(t *testing.T, path string, logPos uint32) {
	t.Helper()
	raw := []byte{0xfe, 'b', 'i', 'n'}
	hdr := make([]byte, 19)
	binary.LittleEndian.PutUint32(hdr[9:13], 19)
	binary.LittleEndian.PutUint32(hdr[13:17], logPos)
	if err := os.WriteFile(path, append(raw, hdr...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListTaskBinlogFilesOnDisk_PositionsFromDurableCursor(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	readable := filepath.Join(taskDir, "mysql-bin.000003")
	writeDiskSegment(t, readable, 23)
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000004"), []byte{0xfe, 'b', 'i', 'n'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, ".takeover-abc"), []byte("temp"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "notes.txt"), []byte("skip"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := listTaskBinlogFilesOnDisk(dir, "1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("got %+v", files)
	}
	cursor, _, _, ok := binlog.DurableCursor(readable)
	start, end, spanOK := binlog.SegmentPositions(readable)
	if !ok || !spanOK || files[0].FileName != "mysql-bin.000003" || files[0].StartPos != FilePos(start) || files[0].EndPos != FilePos(end) || uint32(files[0].EndPos) != cursor {
		t.Fatalf("readable %+v cursor=%d ok=%v span=%d..%d", files[0], cursor, ok, start, end)
	}
	if files[1].FileName != "mysql-bin.000004" || files[1].StartPos != 0 || files[1].EndPos != 0 {
		t.Fatalf("magic-only listing %+v", files[1])
	}
}

func TestFilePositionsForAPI_NullAndLocalSpan(t *testing.T) {
	dir := t.TempDir()
	readable := filepath.Join(dir, "mysql-bin.000003")
	writeDiskSegment(t, readable, 23)
	start, end, ok := binlog.SegmentPositions(readable)
	if !ok {
		t.Fatal("segment positions")
	}
	magic := filepath.Join(dir, "mysql-bin.000004")
	if err := os.WriteFile(magic, []byte{0xfe, 'b', 'i', 'n'}, 0o644); err != nil {
		t.Fatal(err)
	}
	in := []BinlogFile{
		{FileName: "mysql-bin.000003", FilePath: readable, State: "SEALED", EndPos: 0},
		{FileName: "mysql-bin.000004", FilePath: magic, State: "SEALED", StartPos: 4, EndPos: 4},
		{FileName: "mysql-bin.000005", FilePath: filepath.Join(dir, "missing"), State: "SEALED", EndPos: 0},
		{FileName: "mysql-bin.000006", FilePath: filepath.Join(dir, "remote-only"), State: "SEALED", StartPos: 4, EndPos: 1200},
		{FileName: "mysql-bin.000003", FilePath: readable, State: "SEALED", StartPos: 4, EndPos: 1000},
	}
	got := FilePositionsForAPI(in)
	if got[0].StartPos != FilePos(start) || got[0].EndPos != FilePos(end) {
		t.Fatalf("local span %+v, want %d..%d", got[0], start, end)
	}
	if got[1].StartPos != 0 || got[1].EndPos != 0 {
		t.Fatalf("resume cursor still shown: %+v", got[1])
	}
	if got[2].EndPos != 0 || got[3].EndPos != 1200 || got[3].StartPos != 4 {
		t.Fatalf("object-only rows %+v %+v", got[2], got[3])
	}
	if got[4].StartPos != 4 || got[4].EndPos != 1000 {
		t.Fatalf("stored span replaced: %+v", got[4])
	}
	if in[0].EndPos != 0 || in[1].EndPos != 4 {
		t.Fatalf("projection mutated catalog rows %+v", in[:2])
	}
}

func TestRejectedNameIsNotABinlog(t *testing.T) {
	for _, name := range []string{".takeover-abc", "notes.txt", "task-1.binlog"} {
		if binlog.SealedName(name) || binlog.OpenName(name) {
			t.Fatalf("%s classified as a binlog", name)
		}
		if isSealedFileForRetry(BinlogFile{
			FileName: name, FilePath: filepath.Join("/data", name),
			State: "SEALED", SealedAt: time.Now(),
		}) {
			t.Fatalf("retry treated %s as a binlog", name)
		}
	}
}
