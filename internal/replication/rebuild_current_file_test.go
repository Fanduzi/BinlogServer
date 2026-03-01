// Package replication provides module-level functionality for replication.
// input: source replication config, task state, checkpoint/file store dependencies
// output: replication run control, local binlog artifacts, and upload/recovery signals
// pos: data-plane runtime that consumes MySQL binlog stream and emits durable outputs
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"
)

// TestRebuildCurrentFile_AfterTakeover 验证相关行为。
func TestRebuildCurrentFile_AfterTakeover(t *testing.T) {
	start := tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: "mysql-bin.000100",
		Pos:  4,
	}

	checkpointStart := effectiveStartFromCheckpoint(start, binlog.Checkpoint{
		File: "mysql-bin.000123",
		Pos:  789,
	}, true)
	if checkpointStart.Pos != 789 {
		t.Fatalf("expected checkpoint-resume pos 789, got %d", checkpointStart.Pos)
	}

	rebuildStart, rebuilding := effectiveStartForTakeover(
		tasks.Task{ID: "task-a", Epoch: 3, OwnerWorkerID: "worker-b"},
		start,
		binlog.Checkpoint{
			File: "mysql-bin.000123",
			Pos:  789,
		},
		true,
	)
	if !rebuilding {
		t.Fatal("expected takeover to trigger rebuild_current_file")
	}
	if rebuildStart.File != "mysql-bin.000123" || rebuildStart.Pos != 4 {
		t.Fatalf("expected rebuild from file start pos=4, got %+v", rebuildStart)
	}
}

// TestRebuildCurrentFile_TakeoverProducesSingleSealedFile 验证相关行为。
func TestRebuildCurrentFile_TakeoverProducesSingleSealedFile(t *testing.T) {
	dir := t.TempDir()
	openPath := filepath.Join(dir, "mysql-bin.000123.open.e8")
	if err := os.WriteFile(openPath, []byte("rebuilt-by-new-owner"), 0o644); err != nil {
		t.Fatalf("write takeover open file: %v", err)
	}

	uploader := &fileStateUploader{}
	metaStore := &fileStateMetaStore{}
	runner := &MySQLRunner{
		uploader:      uploader,
		fileMetaStore: metaStore,
		uploadPrefix:  "prefix",
		leaseVerifier: leaseVerifierFunc(func(context.Context, tasks.Task) (bool, error) {
			return true, nil
		}),
	}

	err := runner.finalizeSealedFile(
		context.Background(),
		tasks.Task{ID: "task-a", ClusterKey: "cluster-a", Epoch: 8, OwnerWorkerID: "worker-b"},
		"srv-uuid-1",
		openPath,
		4,
		1024,
		time.Now().Add(-time.Minute),
		time.Now(),
	)
	if err != nil {
		t.Fatalf("finalizeSealedFile returned error: %v", err)
	}

	sealedPath := filepath.Join(dir, "mysql-bin.000123")
	if _, err := os.Stat(sealedPath); err != nil {
		t.Fatalf("expected sealed file generated, stat err=%v", err)
	}
	if _, err := os.Stat(openPath); !os.IsNotExist(err) {
		t.Fatalf("expected takeover open file removed, stat err=%v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "mysql-bin.000123"))
	if err != nil {
		t.Fatalf("glob sealed file failed: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly one sealed output file, got %d (%v)", len(matches), matches)
	}
}
