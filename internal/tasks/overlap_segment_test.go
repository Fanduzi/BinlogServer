// Package tasks provides module-level functionality for tasks.
// input: a task directory where one sealed source file has a second sealed copy (mysql-bin.000002 and mysql-bin.000002.sealed.e3) that is a full byte copy or all zeros
// output: assertions that storageHealth calls it an overlap and names the later copy, the storage alert is set, /window has a break naming it, /replay keeps only the segments before both copies with a warning, and a SEGMENT_OVERLAP finding is open; two epochs that continue each other stay ok
// pos: regression for issue 370 (ADR 0009 I4 at runtime)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOverlapSegment_WindowReplayAlertAndFinding(t *testing.T) {
	copies := map[string]func(t *testing.T, src, dst string){
		"full-copy": func(t *testing.T, src, dst string) {
			b, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, b, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"zero-filled": func(t *testing.T, src, dst string) {
			info, err := os.Stat(src)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, make([]byte, info.Size()), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, copyFn := range copies {
		t.Run(name, func(t *testing.T) {
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
			writeAlertSegment(t, filepath.Join(taskDir, "mysql-bin.000001"), 37, 38)
			writeAlertSegment(t, filepath.Join(taskDir, "mysql-bin.000002"), 39, 40)
			writeAlertSegment(t, filepath.Join(taskDir, "mysql-bin.000003"), 41, 42)
			copyFn(t, filepath.Join(taskDir, "mysql-bin.000002"), filepath.Join(taskDir, "mysql-bin.000002.sealed.e3"))

			h := s.storageHealth(task, "")
			if h.Kind != StorageOverlap || h.Problem.Segment != "mysql-bin.000002.sealed.e3" {
				t.Fatalf("verdict %+v, want overlap at mysql-bin.000002.sealed.e3", h)
			}
			if w := h.ReplayWarning(); !strings.HasPrefix(w, "Source file mysql-bin.000002 is stored twice") {
				t.Fatalf("warning %q", w)
			}
			files := mustListFiles(t, s, task.ID)
			if got := replayNames(h.replayCut(files, SelectReplayFiles(files))); got != "mysql-bin.000001" {
				t.Fatalf("replay keeps %q, want only mysql-bin.000001", got)
			}
			chosen, warning := s.SelectReplayFilesFor(task, files)
			if got := replayNames(chosen); got != "mysql-bin.000001" || warning == "" {
				t.Fatalf("/replay keeps %q with warning %q", got, warning)
			}
			if alert := s.AttachStorageAlert(context.Background(), task).StorageAlert; alert == nil || alert.Segment != "mysql-bin.000002.sealed.e3" {
				t.Fatalf("storage alert %+v", alert)
			}
			w, err := s.RecoveryWindow(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if w.Continuous {
				t.Fatal("window continuous over a duplicated source file")
			}
			named := false
			for _, b := range w.Breaks {
				if len(b.Files) > 0 && b.Files[0] == "mysql-bin.000002.sealed.e3" && strings.Contains(b.Reason, "stored twice") {
					named = true
				}
			}
			if !named {
				t.Fatalf("no overlap break naming the copy: %+v", w.Breaks)
			}
			open, err := s.ListFindings(context.Background(), task.ID, false, 50)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, f := range open {
				if f.Kind == FindingSegmentOverlap && f.Subject == "mysql-bin.000002.sealed.e3" && f.Severity == FindingCritical {
					found = true
				}
			}
			if !found {
				t.Fatalf("no SEGMENT_OVERLAP finding: %+v", open)
			}
		})
	}
}

// Two sealed epochs of one source file that continue each other (the second
// holds the transactions after the first) are not an overlap.
func TestOverlapSegment_EpochContinuationStaysOK(t *testing.T) {
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
	writeAlertSegment(t, filepath.Join(taskDir, "mysql-bin.000001"), 37, 38)
	writeAlertSegment(t, filepath.Join(taskDir, "mysql-bin.000002"), 39, 40)
	writeAlertSegment(t, filepath.Join(taskDir, "mysql-bin.000002.sealed.e1"), 41, 42)
	writeAlertSegment(t, filepath.Join(taskDir, "mysql-bin.000003"), 43, 44)
	if h := s.storageHealth(task, ""); h.Kind == StorageOverlap {
		t.Fatalf("epoch continuation reported as overlap: %+v", h)
	}
}
