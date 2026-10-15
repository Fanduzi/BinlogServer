// Package replication provides module-level functionality for replication.
// input: a task directory with a sealed old-source segment holding old:1-10, a new-source open tail, and a checkpoint row that lags at old:1-5
// output: assertions that the resume set and the runtime duplicate filter cover every GTID the sealed segments store, not only the checkpoint plus the open tail
// pos: unit coverage for the stale resume after a source switchover (QA run 3, #391): old:6-10 were sent again and stored twice (SEGMENT_OVERLAP)
// note: if this file changes, update this header and module README.md.
package replication

import (
	"path/filepath"
	"testing"

	"binlog_server/internal/tasks"
)

const newSourceUUID = "19c85968-c87a-11f1-910a-822b383dbcd0"

func writeSwitchoverDir(t *testing.T, dir string) {
	t.Helper()
	old := newDiskSegment()
	for seq := int64(1); seq <= 10; seq++ {
		old.txn(seq)
	}
	old.write(t, filepath.Join(dir, "bin.000002"))
	tail := newDiskSegment()
	for seq := int64(1); seq <= 5; seq++ {
		tail.gtid(newSourceUUID, seq)
		tail.begin()
		tail.rows()
		tail.xid()
	}
	tail.write(t, filepath.Join(dir, newSourceUUID+".bin.000003.open.e1"))
}

func TestResume_StaleCheckpointUsesSealedStoredSet(t *testing.T) {
	root := t.TempDir()
	writeSwitchoverDir(t, filepath.Join(root, "task-sw"))
	r := NewMySQLRunner(root)
	executed := newExecutedGTID("mysql", sampleGTID+":1-5")
	r.reconcileOpenTail(tasks.Task{ID: "task-sw"}, executed)
	for _, g := range []string{sampleGTID + ":6", sampleGTID + ":10", newSourceUUID + ":5"} {
		if !executed.contains(g) {
			t.Fatalf("resume set %q misses stored %s", executed.current(), g)
		}
	}
	if executed.contains(sampleGTID + ":11") {
		t.Fatalf("resume set %q claims old:11", executed.current())
	}
}

func TestResume_StoredSetWithoutOpenTail(t *testing.T) {
	root := t.TempDir()
	old := newDiskSegment()
	for seq := int64(1); seq <= 10; seq++ {
		old.txn(seq)
	}
	old.write(t, filepath.Join(root, "task-sealed", "bin.000002"))
	r := NewMySQLRunner(root)
	executed := newExecutedGTID("mysql", sampleGTID+":1-5")
	r.reconcileOpenTail(tasks.Task{ID: "task-sealed"}, executed)
	if !executed.contains(sampleGTID + ":10") {
		t.Fatalf("resume set %q misses stored old:10", executed.current())
	}
}
