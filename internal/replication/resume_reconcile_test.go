// Package replication provides module-level functionality for replication.
// input: an open segment on disk that holds complete or unfinished transactions past the stored checkpoint, and fake dumps with GTID holes, out-of-order GTIDs, several UUIDs, a mid-transaction drop, and a re-sent file header
// output: assertions that a start or reconnect resumes from what the segment holds without a gap or a duplicate, that an unfinished trailing transaction is cut and fetched again whole, and that only a rotate or a backwards transaction position fails with STREAM_REGRESSION
// pos: unit coverage for the false STREAM_REGRESSION after a reconnect, restart, or upgrade, and for legal GTID holes and order
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

const otherUUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

// diskSegment builds open-segment bytes whose positions chain from 4.
type diskSegment struct {
	buf []byte
	pos uint32
}

func newDiskSegment() *diskSegment {
	s := &diskSegment{buf: []byte{0xfe, 'b', 'i', 'n'}, pos: 4}
	fde := make([]byte, 62)
	binary.LittleEndian.PutUint16(fde[0:2], 4)
	copy(fde[2:52], "8.0.46")
	fde[56] = byte(goreplication.EventHeaderSize)
	fde[57] = byte(goreplication.BINLOG_CHECKSUM_ALG_OFF)
	s.add(goreplication.FORMAT_DESCRIPTION_EVENT, fde)
	return s
}

func (s *diskSegment) add(kind goreplication.EventType, body []byte) {
	size := uint32(goreplication.EventHeaderSize + len(body))
	hdr := make([]byte, goreplication.EventHeaderSize)
	hdr[4] = byte(kind)
	binary.LittleEndian.PutUint32(hdr[9:13], size)
	s.pos += size
	binary.LittleEndian.PutUint32(hdr[13:17], s.pos)
	s.buf = append(s.buf, hdr...)
	s.buf = append(s.buf, body...)
}

func (s *diskSegment) gtid(uuid string, seq int64) {
	sid, err := hex.DecodeString(strings.ReplaceAll(uuid, "-", ""))
	if err != nil {
		panic(err)
	}
	body := make([]byte, 1+goreplication.SidLength+8)
	copy(body[1:], sid)
	binary.LittleEndian.PutUint64(body[1+goreplication.SidLength:], uint64(seq))
	s.add(goreplication.GTID_EVENT, body)
}

func (s *diskSegment) begin() {
	body := make([]byte, 14+5)
	copy(body[14:], "BEGIN")
	s.add(goreplication.QUERY_EVENT, body)
}

func (s *diskSegment) rows() { s.add(goreplication.WRITE_ROWS_EVENTv2, make([]byte, 32)) }
func (s *diskSegment) xid()  { s.add(goreplication.XID_EVENT, make([]byte, 8)) }

func (s *diskSegment) txn(seq int64) {
	s.gtid(sampleGTID, seq)
	s.begin()
	s.rows()
	s.xid()
}

func (s *diskSegment) write(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, s.buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

func eventOf(kind goreplication.EventType, pos uint32) *goreplication.BinlogEvent {
	return &goreplication.BinlogEvent{
		Header:  &goreplication.EventHeader{EventType: kind, LogPos: pos, Timestamp: uint32(time.Now().Unix())},
		RawData: []byte("event"),
	}
}

func gtidOf(uuid string, gno int64, pos uint32) *goreplication.BinlogEvent {
	ev := gtidAt(gno, pos)
	sid, err := hex.DecodeString(strings.ReplaceAll(uuid, "-", ""))
	if err != nil {
		panic(err)
	}
	ev.Event = &goreplication.GTIDEvent{SID: sid, GNO: gno}
	return ev
}

func epochTask(start tasks.StartConfig) tasks.Task {
	task := newRunnerTask(start)
	task.Epoch = 1
	return task
}

func lastCheckpoint(t *testing.T, store *fakeRunnerCheckpointStore) binlog.Checkpoint {
	t.Helper()
	if len(store.upserts) == 0 {
		t.Fatal("no checkpoint written")
	}
	return store.upserts[len(store.upserts)-1]
}

func newDiskRunner(dir string, syncer *fakeSyncer, store *fakeRunnerCheckpointStore) *MySQLRunner {
	return &MySQLRunner{
		dataDir:         dir,
		fetcher:         &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		checkpointStore: store,
		newSyncer:       func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer },
		killDump:        func(tasks.SourceConfig, uint32) error { return nil },
	}
}

