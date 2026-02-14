package replication

import (
	"testing"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"
)

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

func TestEffectiveStart_IgnoreInvalidCheckpoint(t *testing.T) {
	start := tasks.StartConfig{
		Mode: tasks.StartModeGTID,
	}

	got := effectiveStartFromCheckpoint(start, binlog.Checkpoint{}, true)
	if got.Mode != tasks.StartModeGTID {
		t.Fatalf("expected original start unchanged, got %+v", got)
	}
}

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
