// Package tasks provides module-level functionality for tasks.
// input: the segment-ledger test directories (live, failback) and the same eleven mutations the ledger golden uses, with and without object storage, with and without a GTID start set, plus mariadb
// output: a byte-for-byte golden of GET /window (RecoveryWindow) for 47 directory cases, recorded on main e39b6a9 before the window derivation moved into internal/assurance
// pos: behavior freeze for ADR 0009 phase 1 (P1-2); WINDOW_GOLDEN_RECORD=1 rewrites testdata/window/golden.json
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

type goldenUploader struct{}

func (goldenUploader) UploadFile(context.Context, string, string, string) error { return nil }

type windowGoldenCase struct {
	name, dir string
	mutate    func(*testing.T, string)
	objects   bool
	start     string
	flavor    string
}

func windowCopyDir(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		in, err := os.Open(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out, err := os.Create(filepath.Join(dst, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(out, in); err != nil {
			t.Fatal(err)
		}
		in.Close()
		out.Close()
	}
}

func windowRegressInto(dst, src string, n int64) func(*testing.T, string) {
	return func(t *testing.T, dir string) {
		raw, err := os.ReadFile(filepath.Join(dir, src))
		if err != nil {
			t.Fatal(err)
		}
		raw = raw[4:]
		if int64(len(raw)) > n {
			raw = raw[:n]
		}
		f, err := os.OpenFile(filepath.Join(dir, dst), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
}

func windowRemove(name string) func(*testing.T, string) {
	return func(t *testing.T, dir string) {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func windowTruncateBy(name string, n int64) func(*testing.T, string) {
	return func(t *testing.T, dir string) {
		p := filepath.Join(dir, name)
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(p, info.Size()-n); err != nil {
			t.Fatal(err)
		}
	}
}

func windowGoldenCases() []windowGoldenCase {
	const liveUUID = "e2e732df-c5fa-11f1-9e48-822b383dbcd0"
	const a = "5b4bc6b5-c3b5-11f1-84bd-822b383dbcd0"
	const b = "5e86458b-c3b5-11f1-8415-822b383dbcd0"
	ledger := filepath.Join("..", "binlog", "testdata", "ledger")
	live := filepath.Join(ledger, "live")
	fb := filepath.Join(ledger, "failback")
	type variant struct {
		name, dir string
		m         func(*testing.T, string)
		start     string
	}
	variants := []variant{
		{"live", live, nil, liveUUID + ":1-9"},
		{"live-damaged", live, windowRegressInto("mysql-bin.000005", "mysql-bin.000004", 2000), liveUUID + ":1-9"},
		{"live-damaged-first", live, windowRegressInto("mysql-bin.000003", "mysql-bin.000003", 5000), liveUUID + ":1-9"},
		{"live-hole", live, windowRemove("mysql-bin.000004"), liveUUID + ":1-9"},
		{"live-tail-partial", live, windowTruncateBy("mysql-bin.000007.open.e1", 37), liveUUID + ":1-9"},
		{"live-tail-header", live, windowTruncateBy("mysql-bin.000007.open.e1", 4900), liveUUID + ":1-9"},
		{"live-no-open", live, windowRemove("mysql-bin.000007.open.e1"), liveUUID + ":1-9"},
		{"failback", fb, nil, a + ":1-104"},
		{"failback-damaged", fb, windowRegressInto(b+".mysql-bin.000001", "mysql-bin.000002", 3000), a + ":1-104"},
		{"failback-hole", fb, windowRemove("mysql-bin.000003"), a + ":1-104"},
		{"failback-tail-partial", fb, windowTruncateBy("mysql-bin.000003.open.e1", 61), a + ":1-104"},
	}
	var out []windowGoldenCase
	for _, v := range variants {
		for _, objects := range []bool{false, true} {
			for _, withStart := range []bool{false, true} {
				name := v.name
				if objects {
					name += "/objects"
				}
				start := ""
				if withStart {
					name += "/start"
					start = v.start
				}
				out = append(out, windowGoldenCase{name: name, dir: v.dir, mutate: v.m, objects: objects, start: start, flavor: "mysql"})
			}
		}
	}
	for _, v := range []variant{variants[0], variants[3], variants[7]} {
		out = append(out, windowGoldenCase{name: v.name + "/mariadb", dir: v.dir, mutate: v.m, flavor: "mariadb"})
	}
	return out
}

func TestRecoveryWindowGolden(t *testing.T) {
	path := filepath.Join("testdata", "window", "golden.json")
	got := map[string]json.RawMessage{}
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
			t.Fatalf("%s: %v", c.name, err)
		}
		dir := filepath.Join(data, task.ID)
		windowCopyDir(t, c.dir, dir)
		if c.mutate != nil {
			c.mutate(t, dir)
		}
		w, err := s.RecoveryWindow(task.ID)
		var raw []byte
		if err != nil {
			raw, _ = json.Marshal(map[string]string{"error": err.Error()})
		} else {
			raw, _ = json.Marshal(w)
		}
		got[c.name] = raw
	}
	if len(got) != 47 {
		t.Fatalf("%d cases, want 47", len(got))
	}
	raw, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("WINDOW_GOLDEN_RECORD") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	wantRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]json.RawMessage{}
	if err := json.Unmarshal(wantRaw, &want); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(want))
	for k := range want {
		names = append(names, k)
	}
	sort.Strings(names)
	if len(want) != len(got) {
		t.Fatalf("golden has %d cases, run has %d", len(want), len(got))
	}
	for _, k := range names {
		w, _ := json.Marshal(want[k])
		g, _ := json.Marshal(got[k])
		if string(w) != string(g) {
			t.Errorf("%s:\n got  %s\n want %s", k, g, w)
		}
	}
}
