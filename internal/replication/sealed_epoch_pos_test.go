// Package replication provides module-level functionality for replication.
// input: a file/pos dump that seals one source name, a later lease that opens that name again, a third epoch of the same name, a file/pos resume whose first read is MySQL 1236, and a sealed segment left on disk with no catalog row
// output: proof that each sealed epoch's catalog start_pos and end_pos are the first and last event positions in that file, including after failover and a 1236 GTID fallback, and that an enroll of an unjoined sealed file does not record end_pos 0; the seal helper writes through any file metadata store
// pos: regression coverage for issue 189
// note: if this file changes, update this header and module README.md.
package replication

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

const issue189Source = "mysql-bin.000009"

// TestIssue189_LaterEpochSealedPositions is the failover catalog a DBA reads
// from GET /api/tasks/{id}/files. Epoch 0 seals and uploads the source name.
// A new lease opens that same name as .open.e1, copies events, then seals
// .sealed.e1. A third epoch does it again. start_pos is the first event's
// position and end_pos is the last event's end_log_pos.
func TestIssue189_LaterEpochSealedPositions(t *testing.T) {
	base := t.TempDir()
	catalog := &takeoverCatalog{}
	uploader := &recordingUploader{}
	checkpoints := &memCheckpointStore{}
	const next = "mysql-bin.000010"

	epoch0End := sealEpochFromDump(t, base, catalog, uploader, checkpoints, 0, tasks.StartConfig{
		Mode: tasks.StartModeFilePos, File: issue189Source, Pos: 4,
	}, next, []string{"INSERT INTO pitr VALUES ('EPOCH0')"}, 1_700_000_000, false)
	assertSealedPositions(t, catalog, 0, issue189Source, 4, epoch0End)
	epoch0 := sealedEpochRow(t, catalog, issue189Source, 0)

	// The new lease's checkpoint is still on this source file, so the dump
	// opens mysql-bin.000009.open.e1 beside the uploaded epoch 0 segment.
	setCheckpoint(checkpoints, binlog.Checkpoint{File: issue189Source, Pos: 4})
	epoch1End := sealEpochFromDump(t, base, catalog, uploader, checkpoints, 1, tasks.StartConfig{
		Mode: tasks.StartModeFilePos, File: issue189Source, Pos: 4,
	}, next, []string{
		"INSERT INTO pitr VALUES ('EPOCH1-A')",
		"INSERT INTO pitr VALUES ('EPOCH1-B')",
	}, 1_700_000_100, true)
	assertSealedPositions(t, catalog, 1, issue189Source+".sealed.e1", 4, epoch1End)
	assertEpochUnchanged(t, catalog, epoch0)

	setCheckpoint(checkpoints, binlog.Checkpoint{File: issue189Source, Pos: 4})
	epoch2End := sealEpochFromDump(t, base, catalog, uploader, checkpoints, 2, tasks.StartConfig{
		Mode: tasks.StartModeFilePos, File: issue189Source, Pos: 4,
	}, next, []string{
		"INSERT INTO pitr VALUES ('EPOCH2-A')",
		"INSERT INTO pitr VALUES ('EPOCH2-B')",
		"INSERT INTO pitr VALUES ('EPOCH2-C')",
	}, 1_700_000_200, true)
	assertSealedPositions(t, catalog, 2, issue189Source+".sealed.e2", 4, epoch2End)
	assertEpochUnchanged(t, catalog, epoch0)
	row1 := sealedEpochRow(t, catalog, issue189Source, 1)
	if row1.StartPos != 4 || row1.EndPos != epoch1End {
		t.Fatalf("epoch 1 changed while sealing epoch 2: %+v", row1)
	}
}

