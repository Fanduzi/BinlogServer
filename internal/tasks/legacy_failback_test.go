// Package tasks provides module-level functionality for tasks.
// input: task directories laid out the way v0.5.57 / 0a84917a left a real failback A→B→A (the third stint's open file under A's first-stint name, the catalog row pointed at it, a .source-chain of A,B, and a phantom B→A recorded on restart), the same directory with a file whose stint cannot be proven, and healthy directories
// output: assertions that reads raise storage_alert and stop replay before the collision, that the repair renames the later stint to {A}~2, writes the catalog rows, the checkpoint file name and the chain without deleting a file, that a crash part way finishes on the next try, that an unprovable directory is refused, and that healthy directories are never flagged
// pos: unit coverage for the QA 6c88ad5b P1 (legacy failback directories)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/binlog"

	"github.com/go-mysql-org/go-mysql/mysql"
	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

type lfCatalog struct {
	mu   sync.Mutex
	rows map[string]BinlogFile
}

func newLFCatalog() *lfCatalog { return &lfCatalog{rows: map[string]BinlogFile{}} }

func (c *lfCatalog) UpsertBinlogFile(_ context.Context, row BinlogFile) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows[catalogKey(row.FileName, row.Epoch)] = row
	return nil
}

func (c *lfCatalog) ListBinlogFiles(_ context.Context, taskID string, _ int) ([]BinlogFile, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]BinlogFile, 0, len(c.rows))
	for _, row := range c.rows {
		if row.TaskID == taskID {
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return binlogSegmentLess(out[i], out[j]) })
	return out, nil
}

func (c *lfCatalog) DeleteBinlogFile(_ context.Context, _ string, name string, epoch int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.rows, catalogKey(name, epoch))
	return nil
}

type lfCheckpoints struct {
	mu sync.Mutex
	cp map[string]binlog.Checkpoint
}

func (c *lfCheckpoints) LoadCheckpoint(_ context.Context, taskID string) (binlog.Checkpoint, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp, ok := c.cp[taskID]
	return cp, ok, nil
}

func (c *lfCheckpoints) UpsertCheckpoint(_ context.Context, taskID string, cp binlog.Checkpoint) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cp[taskID] = cp
	return nil
}

