// Package replication provides module-level functionality for replication.
// input: GTID events, a stored checkpoint, and a file/pos dump that returns MySQL 1236
// output: proof that a flushed checkpoint keeps the executed GTID and that resume uses it when file/pos returns 1236
// pos: regression coverage for GTID checkpoint retention and purged-file resume
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
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

const sampleSID = "24bc785e9a6111e18a5d080027635ef5"
const sampleGTID = "24bc785e-9a61-11e1-8a5d-080027635ef5"

func TestExecutedGTID_CommitExtendsSeedAndDoesNotWipe(t *testing.T) {
	seed := sampleGTID + ":1-10"
	executed := newExecutedGTID("mysql", seed)
	if executed.current() != seed {
		t.Fatalf("seed: %s", executed.current())
	}

	executed.note(gtidAt(11, 100))
	executed.note(sqlAt("BEGIN", 120))
	if got := executed.current(); got != seed {
		t.Fatalf("uncommitted gtid changed the set: %s", got)
	}
	executed.note(sqlAt("ROLLBACK TO SAVEPOINT s", 130))
	if got := executed.current(); got != seed {
		t.Fatalf("rollback to savepoint committed: %s", got)
	}
	executed.note(xidAt(154))
	assertGTID(t, "mysql", executed.current(), sampleGTID+":1-11")

	executed.note(gtidAt(12, 180))
	executed.note(sqlAt("CREATE TABLE t (id INT)", 200))
	assertGTID(t, "mysql", executed.current(), sampleGTID+":1-12")

	anon := &goreplication.BinlogEvent{
		Header:  &goreplication.EventHeader{EventType: goreplication.ANONYMOUS_GTID_EVENT, LogPos: 210},
		Event:   &goreplication.GTIDEvent{SID: mustSID(t), GNO: 13},
		RawData: []byte("anon"),
	}
	executed.note(anon)
	executed.note(xidAt(220))
	assertGTID(t, "mysql", executed.current(), sampleGTID+":1-12")
}

func TestMysqlError1236(t *testing.T) {
	my := &gomysql.MyError{Code: 1236, State: "HY000", Message: "Could not find first log file name in binary log index file"}
	if !mysqlError1236(my) || !mysqlError1236(fmt.Errorf("dump: %w", my)) {
		t.Fatal("typed 1236")
	}
	if !mysqlError1236(errors.New("ERROR 1236 (HY000): Could not find first log file name in binary log index file")) {
		t.Fatal("text 1236")
	}
	if mysqlError1236(&gomysql.MyError{Code: 1045, Message: "access denied"}) || mysqlError1236(errors.New("dial tcp: connection refused")) {
		t.Fatal("unrelated error looked like a purged binlog")
	}
}

func TestMySQLRunnerRun_GTIDCheckpointSurvivesFlush(t *testing.T) {
	seed := sampleGTID + ":1-10"
	store := &fakeRunnerCheckpointStore{}
	streamer := &fakeStreamer{results: []streamResult{
		{event: gtidAt(11, 100)},
		{event: sqlAt("BEGIN", 120)},
		{event: xidAt(154)},
		{event: rotateTo("mysql-bin.000011", 4, 200)},
		{err: context.Canceled},
	}}
	syncer := &fakeSyncer{streamer: streamer}
	runner := &MySQLRunner{
		dataDir:         t.TempDir(),
		fetcher:         &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		checkpointStore: store,
		newSyncer: func(goreplication.BinlogSyncerConfig) binlogSyncer {
			return syncer
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			dir := t.TempDir()
			path := filepath.Join(dir, fileName)
			if err := os.WriteFile(path, []byte{0xfe, 'b', 'i', 'n'}, 0o644); err != nil {
				return nil, nil, "", err
			}
			return &fakeCloser{}, binlog.NewWriter(&fakeSyncFile{}, binlog.Checkpoint{File: fileName, Pos: initialPos}), path, nil
		},
	}
	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode:    tasks.StartModeGTID,
		GTIDSet: seed,
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.upserts) < 4 {
		t.Fatalf("upserts: %+v", store.upserts)
	}
	for i, cp := range store.upserts {
		if strings.TrimSpace(cp.GTIDSet) == "" {
			t.Fatalf("upsert %d wiped gtid: %+v", i, cp)
		}
	}
	assertGTID(t, "mysql", store.upserts[0].GTIDSet, seed)
	assertGTID(t, "mysql", store.upserts[1].GTIDSet, seed)
	assertGTID(t, "mysql", store.upserts[2].GTIDSet, sampleGTID+":1-11")
	last := store.upserts[len(store.upserts)-1]
	if last.File != "mysql-bin.000011" || last.Pos != 4 {
		t.Fatalf("rotate checkpoint: %+v", last)
	}
	assertGTID(t, "mysql", last.GTIDSet, sampleGTID+":1-11")
}

