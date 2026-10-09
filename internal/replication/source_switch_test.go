// Package replication provides module-level functionality for replication.
// input: a fake source whose identity changes between dump connections or on a MySQL 1236 before the next event, and the GTID subset answer for that new source
// output: assertions that a MySQL GTID backup continues on the new server without appending into the old segment, that a backup with no GTID stops, including when the new server answers 1236 for the old file name, and that a rotate back to an earlier file fails with STREAM_REGRESSION instead of opening a second copy
// pos: runner-level coverage for a VIP source switch
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"errors"
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

func TestPlanSourceSwitch(t *testing.T) {
	const oldID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const newID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	cases := []struct {
		name     string
		flavor   string
		ours     string
		captured bool
		probed   bool
		have     bool
		missing  string
		purged   bool
		want     switchAction
		text     string
	}{
		{name: "same", flavor: "mysql", ours: oldID + ":1-3", captured: true, probed: true, have: true, want: switchSame},
		{name: "adopt", flavor: "mysql", want: switchAdopt, text: "before any transaction was stored"},
		{name: "no gtid", flavor: "mysql", captured: true, want: switchStop, text: "no GTID set"},
		{name: "mariadb", flavor: "mariadb", ours: "0-1-10", captured: true, want: switchStop, text: "MariaDB"},
		{name: "missing", flavor: "mysql", ours: oldID + ":1-8", captured: true, probed: true, have: false, want: switchStop, text: "missing transactions"},
		{name: "purged", flavor: "mysql", ours: oldID + ":1-8", captured: true, probed: true, have: true, missing: newID + ":1-3", purged: true, want: switchStop, text: "purged"},
		{name: "continue", flavor: "mysql", ours: oldID + ":1-8", captured: true, probed: true, have: true, missing: newID + ":6-8", want: switchContinue, text: "continuing from the executed GTID set"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old, newI := oldID, newID
			if tc.want == switchSame {
				newI = oldID
			}
			got := planSourceSwitch(tc.flavor, old, newI, tc.ours, tc.captured, tc.probed, tc.have, tc.missing, tc.purged, "mysql-bin.000003", 1516)
			if got.action != tc.want {
				t.Fatalf("action %v message %s", got.action, got.message)
			}
			if tc.text != "" && !strings.Contains(got.message, tc.text) {
				t.Fatalf("message %q", got.message)
			}
			if tc.want == switchStop && (!strings.Contains(got.message, oldID) || !strings.Contains(got.message, newID) || !strings.Contains(got.message, "Start a new task")) {
				t.Fatalf("stop message %q", got.message)
			}
		})
	}
}

func TestDiskSourceNameKeepsOriginalUnprefixed(t *testing.T) {
	const oldID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const newID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	if got := diskSourceName(chainActivePrefix([]string{oldID}), "mysql-bin.000003"); got != "mysql-bin.000003" {
		t.Fatalf("original %s", got)
	}
	want := newID + ".mysql-bin.000003"
	if got := diskSourceName(chainActivePrefix([]string{oldID, newID}), "mysql-bin.000003"); got != want {
		t.Fatalf("switched %s", got)
	}
	if got := diskSourceName(newID, want); got != want {
		t.Fatalf("already prefixed %s", got)
	}
	if got := serverBinlogName(want, []string{oldID, newID}); got != "mysql-bin.000003" {
		t.Fatalf("strip %s", got)
	}
	// Failback: A, B, A. The third stint has its own prefix, and the names
	// A wrote on the first stint stay with the first stint.
	failback := []string{oldID, newID, oldID}
	stint := oldID + "~2.mysql-bin.000004"
	if got := diskSourceName(chainActivePrefix(failback), "mysql-bin.000004"); got != stint {
		t.Fatalf("failback %s", got)
	}
	if got := serverBinlogName(stint, failback); got != "mysql-bin.000004" {
		t.Fatalf("strip failback %s", got)
	}
	if nameBelongs("mysql-bin.000004", failback, chainActivePrefix(failback)) {
		t.Fatal("first-stint name claimed by the failback stint")
	}
	if !nameBelongs(stint, failback, chainActivePrefix(failback)) || nameBelongs(stint, failback, "") {
		t.Fatal("failback name ownership")
	}
	if got := appendIdentity([]string{oldID, newID}, oldID); len(got) != 3 {
		t.Fatalf("failback chain %v", got)
	}
	if got := appendIdentity(failback, oldID); len(got) != 3 {
		t.Fatalf("same server again %v", got)
	}
}

