// Package tasks provides module-level functionality for tasks.
// input: the 47 directory cases of window_golden_test.go
// output: a byte-for-byte golden of GET /replay (PITRReplay to the end of the chain and to a mid-chain stop, with the data directory replaced by $DATA) and of the storage-health verdict (kind and StorageProblem, with and without a checkpoint), recorded on v0.5.64
// pos: behavior freeze for ADR 0009 phase 1 (P1-3); REPLAY_GOLDEN_RECORD=1 rewrites testdata/replay/golden.json
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestReplayAndHealthGolden(t *testing.T) {
	path := filepath.Join("testdata", "replay", "golden.json")
	stops := map[string]time.Time{
		"end": time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		"mid": time.Date(2026, 10, 9, 7, 48, 30, 0, time.UTC),
	}
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
		windowPinTimes(t, dir)
		entry := map[string]any{}
		for name, stop := range stops {
			set, err := s.PITRReplay(task.ID, nil, stop)
			if err != nil {
				entry["replay_"+name] = map[string]string{"error": err.Error()}
				continue
			}
			raw, _ := json.Marshal(set)
			entry["replay_"+name] = json.RawMessage(strings.ReplaceAll(string(raw), data, "$DATA"))
		}
		task, _ = s.GetTask(task.ID)
		for name, cp := range map[string]string{"health": "", "health_checkpoint": c.start} {
			h := s.storageHealth(task, cp)
			entry[name] = map[string]any{"kind": int(h.Kind), "problem": h.Problem}
		}
		raw, _ := json.Marshal(entry)
		got[c.name] = raw
	}
	raw, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("REPLAY_GOLDEN_RECORD") == "1" {
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
	if len(want) != 47 || len(got) != 47 {
		t.Fatalf("golden %d cases, run %d, want 47", len(want), len(got))
	}
	names := make([]string, 0, len(want))
	for k := range want {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		w, _ := json.Marshal(want[k])
		g, _ := json.Marshal(got[k])
		if string(w) != string(g) {
			t.Errorf("%s:\n got  %s\n want %s", k, g, w)
		}
	}
}