// TestIssue189_GTIDFallbackSealedPositions seals the same source name after
// file/pos resume gets MySQL 1236 and continues with the stored GTID set.
func TestIssue189_GTIDFallbackSealedPositions(t *testing.T) {
	base := t.TempDir()
	catalog := &takeoverCatalog{}
	uploader := &recordingUploader{}
	checkpoints := &memCheckpointStore{}
	const next = "mysql-bin.000010"
	const purged = "mysql-bin.000008"

	epoch0End := sealEpochFromDump(t, base, catalog, uploader, checkpoints, 0, tasks.StartConfig{
		Mode: tasks.StartModeFilePos, File: issue189Source, Pos: 4,
	}, next, []string{"INSERT INTO pitr VALUES ('BEFORE-1236')"}, 1_700_000_000, false)
	assertSealedPositions(t, catalog, 0, issue189Source, 4, epoch0End)
	epoch0 := sealedEpochRow(t, catalog, issue189Source, 0)

	removeOpenSegments(t, filepath.Join(base, "task-1"))
	setCheckpoint(checkpoints, binlog.Checkpoint{
		File: purged, Pos: 400, GTIDSet: sampleGTID + ":1-20",
	})
	// The new segment has to extend past the uploaded epoch. A copy that ends
	// inside that span is the already-uploaded file and is not sealed again.
	fallbackEnd := sealEpochFromDump(t, base, catalog, uploader, checkpoints, 1, tasks.StartConfig{
		Mode: tasks.StartModeFilePos, File: purged, Pos: 400, GTIDSet: sampleGTID + ":1-20",
	}, next, []string{
		"INSERT INTO pitr VALUES ('AFTER-1236-A')",
		"INSERT INTO pitr VALUES ('AFTER-1236-B')",
		"INSERT INTO pitr VALUES ('AFTER-1236-C')",
	}, 1_700_000_300, true, gtidFallbackTo(issue189Source))
	assertSealedPositions(t, catalog, 1, issue189Source+".sealed.e1", 4, fallbackEnd)
	assertEpochUnchanged(t, catalog, epoch0)
	if _, err := os.Stat(filepath.Join(base, "task-1", purged)); !os.IsNotExist(err) {
		t.Fatalf("purged magic-only file still present: %v", err)
	}
}

// TestIssue189_ResumeCursorDoesNotReplaceContainedSpan seals a later epoch
// whose open file already holds a complete binlog. The dump cursor is the
// last event, which is what an earlier epoch recorded as a point. The sealed
// row has to name the first event in the file, and the uploaded epoch 0 row
// keeps the end_pos it was stored with.
func TestIssue189_ResumeCursorDoesNotReplaceContainedSpan(t *testing.T) {
	base := t.TempDir()
	const taskID = "task-1"
	taskDir := filepath.Join(base, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(taskDir, issue189Source)
	if err := os.WriteFile(plain, []byte{0xfe, 'b', 'i', 'n'}, 0o644); err != nil {
		t.Fatal(err)
	}
	openPath := filepath.Join(taskDir, issue189Source+".open.e1")
	_, end := writeSealedBinlog(t, openPath, "INSERT INTO pitr VALUES ('ALREADY-ON-DISK')")
	const objectKey = "prefix/cluster-a/11111111-1111-1111-1111-111111111111/" + issue189Source
	catalog := &takeoverCatalog{rows: []tasks.BinlogFile{{
		TaskID: taskID, FileName: issue189Source, FilePath: plain,
		Epoch: 0, State: "SEALED", StartPos: 4, EndPos: 0,
		SizeBytes: 4, UploadState: "UPLOADED", ObjectKey: objectKey, Checksum: tasks.ChecksumMatch,
	}}}
	uploader := &recordingUploader{}
	checkpoints := &memCheckpointStore{}
	runner := NewMySQLRunner(base,
		WithCheckpointStore(checkpoints),
		WithFileMetaStore(catalog),
		WithUploader(uploader, "prefix"),
	)
	runner.fetcher = &fakeSourceMetaFetcher{
		status:     MasterStatus{File: "mysql-bin.000099", Pos: 9000},
		serverUUID: "11111111-1111-1111-1111-111111111111",
	}
	events := []*goreplication.BinlogEvent{
		preambleEvent(0, time.Unix(1_700_000_000, 0).UTC()),
		artificialRotateAt("mysql-bin.000010", 4),
	}
	runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer {
		return &fakeSyncer{streamer: &fakeStreamer{results: []streamResult{
			{event: events[0]},
			{event: events[1]},
			{err: context.Canceled},
		}}}
	}
	task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest})
	task.Epoch = 1
	task.OwnerWorkerID = "worker-a"
	if err := runner.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertSealedPositions(t, catalog, 1, issue189Source+".sealed.e1", 4, end)
	kept := sealedEpochRow(t, catalog, issue189Source, 0)
	if kept.EndPos != 0 || kept.UploadState != "UPLOADED" || kept.Checksum != tasks.ChecksumMatch || kept.FilePath != plain || kept.ObjectKey != objectKey {
		t.Fatalf("historical epoch 0 row rewritten: %+v", kept)
	}
}

