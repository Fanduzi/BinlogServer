// Package replication provides module-level functionality for replication.
// input: a real sealed MySQL 8.0 segment on disk
// output: withSealFacts records the segment's sha256 and size in the task directory's .seal-sha256 ledger, the rehasher's source without meta (#380 QA P1)
// pos: ADR 0009 phase 2 P2-3 QA round 1
// note: if this file changes, update this header and module README.md.
package replication

import (
	"os"
	"path/filepath"
	"testing"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"
)

func TestWithSealFactsRecordsDiskChecksum(t *testing.T) {
	dir := t.TempDir()
	b, err := os.ReadFile(filepath.Join("..", "tasks", "testdata", "real-mysql80", "mysql-bin.000004"))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "mysql-bin.000004")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	row := withSealFacts(tasks.BinlogFile{TaskID: "1", FileName: "mysql-bin.000004", FilePath: p, State: "SEALED"})
	sums, err := binlog.LoadSealSums(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := sums["mysql-bin.000004"]
	if !ok || got.SHA256 != row.SHA256 || got.Size != int64(len(b)) || row.SHA256 == "" {
		t.Fatalf("ledger %+v, row sha %q", sums, row.SHA256)
	}
}
