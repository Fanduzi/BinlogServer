// Package replication provides module-level functionality for replication.
// input: a dead worker's segment directory recorded in binlog_files.file_path, and a second worker data dir
// output: proof that takeover continues in that directory when it is readable, fails naming the segment when it is not, and does not fail SEGMENT_NOT_ON_WORKER when a local open segment ends with an artificial rotate whose end_log_pos is 0
// pos: regression for lease takeover that must not rebuild a fresh directory from position 4
// note: if this file changes, update this header and module README.md.
package replication

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

// TestTakeoverUsesDeadWorkerSegmentDirectory is the lease-takeover regression.
// Worker A's open segment is not under worker B's data_dir. The catalog file_path
// is the only record of that directory. Current main rewinds to position 4 and
// writes a new directory on B.
func TestTakeoverUsesDeadWorkerSegmentDirectory(t *testing.T) {
	t.Run("visible directory continues", func(t *testing.T) {
		workerB := t.TempDir()
		workerA := t.TempDir()
		const taskID = "task-1"
		aDir := filepath.Join(workerA, taskID)
		if err := os.MkdirAll(aDir, 0o755); err != nil {
			t.Fatal(err)
		}
		eventAt := time.Unix(1_700_000_000, 0).UTC()
		const beforeSQL = "INSERT INTO takeover_boundary VALUES ('ON-A')"
		const afterSQL = "INSERT INTO takeover_boundary VALUES ('ON-B')"
		first, endPos := chainBinlogEvents(4, eventAt, []namedEvent{
			{typ: goreplication.FORMAT_DESCRIPTION_EVENT, body: formatDescriptionBody(eventAt)},
			{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", beforeSQL)},
		})
		openPath := filepath.Join(aDir, "mysql-bin.000003.open.e1")
		writeBinlogSegment(t, openPath, first)
		next, _ := chainBinlogEvents(endPos, eventAt, []namedEvent{
			{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", afterSQL)},
		})
		phase := append([]*goreplication.BinlogEvent{preambleEvent(0, eventAt.Add(-time.Hour))}, next...)

		catalog := &takeoverCatalog{rows: []tasks.BinlogFile{{
			TaskID: taskID, FileName: "mysql-bin.000003", FilePath: openPath,
			State: "OPEN", EndPos: endPos, UploadState: "LOCAL_ONLY",
		}}}
		store := &memCheckpointStore{cp: binlog.Checkpoint{File: "mysql-bin.000003", Pos: endPos}, ok: true}
		syncer := &stopResumeSyncer{phases: [][]*goreplication.BinlogEvent{phase}}
		runner := NewMySQLRunner(workerB, WithCheckpointStore(store), WithFileMetaStore(catalog))
		runner.fetcher = &fakeSourceMetaFetcher{
			status:     MasterStatus{File: "mysql-bin.000099", Pos: 9000},
			serverUUID: "11111111-1111-1111-1111-111111111111",
		}
		runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer }
		task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest})
		task.Epoch = 2
		task.OwnerWorkerID = "worker-b"

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx, task) }()
		waitForMarker(t, workerA, taskID, afterSQL)
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run: %v", err)
		}
		starts := syncer.positions()
		if len(starts) != 1 || starts[0].Name != "mysql-bin.000003" || starts[0].Pos != endPos {
			t.Fatalf("takeover start %+v, want mysql-bin.000003:%d", starts, endPos)
		}
		continued := filepath.Join(aDir, "mysql-bin.000003.open.e2")
		body, err := os.ReadFile(continued)
		if err != nil {
			t.Fatalf("continued segment: %v", err)
		}
		if !strings.Contains(string(body), beforeSQL) || !strings.Contains(string(body), afterSQL) {
			t.Fatalf("segment lost the boundary: %q", body)
		}
		if _, err := os.Stat(openPath); !os.IsNotExist(err) {
			t.Fatalf("old epoch still present: %v", err)
		}
		matches, err := filepath.Glob(filepath.Join(aDir, "mysql-bin.000003*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 1 {
			t.Fatalf("duplicate segment for one source file: %v", matches)
		}
		if _, err := os.Stat(filepath.Join(workerB, taskID)); !os.IsNotExist(err) {
			t.Fatalf("worker B started a fresh directory: %v", err)
		}
	})

	t.Run("missing open segment fails", func(t *testing.T) {
		workerB := t.TempDir()
		missing := filepath.Join(t.TempDir(), "task-1", "mysql-bin.000003.open.e1")
		catalog := &takeoverCatalog{rows: []tasks.BinlogFile{{
			TaskID: "task-1", FileName: "mysql-bin.000003", FilePath: missing,
			State: "OPEN", EndPos: 789, UploadState: "LOCAL_ONLY",
		}}}
		store := &memCheckpointStore{cp: binlog.Checkpoint{File: "mysql-bin.000003", Pos: 789}, ok: true}
		syncer := &fakeSyncer{streamer: &fakeStreamer{results: []streamResult{{err: context.Canceled}}}}
		runner := NewMySQLRunner(workerB, WithCheckpointStore(store), WithFileMetaStore(catalog))
		runner.fetcher = &fakeSourceMetaFetcher{
			status:     MasterStatus{File: "mysql-bin.000003", Pos: 9000},
			serverUUID: "11111111-1111-1111-1111-111111111111",
		}
		runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer }
		task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest})
		task.Epoch = 2
		task.OwnerWorkerID = "worker-b"

		err := runner.Run(context.Background(), task)
		if err == nil || !tasks.IsPermanent(err) {
			t.Fatalf("err=%v, want a permanent failure", err)
		}
		if !strings.Contains(err.Error(), "SEGMENT_NOT_ON_WORKER") || !strings.Contains(err.Error(), missing) {
			t.Fatalf("error should name the missing segment: %v", err)
		}
		if syncer.startPosCalls != 0 {
			t.Fatalf("started a dump at %+v", syncer.startPos)
		}
		if _, statErr := os.Stat(filepath.Join(workerB, task.ID)); !os.IsNotExist(statErr) {
			t.Fatalf("fresh directory: %v", statErr)
		}
	})

	t.Run("uploaded checkpoint resumes from the object", func(t *testing.T) {
		workerB := t.TempDir()
		eventAt := time.Unix(1_700_000_000, 0).UTC()
		const beforeSQL = "INSERT INTO takeover_boundary VALUES ('UPLOADED')"
		const afterSQL = "INSERT INTO takeover_boundary VALUES ('AFTER-OBJECT')"
		first, endPos := chainBinlogEvents(4, eventAt, []namedEvent{
			{typ: goreplication.FORMAT_DESCRIPTION_EVENT, body: formatDescriptionBody(eventAt)},
			{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", beforeSQL)},
		})
		var object bytes.Buffer
		object.Write(binlogMagic)
		for _, ev := range first {
			object.Write(ev.RawData)
		}
		const objectKey = "prefix/cluster-a/uuid/mysql-bin.000003"
		catalog := &takeoverCatalog{rows: []tasks.BinlogFile{{
			TaskID: "task-1", FileName: "mysql-bin.000003",
			FilePath:    filepath.Join(t.TempDir(), "gone", "mysql-bin.000003"),
			State:       "SEALED",
			EndPos:      endPos,
			UploadState: "UPLOADED",
			ObjectKey:   objectKey,
		}}}
		store := &memCheckpointStore{cp: binlog.Checkpoint{File: "mysql-bin.000003", Pos: endPos}, ok: true}
		next, _ := chainBinlogEvents(endPos, eventAt, []namedEvent{
			{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", afterSQL)},
		})
		phase := append([]*goreplication.BinlogEvent{preambleEvent(0, eventAt.Add(-time.Hour))}, next...)
		syncer := &stopResumeSyncer{phases: [][]*goreplication.BinlogEvent{phase}}
		opener := &takeoverObject{key: objectKey, body: object.Bytes()}
		runner := NewMySQLRunner(workerB, WithCheckpointStore(store), WithFileMetaStore(catalog), WithUploader(opener, "prefix"))
		runner.fetcher = &fakeSourceMetaFetcher{
			status:     MasterStatus{File: "mysql-bin.000099", Pos: 9000},
			serverUUID: "11111111-1111-1111-1111-111111111111",
		}
		runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer }
		task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest})
		task.Epoch = 2
		task.OwnerWorkerID = "worker-b"

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx, task) }()
		waitForMarker(t, workerB, task.ID, afterSQL)
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run: %v", err)
		}
		starts := syncer.positions()
		if len(starts) != 1 || starts[0].Name != "mysql-bin.000003" || starts[0].Pos != endPos {
			t.Fatalf("uploaded takeover start %+v, want :%d not 4", starts, endPos)
		}
		if len(opener.opened) != 1 || opener.opened[0] != objectKey {
			t.Fatalf("opened objects %v", opener.opened)
		}
		body, err := os.ReadFile(filepath.Join(workerB, task.ID, "mysql-bin.000003.open.e2"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), beforeSQL) || !strings.Contains(string(body), afterSQL) {
			t.Fatalf("object resume lost bytes: %q", body)
		}
	})

	t.Run("next file after an uploaded segment still starts", func(t *testing.T) {
		workerB := t.TempDir()
		catalog := &takeoverCatalog{rows: []tasks.BinlogFile{{
			TaskID: "task-1", FileName: "mysql-bin.000002",
			FilePath:    filepath.Join(t.TempDir(), "gone", "mysql-bin.000002"),
			State:       "SEALED",
			EndPos:      1000,
			UploadState: "UPLOADED",
			ObjectKey:   "prefix/mysql-bin.000002",
		}}}
		store := &memCheckpointStore{cp: binlog.Checkpoint{File: "mysql-bin.000003", Pos: 4}, ok: true}
		syncer := &fakeSyncer{streamer: &fakeStreamer{results: []streamResult{{err: context.Canceled}}}}
		runner := NewMySQLRunner(workerB, WithCheckpointStore(store), WithFileMetaStore(catalog))
		runner.fetcher = &fakeSourceMetaFetcher{
			status:     MasterStatus{File: "mysql-bin.000003", Pos: 4},
			serverUUID: "11111111-1111-1111-1111-111111111111",
		}
		runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer }
		task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest})
		task.Epoch = 2
		if err := runner.Run(context.Background(), task); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if syncer.startPos.Name != "mysql-bin.000003" || syncer.startPos.Pos != 4 {
			t.Fatalf("next file start %+v", syncer.startPos)
		}
	})

	t.Run("unuploaded sealed segment fails", func(t *testing.T) {
		workerB := t.TempDir()
		missing := filepath.Join(t.TempDir(), "task-1", "mysql-bin.000002")
		catalog := &takeoverCatalog{rows: []tasks.BinlogFile{{
			TaskID: "task-1", FileName: "mysql-bin.000002", FilePath: missing,
			State: "SEALED", EndPos: 1000, UploadState: "LOCAL_ONLY",
		}}}
		store := &memCheckpointStore{cp: binlog.Checkpoint{File: "mysql-bin.000003", Pos: 4}, ok: true}
		syncer := &fakeSyncer{streamer: &fakeStreamer{results: []streamResult{{err: context.Canceled}}}}
		runner := NewMySQLRunner(workerB, WithCheckpointStore(store), WithFileMetaStore(catalog))
		runner.fetcher = &fakeSourceMetaFetcher{
			status:     MasterStatus{File: "mysql-bin.000003", Pos: 4},
			serverUUID: "11111111-1111-1111-1111-111111111111",
		}
		runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer }
		task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest})
		task.Epoch = 2
		err := runner.Run(context.Background(), task)
		if err == nil || !tasks.IsPermanent(err) || !strings.Contains(err.Error(), missing) {
			t.Fatalf("err=%v", err)
		}
		if syncer.startPosCalls != 0 {
			t.Fatalf("started at %+v", syncer.startPos)
		}
		if _, statErr := os.Stat(filepath.Join(workerB, task.ID)); !os.IsNotExist(statErr) {
			t.Fatalf("fresh directory: %v", statErr)
		}
	})
}