// QA P1-1: the segment holds :11 complete, the checkpoint row was written
// at the file end with :1-10 (the state a false STREAM_REGRESSION left).
// Start continues at the file end and the next transaction :12 is not a gap.
func TestRun_ResumeAddsCompleteTailTransactionMissingFromCheckpoint(t *testing.T) {
	dir := t.TempDir()
	seg := newDiskSegment()
	seg.txn(10)
	seg.txn(11)
	end := seg.pos
	path := filepath.Join(dir, "task-1", "mysql-bin.000001.open.e1")
	seg.write(t, path)
	size := int64(len(seg.buf))

	store := &fakeRunnerCheckpointStore{
		loadOK:         true,
		loadCheckpoint: binlog.Checkpoint{File: "mysql-bin.000001", Pos: end, GTIDSet: sampleGTID + ":1-10"},
	}
	syncer := &fakeSyncer{connID: 51, streamer: &fakeStreamer{results: []streamResult{
		{event: rotateDumpEvent("mysql-bin.000001", uint64(end))},
		{event: gtidAt(12, end+100)},
		{event: sqlAt("BEGIN", end+150)},
		{event: xidAt(end + 200)},
		{err: context.Canceled},
	}}}
	err := newDiskRunner(dir, syncer, store).Run(context.Background(), epochTask(tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: sampleGTID + ":1-9"}))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if syncer.startGTIDCalls != 0 || syncer.startPosCalls != 1 || syncer.startPos.Name != "mysql-bin.000001" || syncer.startPos.Pos != end {
		t.Fatalf("dump start %+v pos calls %d gtid calls %d, want mysql-bin.000001:%d", syncer.startPos, syncer.startPosCalls, syncer.startGTIDCalls, end)
	}
	cp := lastCheckpoint(t, store)
	assertGTID(t, "mysql", cp.GTIDSet, sampleGTID+":1-12")
	info, err := os.Stat(path)
	if err != nil || info.Size() <= size {
		t.Fatalf("segment not appended: %v %v", info, err)
	}
}

// A drop or crash inside a transaction leaves its first events on disk.
// The resume cuts them and the source sends the transaction again whole,
// so the commit records its GTID and nothing is stored twice.
func TestRun_ResumeCutsUnfinishedTrailingTransaction(t *testing.T) {
	dir := t.TempDir()
	seg := newDiskSegment()
	seg.txn(10)
	commitEnd := seg.pos
	cut := int64(len(seg.buf))
	seg.gtid(sampleGTID, 11)
	seg.begin()
	seg.rows()
	path := filepath.Join(dir, "task-1", "mysql-bin.000001.open.e1")
	seg.write(t, path)

	store := &fakeRunnerCheckpointStore{
		loadOK:         true,
		loadCheckpoint: binlog.Checkpoint{File: "mysql-bin.000001", Pos: seg.pos, GTIDSet: sampleGTID + ":1-10"},
	}
	var cutSize int64 = -1
	syncer := &fakeSyncer{connID: 52}
	syncer.streamer = &fakeStreamer{
		onCall: func(call int) {
			if call == 1 {
				if info, err := os.Stat(path); err == nil {
					cutSize = info.Size()
				}
			}
		},
		results: []streamResult{
			{event: rotateDumpEvent("mysql-bin.000001", uint64(commitEnd))},
			{event: gtidAt(11, commitEnd+60)},
			{event: sqlAt("BEGIN", commitEnd+100)},
			{event: eventOf(goreplication.WRITE_ROWS_EVENTv2, commitEnd+160)},
			{event: xidAt(commitEnd + 200)},
			{err: context.Canceled},
		},
	}
	err := newDiskRunner(dir, syncer, store).Run(context.Background(), epochTask(tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: sampleGTID + ":1-9"}))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if cutSize != cut {
		t.Fatalf("segment size before the dump %d, want cut at %d", cutSize, cut)
	}
	if syncer.startPos.Pos != commitEnd {
		t.Fatalf("resumed at %d, want the end of :10 at %d", syncer.startPos.Pos, commitEnd)
	}
	assertGTID(t, "mysql", lastCheckpoint(t, store).GTIDSet, sampleGTID+":1-11")
}

// A mid-file first transaction cannot be cut without rewinding to the
// header, so the resume continues inside it and its commit still counts.
func TestRun_ResumeContinuesInsideUncuttableTransaction(t *testing.T) {
	dir := t.TempDir()
	seg := newDiskSegment()
	seg.pos = 5000
	seg.gtid(sampleGTID, 11)
	seg.begin()
	seg.rows()
	path := filepath.Join(dir, "task-1", "mysql-bin.000001.open.e1")
	seg.write(t, path)
	size := int64(len(seg.buf))

	store := &fakeRunnerCheckpointStore{
		loadOK:         true,
		loadCheckpoint: binlog.Checkpoint{File: "mysql-bin.000001", Pos: seg.pos, GTIDSet: sampleGTID + ":1-10"},
	}
	syncer := &fakeSyncer{connID: 53, streamer: &fakeStreamer{results: []streamResult{
		{event: rotateDumpEvent("mysql-bin.000001", uint64(seg.pos))},
		{event: xidAt(seg.pos + 30)},
		{event: gtidAt(12, seg.pos+100)},
		{event: xidAt(seg.pos + 130)},
		{err: context.Canceled},
	}}}
	err := newDiskRunner(dir, syncer, store).Run(context.Background(), epochTask(tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: sampleGTID + ":1-9"}))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if syncer.startPos.Pos != seg.pos {
		t.Fatalf("resumed at %d, want %d", syncer.startPos.Pos, seg.pos)
	}
	if info, err := os.Stat(path); err != nil || info.Size() <= size {
		t.Fatalf("segment was cut or not appended: %v %v", info, err)
	}
	assertGTID(t, "mysql", lastCheckpoint(t, store).GTIDSet, sampleGTID+":1-12")
}

