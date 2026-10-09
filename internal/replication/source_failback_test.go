// Package replication provides module-level functionality for replication.
// input: a task directory whose chain is A then B, a VIP that now reaches A again after A caught up from B, and a later restart on A
// output: assertions that a failback to a server already in the chain records a new stint (A, B, A), writes files under a stint prefix that cannot reuse A's earlier names, that B's open segment from an older epoch is sealed under B before that stint opens, that a restart on that stint records no new switch, and that a stop mid-dump leaves the open segment open while a continued switch seals it
// pos: runner-level coverage for the A→B→A failback chain
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

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

func TestRun_FailbackToEarlierServerStartsNewStint(t *testing.T) {
	const aID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const bID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "task-1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeSourceChain(taskDir, []string{aID, bID}); err != nil {
		t.Fatal(err)
	}
	ours := aID + ":1-5," + bID + ":1-3"
	// B's last segment is still open: a stop on the way back to A leaves it
	// open. The failback must seal it under B before A's stint opens a new
	// epoch, which drops every other epoch's open file.
	bOpen := filepath.Join(taskDir, bID+".mysql-bin.000006.open.e1")
	bEvents, bEnd := chainBinlogEvents(4, time.Unix(1_700_000_000, 0).UTC(), []namedEvent{
		{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", "INSERT INTO t VALUES ('b-rows-after-the-switch')")},
	})
	writeBinlogSegment(t, bOpen, bEvents)
	run := func(stored binlog.Checkpoint, epoch int64) ([]string, []tasks.SourceSwitchNotice) {
		t.Helper()
		fetcher := &fakeSourceMetaFetcher{serverUUID: aID}
		fetcher.switchProbe = func(string) (bool, string, bool, error) { return true, "", false, nil }
		syncer := &fakeSyncer{connID: 31, streamer: &fakeStreamer{results: []streamResult{
			{event: rotateDumpEvent("mysql-bin.000004", 4)},
			{event: newRunnerEvent(300)},
			{err: context.Canceled},
		}}}
		var names []string
		writes := 0
		var notices []tasks.SourceSwitchNotice
		runner := &MySQLRunner{
			dataDir:         dir,
			fetcher:         fetcher,
			checkpointStore: &fakeRunnerCheckpointStore{loadOK: true, loadCheckpoint: stored},
			newSyncer:       func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer },
			writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
				names = append(names, fileName)
				buf := &fakeSyncFile{}
				w := binlog.NewWriter(buf, binlog.Checkpoint{File: fileName, Pos: initialPos})
				return &countingCloser{writes: &writes, buf: buf}, w, filepath.Join(taskDir, fileName), nil
			},
			killDump: func(tasks.SourceConfig, uint32) error { return nil },
		}
		runner.BindSourceSwitch(func(_ string, notice tasks.SourceSwitchNotice) { notices = append(notices, notice) })
		task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: aID + ":1-2"})
		task.Epoch = epoch
		if err := runner.Run(context.Background(), task); err != nil {
			t.Fatal(err)
		}
		return names, notices
	}

	names, notices := run(binlog.Checkpoint{File: bID + ".mysql-bin.000006", Pos: bEnd, GTIDSet: ours}, 3)
	stint := aID + "~2.mysql-bin.000004"
	if len(names) == 0 || names[0] != stint {
		t.Fatalf("failback files %v, want %s (A's earlier mysql-bin.000004 must not be reused)", names, stint)
	}
	if len(notices) != 1 || !notices[0].Continued {
		t.Fatalf("failback notices %+v", notices)
	}
	if !strings.Contains(notices[0].Detail, "file=mysql-bin.000006 ") || strings.Contains(notices[0].Detail, bID+".mysql-bin") {
		t.Fatalf("switch detail names the disk prefix: %s", notices[0].Detail)
	}
	if got := loadSourceChain(taskDir); strings.Join(got, ",") != aID+","+bID+","+aID {
		t.Fatalf("chain %v, want A,B,A", got)
	}
	if _, err := os.Stat(bOpen); !os.IsNotExist(err) {
		t.Fatalf("B's open segment was not sealed before the failback stint: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(taskDir, bID+".mysql-bin.000006")); err != nil || !strings.Contains(string(body), "b-rows-after-the-switch") {
		t.Fatalf("B's sealed segment: %v %q", err, body)
	}

	// Epoch 0: the fake writer keeps the stint bytes in memory, so there is no
	// file on disk for a takeover check to read.
	names, notices = run(binlog.Checkpoint{File: stint, Pos: 300, GTIDSet: ours}, 0)
	if len(notices) != 0 {
		t.Fatalf("restart on the same server recorded a switch: %+v", notices)
	}
	if len(names) == 0 || names[0] != stint {
		t.Fatalf("restart files %v", names)
	}
	if got := loadSourceChain(taskDir); len(got) != 3 {
		t.Fatalf("chain after restart %v", got)
	}
}

// A switch that stops mid-stream must not seal the open segment: the
// checkpoint still names it, and a sealed file there made every later Start
// fail with SEGMENT_NOT_ON_WORKER, so the task could neither re-check the
// switch nor resume when the address pointed back.
func TestApplySwitch_StopKeepsOpenSegment(t *testing.T) {
	const aID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const bID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	dir := t.TempDir()
	runner := &MySQLRunner{dataDir: dir, fetcher: &fakeSourceMetaFetcher{serverUUID: bID}}
	session := &sourceSession{dir: dir, original: aID, active: aID, chain: []string{aID}}
	sealed := 0
	seal := func(string) error { sealed++; return nil }
	task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest})
	err := runner.applySwitch(context.Background(), task, session, bID, "", "mysql-bin.000004", 197, seal)
	var pe *tasks.PermanentError
	if !asPermanent(err, &pe) || pe.Code != tasks.CodeSourceSwitchover {
		t.Fatalf("err %v", err)
	}
	if sealed != 0 {
		t.Fatalf("a stopped switch sealed the open segment %d times", sealed)
	}

	fetcher := &fakeSourceMetaFetcher{serverUUID: bID}
	fetcher.switchProbe = func(string) (bool, string, bool, error) { return true, "", false, nil }
	runner = &MySQLRunner{dataDir: dir, fetcher: fetcher}
	session = &sourceSession{dir: dir, original: aID, active: aID, chain: []string{aID}}
	if err := runner.applySwitch(context.Background(), task, session, bID, aID+":1-5", "mysql-bin.000004", 197, seal); err != errSwitchRestart {
		t.Fatalf("continue err %v", err)
	}
	if sealed != 1 {
		t.Fatalf("a continued switch sealed %d times, want 1", sealed)
	}
}