func TestRun_GTIDSwitchContinuesOnNewFile(t *testing.T) {
	const oldID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const newID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	dir := t.TempDir()
	fetcher := &fakeSourceMetaFetcher{uuidSeq: []string{oldID, newID}}
	fetcher.switchProbe = func(string) (bool, string, bool, error) {
		return true, newID + ":6-8", false, nil
	}
	syncer := &fakeSyncer{connID: 11}
	var names []string
	writes := 0
	streamer := &fakeStreamer{
		onCall: func(call int) {
			if call == 3 {
				syncer.connID = 22
			}
		},
		results: []streamResult{
			{event: rotateDumpEvent("mysql-bin.000003", 4)},
			{event: newRunnerEvent(200)},
			{event: newRunnerEvent(400)},
			{event: rotateDumpEvent("mysql-bin.000009", 4)},
			{err: context.Canceled},
		},
	}
	syncer.streamer = streamer
	var notices []tasks.SourceSwitchNotice
	runner := &MySQLRunner{
		dataDir: dir,
		fetcher: fetcher,
		newSyncer: func(goreplication.BinlogSyncerConfig) binlogSyncer {
			return syncer
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			names = append(names, fileName)
			buf := &fakeSyncFile{}
			w := binlog.NewWriter(buf, binlog.Checkpoint{File: fileName, Pos: initialPos})
			return &countingCloser{writes: &writes, buf: buf}, w, filepath.Join(dir, "task-1", fileName), nil
		},
		killDump: func(tasks.SourceConfig, uint32) error { return nil },
	}
	runner.BindSourceSwitch(func(_ string, notice tasks.SourceSwitchNotice) {
		notices = append(notices, notice)
	})
	task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: oldID + ":1-5"})
	if err := runner.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "mysql-bin.000003" || names[1] != newID+".mysql-bin.000009" {
		t.Fatalf("files %v", names)
	}
	if writes != 1 {
		t.Fatalf("writes %d, the new server event was appended", writes)
	}
	if syncer.startGTIDCalls != 2 {
		t.Fatalf("gtid dumps %d", syncer.startGTIDCalls)
	}
	if len(notices) != 1 || !notices[0].Continued || !strings.Contains(notices[0].Message, oldID) || !strings.Contains(notices[0].Message, newID) {
		t.Fatalf("notices %+v", notices)
	}
	body, err := os.ReadFile(filepath.Join(dir, "task-1", sourceChainFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), newID) || !strings.Contains(string(body), oldID) {
		t.Fatalf("chain %s", body)
	}
}