func lfSegment(t *testing.T, path, previous string, txns ...binlog.GTIDEventRef) {
	t.Helper()
	when := time.Date(2026, 10, 9, 2, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	buf.Write([]byte{0xfe, 'b', 'i', 'n'})
	if previous != "" {
		set, err := mysql.ParseMysqlGTIDSet(previous)
		if err != nil {
			t.Fatal(err)
		}
		writeWindowEvent(&buf, when, goreplication.PREVIOUS_GTIDS_EVENT, set.Encode())
	}
	for _, tx := range txns {
		sid, err := hex.DecodeString(strings.ReplaceAll(tx.UUID, "-", ""))
		if err != nil {
			t.Fatal(err)
		}
		payload := make([]byte, 1+goreplication.SidLength+8)
		copy(payload[1:], sid)
		binary.LittleEndian.PutUint64(payload[1+goreplication.SidLength:], uint64(tx.Seq))
		writeWindowEvent(&buf, when, goreplication.GTID_EVENT, payload)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	// Files are written in stint order, a minute apart.
	lfClock = lfClock.Add(time.Minute)
	if err := os.Chtimes(path, lfClock, lfClock); err != nil {
		t.Fatal(err)
	}
}

var lfClock = time.Date(2026, 10, 9, 1, 0, 0, 0, time.UTC)

func lfSwitch(old, neu, gtid, file string, at time.Time) TaskEvent {
	return TaskEvent{Type: "SOURCE_SWITCHOVER", Time: at,
		Message: "source switched from " + old + " to " + neu + " at " + file + ":4 gtid_set=" + gtid + "; continuing from the executed GTID set",
		Detail:  "old=" + old + " new=" + neu + " gtid_set=" + gtid + " file=" + file + " pos=4"}
}

type lfFixture struct {
	s       *Scheduler
	task    Task
	dir     string
	catalog *lfCatalog
	cps     *lfCheckpoints
	events  *fakeEventStore
}

func newLFFixture(t *testing.T, extra ...Option) *lfFixture {
	t.Helper()
	root := t.TempDir()
	f := &lfFixture{catalog: newLFCatalog(), cps: &lfCheckpoints{cp: map[string]binlog.Checkpoint{}}, events: newFakeEventStore()}
	opts := append([]Option{WithDataDir(root), WithEventStore(f.events), WithFileStore(f.catalog), WithCheckpointReader(f.cps)}, extra...)
	f.s = NewScheduler(opts...)
	task, err := f.s.CreateTask("legacy", "legacy-key")
	if err != nil {
		t.Fatal(err)
	}
	f.s.mu.Lock()
	cur := f.s.tasks[task.ID]
	cur.Source.Flavor = "mysql"
	f.s.tasks[task.ID] = cur
	f.s.mu.Unlock()
	f.task = cur
	f.dir = filepath.Join(root, task.ID)
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *lfFixture) chain(t *testing.T, ids ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, sourceChainFileName), []byte(strings.Join(ids, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *lfFixture) addEvents(t *testing.T, events ...TaskEvent) {
	t.Helper()
	for _, ev := range events {
		ev.TaskID = f.task.ID
		if err := f.events.AppendEvent(t.Context(), ev); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *lfFixture) row(t *testing.T, name string, epoch int64, state, upload, key string) {
	t.Helper()
	path := ""
	for _, entry := range mustReadDir(t, f.dir) {
		named, ok := binlog.ClassifySegment(entry)
		if ok && named.Source == name && rowEpoch(named) == epoch && (state == "OPEN") == named.Open {
			path = filepath.Join(f.dir, entry)
		}
	}
	_ = f.catalog.UpsertBinlogFile(context.Background(), BinlogFile{TaskID: f.task.ID, FileName: name, FilePath: path, Epoch: epoch, State: state, UploadState: upload, ObjectKey: key, CreatedAt: time.Now(), SealedAt: time.Now()})
}

func mustReadDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func segNames(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	for _, name := range mustReadDir(t, dir) {
		if _, ok := binlog.ClassifySegment(name); ok {
			out = append(out, name)
		}
	}
	return strings.Join(out, ",")
}

// legacyDir lays out what v0.5.57 left after A→B→A: A's first stint ended
// in mysql-bin.000004 (A:6-8); the third stint reopened A's mysql-bin.000004
// as mysql-bin.000004.open.e1 (A:9-11). The catalog row of mysql-bin.000004
// points at the open file, the chain file is A,B, and a restart recorded a
// phantom B→A.
func legacyDir(t *testing.T, thirdStint ...binlog.GTIDEventRef) *lfFixture {
	t.Helper()
	return legacyDirWith(t, nil, thirdStint...)
}

func legacyDirWith(t *testing.T, opts []Option, thirdStint ...binlog.GTIDEventRef) *lfFixture {
	t.Helper()
	f := newLFFixture(t, opts...)
	f.chain(t, fbA, fbB)
	lfSegment(t, filepath.Join(f.dir, "mysql-bin.000003"), fbA+":1", fbEvents(fbA, 2, 3, 4, 5)...)
	lfSegment(t, filepath.Join(f.dir, "mysql-bin.000004"), fbA+":1-5", fbEvents(fbA, 6, 7, 8)...)
	lfSegment(t, filepath.Join(f.dir, fbB+".mysql-bin.000002"), fbA+":1-8", fbEvents(fbB, 1, 2, 3)...)
	if len(thirdStint) == 0 {
		thirdStint = fbEvents(fbA, 9, 10, 11)
	}
	lfSegment(t, filepath.Join(f.dir, "mysql-bin.000004.open.e1"), fbA+":1-5", thirdStint...)
	at := time.Date(2026, 10, 9, 2, 0, 0, 0, time.UTC)
	f.addEvents(t,
		lfSwitch(fbA, fbB, fbA+":1-8", "mysql-bin.000004", at),
		lfSwitch(fbB, fbA, fbA+":1-8,"+fbB+":1-3", "mysql-bin.000002", at.Add(time.Minute)),
		lfSwitch(fbB, fbA, fbA+":1-11,"+fbB+":1-3", "mysql-bin.000004", at.Add(2*time.Minute)),
	)
	f.row(t, "mysql-bin.000003", 0, "SEALED", "UPLOADED", "p/c/"+fbA+"/mysql-bin.000003")
	f.row(t, fbB+".mysql-bin.000002", 0, "SEALED", "UPLOADED", "p/c/"+fbB+"/"+fbB+".mysql-bin.000002")
	f.row(t, "mysql-bin.000004", 1, "OPEN", "LOCAL_ONLY", "")
	f.cps.cp[f.task.ID] = binlog.Checkpoint{File: "mysql-bin.000004", Pos: 999, GTIDSet: fbA + ":1-11," + fbB + ":1-3"}
	return f
}

func replayNames(files []BinlogFile) string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, filepath.Base(f.FilePath))
	}
	return strings.Join(out, ",")
}

func TestLegacyFailback_ReadsFlagAndRepairOnStart(t *testing.T) {
	f := legacyDir(t)

	// Reads: before the fix storage_alert was nil and replay listed the
	// open file in place of A's first-stint mysql-bin.000004.
	alerted := f.s.AttachStorageAlert(t.Context(), f.task)
	if alerted.StorageAlert == nil || alerted.StorageAlert.Code != CodeStorageInconsistent {
		t.Fatalf("no storage_alert on a legacy failback directory: %+v", alerted.StorageAlert)
	}
	if alerted.StorageAlert.Segment != "mysql-bin.000004" || !strings.Contains(alerted.StorageAlert.Message, "mysql-bin.000004.open.e1 → "+fbA+"~2.mysql-bin.000004.open.e1") {
		t.Fatalf("alert does not name the files: %+v", alerted.StorageAlert)
	}
	if got := strings.Join(alerted.StorageAlert.ValidSegments, ","); got != "mysql-bin.000003" {
		t.Fatalf("valid segments %q", got)
	}
	files, err := f.s.ListFiles(f.task.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	chosen, warning := f.s.SelectReplayFilesFor(f.task, files)
	if warning == "" || replayNames(chosen) != "mysql-bin.000003" {
		t.Fatalf("replay before repair: %s warning %q", replayNames(chosen), warning)
	}

	// Start repairs.
	if err := f.s.RepairLegacyFailback(t.Context(), f.task); err != nil {
		t.Fatal(err)
	}
	want := fbA + "~2.mysql-bin.000004.open.e1," + fbB + ".mysql-bin.000002,mysql-bin.000003,mysql-bin.000004"
	if got := segNames(t, f.dir); got != want {
		t.Fatalf("files after repair %s", got)
	}
	if got := strings.Join(readSourceChainFile(f.dir), ","); got != fbA+","+fbB+","+fbA {
		t.Fatalf("chain after repair %s", got)
	}
	if _, err := os.Stat(filepath.Join(f.dir, legacyRepairJournal)); !os.IsNotExist(err) {
		t.Fatalf("journal left behind: %v", err)
	}
	if cp := f.cps.cp[f.task.ID]; cp.File != fbA+"~2.mysql-bin.000004" || cp.Pos != 999 {
		t.Fatalf("checkpoint %+v", cp)
	}
	rows := f.catalog.rows
	if _, ok := rows[catalogKey("mysql-bin.000004", 1)]; ok {
		t.Fatalf("old row of the moved open file kept")
	}
	moved, ok := rows[catalogKey(fbA+"~2.mysql-bin.000004", 1)]
	if !ok || moved.State != "OPEN" || filepath.Base(moved.FilePath) != fbA+"~2.mysql-bin.000004.open.e1" {
		t.Fatalf("moved row %+v", moved)
	}
	first, ok := rows[catalogKey("mysql-bin.000004", 0)]
	if !ok || first.State != "SEALED" || first.UploadState != "UPLOAD_FAILED" || first.ObjectKey != "p/c/"+fbA+"/mysql-bin.000004" {
		t.Fatalf("first stint row not listed again for upload: %+v", first)
	}

	// Reads after repair: no alert, the whole chain in stint order.
	if after := f.s.AttachStorageAlert(t.Context(), f.task); after.StorageAlert != nil {
		t.Fatalf("alert after repair: %+v", after.StorageAlert)
	}
	files, err = f.s.ListFiles(f.task.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	chosen, warning = f.s.SelectReplayFilesFor(f.task, files)
	wantReplay := "mysql-bin.000003,mysql-bin.000004," + fbB + ".mysql-bin.000002," + fbA + "~2.mysql-bin.000004.open.e1"
	if warning != "" || replayNames(chosen) != wantReplay {
		t.Fatalf("replay after repair: %s warning %q", replayNames(chosen), warning)
	}
	// A second Start finds nothing to do.
	if err := f.s.RepairLegacyFailback(t.Context(), f.task); err != nil {
		t.Fatal(err)
	}
	if got := segNames(t, f.dir); got != want {
		t.Fatalf("second start changed files: %s", got)
	}
	repaired := 0
	for _, ev := range f.events.events[f.task.ID] {
		if ev.Type == "STORAGE_REPAIRED" {
			repaired++
		}
	}
	if repaired != 1 {
		t.Fatalf("STORAGE_REPAIRED events %d", repaired)
	}
}

func TestLegacyFailback_CrashMidRepairFinishesNextStart(t *testing.T) {
	f := legacyDir(t)
	plan := f.s.legacyFailbackFor(f.task)
	if len(plan.Moves) != 1 {
		t.Fatalf("plan %+v", plan)
	}
	// The journal and the rename happened, then the process died.
	if err := writeLegacyJournal(f.dir, plan); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(f.dir, plan.Moves[0].From), filepath.Join(f.dir, plan.Moves[0].To)); err != nil {
		t.Fatal(err)
	}
	if alert := f.s.AttachStorageAlert(t.Context(), f.task).StorageAlert; alert == nil {
		t.Fatal("an unfinished repair must still be reported")
	}
	if err := f.s.RepairLegacyFailback(t.Context(), f.task); err != nil {
		t.Fatal(err)
	}
	if cp := f.cps.cp[f.task.ID]; cp.File != fbA+"~2.mysql-bin.000004" {
		t.Fatalf("checkpoint %+v", cp)
	}
	if got := strings.Join(readSourceChainFile(f.dir), ","); got != fbA+","+fbB+","+fbA {
		t.Fatalf("chain %s", got)
	}
	if _, ok := f.catalog.rows[catalogKey(fbA+"~2.mysql-bin.000004", 1)]; !ok {
		t.Fatal("catalog row not written")
	}
}

func TestLegacyFailback_UnprovableIsRefused(t *testing.T) {
	// The open file holds A:7, which A's first stint already stored, and
	// A:9: no stint of A can have written it.
	f := legacyDir(t, fbEvents(fbA, 7, 9)...)
	before := segNames(t, f.dir)
	alert := f.s.AttachStorageAlert(t.Context(), f.task).StorageAlert
	if alert == nil || !strings.Contains(alert.Message, "cannot be put back in order automatically") || !strings.Contains(alert.Message, "mysql-bin.000004.open.e1") {
		t.Fatalf("alert %+v", alert)
	}
	err := f.s.RepairLegacyFailback(t.Context(), f.task)
	if err == nil || !IsPermanent(err) || !strings.Contains(err.Error(), CodeStorageInconsistent) {
		t.Fatalf("unprovable directory not refused: %v", err)
	}
	if got := segNames(t, f.dir); got != before {
		t.Fatalf("refusal changed files: %s", got)
	}
	if cp := f.cps.cp[f.task.ID]; cp.File != "mysql-bin.000004" {
		t.Fatalf("refusal changed checkpoint %+v", cp)
	}
	files, _ := f.s.ListFiles(f.task.ID, 100)
	chosen, warning := f.s.SelectReplayFilesFor(f.task, files)
	if warning == "" || replayNames(chosen) != "mysql-bin.000003" {
		t.Fatalf("replay %s warning %q", replayNames(chosen), warning)
	}
}

func TestLegacyFailback_TargetExistsIsRefused(t *testing.T) {
	f := legacyDir(t)
	lfSegment(t, filepath.Join(f.dir, fbA+"~2.mysql-bin.000004.open.e1"), "", fbEvents(fbA, 12)...)
	err := f.s.RepairLegacyFailback(t.Context(), f.task)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing target not refused: %v", err)
	}
}

func TestLegacyFailback_HealthyDirectoriesAreNotFlagged(t *testing.T) {
	at := time.Date(2026, 10, 9, 2, 0, 0, 0, time.UTC)
	t.Run("failback written by this build", func(t *testing.T) {
		f := newLFFixture(t)
		f.chain(t, fbA, fbB, fbA)
		lfSegment(t, filepath.Join(f.dir, "mysql-bin.000004"), fbA+":1-5", fbEvents(fbA, 6, 7, 8)...)
		lfSegment(t, filepath.Join(f.dir, fbB+".mysql-bin.000002"), fbA+":1-8", fbEvents(fbB, 1, 2, 3)...)
		lfSegment(t, filepath.Join(f.dir, fbA+"~2.mysql-bin.000004.open.e2"), fbA+":1-5", fbEvents(fbA, 9, 10)...)
		f.addEvents(t,
			lfSwitch(fbA, fbB, fbA+":1-8", "mysql-bin.000004", at),
			lfSwitch(fbB, fbA, fbA+":1-8,"+fbB+":1-3", "mysql-bin.000002", at.Add(time.Minute)),
		)
		assertHealthy(t, f)
	})
	t.Run("restart continuation of one file, no switch", func(t *testing.T) {
		f := newLFFixture(t)
		f.chain(t, fbA)
		lfSegment(t, filepath.Join(f.dir, "mysql-bin.000004"), fbA+":1-5", fbEvents(fbA, 6, 7)...)
		lfSegment(t, filepath.Join(f.dir, "mysql-bin.000004.sealed.e2"), "", fbEvents(fbA, 8)...)
		lfSegment(t, filepath.Join(f.dir, "mysql-bin.000004.open.e3"), "", fbEvents(fbA, 9)...)
		assertHealthy(t, f)
	})
	t.Run("A to B only, same names on both", func(t *testing.T) {
		f := newLFFixture(t)
		f.chain(t, fbA, fbB)
		lfSegment(t, filepath.Join(f.dir, "mysql-bin.000004"), fbA+":1-5", fbEvents(fbA, 6, 7, 8)...)
		lfSegment(t, filepath.Join(f.dir, fbB+".mysql-bin.000004.open.e1"), fbA+":1-8", fbEvents(fbB, 1)...)
		f.addEvents(t, lfSwitch(fbA, fbB, fbA+":1-8", "mysql-bin.000004", at))
		assertHealthy(t, f)
	})
	t.Run("events trimmed: chain file does not start the event chain", func(t *testing.T) {
		f := newLFFixture(t)
		f.chain(t, fbA, fbB, fbA)
		lfSegment(t, filepath.Join(f.dir, "mysql-bin.000004"), fbA+":1-5", fbEvents(fbA, 6, 7, 8)...)
		lfSegment(t, filepath.Join(f.dir, fbA+"~2.mysql-bin.000004.open.e2"), "", fbEvents(fbA, 9)...)
		f.addEvents(t, lfSwitch(fbB, fbA, fbA+":1-8,"+fbB+":1-3", "mysql-bin.000002", at))
		assertHealthy(t, f)
	})
}

func assertHealthy(t *testing.T, f *lfFixture) {
	t.Helper()
	before := segNames(t, f.dir)
	if alert := f.s.AttachStorageAlert(t.Context(), f.task).StorageAlert; alert != nil {
		t.Fatalf("healthy directory flagged: %+v", alert)
	}
	if err := f.s.RepairLegacyFailback(t.Context(), f.task); err != nil {
		t.Fatalf("healthy directory refused: %v", err)
	}
	if got := segNames(t, f.dir); got != before {
		t.Fatalf("healthy directory changed: %s -> %s", before, got)
	}
	if w := f.s.ReplayWarning(f.task); w != "" {
		t.Fatalf("healthy replay warning %q", w)
	}
}

// QA 6c88ad5b case 7: v0.5.57 restarted once after the failback, sealing
// the third stint as mysql-bin.000006.sealed.e1 and opening an empty
// mysql-bin.000006.open.e1. The empty open follows its sealed file.
func TestLegacyFailback_RestartedOldBuildLayout(t *testing.T) {
	f := newLFFixture(t)
	f.chain(t, fbA, fbB)
	lfSegment(t, filepath.Join(f.dir, "mysql-bin.000006"), fbA+":1-2209", fbEvents(fbA, 2210, 2211)...)
	lfSegment(t, filepath.Join(f.dir, fbB+".mysql-bin.000005"), "", fbEvents(fbB, 3006, 3007)...)
	lfSegment(t, filepath.Join(f.dir, "mysql-bin.000006.sealed.e1"), fbA+":1-2209", fbEvents(fbA, 2410, 2411)...)
	lfSegment(t, filepath.Join(f.dir, "mysql-bin.000006.open.e1"), fbA+":1-2209")
	at := time.Date(2026, 10, 9, 2, 18, 0, 0, time.UTC)
	f.addEvents(t,
		lfSwitch(fbA, fbB, fbA+":1-2409,"+fbB+":1-3005", "mysql-bin.000006", at),
		lfSwitch(fbB, fbA, fbA+":1-2409,"+fbB+":1-3205", "mysql-bin.000005", at.Add(time.Minute)),
		lfSwitch(fbB, fbA, fbA+":1-2609,"+fbB+":1-3205", "mysql-bin.000006", at.Add(3*time.Minute)),
	)
	// v0.5.57 filed the third stint's object under B.
	f.row(t, "mysql-bin.000006", 1, "SEALED", "UPLOADED", "p/c/"+fbB+"/mysql-bin.000006.sealed.e1")
	f.row(t, fbB+".mysql-bin.000005", 0, "SEALED", "UPLOADED", "p/c/"+fbB+"/"+fbB+".mysql-bin.000005")
	f.cps.cp[f.task.ID] = binlog.Checkpoint{File: "mysql-bin.000006", Pos: 628506}
	if err := f.s.RepairLegacyFailback(t.Context(), f.task); err != nil {
		t.Fatal(err)
	}
	want := fbA + "~2.mysql-bin.000006.open.e1," + fbA + "~2.mysql-bin.000006.sealed.e1," + fbB + ".mysql-bin.000005,mysql-bin.000006"
	if got := segNames(t, f.dir); got != want {
		t.Fatalf("files %s", got)
	}
	moved := f.catalog.rows[catalogKey(fbA+"~2.mysql-bin.000006", 1)]
	if moved.UploadState != "UPLOAD_FAILED" || moved.ObjectKey != "p/c/"+fbA+"/"+fbA+"~2.mysql-bin.000006.sealed.e1" {
		t.Fatalf("moved sealed row must upload under its new key: %+v", moved)
	}
	first := f.catalog.rows[catalogKey("mysql-bin.000006", 0)]
	if first.UploadState != "UPLOAD_FAILED" || first.ObjectKey != "p/c/"+fbA+"/mysql-bin.000006" {
		t.Fatalf("first stint row must upload its own bytes again: %+v", first)
	}
	if cp := f.cps.cp[f.task.ID]; cp.File != fbA+"~2.mysql-bin.000006" {
		t.Fatalf("checkpoint %+v", cp)
	}
}