// QA P1-2: holes, out-of-order numbers and several UUIDs are legal.
func TestRun_GTIDHolesOrderAndUUIDsNeverFail(t *testing.T) {
	dir := t.TempDir()
	store := &fakeRunnerCheckpointStore{}
	syncer := &fakeSyncer{connID: 54, streamer: &fakeStreamer{results: []streamResult{
		{event: artificialSourceRotate("mysql-bin.000001")},
		{event: gtidAt(100, 200)}, // SET GTID_NEXT=...:100
		{event: xidAt(260)},
		{event: gtidAt(12, 300)}, // MTA replica: 12 before 11
		{event: xidAt(360)},
		{event: gtidAt(11, 400)},
		{event: xidAt(460)},
		{event: gtidOf(otherUUID, 5, 500)},
		{event: xidAt(560)},
		{event: gtidAt(1000000, 600)}, // group replication block
		{event: xidAt(660)},
		{err: context.Canceled},
	}}}
	err := newDiskRunner(dir, syncer, store).Run(context.Background(), newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: sampleGTID + ":1-10"}))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	assertGTID(t, "mysql", lastCheckpoint(t, store).GTIDSet, sampleGTID+":1-12:100:1000000,"+otherUUID+":5")
}

// A dump drop in the middle of a transaction reopens at the flushed file
// position, not from its GTID, so the rest of that transaction continues.
func TestRun_ReconnectInsideTransactionContinuesAtFlushedPosition(t *testing.T) {
	dumpReconnectDelay = 0
	t.Cleanup(func() { dumpReconnectDelay = time.Second })
	dir := t.TempDir()
	store := &fakeRunnerCheckpointStore{}
	syncer := &fakeSyncer{connID: 55, streamer: &fakeStreamer{results: []streamResult{
		{event: artificialSourceRotate("mysql-bin.000001")},
		{event: gtidAt(11, 100)},
		{event: sqlAt("BEGIN", 150)},
		{err: io.EOF},
		{event: rotateDumpEvent("mysql-bin.000001", 150)},
		{event: eventOf(goreplication.WRITE_ROWS_EVENTv2, 200)},
		{event: xidAt(250)},
		{event: gtidAt(12, 300)},
		{event: xidAt(350)},
		{err: context.Canceled},
	}}}
	err := newDiskRunner(dir, syncer, store).Run(context.Background(), newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: sampleGTID + ":1-10"}))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if syncer.startGTIDCalls != 1 || syncer.startPosCalls != 1 || syncer.startPos.Name != "mysql-bin.000001" || syncer.startPos.Pos != 150 {
		t.Fatalf("reconnect pos %+v calls pos=%d gtid=%d", syncer.startPos, syncer.startPosCalls, syncer.startGTIDCalls)
	}
	assertGTID(t, "mysql", lastCheckpoint(t, store).GTIDSet, sampleGTID+":1-12")
}

// A GTID dump that starts in a file the segment already holds sends that
// file's previous-GTIDs header again. It is not a regression.
func TestRun_ResentFileHeaderBehindCursorIsIgnored(t *testing.T) {
	dir := t.TempDir()
	writes := 0
	syncer := &fakeSyncer{connID: 56, streamer: &fakeStreamer{results: []streamResult{
		{event: rotateDumpEvent("mysql-bin.000001", 500)},
		{event: eventOf(goreplication.PREVIOUS_GTIDS_EVENT, 191)},
		{event: newRunnerEvent(600)},
		{err: context.Canceled},
	}}}
	runner := newDiskRunner(dir, syncer, nil)
	runner.checkpointStore = nil
	runner.writerOpener = func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
		buf := &fakeSyncFile{}
		w := binlog.NewWriter(buf, binlog.Checkpoint{File: fileName, Pos: initialPos})
		return &countingCloser{writes: &writes, buf: buf}, w, filepath.Join(dir, "task-1", fileName), nil
	}
	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeFilePos, File: "mysql-bin.000001", Pos: 500}))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if writes != 1 {
		t.Fatalf("writes %d, want only the event at 600", writes)
	}
}

func TestExecutedGTID_AbsorbTail(t *testing.T) {
	ex := newExecutedGTID("mysql", sampleGTID+":1-10")
	ex.absorbTail(binlog.OpenTail{GTIDs: []string{sampleGTID + ":12", sampleGTID + ":11"}, PartialGTID: sampleGTID + ":13", PartialInTxn: true})
	assertGTID(t, "mysql", ex.current(), sampleGTID+":1-12")
	ex.note(xidAt(100))
	assertGTID(t, "mysql", ex.current(), sampleGTID+":1-13")

	cut := newExecutedGTID("mysql", sampleGTID+":1-10")
	cut.absorbTail(binlog.OpenTail{PartialGTID: sampleGTID + ":11", Truncate: true})
	cut.note(xidAt(100))
	assertGTID(t, "mysql", cut.current(), sampleGTID+":1-10")
	if _, err := gomysql.ParseGTIDSet("mysql", cut.current()); err != nil {
		t.Fatal(err)
	}
}
