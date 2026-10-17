// Package tasks provides module-level functionality for tasks.
// input: local open segments and stored checkpoints
// output: proof that NextResumePosition matches the position Start continues from, that a magic-only or header-only open file on this worker is resumed from the saved checkpoint instead of being reported missing, and that a readable catalog path outside this worker's data_dir is Missing (#421 sc1) while a path under data_dir continues
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
	if !ok || got.File != "mysql-bin.000008" || got.Pos != 4 || got.GTIDSet != "" {
		t.Fatalf("takeover rewind drops the later gtid: ok=%v %+v", ok, got)
	}

	stopped := noLocal
	stopped.Epoch = 0
	got, ok = NextResumePosition(dir, stopped, cp, true)
	if !ok || got.Pos != 400 || got.GTIDSet != "uuid:1-2" {
		t.Fatalf("stopped epoch keeps the stored pos: ok=%v %+v", ok, got)
	}
}

// TestResolveTakeover_PreambleOpenKeepsAnchor is #222.
// Epoch climbed past an open file that has no complete event. The file is
// still on this worker. The saved LATEST anchor must be kept.
func TestResolveTakeover_PreambleOpenKeepsAnchor(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const source = "mysql-bin.000010"
	openPath := filepath.Join(taskDir, source+".open.e1")
	magic := []byte{0xfe, 'b', 'i', 'n'}
	if err := os.WriteFile(openPath, magic, 0o644); err != nil {
		t.Fatal(err)
	}
	cp := binlog.Checkpoint{File: source, Pos: 197}
	row := func(path string) []BinlogFile {
		return []BinlogFile{{
			TaskID: "1", FileName: source, FilePath: path,
			Epoch: 1, State: "OPEN", StartPos: 197, EndPos: 197, UploadState: "LOCAL_ONLY",
		}}
	}
	task := Task{ID: "1", Epoch: 3, OwnerWorkerID: "worker-a"}

	decision := ResolveTakeover(dir, task, cp, true, row(openPath))
	if decision.Missing != "" || !decision.Apply || decision.Dir != taskDir || decision.Checkpoint != cp {
		t.Fatalf("magic-only: %+v", decision)
	}

	header := append([]byte(nil), magic...)
	header = append(header, logPosZeroHeader("HEADER-ONLY")...)
	if err := os.WriteFile(openPath, header, 0o644); err != nil {
		t.Fatal(err)
	}
	decision = ResolveTakeover(dir, task, cp, true, row(openPath))
	if decision.Missing != "" || !decision.Apply || decision.Dir != taskDir || decision.Checkpoint != cp {
		t.Fatalf("header-only: %+v", decision)
	}

	withGTID := cp
	withGTID.GTIDSet = "24bc785e-9a61-11e1-8a5d-080027635ef5:1-20"
	decision = ResolveTakeover(dir, task, withGTID, true, row(openPath))
	if decision.Missing != "" || !decision.Apply || decision.Checkpoint != withGTID {
		t.Fatalf("stored gtid: %+v", decision)
	}

	missing := filepath.Join(taskDir, "gone.open.e1")
	decision = ResolveTakeover(dir, task, cp, true, row(missing))
	if decision.Apply || decision.Missing == "" {
		t.Fatalf("missing file: %+v", decision)
	}

	other := filepath.Join(taskDir, "mysql-bin.000009.open.e1")
	if err := os.WriteFile(other, magic, 0o644); err != nil {
		t.Fatal(err)
	}
	decision = ResolveTakeover(dir, task, cp, true, []BinlogFile{{
		TaskID: "1", FileName: "mysql-bin.000009", FilePath: other,
		Epoch: 1, State: "OPEN", UploadState: "LOCAL_ONLY",
	}})
	if decision.Apply || decision.Missing != source {
		t.Fatalf("different source file: %+v", decision)
	}

	torn := append(append([]byte{}, magic...), 1, 2, 3, 4, 5)
	if err := os.WriteFile(openPath, torn, 0o644); err != nil {
		t.Fatal(err)
	}
	decision = ResolveTakeover(dir, task, cp, true, row(openPath))
	if decision.Apply || decision.Missing != source {
		t.Fatalf("torn tail: %+v", decision)
	}
}

func logPosZeroHeader(body string) []byte {
	raw := make([]byte, 19+len(body))
	raw[4] = 15 // FORMAT_DESCRIPTION_EVENT
	raw[9] = byte(len(raw))
	copy(raw[19:], body)
	return raw
}

