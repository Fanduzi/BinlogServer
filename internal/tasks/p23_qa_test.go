// Package tasks provides module-level functionality for tasks.
// input: real MySQL 8.0 segments (testdata/real-mysql80, CRC32, GTID on) corrupted on disk the way BinlogServerQA did on #380, a real disk-only scheduler and a FileStore
// output: regression tests for the #380 QA findings: mid-file 0x3f/0xff corruption opens SEGMENT_UNREADABLE exactly when /window says the segment is not readable, an in-place flip with size and mtime kept is caught by the rehasher from the seal checksum on disk, a half-length .open copy keeps SEGMENT_OVERLAP open
// pos: ADR 0009 phase 2 P2-3 QA round 1 (docs/design/0009-phase2-health-evaluator.md §17)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/assurance"
	"binlog_server/internal/binlog"
	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
)

const realUUID = "2887ee3f-c849-11f1-8ab4-0242ac11000c"

// realFixture is a pair of real sealed segments from one server.
type realFixture struct {
	flavor, dir string
	files       []string
	start       *StartConfig
}

var (
	realMySQL   = realFixture{"mysql", "real-mysql80", []string{"mysql-bin.000004", "mysql-bin.000005"}, &StartConfig{Mode: StartModeGTID, GTIDSet: realUUID + ":1-5"}}
	realMariaDB = realFixture{"mariadb", "real-mariadb114", []string{"mysql-bin.000003", "mysql-bin.000004"}, nil}
)

// realTask copies real segments into a fresh disk-only task.
func realTask(t *testing.T, fx ...realFixture) (*Scheduler, Task, string, string) {
	t.Helper()
	f := realMySQL
	if len(fx) > 0 {
		f = fx[0]
	}
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, err := s.CreateTaskFromSpec(f.flavor, "real-key-"+f.flavor, &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: f.flavor}, f.start, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(data, task.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range f.files {
		copyFile(t, filepath.Join("testdata", f.dir, n), filepath.Join(dir, n))
	}
	return s, task, data, dir
}

// smash writes b at the middle of path and keeps its mtime, like touch -r.
// Linux stamps ctime from the coarse clock (one tick, up to 10ms), so it
// first waits a tick: a real operator edit is never inside the same tick as
// the evaluation before it, and the rehasher covers one that is.
func smash(t *testing.T, path string, b []byte) {
	t.Helper()
	time.Sleep(20 * time.Millisecond)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	copy(raw[len(raw)/2:], b)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
}

func windowSaysUnreadable(t *testing.T, s *Scheduler, taskID string) bool {
	t.Helper()
	w, err := s.RecoveryWindow(taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range w.Breaks {
		if strings.Contains(b.Reason, "not readable") {
			return true
		}
	}
	return false
}

func TestEvaluator_MidFileCorruptionAgreesWithWindow(t *testing.T) {
	for _, tc := range []struct {
		name string
		fx   realFixture
		b    []byte
	}{
		{"mysql-3f", realMySQL, []byte{0x3f, 0x3f, 0x3f, 0x3f}},
		{"mysql-ff", realMySQL, []byte{0xff, 0xff, 0xff, 0xff}},
		{"mariadb-3f", realMariaDB, []byte{0x3f, 0x3f, 0x3f, 0x3f}},
		{"mariadb-ff", realMariaDB, []byte{0xff, 0xff, 0xff, 0xff}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, task, data, dir := realTask(t, tc.fx)
			store := healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))
			e := newEval(t, s, store, EvaluatorOn)
			ctx := context.Background()
			if err := e.SweepNow(ctx); err != nil {
				t.Fatal(err)
			}
			if open := openOf(t, store, task.ID); len(open) != 0 || windowSaysUnreadable(t, s, task.ID) {
				t.Fatalf("clean real segments: open=%+v", open)
			}
			smash(t, filepath.Join(dir, tc.fx.files[0]), tc.b)
			if err := e.SweepNow(ctx); err != nil {
				t.Fatal(err)
			}
			_, finding := openOf(t, store, task.ID)["SEGMENT_UNREADABLE/"+tc.fx.files[0]]
			if !windowSaysUnreadable(t, s, task.ID) {
				w, _ := s.RecoveryWindow(task.ID)
				t.Fatalf("/window does not see mid-file %s corruption: %+v open=%+v", tc.name, w.Breaks, openOf(t, store, task.ID))
			}
			if !finding {
				t.Fatalf("/window says not readable but no SEGMENT_UNREADABLE: %+v", openOf(t, store, task.ID))
			}
		})
	}
}

