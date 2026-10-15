// Package tasks provides module-level functionality for tasks.
// input: a task directory whose sealed middle segment was truncated on disk: to 0 bytes, to 3 bytes, inside an event header, or exactly at an event boundary with its size at seal in the .seal-sha256 ledger; and real MySQL 8.0 segments truncated to 0 bytes with and without a catalog
// output: assertions that storageHealth calls the segment unreadable and names it, /replay stops before it with a warning, /window has a not-readable break naming it, a SEGMENT_UNREADABLE finding is open (legacy path and evaluator), and that a rewrite recorded in the ledger is not flagged
// pos: regression for issue 381 (BinlogServerQA round 2 on #380: a 0-byte sealed segment raised nothing and /replay kept it)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"binlog_server/internal/binlog"
	"binlog_server/internal/healthstore"
)

// firstEventEnd is the offset just past the first event (the format
// description) of the segment at path.
func firstEventEnd(t *testing.T, path string) int64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return 4 + int64(binary.LittleEndian.Uint32(b[4+9:4+13]))
}

func recordSeal(t *testing.T, path string) {
	t.Helper()
	facts, err := binlog.ReadSealFacts(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := binlog.RecordSealFacts(path, facts); err != nil {
		t.Fatal(err)
	}
}

func TestTruncatedSealedSegment_WindowReplayAndFinding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ledger bool
		size   func(t *testing.T, path string) int64
	}{
		{"zero-bytes", false, func(*testing.T, string) int64 { return 0 }},
		{"zero-bytes-ledger", true, func(*testing.T, string) int64 { return 0 }},
		{"three-bytes", false, func(*testing.T, string) int64 { return 3 }},
		{"inside-event-header", false, func(t *testing.T, p string) int64 { return firstEventEnd(t, p) + 10 }},
		{"event-boundary-ledger", true, func(t *testing.T, p string) int64 { return firstEventEnd(t, p) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			for i, n := range []string{"mysql-bin.000001", "mysql-bin.000002", "mysql-bin.000003"} {
				p := filepath.Join(taskDir, n)
				writeAlertSegment(t, p, int64(37+2*i), int64(38+2*i))
				if tc.ledger {
					recordSeal(t, p)
				}
			}
			target := filepath.Join(taskDir, "mysql-bin.000002")
			if h := s.storageHealth(task, ""); h.Found() {
				t.Fatalf("clean chain: %+v", h)
			}
			if err := os.Truncate(target, tc.size(t, target)); err != nil {
				t.Fatal(err)
			}

			h := s.storageHealth(task, "")
			if h.Kind != StorageUnreadable || h.Problem.Segment != "mysql-bin.000002" {
				t.Fatalf("verdict %+v, want unreadable mysql-bin.000002", h)
			}
			if w := h.ReplayWarning(); !strings.HasPrefix(w, "Segment mysql-bin.000002 is not readable") {
				t.Fatalf("warning %q", w)
			}
			files := mustListFiles(t, s, task.ID)
			if got := replayNames(h.replayCut(files, SelectReplayFiles(files))); got != "mysql-bin.000001" {
				t.Fatalf("replay keeps %q", got)
			}
			w, err := s.RecoveryWindow(task.ID)
			if err != nil {
				t.Fatalf("/window failed instead of degrading: %v", err)
			}
			named := false
			for _, b := range w.Breaks {
				if len(b.Files) > 0 && b.Files[0] == "mysql-bin.000002" && strings.Contains(b.Reason, "not readable") {
					named = true
				}
			}
			if w.Continuous || !named {
				t.Fatalf("no not-readable break naming the truncated segment: %+v", w.Breaks)
			}
			open, err := s.ListFindings(context.Background(), task.ID, false, 50)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, f := range open {
				if f.Kind == FindingSegmentUnreadable && f.Subject == "mysql-bin.000002" && f.Severity == FindingCritical {
					found = true
				}
			}
			if !found {
				t.Fatalf("no SEGMENT_UNREADABLE finding: %+v", open)
			}
		})
	}
}

// A sealed segment rewritten on purpose records its new size in the ledger
// (the last line of a name wins), so it is not flagged.
func TestTruncatedSealedSegment_RewriteRecordedInLedgerIsReadable(t *testing.T) {
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
	p := filepath.Join(taskDir, "mysql-bin.000001")
	writeAlertSegment(t, p, 37, 38)
	recordSeal(t, p)
	writeAlertSegment(t, p, 37, 38, 39)
	recordSeal(t, p)
	if h := s.storageHealth(task, ""); h.Found() {
		t.Fatalf("rewrite recorded at seal is flagged: %+v", h)
	}
}

// TestEvaluator_ZeroByteRealSegment is QA's #381 reproduction on real MySQL
// 8.0 segments, without meta and with a catalog: truncating a sealed segment
// to 0 bytes opens SEGMENT_UNREADABLE and /window names it.
func TestEvaluator_ZeroByteRealSegment(t *testing.T) {
	for _, catalog := range []bool{false, true} {
		name := "disk-only"
		if catalog {
			name = "catalog"
		}
		t.Run(name, func(t *testing.T) {
			var (
				s    *Scheduler
				task Task
				data string
				dir  string
			)
			if catalog {
				s, task, data, dir, _ = catalogTask(t)
			} else {
				s, task, data, dir = realTask(t)
			}
			store := healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))
			e := newEval(t, s, store, EvaluatorOn)
			ctx := context.Background()
			if err := e.SweepNow(ctx); err != nil {
				t.Fatal(err)
			}
			if open := openOf(t, store, task.ID); len(open) != 0 {
				t.Fatalf("clean real segments: open=%+v", open)
			}
			target := realMySQL.files[0]
			if err := os.Truncate(filepath.Join(dir, target), 0); err != nil {
				t.Fatal(err)
			}
			if err := e.SweepNow(ctx); err != nil {
				t.Fatal(err)
			}
			if _, ok := openOf(t, store, task.ID)["SEGMENT_UNREADABLE/"+target]; !ok {
				t.Fatalf("0-byte sealed segment opens no SEGMENT_UNREADABLE: %+v", openOf(t, store, task.ID))
			}
			if !windowSaysUnreadable(t, s, task.ID) {
				w, _ := s.RecoveryWindow(task.ID)
				t.Fatalf("/window does not name the 0-byte segment: %+v", w.Breaks)
			}
		})
	}
}