func TestRun_GTIDRedumpDoesNotCopyEarlierFile(t *testing.T) {
	dir := t.TempDir()
	const id = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	var names []string
	writes := 0
	streamer := &fakeStreamer{results: []streamResult{
		{event: rotateDumpEvent("mysql-bin.000003", 4)},
		{event: newRunnerEvent(200)},
		{event: rotateDumpEvent("mysql-bin.000004", 4)},
		{event: newRunnerEvent(120)},
		{event: rotateDumpEvent("mysql-bin.000002", 4)},
		{event: newRunnerEvent(900)},
		{event: rotateDumpEvent("mysql-bin.000004", 4)},
		{event: newRunnerEvent(400)},
		{err: context.Canceled},
	}}
	syncer := &fakeSyncer{connID: 11, streamer: streamer}
	runner := &MySQLRunner{
		dataDir:   dir,
		fetcher:   &fakeSourceMetaFetcher{serverUUID: id},
		newSyncer: func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer },
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			names = append(names, fileName)
			buf := &fakeSyncFile{}
			w := binlog.NewWriter(buf, binlog.Checkpoint{File: fileName, Pos: initialPos})
			return &countingCloser{writes: &writes, buf: buf}, w, filepath.Join(dir, "task-1", fileName), nil
		},
		killDump: func(tasks.SourceConfig, uint32) error { return nil },
	}
	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: id + ":1-3"}))
	var pe *tasks.PermanentError
	if !asPermanent(err, &pe) || pe.Code != tasks.CodeStreamRegression || !strings.Contains(pe.Message, "mysql-bin.000002") {
		t.Fatalf("err %v", err)
	}
	if len(names) != 2 || names[0] != "mysql-bin.000003" || names[1] != "mysql-bin.000004" {
		t.Fatalf("files %v", names)
	}
	// 200 on 000003 and 120 on 000004. The rotate back to 000002 stops the
	// task before the event at 900 or the later event at 400 is stored.
	if writes != 2 {
		t.Fatalf("writes %d", writes)
	}
}

func TestRun_LatestSwitchStopsWithoutMixing(t *testing.T) {
	const oldID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const newID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	dir := t.TempDir()
	fetcher := &fakeSourceMetaFetcher{
		status:  MasterStatus{File: "mysql-bin.000003", Pos: 100},
		uuidSeq: []string{oldID, newID},
	}
	syncer := &fakeSyncer{connID: 11}
	writes := 0
	streamer := &fakeStreamer{
		onCall: func(call int) {
			if call == 2 {
				syncer.connID = 22
			}
		},
		results: []streamResult{
			{event: newRunnerEvent(200)},
			{event: newRunnerEvent(400)},
		},
	}
	syncer.streamer = streamer
	var notices []tasks.SourceSwitchNotice
	runner := &MySQLRunner{
		dataDir:   dir,
		fetcher:   fetcher,
		newSyncer: func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer },
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			buf := &fakeSyncFile{}
			w := binlog.NewWriter(buf, binlog.Checkpoint{File: fileName, Pos: initialPos})
			return &countingCloser{writes: &writes, buf: buf}, w, filepath.Join(dir, "task-1", fileName), nil
		},
		killDump: func(tasks.SourceConfig, uint32) error { return nil },
	}
	runner.BindSourceSwitch(func(_ string, notice tasks.SourceSwitchNotice) {
		notices = append(notices, notice)
	})
	task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest})
	err := runner.Run(context.Background(), task)
	var pe *tasks.PermanentError
	if !asPermanent(err, &pe) || pe.Code != tasks.CodeSourceSwitchover {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(pe.Message, oldID) || !strings.Contains(pe.Message, newID) || !strings.Contains(pe.Message, "no GTID set") || !strings.Contains(pe.Message, "Start a new task") {
		t.Fatalf("message %s", pe.Message)
	}
	if writes != 1 {
		t.Fatalf("writes %d", writes)
	}
	if len(notices) != 1 || notices[0].Continued {
		t.Fatalf("notices %+v", notices)
	}
	if syncer.startGTIDCalls != 0 {
		t.Fatalf("latest switch started a gtid dump")
	}
}

