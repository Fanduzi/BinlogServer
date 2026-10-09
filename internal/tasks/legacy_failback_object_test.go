// Package tasks provides module-level functionality for tasks.
// input: legacy failback directories whose first-stint segment is kept only in object storage (fits, missing, or holding the wrong transactions), a failback directory with a segment removed between stored transactions, a retention-trimmed one, an unparseable switch gtid_set, and window views with an interior GTID gap
// output: assertions that Start fetches a fitting object back before the repair and refuses otherwise without touching disk or catalog, that upload retry holds the legacy rows until the repair, that STORAGE_REPAIRED lists orphaned object keys, that a stored GTID hole sets storage_alert and stops /replay and /window before it while retention is not a hole, that an unparseable gtid_set still names a cut segment, and that /window never reports continuous with an interior gap
// pos: unit coverage for the QA bbc2c88b findings (first stint only in object storage, refuse path side effects)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func lfObjectFixture(t *testing.T, objects map[string][]byte) *lfFixture {
	t.Helper()
	return legacyDirWith(t, []Option{WithFileUploader(windowObjects{bodies: objects})})
}

func copyRows(rows map[string]BinlogFile) map[string]BinlogFile {
	out := make(map[string]BinlogFile, len(rows))
	for k, v := range rows {
		out[k] = v
	}
	return out
}

func TestLegacyFailback_FirstStintOnlyInObjectStorageIsFetched(t *testing.T) {
	objects := map[string][]byte{}
	f := lfObjectFixture(t, objects)
	first := filepath.Join(f.dir, "mysql-bin.000004")
	body, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	objects["p/c/"+fbA+"/mysql-bin.000004"] = body
	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	// The old build pointed the plain row at the later file and filed it under B.
	f.row(t, "mysql-bin.000004", 1, "OPEN", "UPLOAD_FAILED", "p/c/"+fbB+"/mysql-bin.000004.sealed.e1")

	alert := f.s.AttachStorageAlert(t.Context(), f.task).StorageAlert
	if alert == nil || !strings.Contains(alert.Message, "not on disk") || !strings.Contains(alert.Detail, "not on disk: mysql-bin.000004") {
		t.Fatalf("alert does not name the missing first-stint file: %+v", alert)
	}
	files, _ := f.s.ListFiles(f.task.ID, 100)
	if chosen, warning := f.s.SelectReplayFilesFor(f.task, files); warning == "" || replayNames(chosen) != "mysql-bin.000003" {
		t.Fatalf("replay before repair: %s %q", replayNames(chosen), warning)
	}
	if held := f.s.legacyHeldSources(f.task.ID); !held["mysql-bin.000004"] {
		t.Fatalf("upload retry would push the legacy row: held=%v", held)
	}

	if err := f.s.RepairLegacyFailback(t.Context(), f.task); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(first)
	if err != nil || string(got) != string(body) {
		t.Fatalf("first stint not fetched back: %v", err)
	}
	if names := segNames(t, f.dir); names != fbA+"~2.mysql-bin.000004.open.e1,"+fbB+".mysql-bin.000002,mysql-bin.000003,mysql-bin.000004" {
		t.Fatalf("files %s", names)
	}
	for _, name := range mustReadDir(t, f.dir) {
		if strings.HasPrefix(name, ".legacy-fetch-") {
			t.Fatalf("fetch temp left: %s", name)
		}
	}
	if after := f.s.AttachStorageAlert(t.Context(), f.task); after.StorageAlert != nil {
		t.Fatalf("alert after repair: %+v", after.StorageAlert)
	}
	files, _ = f.s.ListFiles(f.task.ID, 100)
	chosen, warning := f.s.SelectReplayFilesFor(f.task, files)
	if want := "mysql-bin.000003,mysql-bin.000004," + fbB + ".mysql-bin.000002," + fbA + "~2.mysql-bin.000004.open.e1"; warning != "" || replayNames(chosen) != want {
		t.Fatalf("replay after repair: %s %q", replayNames(chosen), warning)
	}
	if held := f.s.legacyHeldSources(f.task.ID); len(held) != 0 {
		t.Fatalf("rows still held after repair: %v", held)
	}
	var detail string
	for _, ev := range f.events.events[f.task.ID] {
		if ev.Type == "STORAGE_REPAIRED" {
			detail = ev.Detail
		}
	}
	if !strings.Contains(detail, "orphaned_objects=p/c/"+fbB+"/mysql-bin.000004.sealed.e1") {
		t.Fatalf("STORAGE_REPAIRED does not list the orphaned key: %q", detail)
	}
}

