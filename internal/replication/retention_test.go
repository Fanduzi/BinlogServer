// input: source replication config, task state, checkpoint/file store dependencies
// output: replication run control, local binlog artifacts, and upload/recovery signals
// pos: data-plane runtime that consumes MySQL binlog stream and emits durable outputs
// note: if this file changes, update this header and module AGENTS.md.
package replication

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCleanupExpiredBinlogs_RemovesOldFiles 验证相关行为。
func TestCleanupExpiredBinlogs_RemovesOldFiles(t *testing.T) {
	dir := t.TempDir()
	oldFile := filepath.Join(dir, "mysql-bin.000001")
	newFile := filepath.Join(dir, "mysql-bin.000002")

	if err := os.WriteFile(oldFile, []byte("old"), 0o644); err != nil {
		t.Fatalf("write old file: %v", err)
	}
	if err := os.WriteFile(newFile, []byte("new"), 0o644); err != nil {
		t.Fatalf("write new file: %v", err)
	}

	now := time.Now()
	oldMtime := now.Add(-10 * 24 * time.Hour)
	newMtime := now.Add(-2 * time.Hour)
	if err := os.Chtimes(oldFile, oldMtime, oldMtime); err != nil {
		t.Fatalf("chtimes old file: %v", err)
	}
	if err := os.Chtimes(newFile, newMtime, newMtime); err != nil {
		t.Fatalf("chtimes new file: %v", err)
	}

	if err := cleanupExpiredBinlogs(dir, 7, now, "mysql-bin.000002"); err != nil {
		t.Fatalf("cleanupExpiredBinlogs returned error: %v", err)
	}

	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Fatalf("expected old file deleted, stat err=%v", err)
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Fatalf("expected new file kept, stat err=%v", err)
	}
}

// TestCleanupExpiredBinlogs_DefaultRetention 验证相关行为。
func TestCleanupExpiredBinlogs_DefaultRetention(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "mysql-bin.000001")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	now := time.Now()
	mtime := now.Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(file, mtime, mtime); err != nil {
		t.Fatalf("chtimes file: %v", err)
	}

	if err := cleanupExpiredBinlogs(dir, 0, now, ""); err != nil {
		t.Fatalf("cleanupExpiredBinlogs returned error: %v", err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("expected file deleted under default retention, stat err=%v", err)
	}
}
