// Package replication provides module-level functionality for replication.
// input: a fake dump that ends with EOF or a contradictory rotate, GTID, or position
// output: assertions that a reconnect resumes from the flushed file position with the flushed GTID set as its 1236 fallback, and that a rotate to the open or an older file or a position behind the cursor fails with STREAM_REGRESSION without recording the dropped GTID
// pos: unit coverage for the stale GTID re-dump that dropped transactions after a dump reconnect
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"encoding/binary"
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

func TestExecutedGTID_ContainsAndPeek(t *testing.T) {
	ex := newExecutedGTID("mysql", sampleGTID+":1-10")
	if !ex.contains(sampleGTID+":10") || ex.contains(sampleGTID+":11") || ex.contains(sampleGTID+":12") {
		t.Fatalf("contains %s", ex.current())
	}
	text, ok := ex.peekGTID(gtidAt(11, 100))
	if !ok || text != sampleGTID+":11" {
		t.Fatalf("peek %q ok=%v", text, ok)
	}
}

// A GTID task's dump that drops reopens at the flushed file position. A
// GTID dump from the flushed set would send the file header and any
// unfinished transaction again behind the cursor.
func TestRun_GTIDReconnectResumesFromFlushedPosition(t *testing.T) {
	dumpReconnectDelay = 0
	t.Cleanup(func() { dumpReconnectDelay = time.Second })

	dir := t.TempDir()
	store := &fakeRunnerCheckpointStore{}
	syncer := &fakeSyncer{connID: 41, streamer: &fakeStreamer{results: []streamResult{
		{event: artificialSourceRotate("mysql-bin.000001")},
		{event: gtidAt(11, 100)},
		{event: xidAt(200)},
		{err: io.EOF},
		{event: gtidAt(12, 300)},
		{event: xidAt(400)},
		{err: context.Canceled},
	}}}
	var names []string
	runner := &MySQLRunner{
		dataDir:         dir,
		fetcher:         &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		checkpointStore: store,
		newSyncer: func(goreplication.BinlogSyncerConfig) binlogSyncer {
			return syncer
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			names = append(names, fileName)
			buf := &fakeSyncFile{}
			w := binlog.NewWriter(buf, binlog.Checkpoint{File: fileName, Pos: initialPos})
			return &countingCloser{buf: buf}, w, filepath.Join(dir, "task-1", fileName), nil
		},
		killDump: func(tasks.SourceConfig, uint32) error { return nil },
	}
	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode:    tasks.StartModeGTID,
		GTIDSet: sampleGTID + ":1-10",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if syncer.startGTIDCalls != 1 {
		t.Fatalf("gtid dumps %d, want only the start", syncer.startGTIDCalls)
	}
	if syncer.startPosCalls != 1 || syncer.startPos.Name != "mysql-bin.000001" || syncer.startPos.Pos != 200 {
		t.Fatalf("reconnect %+v calls %d, want mysql-bin.000001:200", syncer.startPos, syncer.startPosCalls)
	}
	want, err := gomysql.ParseGTIDSet("mysql", sampleGTID+":1-12")
	if err != nil {
		t.Fatal(err)
	}
	got, err := gomysql.ParseGTIDSet("mysql", store.upserts[len(store.upserts)-1].GTIDSet)
	if err != nil || !got.Equal(want) {
		t.Fatalf("checkpoint %v, want %s", got, want)
	}
	if len(names) != 1 || names[0] != "mysql-bin.000001" {
		t.Fatalf("files %v", names)
	}
}

func TestRun_FilePosReconnectResumesFromFlushedPosition(t *testing.T) {
	dumpReconnectDelay = 0
	t.Cleanup(func() { dumpReconnectDelay = time.Second })

	dir := t.TempDir()
	syncer := &fakeSyncer{connID: 42, streamer: &fakeStreamer{results: []streamResult{
		{event: newRunnerEvent(200)},
		{err: io.EOF},
		{event: newRunnerEvent(350)},
		{err: context.Canceled},
	}}}
	runner := &MySQLRunner{
		dataDir: dir,
		fetcher: &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		newSyncer: func(goreplication.BinlogSyncerConfig) binlogSyncer {
			return syncer
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			buf := &fakeSyncFile{}
			w := binlog.NewWriter(buf, binlog.Checkpoint{File: fileName, Pos: initialPos})
			return &countingCloser{buf: buf}, w, filepath.Join(dir, "task-1", fileName), nil
		},
		killDump: func(tasks.SourceConfig, uint32) error { return nil },
	}
	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: "mysql-bin.000001",
		Pos:  100,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if syncer.startGTIDCalls != 0 {
		t.Fatalf("file/pos reconnect opened a GTID dump: %v", syncer.startGTID)
	}
	if syncer.startPosCalls != 2 || syncer.startPos.Name != "mysql-bin.000001" || syncer.startPos.Pos != 200 {
		t.Fatalf("reconnect position %+v calls %d, want mysql-bin.000001:200", syncer.startPos, syncer.startPosCalls)
	}
}