func TestLegacyFailback_FirstStintMissingEverywhereIsRefused(t *testing.T) {
	for name, objects := range map[string]map[string][]byte{
		"no object": {},
		// The key holds the third stint's bytes (A:9-11): not the first stint.
		"wrong transactions": {"p/c/" + fbA + "/mysql-bin.000004": nil},
	} {
		t.Run(name, func(t *testing.T) {
			f := lfObjectFixture(t, objects)
			first := filepath.Join(f.dir, "mysql-bin.000004")
			if name == "wrong transactions" {
				third, err := os.ReadFile(filepath.Join(f.dir, "mysql-bin.000004.open.e1"))
				if err != nil {
					t.Fatal(err)
				}
				objects["p/c/"+fbA+"/mysql-bin.000004"] = third
			}
			if err := os.Remove(first); err != nil {
				t.Fatal(err)
			}
			before := segNames(t, f.dir)
			rows := copyRows(f.catalog.rows)
			err := f.s.RepairLegacyFailback(t.Context(), f.task)
			if err == nil || !IsPermanent(err) || !strings.Contains(err.Error(), CodeStorageInconsistent) || !strings.Contains(err.Error(), "mysql-bin.000004") {
				t.Fatalf("not refused: %v", err)
			}
			if got := segNames(t, f.dir); got != before {
				t.Fatalf("refusal changed files: %s", got)
			}
			for _, n := range mustReadDir(t, f.dir) {
				if strings.HasPrefix(n, ".legacy") {
					t.Fatalf("refusal left %s", n)
				}
			}
			if !reflect.DeepEqual(rows, f.catalog.rows) {
				t.Fatalf("refusal changed the catalog")
			}
			if cp := f.cps.cp[f.task.ID]; cp.File != "mysql-bin.000004" {
				t.Fatalf("refusal changed the checkpoint: %+v", cp)
			}
			files, _ := f.s.ListFiles(f.task.ID, 100)
			if chosen, warning := f.s.SelectReplayFilesFor(f.task, files); warning == "" || replayNames(chosen) != "mysql-bin.000003" {
				t.Fatalf("replay after refusal: %s %q", replayNames(chosen), warning)
			}
		})
	}
}

func TestLegacyFailback_UnparseableGTIDNamesSegment(t *testing.T) {
	f := legacyDir(t)
	evs := f.events.events[f.task.ID]
	for i := range evs {
		if evs[i].Type == "SOURCE_SWITCHOVER" {
			evs[i].Detail = strings.Replace(evs[i].Detail, "gtid_set="+fbA+":1-8 ", "gtid_set=zz-not-a-gtid ", 1)
			evs[i].Message = strings.Replace(evs[i].Message, "gtid_set="+fbA+":1-8;", "gtid_set=zz-not-a-gtid;", 1)
			break
		}
	}
	alert := f.s.AttachStorageAlert(t.Context(), f.task).StorageAlert
	if alert == nil || alert.Segment != "mysql-bin.000004" || strings.Contains(alert.Message, "before .") {
		t.Fatalf("alert without a segment: %+v", alert)
	}
	files, _ := f.s.ListFiles(f.task.ID, 100)
	if chosen, warning := f.s.SelectReplayFilesFor(f.task, files); !strings.HasPrefix(warning, "Segment mysql-bin.000004 ") || replayNames(chosen) != "mysql-bin.000003" {
		t.Fatalf("replay: %s %q", replayNames(chosen), warning)
	}
	if err := f.s.RepairLegacyFailback(t.Context(), f.task); err == nil || !strings.Contains(err.Error(), "cannot be parsed") {
		t.Fatalf("not refused: %v plan=%+v alert=%+v", err, f.s.legacyFailbackFor(f.task), alert)
	}
}

