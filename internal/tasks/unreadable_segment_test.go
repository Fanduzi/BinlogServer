// Package tasks provides module-level functionality for tasks.
// input: a task directory whose sealed middle segment has a corrupt event size (high byte 0xff, about 4 GiB, or 0x3f, under 1 GiB but past the file end)
// output: assertions that storageHealth calls it unreadable and names it, /window degrades with a break naming it instead of failing, /replay stops before it with a warning, and a SEGMENT_UNREADABLE finding is open
// pos: regression for the #361 QA failure (issue 358)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func corruptSecondEventSize(t *testing.T, path string, high byte) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	off := 4 + int(binary.LittleEndian.Uint32(b[4+9:4+13]))
	b[off+9+3] = high
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUnreadableSegment_WindowReplayAndFinding(t *testing.T) {
	for _, high := range []byte{0xff, 0x3f} {
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
		corruptSecondEventSize(t, filepath.Join(taskDir, "mysql-bin.000002"), high)

		h := s.storageHealth(task, "")
		if h.Kind != StorageUnreadable || h.Problem.Segment != "mysql-bin.000002" {
			t.Fatalf("0x%x: verdict %+v, want unreadable mysql-bin.000002", high, h)
		}
		if w := h.ReplayWarning(); !strings.HasPrefix(w, "Segment mysql-bin.000002 is not readable") {
			t.Fatalf("0x%x: warning %q", high, w)
		}
		files := mustListFiles(t, s, task.ID)
		if got := replayNames(h.replayCut(files, SelectReplayFiles(files))); got != "mysql-bin.000001" {
			t.Fatalf("0x%x: replay keeps %q", high, got)
		}
		w, err := s.RecoveryWindow(task.ID)
		if err != nil {
			t.Fatalf("0x%x: /window failed instead of degrading: %v", high, err)
		}
		if w.Continuous {
			t.Fatalf("0x%x: window continuous over an unreadable segment", high)
		}
		named := false
		for _, b := range w.Breaks {
			if len(b.Files) > 0 && b.Files[0] == "mysql-bin.000002" && strings.Contains(b.Reason, "not readable") {
				named = true
			}
		}
		if !named {
			t.Fatalf("0x%x: no unreadable break naming the segment: %+v", high, w.Breaks)
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
			t.Fatalf("0x%x: no SEGMENT_UNREADABLE finding: %+v", high, open)
		}
	}
}
