package replication

import "testing"

func TestBuildObjectKey(t *testing.T) {
	got := buildObjectKey("binlog-backup", "cluster-a", "srv-uuid-1", "mysql-bin.000123")
	if got != "binlog-backup/cluster-a/srv-uuid-1/mysql-bin.000123" {
		t.Fatalf("unexpected object key: %s", got)
	}
}

func TestBuildObjectKey_EmptyPrefix(t *testing.T) {
	got := buildObjectKey("", "cluster-a", "srv-uuid-1", "mysql-bin.000123")
	if got != "cluster-a/srv-uuid-1/mysql-bin.000123" {
		t.Fatalf("unexpected object key: %s", got)
	}
}