// storedHoleDir is a failback written by this build: A (000003, 000004),
// B, then A~2.
func storedHoleDir(t *testing.T) *lfFixture {
	t.Helper()
	at := time.Date(2026, 10, 9, 2, 0, 0, 0, time.UTC)
	f := newLFFixture(t)
	f.chain(t, fbA, fbB, fbA)
	lfSegment(t, filepath.Join(f.dir, "mysql-bin.000003"), fbA+":1", fbEvents(fbA, 2, 3, 4, 5)...)
	lfSegment(t, filepath.Join(f.dir, "mysql-bin.000004"), fbA+":1-5", fbEvents(fbA, 6, 7, 8)...)
	lfSegment(t, filepath.Join(f.dir, fbB+".mysql-bin.000002"), fbA+":1-5", fbEvents(fbB, 1, 2, 3)...)
	lfSegment(t, filepath.Join(f.dir, fbA+"~2.mysql-bin.000004.open.e2"), fbA+":1-5", fbEvents(fbA, 9, 10)...)
	f.addEvents(t,
		lfSwitch(fbA, fbB, fbA+":1-8", "mysql-bin.000004", at),
		lfSwitch(fbB, fbA, fbA+":1-8,"+fbB+":1-3", "mysql-bin.000002", at.Add(time.Minute)),
	)
	return f
}