// TestIssue189_EnrollSealedSegmentUsesEventPositions is a sealed file the
// retry set does not yet name. Enroll must record the positions in the file,
// not start_pos 4 and end_pos 0.
func TestIssue189_EnrollSealedSegmentUsesEventPositions(t *testing.T) {
	base := t.TempDir()
	const taskID = "task-1"
	taskDir := filepath.Join(base, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const name = "mysql-bin.000011.sealed.e1"
	_, end := writeSealedBinlog(t, filepath.Join(taskDir, name), "INSERT INTO pitr VALUES ('ENROLL')")
	catalog := &takeoverCatalog{}
	uploader := &recordingUploader{}
	checkpoints := &memCheckpointStore{cp: binlog.Checkpoint{File: "mysql-bin.000012", Pos: 4}, ok: true}
	sealEpochFromDump(t, base, catalog, uploader, checkpoints, 1, tasks.StartConfig{
		Mode: tasks.StartModeFilePos, File: "mysql-bin.000012", Pos: 4,
	}, "mysql-bin.000013", nil, 0, false)
	row := sealedEpochRow(t, catalog, "mysql-bin.000011", 1)
	if row.State != "SEALED" || row.UploadState != "UPLOAD_FAILED" {
		t.Fatalf("enroll row %+v", row)
	}
	if row.StartPos != 4 || row.EndPos != end || row.EndPos == 0 {
		t.Fatalf("enroll positions start=%d end=%d, want 4..%d", row.StartPos, row.EndPos, end)
	}
	if filepath.Base(row.FilePath) != name {
		t.Fatalf("enroll path %s", row.FilePath)
	}
}

type gtidOpen struct {
	file string
}

func gtidFallbackTo(file string) *gtidOpen { return &gtidOpen{file: file} }

func sealEpochFromDump(
	t *testing.T,
	base string,
	catalog FileMetaStore,
	uploader *recordingUploader,
	checkpoints *memCheckpointStore,
	epoch int64,
	start tasks.StartConfig,
	next string,
	sqls []string,
	ts uint32,
	zeroCursor bool,
	fallback ...*gtidOpen,
) uint32 {
	t.Helper()
	events, end := issue189Events(start.File, next, sqls, ts, zeroCursor)
	openName := start.File
	if len(fallback) == 1 && fallback[0] != nil {
		events, end = issue189Events(fallback[0].file, next, sqls, ts, zeroCursor)
		events = append([]*goreplication.BinlogEvent{artificialRotateAt(fallback[0].file, 4)}, events...)
		openName = fallback[0].file
	}
	var syncer binlogSyncer
	results := make([]streamResult, 0, len(events)+1)
	for _, ev := range events {
		results = append(results, streamResult{event: ev})
	}
	results = append(results, streamResult{err: context.Canceled})
	if len(fallback) == 1 && fallback[0] != nil {
		purged := &gomysql.MyError{Code: 1236, State: "HY000", Message: "Could not find first log file name in binary log index file"}
		syncer = &purgeFileSyncer{
			streamer:     &fakeStreamer{results: []streamResult{{err: purged}}},
			gtidStreamer: &fakeStreamer{results: results},
		}
	} else {
		syncer = &fakeSyncer{streamer: &fakeStreamer{results: results}}
	}
	task := newRunnerTask(start)
	task.Epoch = epoch
	task.OwnerWorkerID = "worker-a"
	runner := NewMySQLRunner(base,
		WithCheckpointStore(checkpoints),
		WithFileMetaStore(catalog),
		WithUploader(uploader, "prefix"),
	)
	runner.fetcher = &fakeSourceMetaFetcher{serverUUID: "11111111-1111-1111-1111-111111111111"}
	runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer }
	if err := runner.Run(context.Background(), task); err != nil {
		t.Fatalf("epoch %d (%s) Run: %v", epoch, openName, err)
	}
	return end
}

