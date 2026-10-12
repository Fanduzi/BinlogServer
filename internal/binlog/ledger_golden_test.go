// Package binlog provides module-level functionality for binlog.
// input: two real task directories in testdata/ledger (a GTID task written by v0.5.59 with four sealed segments and an open one, and a v0.5.57 A→B→A failback directory from QA) and damage applied to temporary copies (position regress, a removed segment, a torn open tail)
// output: the ledger's damage, recovery sets, open tail and resume GTID for each case, compared with testdata/ledger/golden.json recorded from main before the ledger existed (LEDGER_GOLDEN_RECORD=1 rewrites it)
// pos: behaviour freeze for the segment ledger refactor (ADR 0006)
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

type ledgerGoldenCase struct {
	name       string
	dir        string
	mutate     func(t *testing.T, dir string)
	checkpoint string
	start      string
}

type ledgerGoldenResult struct {
	Problem    StorageProblem
	Damaged    bool
	Tail       OpenTail
	TailOK     bool
	ResumeGTID string
}

func copyLedgerDir(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
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
	return dst
}

func regressInto(dst, src string, n int64) func(*testing.T, string) {
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

func removeSegment(name string) func(*testing.T, string) {
	return func(t *testing.T, dir string) {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func truncateBy(name string, n int64) func(*testing.T, string) {
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

func ledgerGoldenCases() []ledgerGoldenCase {
	const liveUUID = "e2e732df-c5fa-11f1-9e48-822b383dbcd0"
	const a = "5b4bc6b5-c3b5-11f1-84bd-822b383dbcd0"
	const b = "5e86458b-c3b5-11f1-8415-822b383dbcd0"
	live := filepath.Join("testdata", "ledger", "live")
	fb := filepath.Join("testdata", "ledger", "failback")
	liveCP := liveUUID + ":1-184"
	liveStart := liveUUID + ":1-9"
	fbCP := a + ":1-1605," + b + ":1-600"
	fbStart := a + ":1-104"
	var out []ledgerGoldenCase
	add := func(name, dir string, m func(*testing.T, string), cp, start string) {
		out = append(out,
			ledgerGoldenCase{name + "/bare", dir, m, "", ""},
			ledgerGoldenCase{name + "/checkpoint", dir, m, cp, start},
			ledgerGoldenCase{name + "/checkpoint-ahead", dir, m, cp + ",3e11fa47-71ca-11e1-9e33-c80aa9429562:1-5", ""},
		)
	}
	add("live", live, nil, liveCP, liveStart)
	add("live-damaged", live, regressInto("mysql-bin.000005", "mysql-bin.000004", 2000), liveCP, liveStart)
	add("live-damaged-first", live, regressInto("mysql-bin.000003", "mysql-bin.000003", 5000), liveCP, liveStart)
	add("live-hole", live, removeSegment("mysql-bin.000004"), liveCP, liveStart)
	add("live-tail-partial", live, truncateBy("mysql-bin.000007.open.e1", 37), liveCP, liveStart)
	add("live-tail-header", live, truncateBy("mysql-bin.000007.open.e1", 4900), liveCP, liveStart)
	add("live-no-open", live, removeSegment("mysql-bin.000007.open.e1"), liveCP, liveStart)
	add("failback", fb, nil, fbCP, fbStart)
	add("failback-damaged", fb, regressInto(b+".mysql-bin.000001", "mysql-bin.000002", 3000), fbCP, fbStart)
	add("failback-hole", fb, removeSegment("mysql-bin.000003"), fbCP, fbStart)
	add("failback-tail-partial", fb, truncateBy("mysql-bin.000003.open.e1", 61), fbCP, fbStart)
	return out
}

func TestLedgerGolden(t *testing.T) {
	path := filepath.Join("testdata", "ledger", "golden.json")
	got := map[string]ledgerGoldenResult{}
	for _, c := range ledgerGoldenCases() {
		dir := copyLedgerDir(t, c.dir)
		if c.mutate != nil {
			c.mutate(t, dir)
		}
		var r ledgerGoldenResult
		r.Problem, r.Damaged = goldenDamage(dir, c.checkpoint, c.start, "mysql")
		r.Tail, r.TailOK = goldenTail(dir)
		if r.TailOK {
			r.Tail.Path = filepath.Base(r.Tail.Path)
			r.ResumeGTID = goldenUnion("mysql", c.checkpoint, r.Tail)
		}
		got[c.name] = r
	}
	raw, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LEDGER_GOLDEN_RECORD") == "1" {
		if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	wantRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]ledgerGoldenResult{}
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