func TestRun_StreamRegressionDoesNotRecordTheGTID(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name   string
		start  tasks.StartConfig
		events []streamResult
		want   string
		writes int
	}{
		{
			name:  "older file rotate",
			start: tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: sampleGTID + ":1-10"},
			events: []streamResult{
				{event: artificialSourceRotate("mysql-bin.000006")},
				{event: newRunnerEvent(200)},
				{event: rotateAt("mysql-bin.000005", 4, 0)},
			},
			want:   "is behind open file",
			writes: 1,
		},
		{
			name:  "transaction behind cursor",
			start: tasks.StartConfig{Mode: tasks.StartModeFilePos, File: "mysql-bin.000001", Pos: 500},
			events: []streamResult{
				{event: gtidAt(11, 300)},
			},
			want: "behind stored pos",
		},
		{
			name:  "same file rotate",
			start: tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: sampleGTID + ":1-10"},
			events: []streamResult{
				{event: artificialSourceRotate("mysql-bin.000006")},
				{event: newRunnerEvent(200)},
				{event: rotateAt("mysql-bin.000006", 4, 1758)},
			},
			want:   "repeats open file",
			writes: 1,
		},
		{
			name:  "position behind cursor",
			start: tasks.StartConfig{Mode: tasks.StartModeFilePos, File: "mysql-bin.000001", Pos: 500},
			events: []streamResult{
				{event: newRunnerEvent(120)},
			},
			want: "behind stored pos",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			syncer := &fakeSyncer{connID: 43, streamer: &fakeStreamer{results: tc.events}}
			runner := &MySQLRunner{
				dataDir: dir,
				fetcher: &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
				newSyncer: func(goreplication.BinlogSyncerConfig) binlogSyncer {
					return syncer
				},
				writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
					buf := &fakeSyncFile{}
					w := binlog.NewWriter(buf, binlog.Checkpoint{File: fileName, Pos: initialPos})
					return &countingCloser{writes: &writes, buf: buf}, w, filepath.Join(dir, "task-1", fileName), nil
				},
				killDump: func(tasks.SourceConfig, uint32) error { return nil },
			}
			err := runner.Run(context.Background(), newRunnerTask(tc.start))
			var pe *tasks.PermanentError
			if !asPermanent(err, &pe) || pe.Code != tasks.CodeStreamRegression || !strings.Contains(pe.Message, tc.want) {
				t.Fatalf("err %v", err)
			}
			if writes != tc.writes {
				t.Fatalf("writes %d, want %d", writes, tc.writes)
			}
		})
	}
}

func TestRun_StoredGTIDIsSkipped(t *testing.T) {
	dir := t.TempDir()
	writes := 0
	syncer := &fakeSyncer{connID: 44, streamer: &fakeStreamer{results: []streamResult{
		{event: artificialSourceRotate("mysql-bin.000001")},
		{event: gtidAt(10, 50)},
		{event: xidAt(80)},
		{event: gtidAt(11, 120)},
		{event: xidAt(160)},
		{err: context.Canceled},
	}}}
	runner := &MySQLRunner{
		dataDir:   dir,
		fetcher:   &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		newSyncer: func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer },
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			buf := &fakeSyncFile{}
			w := binlog.NewWriter(buf, binlog.Checkpoint{File: fileName, Pos: initialPos})
			return &countingCloser{writes: &writes, buf: buf}, w, filepath.Join(dir, "task-1", fileName), nil
		},
		killDump: func(tasks.SourceConfig, uint32) error { return nil },
	}
	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode:    tasks.StartModeGTID,
		GTIDSet: sampleGTID + ":1-10",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if writes != 2 {
		t.Fatalf("writes %d, want only the new transaction", writes)
	}
}

func TestRun_StorageInconsistentRefusesStart(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "task-1")
	if err := writeStoredSegment(taskDir, "mysql-bin.000006", "mysql-bin.000006"); err != nil {
		t.Fatal(err)
	}
	syncer := &fakeSyncer{connID: 45, streamer: &fakeStreamer{results: []streamResult{{err: context.Canceled}}}}
	runner := &MySQLRunner{
		dataDir: dir,
		fetcher: &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		newSyncer: func(goreplication.BinlogSyncerConfig) binlogSyncer {
			return syncer
		},
		killDump: func(tasks.SourceConfig, uint32) error { return nil },
	}
	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode:    tasks.StartModeGTID,
		GTIDSet: sampleGTID + ":1-10",
	}))
	var pe *tasks.PermanentError
	if !asPermanent(err, &pe) || pe.Code != tasks.CodeStorageInconsistent || !strings.Contains(pe.Message, "rotate to mysql-bin.000006") {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(pe.Message, "Create a new task from a GTID the source still has") {
		t.Fatalf("message %s", pe.Message)
	}
	if syncer.startGTIDCalls != 0 || syncer.startPosCalls != 0 {
		t.Fatalf("dump started despite inconsistent storage: gtid %d file %d", syncer.startGTIDCalls, syncer.startPosCalls)
	}
}

func rotateAt(name string, pos uint64, logPos uint32) *goreplication.BinlogEvent {
	ev := rotateDumpEvent(name, pos)
	ev.Header.LogPos = logPos
	return ev
}

func writeStoredSegment(dir, name, rotateTo string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var buf []byte
	buf = append(buf, 0xfe, 'b', 'i', 'n')
	body := make([]byte, 8+len(rotateTo))
	binary.LittleEndian.PutUint64(body[:8], 4)
	copy(body[8:], rotateTo)
	hdr := make([]byte, goreplication.EventHeaderSize)
	hdr[4] = byte(goreplication.ROTATE_EVENT)
	binary.LittleEndian.PutUint32(hdr[9:13], uint32(len(hdr)+len(body)))
	binary.LittleEndian.PutUint32(hdr[13:17], 1758)
	buf = append(buf, hdr...)
	buf = append(buf, body...)
	return os.WriteFile(filepath.Join(dir, name), buf, 0o644)
}