// issue189Events is a format description and the queries, then an optional
// same-file artificial rotate whose position is 0, then an artificial rotate
// onto the next file. The same-file rotate is what a dump sends for the
// current file. Position 0 is not a binlog position. The next-file rotate is
// the source-restart rotate that seals without being written.
func issue189Events(source, next string, sqls []string, ts uint32, zeroCursor bool) ([]*goreplication.BinlogEvent, uint32) {
	if len(sqls) == 0 {
		return nil, 0
	}
	pos := uint32(4)
	fde, pos := crcFormatDescription(ts, pos)
	out := []*goreplication.BinlogEvent{fde}
	var end uint32
	for i, sql := range sqls {
		ev, nextPos := crcQueryEvent("t", sql, ts+uint32(i)+1, pos)
		pos = nextPos
		end = nextPos
		out = append(out, ev)
	}
	if zeroCursor {
		out = append(out, artificialRotateAt(source, 0))
	}
	out = append(out, artificialRotateAt(next, 4))
	return out, end
}

func artificialRotateAt(next string, pos uint64) *goreplication.BinlogEvent {
	name := []byte(next)
	body := make([]byte, 8+len(name))
	binary.LittleEndian.PutUint64(body[:8], pos)
	copy(body[8:], name)
	size := uint32(goreplication.EventHeaderSize + len(body))
	raw := make([]byte, size)
	raw[4] = byte(goreplication.ROTATE_EVENT)
	binary.LittleEndian.PutUint32(raw[5:9], 1)
	binary.LittleEndian.PutUint32(raw[9:13], size)
	binary.LittleEndian.PutUint16(raw[17:19], goreplication.LOG_EVENT_ARTIFICIAL_F)
	return &goreplication.BinlogEvent{
		Header: &goreplication.EventHeader{
			EventType: goreplication.ROTATE_EVENT,
			ServerID:  1,
			EventSize: size,
			Flags:     goreplication.LOG_EVENT_ARTIFICIAL_F,
		},
		Event:   &goreplication.RotateEvent{Position: pos, NextLogName: append([]byte(nil), name...)},
		RawData: raw,
	}
}

func writeSealedBinlog(t *testing.T, path, sql string) (start, end uint32) {
	t.Helper()
	ts := uint32(1_700_000_000)
	fde, pos := crcFormatDescription(ts, 4)
	query, pos := crcQueryEvent("t", sql, ts+1, pos)
	var buf bytes.Buffer
	buf.Write(binlogMagic)
	buf.Write(fde.RawData)
	buf.Write(query.RawData)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return 4, pos
}

