// Package replication provides module-level functionality for replication.
// input: a semi-sync dump whose events arrive through SynchronousEventHandler, and a dump that goes silent
// output: proof that every event path resets the silence timer and that a stall leaves the run at once
// pos: #338 re-QA-2 regression tests for ADR 0009 S12
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"testing"
	"time"

	"binlog_server/internal/tasks"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

// capturingSyncer records the config so the test can drive the semi-sync handler.
type capturingSyncer struct {
	fakeSyncer
	release chan struct{}
}

func (c *capturingSyncer) Close() {
	if c.release != nil {
		<-c.release
	}
}

// #338 re-QA-2 P1: semi-sync events never pass through GetEvent, so the
// silence timer never reset and every semi-sync task reconnected each ~50s.
func TestSemiSyncEventsResetSilence(t *testing.T) {
	r := NewMySQLRunner(t.TempDir())
	clock := &stepClock{at: time.Unix(1_700_000_000, 0)}
	r.clock = clock.now
	rec := bindRecorder(r)
	r.fetcher = &fakeSourceMetaFetcher{status: MasterStatus{File: "mysql-bin.000001", Pos: 4}, serverUUID: "11111111-1111-1111-1111-111111111111"}
	var cfg goreplication.BinlogSyncerConfig
	st := &fakeStreamer{}
	sy := &capturingSyncer{}
	sy.streamer = st
	r.newSyncer = func(c goreplication.BinlogSyncerConfig) binlogSyncer { cfg = c; return sy }
	st.onCall = func(call int) {
		if cfg.SynchronousEventHandler == nil {
			t.Fatal("semi-sync task has no SynchronousEventHandler")
		}
		// The source sends a heartbeat every 15s; the poll waits 2s.
		clock.at = clock.at.Add(15 * time.Second)
		hb := frameBinlogEvent(goreplication.HEARTBEAT_EVENT, nil, 4, clock.at)
		if err := cfg.SynchronousEventHandler.HandleEvent(hb); err != nil {
			t.Fatalf("heartbeat: %v", err)
		}
	}
	for i := 0; i < 10; i++ {
		st.results = append(st.results, streamResult{err: context.DeadlineExceeded})
	}
	st.results = append(st.results, streamResult{err: context.Canceled})
	task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeFilePos, File: "mysql-bin.000001", Pos: 4})
	task.Source.SemiSync = true
	task.Epoch = 1
	if err := r.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v (a semi-sync dump fed by heartbeats must not reconnect)", err)
	}
	if sy.startPosCalls != 1 {
		t.Fatalf("StartSync calls %d, want 1 (no silent reconnect)", sy.startPosCalls)
	}
	if _, ok := rec.has(task.ID, tasks.FindingDumpStalled, ""); ok {
		t.Fatal("DUMP_STALLED on a semi-sync dump that receives heartbeats")
	}
}

// #338 re-QA-2: on a full blackhole the task stayed RUNNING until t+63
// because the stall went through identity probes and in-run reconnects.
// A stall must end the run at once (the scheduler leaves RUNNING), without
// waiting for the dead dump to close.
func TestDumpStallEndsRunAtOnce(t *testing.T) {
	r := NewMySQLRunner(t.TempDir())
	clock := &stepClock{at: time.Unix(1_700_000_000, 0)}
	r.clock = clock.now
	rec := bindRecorder(r)
	r.fetcher = &fakeSourceMetaFetcher{status: MasterStatus{File: "mysql-bin.000001", Pos: 4}, serverUUID: "11111111-1111-1111-1111-111111111111"}
	st := &fakeStreamer{results: []streamResult{{err: context.DeadlineExceeded}, {err: context.DeadlineExceeded}}}
	st.onCall = func(call int) {
		if call == 2 {
			clock.at = clock.at.Add(dumpReadTimeout + time.Second)
		}
	}
	sy := &capturingSyncer{release: make(chan struct{})}
	defer close(sy.release)
	sy.streamer = st
	r.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return sy }
	task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeFilePos, File: "mysql-bin.000001", Pos: 4})
	task.Epoch = 1
	var err error
	within(t, 2*time.Second, "Run after a stall", func() { err = r.Run(context.Background(), task) })
	if !tasks.IsSourceUnreachable(err) {
		t.Fatalf("stall error %v, want SOURCE_UNREACHABLE so the scheduler backs off and reconnects", err)
	}
	if sy.startPosCalls != 1 {
		t.Fatalf("StartSync calls %d, want 1: a stalled dump is not reopened inside the run", sy.startPosCalls)
	}
	if _, ok := rec.has(task.ID, tasks.FindingDumpStalled, ""); !ok {
		t.Fatalf("no DUMP_STALLED: %v", rec.open)
	}
}
