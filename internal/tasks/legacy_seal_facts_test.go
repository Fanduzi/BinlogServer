// Package tasks provides module-level functionality for tasks.
// input: sealed and open segments copied from the ledger testdata, and a sealed name with no file
// output: assertions that a row the legacy failback repair writes from disk carries the file's seal facts, an open row carries none, and a sealed row whose file cannot be read asks the catalog to clear stale facts
// pos: regression for #351 QA P3 (repair rewrote rows without facts; a repointed row kept a stale sha256)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"os"
	"path/filepath"
	"testing"

	"binlog_server/internal/binlog"
)

func TestDiskCatalogRowCarriesSealFacts(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"mysql-bin.000004", "mysql-bin.000007.open.e1"} {
		raw, err := os.ReadFile(filepath.Join("..", "binlog", "testdata", "ledger", "live", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := binlog.ReadSealFacts(filepath.Join(dir, "mysql-bin.000004"))
	if err != nil {
		t.Fatal(err)
	}
	row := diskCatalogRow("1", dir, "mysql-bin.000004")
	if row.SHA256 != want.SHA256 || row.FirstEventAt == nil || !row.FirstEventAt.Equal(want.First) ||
		row.LastEventAt == nil || !row.LastEventAt.Equal(want.Last) || row.ResetSealFacts {
		t.Fatalf("sealed row facts %+v, want %+v", row, want)
	}
	open := diskCatalogRow("1", dir, "mysql-bin.000007.open.e1")
	if open.SHA256 != "" || open.FirstEventAt != nil || open.ResetSealFacts {
		t.Fatalf("open row has facts: %+v", open)
	}
	gone := diskCatalogRow("1", dir, "mysql-bin.000005")
	if gone.SHA256 != "" || !gone.ResetSealFacts {
		t.Fatalf("unreadable sealed row must clear stale facts: %+v", gone)
	}
}
