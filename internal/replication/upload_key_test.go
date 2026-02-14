package replication

import "testing"

func TestBuildObjectKey(t *testing.T) {
	got := buildObjectKey("binlog-backup", "task-1", "mysql-bin.000123")
	if got != "binlog-backup/task-1/mysql-bin.000123" {
		t.Fatalf("unexpected object key: %s", got)
	}
}

func TestBuildObjectKey_EmptyPrefix(t *testing.T) {
	got := buildObjectKey("", "task-1", "mysql-bin.000123")
	if got != "task-1/mysql-bin.000123" {
		t.Fatalf("unexpected object key: %s", got)
	}
}