// TestResolveTakeover_ForeignReadablePathIsMissing is #421 sc1.
// A catalog file_path that is readable but not under this worker's data_dir
// must not become TakeoverResume.Dir.
func TestResolveTakeover_ForeignReadablePathIsMissing(t *testing.T) {
	workerB := t.TempDir()
	workerA := t.TempDir()
	const taskID = "1"
	aDir := filepath.Join(workerA, taskID)
	if err := os.MkdirAll(aDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// One complete event so DurableResumeDir would succeed if we allowed foreign dirs.
	raw := []byte{0xfe, 'b', 'i', 'n'}
	hdr := make([]byte, 19)
	hdr[9] = 19
	hdr[13] = 154
	raw = append(raw, hdr...)
	openPath := filepath.Join(aDir, "mysql-bin.000003.open.e1")
	if err := os.WriteFile(openPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	cp := binlog.Checkpoint{File: "mysql-bin.000003", Pos: 154}
	files := []BinlogFile{{
		TaskID: taskID, FileName: "mysql-bin.000003", FilePath: openPath,
		Epoch: 1, State: "OPEN", EndPos: 154, UploadState: "LOCAL_ONLY",
	}}
	task := Task{ID: taskID, Epoch: 2, OwnerWorkerID: "worker-b"}

	decision := ResolveTakeover(workerB, task, cp, true, files)
	if decision.Apply || decision.Dir != "" || decision.Covering != nil {
		t.Fatalf("foreign readable path must not continue: %+v", decision)
	}
	if decision.Missing != openPath {
		t.Fatalf("Missing=%q, want %q", decision.Missing, openPath)
	}

	// Magic-only open under this worker's data_dir: DurableResume does not
	// see a complete event, so ResolveTakeover applies the saved checkpoint
	// with Dir under data_dir (same contract as #222).
	localDir := filepath.Join(workerB, taskID)
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(localDir, "mysql-bin.000003.open.e1")
	if err := os.WriteFile(localPath, []byte{0xfe, 'b', 'i', 'n'}, 0o644); err != nil {
		t.Fatal(err)
	}
	localFiles := []BinlogFile{{
		TaskID: taskID, FileName: "mysql-bin.000003", FilePath: localPath,
		Epoch: 1, State: "OPEN", StartPos: 154, EndPos: 154, UploadState: "LOCAL_ONLY",
	}}
	decision = ResolveTakeover(workerB, task, cp, true, localFiles)
	if decision.Missing != "" || !decision.Apply || decision.Dir != localDir {
		t.Fatalf("under data_dir: %+v", decision)
	}
	if decision.Checkpoint != cp {
		t.Fatalf("checkpoint %+v, want %+v", decision.Checkpoint, cp)
	}
}

// TestResolveTakeover_UploadedForeignLocalMaterializes covers UPLOADED rows whose
// local file_path is readable outside data_dir: resume via Covering, not Dir.
func TestResolveTakeover_UploadedForeignLocalMaterializes(t *testing.T) {
	workerB := t.TempDir()
	foreign := t.TempDir()
	const taskID = "1"
	raw := []byte{0xfe, 'b', 'i', 'n'}
	hdr := make([]byte, 19)
	hdr[9] = 19
	hdr[13] = 154
	raw = append(raw, hdr...)
	foreignPath := filepath.Join(foreign, "mysql-bin.000003")
	if err := os.WriteFile(foreignPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	cp := binlog.Checkpoint{File: "mysql-bin.000003", Pos: 154}
	row := BinlogFile{
		TaskID: taskID, FileName: "mysql-bin.000003", FilePath: foreignPath,
		State: "SEALED", EndPos: 154, UploadState: "UPLOADED", ObjectKey: "obj/k",
	}
	task := Task{ID: taskID, Epoch: 2}
	decision := ResolveTakeover(workerB, task, cp, true, []BinlogFile{row})
	if decision.Missing != "" || !decision.Apply || decision.Dir != "" || decision.Covering == nil {
		t.Fatalf("want Covering materialize, got %+v", decision)
	}
	if decision.Covering.ObjectKey != "obj/k" {
		t.Fatalf("covering %+v", decision.Covering)
	}

	localDir := filepath.Join(workerB, taskID)
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(localDir, "mysql-bin.000003")
	if err := os.WriteFile(localPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	row.FilePath = localPath
	decision = ResolveTakeover(workerB, task, cp, true, []BinlogFile{row})
	if decision.Missing != "" || !decision.Apply || decision.Dir != localDir || decision.Covering != nil {
		t.Fatalf("local under data_dir should Apply+Dir: %+v", decision)
	}
}