func TestMySQLRunnerRun_FilePosDoesNotInventGTID(t *testing.T) {
	store := &fakeRunnerCheckpointStore{}
	streamer := &fakeStreamer{results: []streamResult{
		{event: gtidAt(11, 100)},
		{event: xidAt(154)},
		{err: context.Canceled},
	}}
	runner := &MySQLRunner{
		dataDir:         t.TempDir(),
		fetcher:         &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		checkpointStore: store,
		newSyncer: func(goreplication.BinlogSyncerConfig) binlogSyncer {
			return &fakeSyncer{streamer: streamer}
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			return &fakeCloser{}, binlog.NewWriter(&fakeSyncFile{}, binlog.Checkpoint{File: fileName, Pos: initialPos}), t.TempDir() + "/" + fileName, nil
		},
	}
	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: "mysql-bin.000010",
		Pos:  4,
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.upserts) == 0 {
		t.Fatal("no checkpoint")
	}
	for i, cp := range store.upserts {
		if cp.GTIDSet != "" {
			t.Fatalf("upsert %d invented a gtid: %+v", i, cp)
		}
	}
}

func TestMySQLRunnerRun_FilePos1236ResumesByGTID(t *testing.T) {
	purged := &gomysql.MyError{Code: 1236, State: "HY000", Message: "Could not find first log file name in binary log index file"}
	executed := sampleGTID + ":1-20"
	startSet := sampleGTID + ":1-5"
	store := &fakeRunnerCheckpointStore{
		loadOK: true,
		loadCheckpoint: binlog.Checkpoint{
			File:    "mysql-bin.000008",
			Pos:     400,
			GTIDSet: executed,
		},
	}
	syncer := &purgeFileSyncer{
		posErr:   purged,
		streamer: &fakeStreamer{results: []streamResult{{err: context.Canceled}}},
	}
	runner := &MySQLRunner{
		dataDir:         t.TempDir(),
		fetcher:         &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		checkpointStore: store,
		newSyncer: func(goreplication.BinlogSyncerConfig) binlogSyncer {
			return syncer
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			return &fakeCloser{}, binlog.NewWriter(&fakeSyncFile{}, binlog.Checkpoint{File: fileName, Pos: initialPos}), t.TempDir() + "/" + fileName, nil
		},
	}
	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode:    tasks.StartModeGTID,
		GTIDSet: startSet,
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if syncer.posCalls != 1 || syncer.pos.Name != "mysql-bin.000008" || syncer.pos.Pos != 400 {
		t.Fatalf("file/pos was not tried first: %+v calls=%d", syncer.pos, syncer.posCalls)
	}
	if syncer.gtidCalls != 1 || syncer.gtid == nil {
		t.Fatalf("gtid resume calls=%d set=%v", syncer.gtidCalls, syncer.gtid)
	}
	assertGTID(t, "mysql", syncer.gtid.String(), executed)
}

func TestMySQLRunnerRun_FilePosWithoutGTIDStillReturns1236(t *testing.T) {
	purged := &gomysql.MyError{Code: 1236, State: "HY000", Message: "Could not find first log file name in binary log index file"}
	store := &fakeRunnerCheckpointStore{
		loadOK:         true,
		loadCheckpoint: binlog.Checkpoint{File: "mysql-bin.000008", Pos: 400},
	}
	syncer := &purgeFileSyncer{posErr: purged}
	runner := &MySQLRunner{
		dataDir:         t.TempDir(),
		fetcher:         &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		checkpointStore: store,
		newSyncer: func(goreplication.BinlogSyncerConfig) binlogSyncer {
			return syncer
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			return &fakeCloser{}, binlog.NewWriter(&fakeSyncFile{}, binlog.Checkpoint{File: fileName, Pos: initialPos}), t.TempDir() + "/" + fileName, nil
		},
	}
	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: "mysql-bin.000001",
		Pos:  4,
	}))
	if !mysqlError1236(err) {
		t.Fatalf("err=%v", err)
	}
	if syncer.gtidCalls != 0 {
		t.Fatalf("gtid resume without a set: %d", syncer.gtidCalls)
	}
}

