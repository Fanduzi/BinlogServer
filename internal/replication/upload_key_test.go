// input: source replication config, task state, checkpoint/file store dependencies
// output: replication run control, local binlog artifacts, and upload/recovery signals
// pos: data-plane runtime that consumes MySQL binlog stream and emits durable outputs
// note: if this file changes, update this header and module AGENTS.md.
package replication

import "testing"

// TestBuildObjectKey 验证相关行为。
func TestBuildObjectKey(t *testing.T) {
	got := buildObjectKey("binlog-backup", "cluster-a", "srv-uuid-1", "mysql-bin.000123")
	if got != "binlog-backup/cluster-a/srv-uuid-1/mysql-bin.000123" {
		t.Fatalf("unexpected object key: %s", got)
	}
}

// TestBuildObjectKey_EmptyPrefix 验证相关行为。
func TestBuildObjectKey_EmptyPrefix(t *testing.T) {
	got := buildObjectKey("", "cluster-a", "srv-uuid-1", "mysql-bin.000123")
	if got != "cluster-a/srv-uuid-1/mysql-bin.000123" {
		t.Fatalf("unexpected object key: %s", got)
	}
}

// TestBuildObjectKey_TrimSlashPerSegment 验证相关行为。
func TestBuildObjectKey_TrimSlashPerSegment(t *testing.T) {
	got := buildObjectKey("/backup-root/", "/cluster-a/", "/srv-uuid-1/", "/mysql-bin.000123/")
	if got != "backup-root/cluster-a/srv-uuid-1/mysql-bin.000123" {
		t.Fatalf("unexpected object key: %s", got)
	}
}

// TestBuildObjectKey_DangerousInputNoPathClean 验证相关行为。
func TestBuildObjectKey_DangerousInputNoPathClean(t *testing.T) {
	got := buildObjectKey("backup", "../evil", "srv-uuid-1", "mysql-bin.000123")
	if got != "backup/../evil/srv-uuid-1/mysql-bin.000123" {
		t.Fatalf("expected no filepath clean collapse, got %s", got)
	}
}