func TestRun_Latest1236OnNewServerStops(t *testing.T) {
	const oldID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const newID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	dir := t.TempDir()
	fetcher := &fakeSourceMetaFetcher{
		status:  MasterStatus{File: "mysql-bin.000003", Pos: 100},
		uuidSeq: []string{oldID, newID},
	}
	purged := &gomysql.MyError{Code: 1236, State: "HY000", Message: "Could not find first log file name in binary log index file"}
	syncer := &fakeSyncer{connID: 11}
	writes := 0
	syncer.streamer = &fakeStreamer{results: []streamResult{
		{event: newRunnerEvent(200)},
		{err: purged},
	}}
	var notices []tasks.SourceSwitchNotice
	runner := &MySQLRunner{
		dataDir:   dir,
		fetcher:   fetcher,
		newSyncer: func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer },
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			buf := &fakeSyncFile{}
			w := binlog.NewWriter(buf, binlog.Checkpoint{File: fileName, Pos: initialPos})
			return &countingCloser{writes: &writes, buf: buf}, w, filepath.Join(dir, "task-1", fileName), nil
		},
		killDump: func(tasks.SourceConfig, uint32) error { return nil },
	}
	runner.BindSourceSwitch(func(_ string, notice tasks.SourceSwitchNotice) {
		notices = append(notices, notice)
	})
	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest}))
	var pe *tasks.PermanentError
	if !asPermanent(err, &pe) || pe.Code != tasks.CodeSourceSwitchover {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(strings.ToLower(pe.Message), "purged") {
		t.Fatalf("1236 on a new server was reported as a purged binlog: %s", pe.Message)
	}
	if !strings.Contains(pe.Message, oldID) || !strings.Contains(pe.Message, newID) || !strings.Contains(pe.Message, "no GTID set") || !strings.Contains(pe.Message, "Start a new task") {
		t.Fatalf("message %s", pe.Message)
	}
	if writes != 1 {
		t.Fatalf("writes %d", writes)
	}
	if len(notices) != 1 || notices[0].Continued {
		t.Fatalf("notices %+v", notices)
	}
	if syncer.startGTIDCalls != 0 {
		t.Fatalf("latest 1236 started a gtid dump")
	}
}

func TestRun_FilePos1236OnNewServerContinuesByGTID(t *testing.T) {
	const oldID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const newID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	dir := t.TempDir()
	ours := oldID + ":1-4"
	fetcher := &fakeSourceMetaFetcher{uuidSeq: []string{oldID, newID}}
	fetcher.switchProbe = func(string) (bool, string, bool, error) {
		return true, "", false, nil
	}
	purged := &gomysql.MyError{Code: 1236, State: "HY000", Message: "Could not find first log file name in binary log index file"}
	syncer := &fakeSyncer{connID: 11, streamer: &fakeStreamer{results: []streamResult{
		{err: purged},
		{err: context.Canceled},
	}}}
	var notices []tasks.SourceSwitchNotice
	runner := &MySQLRunner{
		dataDir: dir,
		fetcher: fetcher,
		checkpointStore: &fakeRunnerCheckpointStore{loadOK: true, loadCheckpoint: binlog.Checkpoint{
			File: "mysql-bin.000003", Pos: 100, GTIDSet: ours,
		}},
		newSyncer: func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer },
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			return &fakeCloser{}, binlog.NewWriter(&fakeSyncFile{}, binlog.Checkpoint{File: fileName, Pos: initialPos}), filepath.Join(dir, "task-1", fileName), nil
		},
		killDump: func(tasks.SourceConfig, uint32) error { return nil },
	}
	runner.BindSourceSwitch(func(_ string, notice tasks.SourceSwitchNotice) {
		notices = append(notices, notice)
	})
	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode: tasks.StartModeFilePos, File: "mysql-bin.000003", Pos: 100, GTIDSet: ours,
	}))
	if err != nil {
		t.Fatalf("err %v", err)
	}
	if len(notices) != 1 || !notices[0].Continued || !strings.Contains(notices[0].Message, oldID) || !strings.Contains(notices[0].Message, newID) {
		t.Fatalf("notices %+v", notices)
	}
	if syncer.startPosCalls != 1 || syncer.startGTIDCalls != 1 {
		t.Fatalf("pos=%d gtid=%d", syncer.startPosCalls, syncer.startGTIDCalls)
	}
	body, readErr := os.ReadFile(filepath.Join(dir, "task-1", sourceChainFile))
	if readErr != nil || !strings.Contains(string(body), oldID) || !strings.Contains(string(body), newID) {
		t.Fatalf("chain %s err %v", body, readErr)
	}
}