func TestMySQLRunnerRun_FilePosWithGTIDStaysOnFilePos(t *testing.T) {
	store := &fakeRunnerCheckpointStore{
		loadOK: true,
		loadCheckpoint: binlog.Checkpoint{
			File:    "mysql-bin.000008",
			Pos:     400,
			GTIDSet: sampleGTID + ":1-20",
		},
	}
	syncer := &purgeFileSyncer{streamer: &fakeStreamer{results: []streamResult{{err: context.Canceled}}}}
	runner := &MySQLRunner{
		dataDir:         t.TempDir(),
		fetcher:         &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		checkpointStore: store,
		newSyncer: func(goreplication.BinlogSyncerConfig) binlogSyncer {
			return syncer
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			return &fakeCloser{}, binlog.NewWriter(&fakeSyncFile{}, binlog.Checkpoint{File: fileName, Pos: initialPos}), t.TempDir() + "/" + fileName, nil
		},
	}
	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode:    tasks.StartModeGTID,
		GTIDSet: sampleGTID + ":1-5",
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if syncer.posCalls != 1 || syncer.gtidCalls != 0 {
		t.Fatalf("pos=%d gtid=%d", syncer.posCalls, syncer.gtidCalls)
	}
}

type purgeFileSyncer struct {
	posErr    error
	streamer  binlogStreamer
	pos       gomysql.Position
	gtid      gomysql.GTIDSet
	posCalls  int
	gtidCalls int
}

func (s *purgeFileSyncer) StartSync(pos gomysql.Position) (binlogStreamer, error) {
	s.posCalls++
	s.pos = pos
	if s.posErr != nil {
		return nil, s.posErr
	}
	return s.streamer, nil
}

func (s *purgeFileSyncer) StartSyncGTID(set gomysql.GTIDSet) (binlogStreamer, error) {
	s.gtidCalls++
	s.gtid = set
	return s.streamer, nil
}

func (s *purgeFileSyncer) Close() {}

func gtidAt(gno int64, pos uint32) *goreplication.BinlogEvent {
	return &goreplication.BinlogEvent{
		Header: &goreplication.EventHeader{
			EventType: goreplication.GTID_EVENT,
			LogPos:    pos,
			Timestamp: uint32(time.Now().Unix()),
		},
		Event:   &goreplication.GTIDEvent{SID: mustSID(nil), GNO: gno},
		RawData: []byte("gtid"),
	}
}

func sqlAt(sql string, pos uint32) *goreplication.BinlogEvent {
	return &goreplication.BinlogEvent{
		Header: &goreplication.EventHeader{
			EventType: goreplication.QUERY_EVENT,
			LogPos:    pos,
			Timestamp: uint32(time.Now().Unix()),
		},
		Event:   &goreplication.QueryEvent{Query: []byte(sql)},
		RawData: []byte("query"),
	}
}

func xidAt(pos uint32) *goreplication.BinlogEvent {
	return &goreplication.BinlogEvent{
		Header: &goreplication.EventHeader{
			EventType: goreplication.XID_EVENT,
			LogPos:    pos,
			Timestamp: uint32(time.Now().Unix()),
		},
		Event:   &goreplication.XIDEvent{XID: 1},
		RawData: []byte("xid"),
	}
}

func mustSID(t *testing.T) []byte {
	sid, err := hex.DecodeString(sampleSID)
	if err != nil {
		if t != nil {
			t.Fatal(err)
		}
		panic(err)
	}
	return sid
}

func assertGTID(t *testing.T, flavor, got, want string) {
	t.Helper()
	if strings.TrimSpace(got) == "" {
		t.Fatalf("empty gtid, want %s", want)
	}
	gs, err := gomysql.ParseGTIDSet(flavor, got)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := gomysql.ParseGTIDSet(flavor, want)
	if err != nil {
		t.Fatal(err)
	}
	if !gs.Equal(ws) {
		t.Fatalf("gtid %s want %s", got, want)
	}
}