func TestStoredHole_ReplayAndWindowStopBeforeIt(t *testing.T) {
	f := storedHoleDir(t)
	assertHealthy(t, f)
	if err := os.Remove(filepath.Join(f.dir, "mysql-bin.000004")); err != nil {
		t.Fatal(err)
	}
	alert := f.s.AttachStorageAlert(t.Context(), f.task).StorageAlert
	if alert == nil || alert.Segment != fbB+".mysql-bin.000002" || alert.MissingGTIDs != fbA+":6-8" || strings.Join(alert.ValidSegments, ",") != "mysql-bin.000003" {
		t.Fatalf("stored hole not reported: %+v", alert)
	}
	files, _ := f.s.ListFiles(f.task.ID, 100)
	chosen, warning := f.s.SelectReplayFilesFor(f.task, files)
	if replayNames(chosen) != "mysql-bin.000003" || !strings.Contains(warning, fbA+":6-8") {
		t.Fatalf("replay crosses the hole: %s %q", replayNames(chosen), warning)
	}
	w, err := f.s.RecoveryWindow(f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w.Continuous || len(w.Breaks) == 0 || !strings.Contains(w.Breaks[len(w.Breaks)-1].Reason, fbA+":6-8") {
		t.Fatalf("window continuous over a stored hole: %+v", w)
	}
	// Start is not refused for it: there is nothing to repair.
	if err := f.s.RepairLegacyFailback(t.Context(), f.task); err != nil {
		t.Fatalf("start refused: %v", err)
	}
}

func TestStoredHole_RetentionIsNotAHole(t *testing.T) {
	f := storedHoleDir(t)
	for _, name := range []string{"mysql-bin.000003", "mysql-bin.000004"} {
		if err := os.Remove(filepath.Join(f.dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	assertHealthy(t, f)
}

func TestRecoveryWindow_InteriorGapIsABreak(t *testing.T) {
	at := time.Date(2026, 10, 9, 3, 0, 0, 0, time.UTC)
	// B's header predates A:6-8, so the switch check alone sees no hole.
	views := []segmentView{
		fbView("mysql-bin.000003", at, fbA+":1", fbEvents(fbA, 2, 3, 4, 5)...),
		fbView(fbB+".mysql-bin.000002", at.Add(time.Hour), fbA+":1-5", fbEvents(fbB, 1, 2)...),
		fbView(fbA+"~2.mysql-bin.000004", at.Add(2*time.Hour), fbA+":1-5", fbEvents(fbA, 9, 10)...),
	}
	got, err := assessRecoveryFrom(views, "mysql", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Continuous || len(got.Breaks) != 1 || !strings.Contains(got.Breaks[0].Reason, fbA+":6-8") || got.Breaks[0].Files[0] != "mysql-bin.000003" || got.Breaks[0].Files[1] != fbA+"~2.mysql-bin.000004" {
		t.Fatalf("interior gap not a break: %+v", got)
	}
	// The start set covers it: a seed, not a hole.
	seeded, err := assessRecoveryFrom(views, "mysql", false, fbA+":1-8")
	if err != nil {
		t.Fatal(err)
	}
	if !seeded.Continuous {
		t.Fatalf("seeded gap reported: %+v", seeded)
	}
}

func (f *lfFixture) gtidStart(set string) {
	f.s.mu.Lock()
	cur := f.s.tasks[f.task.ID]
	cur.Start.Mode = StartModeGTID
	cur.Start.GTIDSet = set
	f.s.tasks[f.task.ID] = cur
	f.s.mu.Unlock()
	f.task = cur
}

// The upgrade harness layout: the first file holds only the mysqldump seed
// (skipped by the dump), so no stored transaction sits below the hole; the
// start set anchors it while the task's first file is still on disk.
func TestStoredHole_StartSetAnchorsWhileFirstFileIsHere(t *testing.T) {
	at := time.Date(2026, 10, 9, 2, 0, 0, 0, time.UTC)
	f := newLFFixture(t)
	f.gtidStart(fbA + ":1-5")
	f.chain(t, fbA, fbB, fbA)
	lfSegment(t, filepath.Join(f.dir, "mysql-bin.000003"), fbA+":1")
	lfSegment(t, filepath.Join(f.dir, "mysql-bin.000004"), fbA+":1-5", fbEvents(fbA, 6, 7, 8)...)
	lfSegment(t, filepath.Join(f.dir, fbB+".mysql-bin.000002"), fbA+":1-8", fbEvents(fbB, 1, 2, 3)...)
	lfSegment(t, filepath.Join(f.dir, fbA+"~2.mysql-bin.000004.open.e2"), fbA+":1-8", fbEvents(fbA, 9, 10)...)
	f.addEvents(t,
		lfSwitch(fbA, fbB, fbA+":1-8", "mysql-bin.000004", at),
		lfSwitch(fbB, fbA, fbA+":1-8,"+fbB+":1-3", "mysql-bin.000002", at.Add(time.Minute)),
	)
	assertHealthy(t, f)
	if err := os.Remove(filepath.Join(f.dir, "mysql-bin.000004")); err != nil {
		t.Fatal(err)
	}
	alert := f.s.AttachStorageAlert(t.Context(), f.task).StorageAlert
	if alert == nil || alert.Segment != fbB+".mysql-bin.000002" || alert.MissingGTIDs != fbA+":6-8" {
		t.Fatalf("hole after the seed-only first file not reported: %+v", alert)
	}
	files, _ := f.s.ListFiles(f.task.ID, 100)
	if chosen, warning := f.s.SelectReplayFilesFor(f.task, files); replayNames(chosen) != "mysql-bin.000003" || warning == "" {
		t.Fatalf("replay crosses the hole: %s %q", replayNames(chosen), warning)
	}
}

// Retention removed A's first stint. B's first header predates A:7-8, which
// B received inside that file and the dump skipped: not a hole.
func TestStoredHole_RetentionBeforeEarlyHeaderIsNotAHole(t *testing.T) {
	at := time.Date(2026, 10, 9, 2, 0, 0, 0, time.UTC)
	f := newLFFixture(t)
	f.gtidStart(fbA + ":1-5")
	f.chain(t, fbA, fbB, fbA)
	lfSegment(t, filepath.Join(f.dir, fbB+".mysql-bin.000002"), fbA+":1-6", fbEvents(fbB, 1, 2, 3)...)
	lfSegment(t, filepath.Join(f.dir, fbA+"~2.mysql-bin.000004.open.e2"), fbA+":1-8", fbEvents(fbA, 9, 10)...)
	f.addEvents(t,
		lfSwitch(fbA, fbB, fbA+":1-8", "mysql-bin.000004", at),
		lfSwitch(fbB, fbA, fbA+":1-8,"+fbB+":1-3", "mysql-bin.000002", at.Add(time.Minute)),
	)
	assertHealthy(t, f)
}

// A file removed from disk whose catalog row is UPLOADED still restores
// from object storage: not a hole.
func TestStoredHole_UploadedRowIsNotAHole(t *testing.T) {
	f := storedHoleDir(t)
	f.row(t, "mysql-bin.000003", 0, "SEALED", "UPLOADED", "p/c/"+fbA+"/mysql-bin.000003")
	f.row(t, "mysql-bin.000004", 0, "SEALED", "UPLOADED", "p/c/"+fbA+"/mysql-bin.000004")
	f.row(t, fbB+".mysql-bin.000002", 0, "SEALED", "UPLOADED", "p/c/"+fbB+"/"+fbB+".mysql-bin.000002")
	f.row(t, fbA+"~2.mysql-bin.000004", 2, "OPEN", "LOCAL_ONLY", "")
	if err := os.Remove(filepath.Join(f.dir, "mysql-bin.000004")); err != nil {
		t.Fatal(err)
	}
	if alert := f.s.AttachStorageAlert(t.Context(), f.task).StorageAlert; alert != nil {
		t.Fatalf("object-backed file reported as a hole: %+v", alert)
	}
}
