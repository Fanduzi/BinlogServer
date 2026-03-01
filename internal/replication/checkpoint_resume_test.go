// input: source replication config, task state, checkpoint/file store dependencies
// output: replication run control, local binlog artifacts, and upload/recovery signals
// pos: data-plane runtime that consumes MySQL binlog stream and emits durable outputs
// note: if this file changes, update this header and module AGENTS.md.
package replication

import (
	"testing"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"
)

// TestEffectiveStart_UsesCheckpointWhenPresent 验证相关行为。
func TestEffectiveStart_UsesCheckpointWhenPresent(t *testing.T) {
	start := tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: "mysql-bin.000100",
		Pos:  4,
	}
	cp := binlog.Checkpoint{
		File: "mysql-bin.000123",
		Pos:  456,
	}

	got := effectiveStartFromCheckpoint(start, cp, true)
	if got.Mode != tasks.StartModeFilePos || got.File != "mysql-bin.000123" || got.Pos != 456 {
		t.Fatalf("unexpected effective start: %+v", got)
	}
}

// TestEffectiveStart_IgnoreInvalidCheckpoint 验证相关行为。
func TestEffectiveStart_IgnoreInvalidCheckpoint(t *testing.T) {
	start := tasks.StartConfig{
		Mode: tasks.StartModeGTID,
	}

	got := effectiveStartFromCheckpoint(start, binlog.Checkpoint{}, true)
	if got.Mode != tasks.StartModeGTID {
		t.Fatalf("expected original start unchanged, got %+v", got)
	}
}

// TestEffectiveStart_NoCheckpoint 验证相关行为。
func TestEffectiveStart_NoCheckpoint(t *testing.T) {
	start := tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: "mysql-bin.000100",
		Pos:  4,
	}

	got := effectiveStartFromCheckpoint(start, binlog.Checkpoint{}, false)
	if got != start {
		t.Fatalf("expected original start unchanged, got %+v", got)
	}
}

// TestRebuildCurrentFile_UsesCheckpointFileFromPos4OnTakeover 验证相关行为。
func TestRebuildCurrentFile_UsesCheckpointFileFromPos4OnTakeover(t *testing.T) {
	start := tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: "mysql-bin.000100",
		Pos:  4,
	}
	cp := binlog.Checkpoint{
		File: "mysql-bin.000123",
		Pos:  456,
	}

	got, rebuilding := effectiveStartForTakeover(tasks.Task{Epoch: 2}, start, cp, true)
	if !rebuilding {
		t.Fatal("expected rebuild_current_file mode for takeover")
	}
	if got.Mode != tasks.StartModeFilePos || got.File != "mysql-bin.000123" || got.Pos != 4 {
		t.Fatalf("unexpected takeover rebuild start: %+v", got)
	}
}
