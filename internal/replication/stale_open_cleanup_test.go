// Package replication provides module-level functionality for replication.
// input: source replication config, task state, checkpoint/file store dependencies
// output: replication run control, local binlog artifacts, and upload/recovery signals
// pos: data-plane runtime that consumes MySQL binlog stream and emits durable outputs
// note: if this file changes, update this header and module README.md.
package replication

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCleanupStaleOpenFilesOnStartup 验证相关行为。
func TestCleanupStaleOpenFilesOnStartup(t *testing.T) {
	dir := t.TempDir()

	staleOpen := filepath.Join(dir, "mysql-bin.000123.open.e7")
	currentOpen := filepath.Join(dir, "mysql-bin.000123.open.e8")
	sealed := filepath.Join(dir, "mysql-bin.000123")
	other := filepath.Join(dir, "notes.txt")

	for _, path := range []string{staleOpen, currentOpen, sealed, other} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", filepath.Base(path), err)
		}
	}

	if err := cleanupStaleOpenFiles(dir, 8); err != nil {
		t.Fatalf("cleanupStaleOpenFiles returned error: %v", err)
	}

	if _, err := os.Stat(staleOpen); !os.IsNotExist(err) {
		t.Fatalf("expected stale open file removed, stat err=%v", err)
	}
	if _, err := os.Stat(currentOpen); err != nil {
		t.Fatalf("expected current epoch open file kept, stat err=%v", err)
	}
	if _, err := os.Stat(sealed); err != nil {
		t.Fatalf("expected sealed file kept, stat err=%v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("expected unrelated file kept, stat err=%v", err)
	}
}
