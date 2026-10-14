// Package tasks provides module-level functionality for tasks.
// input: on-disk segments whose open segment did not start at source pos 4, a torn tail, and a 4-byte empty epoch left by a failed Start
// output: proof that no-meta adopt resumes at the last complete event's end log_pos, never at a file size
// pos: regression for issue 356
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiskResumeStart_UsesLastEventEndNotFileSize(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string){
		// e2 continues mysql-bin.000012 mid-file: its size (23) is not a source offset.
		"continuation": func(t *testing.T, dir string) {
			writeDiskSegment(t, filepath.Join(dir, "mysql-bin.000012.open.e2"), 369367)
		},
		// A torn tail after the last complete event.
		"torn-tail": func(t *testing.T, dir string) {
			p := filepath.Join(dir, "mysql-bin.000012.open.e2")
			writeDiskSegment(t, p, 369367)
			f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.Write([]byte{1, 2, 3, 4, 5, 6, 7})
			_ = f.Close()
		},
		// A failed Start left a magic-only .open.e3 that must not hide e2.
		"empty-epoch": func(t *testing.T, dir string) {
			writeDiskSegment(t, filepath.Join(dir, "mysql-bin.000012.open.e2"), 369367)
			if err := os.WriteFile(filepath.Join(dir, "mysql-bin.000012.open.e3"), []byte{0xfe, 'b', 'i', 'n'}, 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			data := t.TempDir()
			dir := filepath.Join(data, "7")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			build(t, dir)
			start, err := diskResumeStart(data, "7")
			if err != nil {
				t.Fatalf("diskResumeStart: %v", err)
			}
			if start.Mode != StartModeFilePos || start.File != "mysql-bin.000012" || start.Pos != 369367 {
				t.Fatalf("resume %+v, want mysql-bin.000012:369367 (last event end, not file size)", start)
			}
		})
	}
}