// TestRehasher_DiskLedgerCatchesInPlaceFlip is QA's bitrot case without
// meta: the seal records the checksum in the task directory (the same call
// the runner makes), and a 4-byte flip that keeps size and mtime opens
// SEGMENT_UNREADABLE within one cycle, reading every sealed byte once.
func TestRehasher_DiskLedgerCatchesInPlaceFlip(t *testing.T) {
	for _, fx := range []realFixture{realMySQL, realMariaDB} {
		t.Run(fx.flavor, func(t *testing.T) {
			s, task, data, dir := realTask(t, fx)
			var total int64
			for _, n := range fx.files {
				p := filepath.Join(dir, n)
				facts, err := binlog.ReadSealFacts(p)
				if err != nil {
					t.Fatal(err)
				}
				if err := binlog.RecordSealFacts(p, facts); err != nil {
					t.Fatal(err)
				}
				total += facts.Size
			}
			store := healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))
			e := newEval(t, s, store, EvaluatorOn)
			r := NewRehasher(e, 64<<20)
			ctx := context.Background()
			cycle := func() {
				for range fx.files {
					if _, err := r.Step(ctx); err != nil {
						t.Fatal(err)
					}
				}
				if err := e.SweepNow(ctx); err != nil {
					t.Fatal(err)
				}
			}
			cycle()
			if st := r.Stats(); st.Bytes != total || st.Segments != int64(len(fx.files)) || st.Unavailable != 0 || st.Mismatches != 0 {
				t.Fatalf("clean cycle stats %+v, want %d bytes over %d segments", st, total, len(fx.files))
			}
			target := filepath.Join(dir, fx.files[1])
			orig, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			info, _ := os.Stat(target)
			// The last 4 bytes are the CRC32 trailer of the last event: only
			// a checksum sees them change, never the event walk.
			raw := append([]byte(nil), orig...)
			for i := len(raw) - 4; i < len(raw); i++ {
				raw[i] ^= 0xff
			}
			if err := os.WriteFile(target, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(target, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			cycle()
			// The chain may also see the bad CRC once ctime has ticked; the
			// rehasher must see it either way (bit rot does not touch ctime).
			_, ok := openOf(t, store, task.ID)["SEGMENT_UNREADABLE/"+fx.files[1]]
			if st := r.Stats(); !ok || st.Mismatches != 1 || st.Bytes != 2*total {
				t.Fatalf("in-place flip not found within one cycle: %+v stats=%+v", openOf(t, store, task.ID), r.Stats())
			}
			time.Sleep(20 * time.Millisecond) // a ctime tick, as in smash
			if err := os.WriteFile(target, orig, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(target, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			cycle()
			if open := openOf(t, store, task.ID); len(open) != 0 {
				t.Fatalf("repaired segment still open: %+v", open)
			}
		})
	}
}

// TestEvaluator_HalfOpenCopyStaysOpen is QA's "half" case: the first half
// of a sealed segment copied to <name>.open.e3 is a second copy of that
// source file. SEGMENT_OVERLAP opens and stays open over later sweeps (no
// false close while the file is there), then closes when it is removed.
func TestEvaluator_HalfOpenCopyStaysOpen(t *testing.T) {
	for _, fx := range []realFixture{realMySQL, realMariaDB} {
		t.Run(fx.flavor, func(t *testing.T) {
			s, task, data, dir := realTask(t, fx)
			store := healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))
			e := newEval(t, s, store, EvaluatorOn)
			ctx := context.Background()
			src := filepath.Join(dir, fx.files[0])
			raw, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			half := src + ".open.e3"
			if err := os.WriteFile(half, raw[:len(raw)/2], 0o644); err != nil {
				t.Fatal(err)
			}
			key := "SEGMENT_OVERLAP/" + filepath.Base(half)
			for i := 0; i < 5; i++ {
				if err := e.SweepNow(ctx); err != nil {
					t.Fatal(err)
				}
				if _, ok := openOf(t, store, task.ID)[key]; !ok {
					t.Fatalf("sweep %d: half-length open copy not an open overlap: %+v order=%v", i, openOf(t, store, task.ID), s.replayOrder(task.ID))
				}
				time.Sleep(15 * time.Millisecond)
			}
			if err := os.Remove(half); err != nil {
				t.Fatal(err)
			}
			if err := e.SweepNow(ctx); err != nil {
				t.Fatal(err)
			}
			if open := openOf(t, store, task.ID); len(open) != 0 {
				t.Fatalf("removed copy still open: %+v", open)
			}
		})
	}
}