// TestTakeover_ArtificialRotatePosZeroStaysOnWorker is #214.
// The open segment is on this worker and the catalog file_path names it.
// A trailing artificial Rotate (end_log_pos 0) must not make resume decide
// the segment is missing. The extra bytes are dropped and the dump continues
// at the last real event.
func TestTakeover_ArtificialRotatePosZeroStaysOnWorker(t *testing.T) {
	dir := t.TempDir()
	const taskID = "task-1"
	taskDir := filepath.Join(dir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	eventAt := time.Unix(1_700_000_000, 0).UTC()
	const beforeSQL = "INSERT INTO restart_boundary VALUES ('BEFORE-ROTATE')"
	const afterSQL = "INSERT INTO restart_boundary VALUES ('AFTER-RECLAIM')"
	first, endPos := chainBinlogEvents(4, eventAt, []namedEvent{
		{typ: goreplication.FORMAT_DESCRIPTION_EVENT, body: formatDescriptionBody(eventAt)},
		{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", beforeSQL)},
	})
	openPath := filepath.Join(taskDir, "mysql-bin.000010.open.e1")
	writeBinlogSegment(t, openPath, first)
	real, err := os.ReadFile(openPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(openPath, append(real, artificialRotateRaw("mysql-bin.000011")...), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog := &takeoverCatalog{rows: []tasks.BinlogFile{{
		TaskID: taskID, FileName: "mysql-bin.000010", FilePath: openPath,
		Epoch: 1, State: "OPEN", EndPos: endPos, UploadState: "LOCAL_ONLY",
	}}}
	store := &memCheckpointStore{cp: binlog.Checkpoint{File: "mysql-bin.000010", Pos: endPos}, ok: true}
	next, _ := chainBinlogEvents(endPos, eventAt, []namedEvent{
		{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", afterSQL)},
	})
	syncer := &fakeSyncer{streamer: &fakeStreamer{results: []streamResult{
		{event: next[0]},
		{err: context.Canceled},
	}}}
	runner := NewMySQLRunner(dir, WithCheckpointStore(store), WithFileMetaStore(catalog))
	runner.fetcher = &fakeSourceMetaFetcher{
		status:     MasterStatus{File: "mysql-bin.000099", Pos: 9000},
		serverUUID: "11111111-1111-1111-1111-111111111111",
	}
	runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer }
	task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest})
	task.Epoch = 2
	task.OwnerWorkerID = "worker-a"

	err = runner.Run(context.Background(), task)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if syncer.startPos.Name != "mysql-bin.000010" || syncer.startPos.Pos != endPos {
		t.Fatalf("start %+v, want mysql-bin.000010:%d", syncer.startPos, endPos)
	}
	continued := filepath.Join(taskDir, "mysql-bin.000010.open.e2")
	body, err := os.ReadFile(continued)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(body, real) || bytes.Contains(body, artificialRotateRaw("mysql-bin.000011")) {
		t.Fatalf("continued segment kept the artificial rotate or lost the real prefix (%d bytes)", len(body))
	}
	if !bytes.Contains(body, []byte(afterSQL)) {
		t.Fatal("reclaim did not append the next event")
	}
	if _, statErr := os.Stat(openPath); !os.IsNotExist(statErr) {
		t.Fatalf("old epoch still present: %v", statErr)
	}
}

// TestArtificialRotate_NotWrittenAndLeaseYieldResumes is the reconnect that
// sees the invented next-file rotate while the claim loop has already bumped
// the epoch. The rotate is not appended. Seal fails the lease check. The same
// worker's next epoch resumes at the last real event.
func TestArtificialRotate_NotWrittenAndLeaseYieldResumes(t *testing.T) {
	dir := t.TempDir()
	eventAt := time.Unix(1_700_000_000, 0).UTC()
	const sql = "INSERT INTO restart_boundary VALUES ('KEPT')"
	query := frameBinlogEvent(goreplication.QUERY_EVENT, queryEventBody("t", sql), 4, eventAt)
	marker := []byte("ARTIFICIAL-ROTATE-POS0")
	rotate := &goreplication.BinlogEvent{
		Header:  &goreplication.EventHeader{EventType: goreplication.ROTATE_EVENT, LogPos: 0, Flags: 0x0020},
		Event:   &goreplication.RotateEvent{Position: 4, NextLogName: []byte("mysql-bin.000011")},
		RawData: marker,
	}
	store := &memCheckpointStore{}
	syncer := &fakeSyncer{streamer: &fakeStreamer{results: []streamResult{
		{event: query},
		{event: rotate},
	}}}
	runner := NewMySQLRunner(dir, WithCheckpointStore(store), WithLeaseVerifier(leaseVerifierFunc(func(context.Context, string, string, int64) (bool, error) {
		return false, nil
	})))
	runner.fetcher = &fakeSourceMetaFetcher{
		status:     MasterStatus{File: "mysql-bin.000010", Pos: 4},
		serverUUID: "11111111-1111-1111-1111-111111111111",
	}
	runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer }
	task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest})
	task.Epoch = 2
	task.OwnerWorkerID = "worker-a"

	err := runner.Run(context.Background(), task)
	if !tasks.IsLeaseHandoff(err) || strings.Contains(err.Error(), "SEGMENT_NOT_ON_WORKER") {
		t.Fatalf("err=%v, want a lease handoff", err)
	}
	openPath := filepath.Join(dir, task.ID, "mysql-bin.000010.open.e2")
	body, err := os.ReadFile(openPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, marker) || !bytes.Contains(body, []byte(sql)) {
		t.Fatalf("open segment should keep the query and drop the artificial rotate (%d bytes)", len(body))
	}
	file, pos, ok := binlog.DurableResume(dir, task.ID)
	if !ok || file != "mysql-bin.000010" || pos != query.Header.LogPos {
		t.Fatalf("resume %s:%d ok=%v, want mysql-bin.000010:%d", file, pos, ok, query.Header.LogPos)
	}

	nextSyncer := &fakeSyncer{streamer: &fakeStreamer{results: []streamResult{{err: context.Canceled}}}}
	nextRunner := NewMySQLRunner(dir, WithCheckpointStore(store))
	nextRunner.fetcher = runner.fetcher
	nextRunner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return nextSyncer }
	task.Epoch = 3
	if err := nextRunner.Run(context.Background(), task); err != nil {
		t.Fatalf("reclaim Run: %v", err)
	}
	if nextSyncer.startPos.Name != "mysql-bin.000010" || nextSyncer.startPos.Pos != query.Header.LogPos {
		t.Fatalf("reclaim start %+v, want :%d", nextSyncer.startPos, query.Header.LogPos)
	}
}

