// Package tasks provides module-level functionality for tasks.
// input: phase-1 golden directories and a sized legacy directory whose sealed row or local file is wrong
// output: proof that the P2-5.1 chain.local kinds open and close through the evaluator, and that a cluster owner derives I6_MISSING_LOCAL for a sealed file that is gone (§31)
// pos: evaluator coverage for design 0009 §20.5 and §31
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// design 0009 §20.5: the P2-5.1 chain.local kinds through the evaluator.

var p251 = map[health.Kind]bool{health.SegmentDamaged: true, health.ChainIndexGap: true, health.LegacyFailback: true, health.SegmentMissing: true}

func p251Open(t *testing.T, store healthstore.Store, id string) map[string]health.Finding {
	t.Helper()
	out := map[string]health.Finding{}
	for k, f := range openOf(t, store, id) {
		if p251[f.Kind] {
			out[k] = f
		}
	}
	return out
}

func sweepTwice(t *testing.T, e *Evaluator) {
	t.Helper()
	for i := 0; i < 2; i++ {
		if err := e.SweepNow(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

// The 47 phase-1 golden directories: no false positive on a healthy or
// failback directory; damage and the live hole are found.
func TestP251GoldenDirectories(t *testing.T) {
	for _, c := range windowGoldenCases() {
		data := t.TempDir()
		opts := []Option{WithDataDir(data)}
		if c.objects {
			opts = append(opts, WithFileUploader(goldenUploader{}))
		}
		s := NewScheduler(opts...)
		var start *StartConfig
		if c.start != "" {
			start = &StartConfig{Mode: StartModeGTID, GTIDSet: c.start}
		}
		task, err := s.CreateTaskFromSpec("g", "g-key", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: c.flavor}, start, nil)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(data, task.ID)
		windowCopyDir(t, c.dir, dir)
		if c.mutate != nil {
			c.mutate(t, dir)
		}
		windowPinTimes(t, dir)
		store := healthstore.NewFileStore(filepath.Join(t.TempDir(), "h"))
		sweepTwice(t, newEval(t, s, store, EvaluatorOn))
		kinds := map[health.Kind]bool{}
		for _, f := range p251Open(t, store, task.ID) {
			kinds[f.Kind] = true
		}
		want := map[health.Kind]bool{}
		switch {
		case strings.Contains(c.name, "-damaged"):
			want[health.SegmentDamaged] = true
		case strings.HasPrefix(c.name, "live-hole"):
			want[health.ChainIndexGap] = true
		}
		if len(kinds) != len(want) {
			t.Errorf("%s: open P2-5.1 kinds %v, want %v", c.name, kinds, want)
			continue
		}
		for k := range want {
			if !kinds[k] {
				t.Errorf("%s: %s not open (open %v)", c.name, k, kinds)
			}
		}
	}
}

func TestP251LegacyFailbackOpensAndClosesAfterRepair(t *testing.T) {
	f := legacyDir(t)
	store := healthstore.NewFileStore(filepath.Join(t.TempDir(), "h"))
	e := newEval(t, f.s, store, EvaluatorOn)
	sweepTwice(t, e)
	open := p251Open(t, store, f.task.ID)
	if _, ok := open[string(health.LegacyFailback)+"/mysql-bin.000004"]; !ok || len(open) != 1 {
		t.Fatalf("open before repair %v, want only LEGACY_FAILBACK/mysql-bin.000004", open)
	}
	if err := f.s.RepairLegacyFailback(t.Context(), f.task); err != nil {
		t.Fatal(err)
	}
	sweepTwice(t, e)
	if open := p251Open(t, store, f.task.ID); len(open) != 0 {
		t.Fatalf("open after repair %v", open)
	}
}

// sizedDir: three sealed legacy segments (no ledger lines), rows with
// the disk sizes.
func sizedDir(t *testing.T) *lfFixture {
	t.Helper()
	return sizedDirWith(t)
}

func sizedDirWith(t *testing.T, opts ...Option) *lfFixture {
	t.Helper()
	f := newLFFixture(t, opts...)
	f.chain(t, fbA)
	lfSegment(t, filepath.Join(f.dir, "mysql-bin.000001"), fbA+":1", fbEvents(fbA, 2, 3)...)
	lfSegment(t, filepath.Join(f.dir, "mysql-bin.000002"), fbA+":1-3", fbEvents(fbA, 4, 5)...)
	lfSegment(t, filepath.Join(f.dir, "mysql-bin.000003.open.e0"), fbA+":1-5", fbEvents(fbA, 6)...)
	f.row(t, "mysql-bin.000001", 0, "SEALED", "LOCAL_ONLY", "")
	f.row(t, "mysql-bin.000002", 0, "SEALED", "LOCAL_ONLY", "")
	f.row(t, "mysql-bin.000003", 0, "OPEN", "LOCAL_ONLY", "")
	for k, row := range f.catalog.rows {
		if info, err := os.Stat(row.FilePath); err == nil {
			row.SizeBytes = info.Size()
			f.catalog.rows[k] = row
		}
	}
	return f
}

func setRowSize(f *lfFixture, name string, delta int64) {
	k := catalogKey(name, 0)
	row := f.catalog.rows[k]
	row.SizeBytes += delta
	f.catalog.rows[k] = row
}

func TestP251LegacyRowSizeMismatchOpensAndCloses(t *testing.T) {
	f := sizedDir(t)
	setRowSize(f, "mysql-bin.000001", -10)
	store := healthstore.NewFileStore(filepath.Join(t.TempDir(), "h"))
	e := newEval(t, f.s, store, EvaluatorOn)
	sweepTwice(t, e)
	open := p251Open(t, store, f.task.ID)
	got, ok := open[string(health.SegmentMissing)+"/mysql-bin.000001"]
	if !ok || len(open) != 1 {
		t.Fatalf("open %v, want SEGMENT_MISSING/mysql-bin.000001", open)
	}
	if !strings.Contains(got.Detail+got.Evidence, "row=") {
		t.Errorf("finding carries no row size: %+v", got)
	}
	setRowSize(f, "mysql-bin.000001", 10)
	e.MarkDirty(f.task.ID, ReasonCatalog)
	sweepTwice(t, e)
	if open := p251Open(t, store, f.task.ID); len(open) != 0 {
		t.Fatalf("open after the row is fixed %v", open)
	}
}

func TestP251LedgerLineWinsOverStaleRow(t *testing.T) {
	f := sizedDir(t)
	p := filepath.Join(f.dir, "mysql-bin.000001")
	info, _ := os.Stat(p)
	if err := binlog.RecordSealSum(f.dir, "mysql-bin.000001", info.Size(), fileSHA(t, p)); err != nil {
		t.Fatal(err)
	}
	setRowSize(f, "mysql-bin.000001", -10)
	store := healthstore.NewFileStore(filepath.Join(t.TempDir(), "h"))
	sweepTwice(t, newEval(t, f.s, store, EvaluatorOn))
	if open := p251Open(t, store, f.task.ID); len(open) != 0 {
		t.Fatalf("stale row reported despite a matching ledger line: %v", open)
	}
}

func TestP251SealedRowLocalMissing(t *testing.T) {
	f := sizedDir(t)
	if err := os.Remove(filepath.Join(f.dir, "mysql-bin.000002")); err != nil {
		t.Fatal(err)
	}
	store := healthstore.NewFileStore(filepath.Join(t.TempDir(), "h"))
	e := newEval(t, f.s, store, EvaluatorOn)
	sweepTwice(t, e)
	if _, ok := p251Open(t, store, f.task.ID)[string(health.SegmentMissing)+"/mysql-bin.000002"]; !ok {
		t.Fatalf("SEGMENT_MISSING/mysql-bin.000002 not open: %v", p251Open(t, store, f.task.ID))
	}
	// §31: the owning disk reports a missing sealed segment in cluster mode too.
	e.cfg.Cluster = true
	facts := e.chainFacts(f.task, time.Now())
	found := false
	for _, v := range facts.Violations {
		if v.Kind == assurance.I6MissingLocal {
			found = true
		}
	}
	if !found {
		t.Fatal("cluster owner did not derive I6_MISSING_LOCAL")
	}
}

// One pass that sees a structural problem opens nothing and queues a CONFIRM pass.
func TestP251SinglePassDoesNotOpen(t *testing.T) {
	f := sizedDir(t)
	setRowSize(f, "mysql-bin.000001", -10)
	store := healthstore.NewFileStore(filepath.Join(t.TempDir(), "h"))
	e := newEval(t, f.s, store, EvaluatorOn)
	if _, err := e.EvaluateNow(t.Context(), f.task.ID); err != nil {
		t.Fatal(err)
	}
	if open := p251Open(t, store, f.task.ID); len(open) != 0 {
		t.Fatalf("one pass opened %v", open)
	}
	e.mu.Lock()
	_, queued := e.dirty[f.task.ID][ReasonConfirm]
	e.mu.Unlock()
	if !queued {
		t.Fatal("no CONFIRM pass queued")
	}
}

func fileSHA(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
