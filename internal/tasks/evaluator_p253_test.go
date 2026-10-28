// Package tasks provides module-level functionality for tasks.
// input: segments with intra-file GTID holes, waived evidence ranges, checkpoints ahead of the chain, and a source switch
// output: failback opens no false GTID_HOLE, a waiver closes the finding, intra-file holes produce canonical evidence ranges, CHECKPOINT_AHEAD stays managed, and assurance switches with the source chain
// pos: design 0009 §22.5 (P2-5.3)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"binlog_server/internal/binlog"
	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
)

// design 0009 §22.5: P2-5.3 through the evaluator.

func TestP253FailbackNoFalseGTIDHole(t *testing.T) {
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, err := s.CreateTaskFromSpec("g", "g-key", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "u", Password: "p", Flavor: "mysql"},
		&StartConfig{Mode: StartModeGTID, GTIDSet: "5b4bc6b5-c3b5-11f1-84bd-822b383dbcd0:1-104"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(data, task.ID)
	windowCopyDir(t, filepath.Join("..", "binlog", "testdata", "ledger", "failback"), dir)
	windowPinTimes(t, dir)
	store := healthstore.NewFileStore(filepath.Join(t.TempDir(), "h"))
	sweepTwice(t, newEval(t, s, store))
	for k, f := range openOf(t, store, task.ID) {
		if f.Kind == health.GTIDHole {
			t.Fatalf("failback opened GTID_HOLE %s: %+v", k, f)
		}
	}
}

func TestP253GTIDHoleWaiveCloses(t *testing.T) {
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, err := s.CreateTaskFromSpec("g", "g-key", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "u", Password: "p", Flavor: "mysql"},
		&StartConfig{Mode: StartModeGTID, GTIDSet: gtidPITRUUID + ":1-1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(data, task.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// One segment with an intra-file hole (seqs 2,3 then 6,7 → missing 4-5).
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000001"), 2, 3, 6, 7)
	store := healthstore.NewFileStore(filepath.Join(t.TempDir(), "h"))
	e := newEval(t, s, store)
	if err := e.SweepNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	open := openOf(t, store, task.ID)
	var hole health.Finding
	found := false
	for _, f := range open {
		if f.Kind == health.GTIDHole {
			hole, found = f, true
			break
		}
	}
	if !found {
		t.Fatalf("want GTID_HOLE open, got %+v", open)
	}
	ev := hole.Evidence
	if !strings.Contains(ev, gtidPITRUUID) {
		t.Fatalf("evidence %q should be a GTID range", ev)
	}
	// Uppercase uuid; CanonicalGTIDRanges normalizes to the finding's evidence.
	submit := strings.ToUpper(ev)
	if err := e.WaiveGTIDHole(context.Background(), task.ID, hole.Subject, submit, "gtid_next skip", "dba"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.EvaluateNow(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	for _, f := range openOf(t, store, task.ID) {
		if f.Kind == health.GTIDHole {
			t.Fatalf("waived hole still open: %+v", f)
		}
	}
}

type p253CP struct {
	cp binlog.Checkpoint
}

func (c *p253CP) LoadCheckpoint(_ context.Context, _ string) (binlog.Checkpoint, bool, error) {
	return c.cp, c.cp.File != "" || c.cp.GTIDSet != "", nil
}

func TestP253CheckpointAheadManaged(t *testing.T) {
	data := t.TempDir()
	cps := &p253CP{}
	s := NewScheduler(WithDataDir(data), WithCheckpointReader(cps))
	task, err := s.CreateTaskFromSpec("g", "g-key", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "u", Password: "p", Flavor: "mysql"},
		&StartConfig{Mode: StartModeGTID, GTIDSet: gtidPITRUUID + ":1-1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(data, task.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000001"), 2, 3)
	store := healthstore.NewFileStore(filepath.Join(t.TempDir(), "h"))
	e := newEval(t, s, store)
	cps.cp = binlog.Checkpoint{GTIDSet: "bbbbbbbb-0000-0000-0000-000000000002:1-1", File: "mysql-bin.000001", Pos: 4}
	if err := e.SweepNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range openOf(t, store, task.ID) {
		if f.Kind == health.CheckpointAhead {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("want CHECKPOINT_AHEAD, got %+v", openOf(t, store, task.ID))
	}
	// Checkpoint only lists what the chain holds (start 1 + stored 2-3).
	cps.cp = binlog.Checkpoint{GTIDSet: gtidPITRUUID + ":1-3", File: "mysql-bin.000001", Pos: 4}
	if err := e.SweepNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, f := range openOf(t, store, task.ID) {
		if f.Kind == health.CheckpointAhead {
			t.Fatalf("CHECKPOINT_AHEAD still open: %+v", f)
		}
	}
}

func TestP253AssuranceSwitchesFromSourceChain(t *testing.T) {
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, err := s.CreateTaskFromSpec("g", "g-key", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "u", Password: "p", Flavor: "mysql"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(data, task.ID)
	windowCopyDir(t, filepath.Join("..", "binlog", "testdata", "ledger", "failback"), dir)
	sw := s.assuranceSwitches(task.ID)
	if len(sw) == 0 {
		t.Fatal("expected Switches from .source-chain")
	}
	want := "5e86458b-c3b5-11f1-8415-822b383dbcd0"
	found := false
	for _, x := range sw {
		if x.Identity == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("switches %+v missing %s", sw, want)
	}
}
