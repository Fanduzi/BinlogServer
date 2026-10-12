// Package replication provides module-level functionality for replication.
// input: run phases built directly (no source, no dump): a runLoop with a fake writer opener and an executed GTID set
// output: assertions that a cancelled stream ends the run with nil and any other error is returned, that a reconnect resumes from the open segment's flushed file/pos (refreshing the 1236 fallback from the executed set), then the executed GTID set, then the last cursor, then the planned start; that the dump's synthetic rotate only moves the cursor forward, the first rotate of a GTID dump opens the named file without writing it, a rotate behind the open file is STREAM_REGRESSION, and a format description before any file is held
// pos: interface tests of MySQLRunner.run's phases (ADR 0008)
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"

	"github.com/go-mysql-org/go-mysql/replication"
)

const phaseUUID = "3e11fa47-71ca-11e1-9e33-c80aa9429562"

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

func phaseLoop(t *testing.T, start tasks.StartConfig, seed string) (*runLoop, *[]string) {
	t.Helper()
	opened := &[]string{}
	session := &sourceSession{}
	executed := newExecutedGTID("mysql", seed)
	plan := &runStart{start: start, flavor: "mysql", executed: executed, trackGTID: executed.current() != "", session: session}
	l := &runLoop{r: &MySQLRunner{}, ctx: context.Background(), task: tasks.Task{ID: "1"}, runStart: plan, dump: &dumpConn{}}
	l.seg = &openSegment{r: l.r, ctx: l.ctx, task: l.task, session: session, executed: executed,
		opener: func(_ tasks.Task, name string, _ uint32) (io.Closer, *binlog.Writer, string, error) {
			*opened = append(*opened, name)
			return nopCloser{}, nil, t.TempDir() + "/" + name, nil
		}}
	return l, opened
}

func rotateEvent(next string, pos uint64, logPos uint32) *replication.BinlogEvent {
	return &replication.BinlogEvent{
		Header: &replication.EventHeader{EventType: replication.ROTATE_EVENT, LogPos: logPos},
		Event:  &replication.RotateEvent{Position: pos, NextLogName: []byte(next)},
	}
}

func TestStreamEnded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	boom := errors.New("boom")
	if got := streamEnded(ctx, boom); got != boom {
		t.Fatalf("live run: %v", got)
	}
	if got := streamEnded(ctx, context.Canceled); got != nil {
		t.Fatalf("canceled error: %v", got)
	}
	cancel()
	if got := streamEnded(ctx, boom); got != nil {
		t.Fatalf("cancelled run: %v", got)
	}
}

func TestDurableDumpStart(t *testing.T) {
	gtidStart := tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: phaseUUID + ":1-3"}
	l, _ := phaseLoop(t, gtidStart, phaseUUID+":1-7")
	l.gtidFallback = phaseUUID + ":1-3"
	if got := l.durableDumpStart(); got.Mode != tasks.StartModeGTID || got.GTIDSet != phaseUUID+":1-7" {
		t.Fatalf("no open file, GTID mode: %+v", got)
	}
	if l.gtidFallback != phaseUUID+":1-7" {
		t.Fatalf("fallback not refreshed: %q", l.gtidFallback)
	}
	l.seg.file, l.seg.name, l.seg.pos = nopCloser{}, "mysql-bin.000002", 900
	if got := l.durableDumpStart(); got.Mode != tasks.StartModeFilePos || got.File != "mysql-bin.000002" || got.Pos != 900 {
		t.Fatalf("open file: %+v", got)
	}

	fp := tasks.StartConfig{Mode: tasks.StartModeFilePos, File: "mysql-bin.000001", Pos: 4}
	l, _ = phaseLoop(t, fp, "")
	if got := l.durableDumpStart(); got != fp {
		t.Fatalf("nothing flushed: %+v", got)
	}
	l.seg.name, l.seg.pos = "mysql-bin.000003", 120
	if got := l.durableDumpStart(); got.File != "mysql-bin.000003" || got.Pos != 120 {
		t.Fatalf("closed cursor: %+v", got)
	}
}

func TestHandleRotate(t *testing.T) {
	l, opened := phaseLoop(t, tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: phaseUUID + ":1"}, phaseUUID+":1")
	fde := &replication.BinlogEvent{Header: &replication.EventHeader{EventType: replication.FORMAT_DESCRIPTION_EVENT}, RawData: []byte{1, 2, 3}}
	if err := l.handleEvent(fde); err != nil || len(l.seg.pendingFormat) != 3 || len(*opened) != 0 {
		t.Fatalf("held format: err %v pending %d opened %v", err, len(l.seg.pendingFormat), *opened)
	}
	if err := l.handleEvent(rotateEvent("mysql-bin.000004", 4, 0)); err != nil {
		t.Fatal(err)
	}
	if strings.Join(*opened, ",") != "mysql-bin.000004" || l.seg.name != "mysql-bin.000004" || l.seg.pos != 4 || l.seg.startPos != 4 {
		t.Fatalf("first rotate: opened %v seg %s:%d", *opened, l.seg.name, l.seg.pos)
	}
	l.seg.pos = 500
	if err := l.handleEvent(rotateEvent("mysql-bin.000004", 4, 0)); err != nil || l.seg.pos != 500 {
		t.Fatalf("synthetic rotate moved the cursor back: %d %v", l.seg.pos, err)
	}
	if err := l.handleEvent(rotateEvent("mysql-bin.000004", 800, 0)); err != nil || l.seg.pos != 800 {
		t.Fatalf("synthetic rotate forward: %d %v", l.seg.pos, err)
	}
	err := l.handleEvent(rotateEvent("mysql-bin.000003", 4, 900))
	if err == nil || !strings.Contains(err.Error(), "is behind open file mysql-bin.000004") {
		t.Fatalf("rotate behind: %v", err)
	}
	err = l.handleEvent(rotateEvent("mysql-bin.000004", 4, 900))
	if err == nil || !strings.Contains(err.Error(), "repeats open file") {
		t.Fatalf("rotate repeat: %v", err)
	}
}
