// Package tasks provides module-level functionality for tasks.
// input: the live ledger fixture with a stale .open.e4 copy of a sealed .sealed.e5 (#372 openstale), and with a 4-byte .open.e3 next to its sealed file (#357)
// output: a byte-for-byte golden of GET /replay and the storage-health verdict for those two cases (testdata/replay/p2-3.json; REPLAY_GOLDEN_RECORD=1 rewrites it); the 47 phase 1 cases stay frozen in golden.json
// pos: the one golden P2-3 adds (design §13: open ≡ sealed); the empty leftover proves replay is not cut
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReplayGoldenP23(t *testing.T) {
	const liveUUID = "e2e732df-c5fa-11f1-9e48-822b383dbcd0"
	live := filepath.Join("..", "binlog", "testdata", "ledger", "live")
	cases := map[string]func(t *testing.T, dir string){
		"open-e4-equals-sealed-e5": func(t *testing.T, dir string) {
			if err := os.Rename(filepath.Join(dir, "mysql-bin.000006"), filepath.Join(dir, "mysql-bin.000006.sealed.e5")); err != nil {
				t.Fatal(err)
			}
			copyFile(t, filepath.Join(dir, "mysql-bin.000006.sealed.e5"), filepath.Join(dir, "mysql-bin.000006.open.e4"))
		},
		"empty-open-e3-leftover": func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "mysql-bin.000006.open.e3"), []byte{0xfe, 'b', 'i', 'n'}, 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	got := map[string]json.RawMessage{}
	for name, mutate := range cases {
		data := t.TempDir()
		s := NewScheduler(WithDataDir(data))
		task, err := s.CreateTaskFromSpec("g", "g-key", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: "mysql"}, &StartConfig{Mode: StartModeGTID, GTIDSet: liveUUID + ":1-9"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(data, task.ID)
		windowCopyDir(t, live, dir)
		mutate(t, dir)
		windowPinTimes(t, dir)
		entry := map[string]any{}
		set, err := s.PITRReplay(task.ID, nil, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
		if err != nil {
			entry["replay_end"] = map[string]string{"error": err.Error()}
		} else {
			raw, _ := json.Marshal(set)
			entry["replay_end"] = json.RawMessage(strings.ReplaceAll(string(raw), data, "$DATA"))
		}
		h := s.storageHealth(task, "")
		entry["health"] = map[string]any{"kind": int(h.Kind), "problem": h.Problem}
		raw, _ := json.Marshal(entry)
		got[name] = raw
	}
	path := filepath.Join("testdata", "replay", "p2-3.json")
	raw, _ := json.MarshalIndent(got, "", "  ")
	if os.Getenv("REPLAY_GOLDEN_RECORD") == "1" {
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
	if len(want) != len(cases) {
		t.Fatalf("golden has %d cases, want %d", len(want), len(cases))
	}
	for k := range cases {
		w, _ := json.Marshal(want[k])
		g, _ := json.Marshal(got[k])
		if string(w) != string(g) {
			t.Errorf("%s:\n got  %s\n want %s", k, g, w)
		}
	}
	if h := entryKind(t, want["open-e4-equals-sealed-e5"]); h != int(StorageOverlap) {
		t.Errorf("open ≡ sealed verdict kind %d, want overlap", h)
	}
	if h := entryKind(t, want["empty-open-e3-leftover"]); h != int(StorageOK) {
		t.Errorf("empty leftover verdict kind %d, want ok (replay not cut)", h)
	}
}

func entryKind(t *testing.T, raw json.RawMessage) int {
	var e struct {
		Health struct {
			Kind int `json:"kind"`
		} `json:"health"`
	}
	_ = json.Unmarshal(raw, &e)
	return e.Health.Kind
}
