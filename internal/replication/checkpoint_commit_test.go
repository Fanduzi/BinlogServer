// Package replication provides module-level functionality for replication.
// input: a GTID dump with one large transaction (BEGIN, thousands of row events, XID) and a checkpoint store whose every write costs time, like the meta DB
// output: assertions that the checkpoint is written at the commit and not per row event, that it never names a position or GTID the segment has not flushed, and that the large transaction stores in a bounded time
// pos: unit coverage for #391's P1: a 3MB, ~20k-event transaction did ~28k meta INSERTs (47.5s with meta, 264ms without), so a drop restarted it and the backup stalled silently
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

func rowsEventAt(pos uint32) *goreplication.BinlogEvent {
	return &goreplication.BinlogEvent{
		Header:  &goreplication.EventHeader{EventType: goreplication.WRITE_ROWS_EVENTv2, LogPos: pos, Timestamp: uint32(time.Now().Unix())},
		RawData: []byte("rows"),
	}
}

func TestRun_LargeTransactionCheckpointsAtCommitOnly(t *testing.T) {
	const rows = 2000
	const perWrite = 2 * time.Millisecond // a meta INSERT round trip
	results := []streamResult{
		{event: artificialSourceRotate("mysql-bin.000001")},
		{event: gtidAt(11, 100)},
		{event: sqlAt("BEGIN", 150)},
	}
	pos := uint32(150)
	for i := 0; i < rows; i++ {
		pos += 50
		results = append(results, streamResult{event: rowsEventAt(pos)})
	}
	xidPos := pos + 31
	results = append(results, streamResult{event: xidAt(xidPos)}, streamResult{err: context.Canceled})

	dir := t.TempDir()
	store := &fakeRunnerCheckpointStore{onUpsert: func(binlog.Checkpoint) error { time.Sleep(perWrite); return nil }}
	runner := &MySQLRunner{
		dataDir:         dir,
		checkpointStore: store,
		fetcher:         &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		newSyncer: func(goreplication.BinlogSyncerConfig) binlogSyncer {
			return &fakeSyncer{connID: 44, streamer: &fakeStreamer{results: results}}
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			buf := &fakeSyncFile{}
			w := binlog.NewWriter(buf, binlog.Checkpoint{File: fileName, Pos: initialPos})
			return &countingCloser{writes: new(int), buf: buf}, w, filepath.Join(dir, "task-1", fileName), nil
		},
		killDump: func(tasks.SourceConfig, uint32) error { return nil },
	}
	began := time.Now()
	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: sampleGTID + ":1-10"}))
	took := time.Since(began)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.upserts) > 3 {
		t.Fatalf("%d checkpoint writes for one %d-event transaction, want the commit only", len(store.upserts), rows+3)
	}
	if took > time.Second {
		t.Fatalf("storing one large transaction took %s with a %s meta write", took, perWrite)
	}
	var last binlog.Checkpoint
	for _, cp := range store.upserts {
		if cp.Pos > 150 && cp.Pos < xidPos {
			t.Fatalf("checkpoint %+v inside the open transaction", cp)
		}
		last = cp
	}
	if last.Pos != xidPos || last.GTIDSet != sampleGTID+":1-11" {
		t.Fatalf("last checkpoint %+v, want the commit at %d with :1-11", last, xidPos)
	}
}

func TestTxnBoundary_FramingAndLongWarning(t *testing.T) {
	var b txnBoundary
	now := time.Now()
	steps := []struct {
		ev   *goreplication.BinlogEvent
		want bool
	}{
		{gtidAt(1, 10), false},
		{sqlAt("BEGIN", 20), false},
		{rowsEventAt(30), false},
		{sqlAt("ROLLBACK TO SAVEPOINT s", 40), false},
		{xidAt(50), true},
		{gtidAt(2, 60), false},
		{sqlAt("CREATE TABLE t (id INT)", 70), true},
		{sqlAt("BEGIN", 80), false},
		{rowsEventAt(90), false},
		{sqlAt("COMMIT", 100), true},
		{queryAt(110), true},
	}
	for i, s := range steps {
		if got := b.observe(s.ev, nil, now); got != s.want {
			t.Fatalf("step %d (%v): boundary %v want %v", i, s.ev.Header.EventType, got, s.want)
		}
	}
	orig := longTxnWarnAfter
	longTxnWarnAfter = time.Minute
	defer func() { longTxnWarnAfter = orig }()
	b.observe(gtidAt(3, 120), nil, now)
	if b.warnLong("t", "f", 1, now.Add(30*time.Second)) {
		t.Fatal("warned before the threshold")
	}
	if !b.warnLong("t", "f", 1, now.Add(61*time.Second)) || b.warnLong("t", "f", 1, now.Add(90*time.Second)) {
		t.Fatal("want one warning per interval")
	}
	if !b.warnLong("t", "f", 1, now.Add(122*time.Second)) {
		t.Fatal("no repeat warning after another interval")
	}
	b.observe(xidAt(130), nil, now)
	if b.warnLong("t", "f", 1, now.Add(time.Hour)) {
		t.Fatal("warned with no open transaction")
	}
}

// Many small transactions within the batch window cost one write, and the
// held commit is still written when the run ends.
func TestRun_SmallCommitsAreBatched(t *testing.T) {
	orig := checkpointBatchWindow
	checkpointBatchWindow = time.Hour
	defer func() { checkpointBatchWindow = orig }()
	results := []streamResult{{event: artificialSourceRotate("mysql-bin.000001")}}
	pos := uint32(100)
	for gno := int64(11); gno <= 60; gno++ {
		results = append(results, streamResult{event: gtidAt(gno, pos)}, streamResult{event: xidAt(pos + 30)})
		pos += 50
	}
	results = append(results, streamResult{err: context.Canceled})
	dir := t.TempDir()
	store := &fakeRunnerCheckpointStore{}
	runner := &MySQLRunner{
		dataDir:         dir,
		checkpointStore: store,
		fetcher:         &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		newSyncer: func(goreplication.BinlogSyncerConfig) binlogSyncer {
			return &fakeSyncer{connID: 44, streamer: &fakeStreamer{results: results}}
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			buf := &fakeSyncFile{}
			w := binlog.NewWriter(buf, binlog.Checkpoint{File: fileName, Pos: initialPos})
			return &countingCloser{writes: new(int), buf: buf}, w, filepath.Join(dir, "task-1", fileName), nil
		},
		killDump: func(tasks.SourceConfig, uint32) error { return nil },
	}
	if err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: sampleGTID + ":1-10"})); err != nil {
		t.Fatal(err)
	}
	if len(store.upserts) != 2 {
		t.Fatalf("upserts %+v, want the first commit and the held last one", store.upserts)
	}
	last := store.upserts[1]
	if last.Pos != pos-50+30 || last.GTIDSet != sampleGTID+":1-60" {
		t.Fatalf("held checkpoint %+v", last)
	}
}
