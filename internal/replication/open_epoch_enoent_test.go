// Package replication tests the dump runner.
// input: a fresh local fence, a lease verifier, a checkpoint store that blocks one flush, and a seeded .open.eN
// output: ENOENT on this epoch's open file after the lease moved is a lease handoff; the same ENOENT while this epoch still holds the lease is not
// pos: regression for #457, the stat after #415 removed the old open file
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	goreplication "github.com/go-mysql-org/go-mysql/replication"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"
)

// holdCheckpoint blocks the first checkpoint at or past minPos, which is the
// flush after the event bytes are already on disk and before os.Stat.
type holdCheckpoint struct {
	memCheckpointStore
	minPos  uint32
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *holdCheckpoint) UpsertCheckpoint(ctx context.Context, id string, cp binlog.Checkpoint) error {
	if cp.Pos >= h.minPos {
		block := false
		h.once.Do(func() {
			block = true
			close(h.entered)
		})
		if block {
			select {
			case <-h.release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return h.memCheckpointStore.UpsertCheckpoint(ctx, id, cp)
}

// TestOpenEpochENOENTAfterLeaseMovedIsHandoff is #457: takeover already
// deleted this epoch's .open.eN, the local fence is still fresh, and the
// lease verifier says this epoch no longer holds the task. The stat must
// end the run as a handoff.
func TestOpenEpochENOENTAfterLeaseMovedIsHandoff(t *testing.T) {
	err := runOpenEpochStatAfterDelete(t, false)
	if !tasks.IsLeaseHandoff(err) || !errors.Is(err, ErrLeaseEpochMismatch) {
		t.Fatalf("run error = %v, want a lease handoff after the open file was removed", err)
	}
}

// TestOpenEpochENOENTWhileLeaseHeldStaysFailure keeps the permanent failure
// for a missing open file while this epoch still holds the lease.
func TestOpenEpochENOENTWhileLeaseHeldStaysFailure(t *testing.T) {
	err := runOpenEpochStatAfterDelete(t, true)
	if err == nil || tasks.IsLeaseHandoff(err) || errors.Is(err, ErrLeaseEpochMismatch) {
		t.Fatalf("run error = %v, want the missing-file failure while the lease is still held", err)
	}
	if !os.IsNotExist(err) && !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("run error = %v, want ENOENT", err)
	}
}

func runOpenEpochStatAfterDelete(t *testing.T, stillHeld bool) error {
	t.Helper()
	orig := checkpointBatchWindow
	checkpointBatchWindow = 0
	t.Cleanup(func() { checkpointBatchWindow = orig })

	dataDir := t.TempDir()
	const taskID = "task-1"
	dir := filepath.Join(dataDir, taskID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	eventAt := time.Unix(1_700_000_000, 0).UTC()
	const marker = "INSERT INTO handoff VALUES ('BEFORE-STAT')"
	seed, endPos := chainBinlogEvents(4, eventAt, []namedEvent{
		{typ: goreplication.FORMAT_DESCRIPTION_EVENT, body: formatDescriptionBody(eventAt)},
		{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", "INSERT INTO handoff VALUES ('SEED')")},
	})
	openPath := filepath.Join(dir, "mysql-bin.000003.open.e2")
	writeBinlogSegment(t, openPath, seed)
	events, nextPos := chainBinlogEvents(endPos, eventAt, []namedEvent{
		{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", marker)},
	})
	syncer := &gatedSyncer{
		first:   append([]*goreplication.BinlogEvent{preambleEvent(0, eventAt.Add(-time.Hour))}, events...),
		release: make(chan struct{}),
	}
	closeRelease := make(chan struct{})
	var closeOnce sync.Once
	release := func() { closeOnce.Do(func() { close(closeRelease) }) }
	t.Cleanup(release)
	store := &holdCheckpoint{
		memCheckpointStore: memCheckpointStore{cp: binlog.Checkpoint{File: "mysql-bin.000003", Pos: endPos}, ok: true},
		minPos:             nextPos,
		entered:            make(chan struct{}),
		release:            closeRelease,
	}
	catalog := &takeoverCatalog{rows: []tasks.BinlogFile{{
		TaskID: taskID, FileName: "mysql-bin.000003", FilePath: openPath, Epoch: 2,
		State: "OPEN", EndPos: tasks.FilePos(endPos), UploadState: "LOCAL_ONLY",
	}}}
	runner := NewMySQLRunner(dataDir,
		WithCheckpointStore(store),
		WithFileMetaStore(catalog),
		WithWriteFence(&switchFence{}),
		WithLeaseVerifier(leaseVerifierFunc(func(context.Context, string, string, int64) (bool, error) {
			return stillHeld, nil
		})),
	)
	runner.fetcher = &fakeSourceMetaFetcher{
		status:     MasterStatus{File: "mysql-bin.000099", Pos: 9000},
		serverUUID: "11111111-1111-1111-1111-111111111111",
	}
	runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer }
	task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest})
	task.Epoch = 2
	task.OwnerWorkerID = "worker-a"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx, task) }()

	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("checkpoint flush did not reach the stat")
	}
	if err := os.Remove(openPath); err != nil {
		t.Fatal(err)
	}
	release()

	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		cancel()
		return <-done
	}
}
