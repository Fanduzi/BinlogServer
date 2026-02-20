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

func TestBuildObjectKey_TrimSlashPerSegment(t *testing.T) {
	got := buildObjectKey("/backup-root/", "/cluster-a/", "/srv-uuid-1/", "/mysql-bin.000123/")
	if got != "backup-root/cluster-a/srv-uuid-1/mysql-bin.000123" {
		t.Fatalf("unexpected object key: %s", got)
	}
}

func TestBuildObjectKey_DangerousInputNoPathClean(t *testing.T) {
	got := buildObjectKey("backup", "../evil", "srv-uuid-1", "mysql-bin.000123")
	if got != "backup/../evil/srv-uuid-1/mysql-bin.000123" {
		t.Fatalf("expected no filepath clean collapse, got %s", got)
	}
}
