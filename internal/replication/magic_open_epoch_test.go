// Package replication provides module-level functionality for replication.
// input: a LATEST anchor checkpoint, an open segment that holds only magic or a log_pos 0 header, and a later epoch after the source outage
// output: proof that the bumped epoch resumes from that anchor, renames the open file, reaches ready, and keeps every transaction committed during the outage
// pos: regression for #222, where that file was reported SEGMENT_NOT_ON_WORKER
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/tasks"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

// TestLatestOpenPreamble_EpochBumpResumesFromAnchor is #222.
// Epoch 1 connects, saves the LATEST anchor, and leaves an open file with no
// complete event. The claim loop then bumps the epoch while the source is
// down. When the source returns, the dump must start at the anchor and pull
// the transactions committed during the outage. Epoch 1, which did not bump,
// already does this.
func TestLatestOpenPreamble_EpochBumpResumesFromAnchor(t *testing.T) {
	t.Run("magic epoch stays 1", func(t *testing.T) {
		recoverPreamble(t, false, 1)
	})
	t.Run("magic epoch bumped", func(t *testing.T) {
		recoverPreamble(t, false, 3)
	})
	t.Run("header epoch stays 1", func(t *testing.T) {
		recoverPreamble(t, true, 1)
	})
	t.Run("header epoch bumped", func(t *testing.T) {
		recoverPreamble(t, true, 3)
	})
}

func recoverPreamble(t *testing.T, header bool, nextEpoch int64) {
	t.Helper()
	const (
		anchorFile = "mysql-bin.000010"
		anchorPos  = uint32(197)
		gap1       = "INSERT INTO outage_gap VALUES (1)"
		gap2       = "INSERT INTO outage_gap VALUES (2)"
		gap3       = "INSERT INTO outage_gap VALUES (3)"
	)
	dir := t.TempDir()
	store := &memCheckpointStore{}
	catalog := &takeoverCatalog{}
	fetcher := &fakeSourceMetaFetcher{
		status:     MasterStatus{File: anchorFile, Pos: anchorPos},
		serverUUID: "11111111-1111-1111-1111-111111111111",
	}
	var current *fakeSyncer
	runner := NewMySQLRunner(dir, WithCheckpointStore(store), WithFileMetaStore(catalog))
	runner.fetcher = fetcher
	runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return current }

	current = &fakeSyncer{streamer: &fakeStreamer{results: []streamResult{
		{event: preambleEvent(0, time.Unix(1_700_000_000, 0).UTC())},
		{err: io.ErrUnexpectedEOF},
	}}}
	task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest})
	task.Epoch = 1
	task.OwnerWorkerID = "worker-a"
	err := runner.Run(context.Background(), task)
	if err == nil || !tasks.IsSourceUnreachable(err) || tasks.IsPermanent(err) {
		t.Fatalf("first run err=%v, want retryable SOURCE_UNREACHABLE", err)
	}
	open1 := filepath.Join(dir, task.ID, anchorFile+".open.e1")
	body, err := os.ReadFile(open1)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(binlogMagic) {
		t.Fatalf("open file is %d bytes, want 4-byte magic before the epoch bump", len(body))
	}
	cp, ok := store.snapshot()
	if !ok || cp.File != anchorFile || cp.Pos != anchorPos || cp.GTIDSet != "" {
		t.Fatalf("anchor checkpoint %+v ok=%v", cp, ok)
	}

	var headerBytes []byte
	if header {
		headerBytes = append(append([]byte{}, binlogMagic...), logPosZeroHeader("HEADER-ONLY")...)
		if err := os.WriteFile(open1, headerBytes, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	eventAt := time.Unix(1_700_000_100, 0).UTC()
	gaps, endPos := chainBinlogEvents(anchorPos, eventAt, []namedEvent{
		{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", gap1)},
		{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", gap2)},
		{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", gap3)},
	})
	results := []streamResult{{event: preambleEvent(126, eventAt.Add(-time.Hour))}}
	for _, ev := range gaps {
		results = append(results, streamResult{event: ev})
	}
	results = append(results, streamResult{err: context.Canceled})
	current = &fakeSyncer{streamer: &fakeStreamer{results: results}}
	fetcher.status = MasterStatus{File: "mysql-bin.000011", Pos: 9000}
	task.Epoch = nextEpoch
	ready := false
	err = runner.RunWithNotify(context.Background(), task, func() { ready = true })
	if err != nil || !ready {
		t.Fatalf("recovery err=%v ready=%v", err, ready)
	}
	if current.startPosCalls != 1 || current.startPos.Name != anchorFile || current.startPos.Pos != anchorPos || current.startGTIDCalls != 0 {
		t.Fatalf("StartSync %+v calls=%d gtid=%d, want %s:%d", current.startPos, current.startPosCalls, current.startGTIDCalls, anchorFile, anchorPos)
	}
	continued := filepath.Join(dir, task.ID, openFileName(anchorFile, nextEpoch))
	got, err := os.ReadFile(continued)
	if err != nil {
		t.Fatal(err)
	}
	if header && !strings.HasPrefix(string(got), string(headerBytes)) {
		t.Fatalf("header-only bytes were not kept (%d bytes)", len(got))
	}
	i1 := strings.Index(string(got), gap1)
	i2 := strings.Index(string(got), gap2)
	i3 := strings.Index(string(got), gap3)
	if i1 < 0 || i2 < i1 || i3 < i2 {
		t.Fatalf("gap events missing or out of order: %d %d %d", i1, i2, i3)
	}
	if nextEpoch != 1 {
		if _, statErr := os.Stat(open1); !os.IsNotExist(statErr) {
			t.Fatalf("epoch 1 file still present: %v", statErr)
		}
	}
	cp, ok = store.snapshot()
	if !ok || cp.File != anchorFile || cp.Pos != endPos || cp.GTIDSet != "" {
		t.Fatalf("checkpoint after recovery %+v ok=%v, want %s:%d", cp, ok, anchorFile, endPos)
	}
}

func logPosZeroHeader(body string) []byte {
	raw := make([]byte, goreplication.EventHeaderSize+len(body))
	raw[4] = byte(goreplication.FORMAT_DESCRIPTION_EVENT)
	raw[9] = byte(len(raw))
	copy(raw[goreplication.EventHeaderSize:], body)
	return raw
}