// catalogTask is realTask with a catalog that has a sealed row for each
// real segment, as the runner writes at seal.
func catalogTask(t *testing.T) (*Scheduler, Task, string, string, *shaFileStore) {
	t.Helper()
	data := t.TempDir()
	cat := &shaFileStore{}
	s := NewScheduler(WithDataDir(data), WithFileStore(cat))
	task, err := s.CreateTaskFromSpec("mysql", "real-cat", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: "mysql"}, realMySQL.start, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(data, task.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range realMySQL.files {
		p := filepath.Join(dir, n)
		copyFile(t, filepath.Join("testdata", realMySQL.dir, n), p)
		cat.add(t, task.ID, p)
	}
	return s, task, data, dir, cat
}

func kindsOf(f *health.ChainFacts) map[assurance.Kind][]string {
	out := map[assurance.Kind][]string{}
	if f == nil {
		return out
	}
	for _, v := range f.Violations {
		out[v.Kind] = append(out[v.Kind], v.Segments...)
	}
	return out
}

// TestEvaluator_CatalogRowsReachTheChain is QA's I6_MISSING_ROW flood with
// meta: every catalogued sealed segment has its row, so the chain reports
// no I6_MISSING_ROW; without a catalog there are no rows to miss.
func TestEvaluator_CatalogRowsReachTheChain(t *testing.T) {
	s, task, data, _, _ := catalogTask(t)
	e := newEval(t, s, healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName)), EvaluatorOn)
	if got := kindsOf(e.chainFacts(task, time.Now())); len(got[assurance.I6MissingRow]) != 0 {
		t.Fatalf("catalogued segments reported without a row: %v", got)
	}
	ds, dtask, ddata, _ := realTask(t)
	de := newEval(t, ds, healthstore.NewFileStore(filepath.Join(ddata, healthstore.HealthFileName)), EvaluatorOn)
	if got := kindsOf(de.chainFacts(dtask, time.Now())); len(got[assurance.I6MissingRow]) != 0 {
		t.Fatalf("disk-only task has no catalog to miss rows in: %v", got)
	}
}

// TestEvaluator_CatalogModeSeesHandAddedDiskCopy is QA's meta-mode case: a
// zero-filled or full .sealed.e3 copy put on disk by hand has no catalog
// row, yet it is reconciled into the chain, so SEGMENT_OVERLAP opens (and
// the chain names the missing row), and closes when the copy is removed.
func TestEvaluator_CatalogModeSeesHandAddedDiskCopy(t *testing.T) {
	for _, kind := range []string{"zero", "copy"} {
		t.Run(kind, func(t *testing.T) {
			s, task, data, dir, _ := catalogTask(t)
			store := healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))
			e := newEval(t, s, store, EvaluatorOn)
			ctx := context.Background()
			src := filepath.Join(dir, realMySQL.files[0])
			dup := src + ".sealed.e3"
			b := make([]byte, 4096)
			if kind == "copy" {
				var err error
				if b, err = os.ReadFile(src); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(dup, b, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := e.SweepNow(ctx); err != nil {
				t.Fatal(err)
			}
			open := openOf(t, store, task.ID)
			if _, ok := open["SEGMENT_OVERLAP/"+filepath.Base(dup)]; !ok {
				t.Fatalf("hand-added %s copy not seen with a catalog: %+v", kind, open)
			}
			if got := kindsOf(e.chainFacts(task, time.Now())); len(got[assurance.I6MissingRow]) != 1 || got[assurance.I6MissingRow][0] != filepath.Base(dup) {
				t.Fatalf("I6_MISSING_ROW %v, want only the hand-added copy", got[assurance.I6MissingRow])
			}
			if err := os.Remove(dup); err != nil {
				t.Fatal(err)
			}
			if err := e.SweepNow(ctx); err != nil {
				t.Fatal(err)
			}
			if open := openOf(t, store, task.ID); len(open) != 0 {
				t.Fatalf("removed copy still open: %+v", open)
			}
		})
	}
}