func assertSealedPositions(t *testing.T, catalog *takeoverCatalog, epoch int64, baseName string, start, end uint32) {
	t.Helper()
	row := sealedEpochRow(t, catalog, issue189Source, epoch)
	if row.State != "SEALED" || row.UploadState != "UPLOADED" || row.Checksum != tasks.ChecksumMatch {
		t.Fatalf("epoch %d files API row %+v", epoch, row)
	}
	if filepath.Base(row.FilePath) != baseName {
		t.Fatalf("epoch %d path %s, want %s", epoch, row.FilePath, baseName)
	}
	if row.StartPos != start || row.EndPos != end {
		t.Fatalf("epoch %d start_pos=%d end_pos=%d size=%d, want %d..%d", epoch, row.StartPos, row.EndPos, row.SizeBytes, start, end)
	}
	body, err := os.ReadFile(row.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, artificialRotateAt(issue189Source, 0).RawData) || bytes.Contains(body, artificialRotateAt("mysql-bin.000010", 4).RawData) {
		t.Fatalf("epoch %d sealed segment contains an artificial rotate", epoch)
	}
	assertChecksumSegment(t, row.FilePath, "", nil)
	fileStart, fileEnd, ok := containedEventSpan(row.FilePath)
	if !ok || fileStart != start || fileEnd != end {
		t.Fatalf("epoch %d file span %d..%d ok=%v, catalog %d..%d", epoch, fileStart, fileEnd, ok, start, end)
	}
}

func assertEpochUnchanged(t *testing.T, catalog *takeoverCatalog, before tasks.BinlogFile) {
	t.Helper()
	row := sealedEpochRow(t, catalog, before.FileName, before.Epoch)
	if row.StartPos != before.StartPos || row.EndPos != before.EndPos || row.FilePath != before.FilePath ||
		row.UploadState != before.UploadState || row.Checksum != before.Checksum || row.ObjectKey != before.ObjectKey ||
		row.SizeBytes != before.SizeBytes || row.State != before.State {
		t.Fatalf("epoch %d row changed\nbefore %+v\nafter  %+v", before.Epoch, before, row)
	}
}

func sealedEpochRow(t *testing.T, catalog *takeoverCatalog, name string, epoch int64) tasks.BinlogFile {
	t.Helper()
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	for _, row := range catalog.rows {
		if row.FileName == name && row.Epoch == epoch {
			return row
		}
	}
	t.Fatalf("no catalog row %s epoch %d in %+v", name, epoch, catalog.rows)
	return tasks.BinlogFile{}
}

func setCheckpoint(store *memCheckpointStore, cp binlog.Checkpoint) {
	store.mu.Lock()
	store.cp = cp
	store.ok = true
	store.mu.Unlock()
}

func removeOpenSegments(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		named, ok := binlog.ClassifySegment(entry.Name())
		if !ok || !named.Open {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			t.Fatal(err)
		}
	}
}

// containedEventSpan is the first and last binlog position in path.
// Events with end log_pos 0 are skipped. A gap stops the walk backward, so a
// leading format description does not pull a later event's start back to 4.
func containedEventSpan(path string) (start, end uint32, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, 0, false
	}
	size := info.Size()
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil || string(magic) != string(binlogMagic) {
		return 0, 0, false
	}
	type span struct{ start, end uint32 }
	var events []span
	offset := int64(4)
	hdr := make([]byte, goreplication.EventHeaderSize)
	for offset+int64(goreplication.EventHeaderSize) <= size {
		if _, err := io.ReadFull(f, hdr); err != nil {
			break
		}
		eventSize := binary.LittleEndian.Uint32(hdr[9:13])
		logPos := binary.LittleEndian.Uint32(hdr[13:17])
		if eventSize < uint32(goreplication.EventHeaderSize) || offset+int64(eventSize) > size {
			break
		}
		if _, err := f.Seek(int64(eventSize)-int64(goreplication.EventHeaderSize), io.SeekCurrent); err != nil {
			break
		}
		offset += int64(eventSize)
		if logPos == 0 || logPos < eventSize {
			continue
		}
		events = append(events, span{start: logPos - eventSize, end: logPos})
	}
	if len(events) == 0 {
		return 0, 0, false
	}
	last := events[len(events)-1]
	start = last.start
	for i := len(events) - 2; i >= 0; i-- {
		if events[i].end < start {
			break
		}
		if events[i].start < start {
			start = events[i].start
		}
	}
	return start, last.end, true
}
