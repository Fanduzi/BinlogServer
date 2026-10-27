// Package tasks provides module-level functionality for tasks.
// input: a scheduler with on-disk segments and, separately, cluster mode
// output: coverage stop offsets that match the next GTID, a plan whose full-backup overlap is red, and a cluster refusal that is not a partial answer
// pos: adapter coverage for ADR 0009 phases 4 and 5
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCoverage_GTIDStopAndClusterRefusal(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(WithDataDir(dir))
	task, err := s.CreateTaskFromSpec("mysql", "mysql-key", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: "mysql"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(dir, task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	raw := coverageGTIDFile(t, when, when.Add(10*time.Second))
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000001"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.Coverage(task.ID, CoverageQuery{GTID: "3E11FA47-71CA-11E1-9E33-C80AA9429562:1"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Covered || got.StopAt == nil || got.StopAt.Pos <= 4 || !strings.Contains(got.Applied, ":1") || strings.Contains(got.Applied, ":2") {
		t.Fatalf("%+v applied %s", got, got.Applied)
	}
	second, err := s.Coverage(task.ID, CoverageQuery{GTID: "3e11fa47-71ca-11e1-9e33-c80aa9429562:2"})
	if err != nil || !second.Covered || second.StopAt.Pos != uint64(len(raw)) {
		t.Fatalf("second %+v err %v len %d", second, err, len(raw))
	}
	plan, err := s.RestorePlan(task.ID, CoverageQuery{GTID: "3e11fa47-71ca-11e1-9e33-c80aa9429562:2"}, "3e11fa47-71ca-11e1-9e33-c80aa9429562:1-2")
	if err != nil {
		t.Fatal(err)
	}
	var overlap string
	for _, p := range plan.Prechecks {
		if p.ID == "full_backup_gtid" {
			overlap = p.Result
		}
	}
	if overlap != "red" || !strings.Contains(plan.RestoreSh, "--stop-position=") {
		t.Fatalf("plan %+v", plan)
	}

	cluster := NewScheduler(WithDataDir(dir), WithClusterMode())
	if _, err := cluster.Coverage(task.ID, CoverageQuery{At: &when}); err == nil || err.Error() != ErrCoverageUnsupported.Error() {
		t.Fatalf("cluster coverage %v", err)
	}
	if _, err := cluster.CoverageBySource("127.0.0.1", 3306, CoverageQuery{At: &when}); err == nil {
		t.Fatal("cluster source answered")
	}
	if _, err := cluster.RestorePlan(task.ID, CoverageQuery{Latest: true}, ""); err == nil {
		t.Fatal("cluster plan answered")
	}
}

func coverageGTIDFile(t *testing.T, stamps ...time.Time) []byte {
	t.Helper()
	// Reuse the API writer's layout by building the same bytes here.
	var out []byte
	out = append(out, 0xfe, 'b', 'i', 'n')
	sid := []byte{0x3e, 0x11, 0xfa, 0x47, 0x71, 0xca, 0x11, 0xe1, 0x9e, 0x33, 0xc8, 0x0a, 0xa9, 0x42, 0x95, 0x62}
	for i, when := range stamps {
		payload := make([]byte, 1+16+8)
		copy(payload[1:], sid)
		seq := uint64(i + 1)
		payload[1+16] = byte(seq)
		payload[1+17] = byte(seq >> 8)
		size := 19 + len(payload)
		hdr := make([]byte, 19)
		unix := uint32(when.Unix())
		hdr[0] = byte(unix)
		hdr[1] = byte(unix >> 8)
		hdr[2] = byte(unix >> 16)
		hdr[3] = byte(unix >> 24)
		hdr[4] = 33 // GTID_EVENT
		hdr[9] = byte(size)
		hdr[10] = byte(size >> 8)
		out = append(out, hdr...)
		out = append(out, payload...)
	}
	return out
}
