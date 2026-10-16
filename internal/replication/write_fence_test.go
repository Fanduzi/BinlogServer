package replication

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	goreplication "github.com/go-mysql-org/go-mysql/replication"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"
)

// switchFence is a WriteFence the test turns off to model the lease moving
// to another worker while this one is frozen.
type switchFence struct{ stale atomic.Bool }

func (f *switchFence) Fresh(string, int64) bool { return !f.stale.Load() }

// gatedSyncer hands out the first batch, then blocks until release, then
// hands out the second batch: the events a SIGSTOPped owner had buffered.
type gatedSyncer struct {
	first, second []*goreplication.BinlogEvent
	release       chan struct{}
	once          sync.Once
}

func (s *gatedSyncer) StartSync(gomysql.Position) (binlogStreamer, error) {
	return &gatedStreamer{s: s, events: append([]*goreplication.BinlogEvent(nil), s.first...)}, nil
}
func (s *gatedSyncer) StartSyncGTID(gomysql.GTIDSet) (binlogStreamer, error) {
	return nil, errors.New("gtid start is not used")
}
func (s *gatedSyncer) Close() {}

type gatedStreamer struct {
	s      *gatedSyncer
	mu     sync.Mutex
	events []*goreplication.BinlogEvent
	gated  bool
}

func (g *gatedStreamer) GetEvent(ctx context.Context) (*goreplication.BinlogEvent, error) {
	g.mu.Lock()
	if len(g.events) == 0 && !g.gated {
		g.gated = true
		g.mu.Unlock()
		select {
		case <-g.s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		g.mu.Lock()
		g.events = append(g.events, g.s.second...)
	}
	if len(g.events) > 0 {
		ev := g.events[0]
		g.events = g.events[1:]
		g.mu.Unlock()
		return ev, nil
	}
	g.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

type countingCheckpointStore struct {
	memCheckpointStore
	writes atomic.Int64
}

func (c *countingCheckpointStore) UpsertCheckpoint(ctx context.Context, id string, cp binlog.Checkpoint) error {
	c.writes.Add(1)
	return c.memCheckpointStore.UpsertCheckpoint(ctx, id, cp)
}

// TestWriteFence_FrozenOwnerCannotAppendAfterLeaseMoves is #417: an owner
// frozen for longer than its lease wakes with events already in hand. Its
// next append, checkpoint and catalog row must be refused at the write
// boundary, and the run must end as a lease handoff, not a failure.
func TestWriteFence_FrozenOwnerCannotAppendAfterLeaseMoves(t *testing.T) {
	dataDir := t.TempDir()
	const taskID = "task-1"
	dir := filepath.Join(dataDir, taskID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	eventAt := time.Unix(1_700_000_000, 0).UTC()
	const beforeSQL = "INSERT INTO fenced VALUES ('BEFORE-FREEZE')"
	const afterSQL = "INSERT INTO fenced VALUES ('AFTER-THAW')"
	seed, endPos := chainBinlogEvents(4, eventAt, []namedEvent{
		{typ: goreplication.FORMAT_DESCRIPTION_EVENT, body: formatDescriptionBody(eventAt)},
		{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", "INSERT INTO fenced VALUES ('SEED')")},
	})
	openPath := filepath.Join(dir, "mysql-bin.000003.open.e2")
	writeBinlogSegment(t, openPath, seed)
	firstEvents, mid := chainBinlogEvents(endPos, eventAt, []namedEvent{
		{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", beforeSQL)},
	})
	secondEvents, _ := chainBinlogEvents(mid, eventAt, []namedEvent{
		{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", afterSQL)},
	})
	syncer := &gatedSyncer{
		first:   append([]*goreplication.BinlogEvent{preambleEvent(0, eventAt.Add(-time.Hour))}, firstEvents...),
		second:  secondEvents,
		release: make(chan struct{}),
	}
	catalog := &takeoverCatalog{rows: []tasks.BinlogFile{{
		TaskID: taskID, FileName: "mysql-bin.000003", FilePath: openPath, Epoch: 2,
		State: "OPEN", EndPos: tasks.FilePos(endPos), UploadState: "LOCAL_ONLY",
	}}}
	store := &countingCheckpointStore{memCheckpointStore: memCheckpointStore{cp: binlog.Checkpoint{File: "mysql-bin.000003", Pos: endPos}, ok: true}}
	fence := &switchFence{}
	runner := NewMySQLRunner(dataDir, WithCheckpointStore(store), WithFileMetaStore(catalog), WithWriteFence(fence))
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
	waitForMarker(t, dataDir, taskID, beforeSQL)
	time.Sleep(checkpointBatchWindow + 50*time.Millisecond)

	// Frozen past the ttl: another worker now holds epoch 3.
	fence.stale.Store(true)
	before, err := os.ReadFile(openPath)
	if err != nil {
		t.Fatal(err)
	}
	writesBefore := store.writes.Load()
	close(syncer.release)

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(5 * time.Second):
		cancel()
		runErr = <-done
		t.Fatalf("thawed owner kept running past the fence: %v", runErr)
	}
	if !tasks.IsLeaseHandoff(runErr) || !errors.Is(runErr, tasks.ErrWriteFenced) {
		t.Fatalf("run error = %v, want a lease handoff carrying ErrWriteFenced", runErr)
	}
	after, err := os.ReadFile(openPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), afterSQL) || len(after) != len(before) {
		t.Fatalf("fenced owner wrote segment bytes: %d -> %d", len(before), len(after))
	}
	if got := store.writes.Load(); got != writesBefore {
		t.Fatalf("fenced owner wrote %d checkpoint(s) after the lease moved", got-writesBefore)
	}
}

// TestWriteFence_StaleOwnerDoesNotOpenSegment: a run whose epoch is already
// stale when it starts creates no open file and writes no checkpoint.
func TestWriteFence_StaleOwnerDoesNotOpenSegment(t *testing.T) {
	dataDir := t.TempDir()
	eventAt := time.Unix(1_700_000_000, 0).UTC()
	events, _ := chainBinlogEvents(4, eventAt, []namedEvent{
		{typ: goreplication.FORMAT_DESCRIPTION_EVENT, body: formatDescriptionBody(eventAt)},
		{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", "INSERT INTO fenced VALUES ('X')")},
	})
	syncer := &gatedSyncer{first: append([]*goreplication.BinlogEvent{preambleEvent(0, eventAt)}, events...), release: make(chan struct{})}
	store := &countingCheckpointStore{}
	fence := &switchFence{}
	fence.stale.Store(true)
	runner := NewMySQLRunner(dataDir, WithCheckpointStore(store), WithWriteFence(fence))
	runner.fetcher = &fakeSourceMetaFetcher{
		status:     MasterStatus{File: "mysql-bin.000001", Pos: 4},
		serverUUID: "11111111-1111-1111-1111-111111111111",
	}
	runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer }
	task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest})
	task.Epoch = 3
	task.OwnerWorkerID = "worker-a"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := runner.Run(ctx, task)
	if !tasks.IsLeaseHandoff(err) || !errors.Is(err, tasks.ErrWriteFenced) {
		t.Fatalf("run error = %v, want a fenced lease handoff", err)
	}
	if n := store.writes.Load(); n != 0 {
		t.Fatalf("stale owner wrote %d checkpoints", n)
	}
	matches, _ := filepath.Glob(filepath.Join(dataDir, task.ID, "*.open.e*"))
	if len(matches) != 0 {
		t.Fatalf("stale owner created open segments: %v", matches)
	}
}