func TestRun_SwitchAtStartStopsWhenGTIDMissing(t *testing.T) {
	const oldID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const newID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "task-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeSourceChain(filepath.Join(dir, "task-1"), []string{oldID}); err != nil {
		t.Fatal(err)
	}
	fetcher := &fakeSourceMetaFetcher{serverUUID: newID}
	fetcher.switchProbe = func(string) (bool, string, bool, error) {
		return false, "", false, nil
	}
	syncer := &fakeSyncer{streamer: &fakeStreamer{results: []streamResult{{err: context.Canceled}}}}
	runner := &MySQLRunner{
		dataDir:         dir,
		fetcher:         fetcher,
		checkpointStore: &fakeRunnerCheckpointStore{loadOK: true, loadCheckpoint: binlog.Checkpoint{File: "mysql-bin.000003", Pos: 100, GTIDSet: oldID + ":1-4"}},
		newSyncer:       func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer },
		killDump:        func(tasks.SourceConfig, uint32) error { return nil },
	}
	task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: oldID + ":1-4"})
	err := runner.Run(context.Background(), task)
	var pe *tasks.PermanentError
	if !asPermanent(err, &pe) || pe.Code != tasks.CodeSourceSwitchover || !strings.Contains(pe.Message, "missing transactions") {
		t.Fatalf("err %v", err)
	}
	if syncer.startGTIDCalls != 0 || syncer.startPosCalls != 0 {
		t.Fatalf("dump started pos=%d gtid=%d", syncer.startPosCalls, syncer.startGTIDCalls)
	}
}

func TestRun_MariaDBSwitchStops(t *testing.T) {
	const oldID = "mariadb:101:0"
	const newID = "mariadb:102:0"
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "task-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeSourceChain(filepath.Join(dir, "task-1"), []string{oldID}); err != nil {
		t.Fatal(err)
	}
	fetcher := &fakeSourceMetaFetcher{serverUUID: newID}
	syncer := &fakeSyncer{streamer: &fakeStreamer{results: []streamResult{{err: context.Canceled}}}}
	runner := &MySQLRunner{
		dataDir:         dir,
		fetcher:         fetcher,
		checkpointStore: &fakeRunnerCheckpointStore{loadOK: true, loadCheckpoint: binlog.Checkpoint{File: "mysql-bin.000003", Pos: 100, GTIDSet: "0-1-10"}},
		newSyncer:       func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer },
		killDump:        func(tasks.SourceConfig, uint32) error { return nil },
	}
	task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: "0-1-10"})
	task.Source.Flavor = "mariadb"
	err := runner.Run(context.Background(), task)
	var pe *tasks.PermanentError
	if !asPermanent(err, &pe) || pe.Code != tasks.CodeSourceSwitchover || !strings.Contains(pe.Message, "MariaDB") || !strings.Contains(pe.Message, oldID) || !strings.Contains(pe.Message, newID) {
		t.Fatalf("err %v", err)
	}
}

type countingCloser struct {
	writes *int
	buf    *fakeSyncFile
}

func (c *countingCloser) Close() error {
	if c.buf != nil && c.writes != nil {
		*c.writes += len(c.buf.writes)
	}
	return nil
}

func rotateDumpEvent(name string, pos uint64) *goreplication.BinlogEvent {
	return &goreplication.BinlogEvent{
		Header: &goreplication.EventHeader{
			EventType: goreplication.ROTATE_EVENT,
			Timestamp: uint32(time.Now().Unix()),
		},
		Event:   &goreplication.RotateEvent{Position: pos, NextLogName: []byte(name)},
		RawData: []byte{0x01},
	}
}

func asPermanent(err error, pe **tasks.PermanentError) bool {
	if err == nil {
		return false
	}
	var got *tasks.PermanentError
	if !errors.As(err, &got) {
		return false
	}
	*pe = got
	return true
}