// TestArtificialRotate_SwitchesWithoutWriting seals the current file at the
// last real event and opens the next file. The artificial rotate bytes are
// not in either file.
func TestArtificialRotate_SwitchesWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	eventAt := time.Unix(1_700_000_000, 0).UTC()
	const firstSQL = "INSERT INTO restart_boundary VALUES ('OLD-FILE')"
	const nextSQL = "INSERT INTO restart_boundary VALUES ('NEW-FILE')"
	first := frameBinlogEvent(goreplication.QUERY_EVENT, queryEventBody("t", firstSQL), 4, eventAt)
	marker := []byte("ARTIFICIAL-ROTATE-POS0")
	rotate := &goreplication.BinlogEvent{
		Header:  &goreplication.EventHeader{EventType: goreplication.ROTATE_EVENT, LogPos: 0, Flags: 0x0020},
		Event:   &goreplication.RotateEvent{Position: 4, NextLogName: []byte("mysql-bin.000011")},
		RawData: marker,
	}
	second := frameBinlogEvent(goreplication.QUERY_EVENT, queryEventBody("t", nextSQL), 4, eventAt.Add(time.Second))
	store := &memCheckpointStore{}
	syncer := &fakeSyncer{streamer: &fakeStreamer{results: []streamResult{
		{event: first},
		{event: rotate},
		{event: second},
		{err: context.Canceled},
	}}}
	runner := NewMySQLRunner(dir, WithCheckpointStore(store))
	runner.fetcher = &fakeSourceMetaFetcher{
		status:     MasterStatus{File: "mysql-bin.000010", Pos: 4},
		serverUUID: "11111111-1111-1111-1111-111111111111",
	}
	runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer }
	task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest})
	task.Epoch = 2
	task.OwnerWorkerID = "worker-a"
	if err := runner.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sealedPath := filepath.Join(dir, task.ID, "mysql-bin.000010")
	sealed, err := os.ReadFile(sealedPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, marker) || !bytes.Contains(sealed, []byte(firstSQL)) {
		t.Fatal("sealed file should be the real events only")
	}
	pos, _, _, ok := binlog.DurableCursor(sealedPath)
	if !ok || pos != first.Header.LogPos {
		t.Fatalf("sealed cursor pos=%d ok=%v, want %d", pos, ok, first.Header.LogPos)
	}
	opened, err := os.ReadFile(filepath.Join(dir, task.ID, "mysql-bin.000011.open.e2"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(opened, marker) || !bytes.Contains(opened, []byte(nextSQL)) {
		t.Fatal("next segment should hold the following event and not the artificial rotate")
	}
	cp, ok := store.snapshot()
	if !ok || cp.File != "mysql-bin.000011" || cp.Pos != second.Header.LogPos {
		t.Fatalf("checkpoint %+v ok=%v", cp, ok)
	}
}

func artificialRotateRaw(next string) []byte {
	name := []byte(next)
	body := make([]byte, 8+len(name)+4)
	binary.LittleEndian.PutUint64(body[:8], 4)
	copy(body[8:], name)
	size := uint32(goreplication.EventHeaderSize + len(body))
	raw := make([]byte, size)
	raw[4] = byte(goreplication.ROTATE_EVENT)
	binary.LittleEndian.PutUint32(raw[9:13], size)
	binary.LittleEndian.PutUint32(raw[13:17], 0)
	binary.LittleEndian.PutUint16(raw[17:19], 0x0020)
	copy(raw[goreplication.EventHeaderSize:], body)
	return raw
}

type takeoverCatalog struct {
	mu   sync.Mutex
	rows []tasks.BinlogFile
}

func (c *takeoverCatalog) UpsertBinlogFile(_ context.Context, meta tasks.BinlogFile) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.rows {
		if c.rows[i].FileName == meta.FileName && c.rows[i].Epoch == meta.Epoch {
			c.rows[i] = meta
			return nil
		}
	}
	c.rows = append(c.rows, meta)
	return nil
}

func (c *takeoverCatalog) ListBinlogFiles(_ context.Context, taskID string, limit int) ([]tasks.BinlogFile, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]tasks.BinlogFile, 0, len(c.rows))
	for _, row := range c.rows {
		if row.TaskID == taskID {
			out = append(out, row)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (c *takeoverCatalog) DeleteBinlogFile(_ context.Context, taskID, fileName string, epoch int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.rows[:0]
	for _, row := range c.rows {
		if row.TaskID == taskID && row.FileName == fileName && row.Epoch == epoch {
			continue
		}
		kept = append(kept, row)
	}
	c.rows = kept
	return nil
}

type takeoverObject struct {
	mu     sync.Mutex
	key    string
	body   []byte
	opened []string
}

func (o *takeoverObject) UploadFile(context.Context, string, string, string) error { return nil }

func (o *takeoverObject) DeleteObject(context.Context, string) error { return nil }

func (o *takeoverObject) OpenObject(_ context.Context, key string) (io.ReadCloser, int64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.opened = append(o.opened, key)
	if key != o.key {
		return nil, 0, os.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(string(o.body))), int64(len(o.body)), nil
}
