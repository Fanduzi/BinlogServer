// Package replication provides module-level functionality for replication.
// input: a runner bound to a recording finding sink, a failing open-tail truncate and a dump read that times out
// output: proof that S4 (open tail cut failed) and S12 (dump stalled past the read timeout) reach the finding sink
// pos: fault-injection tests for ADR 0009 phase 0, dump and open tail
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/tasks"

	"github.com/go-mysql-org/go-mysql/packet"
)

// S4: the unfinished trailing transaction could not be cut.
func TestS4_OpenTailCutFailureIsFinding(t *testing.T) {
	dir := t.TempDir()
	seg := newDiskSegment()
	seg.txn(10)
	seg.gtid(sampleGTID, 11)
	seg.begin()
	seg.rows()
	seg.write(t, filepath.Join(dir, "task-4", "mysql-bin.000001.open.e1"))

	orig := truncateFile
	truncateFile = func(string, int64) error { return errors.New("EROFS") }
	defer func() { truncateFile = orig }()

	r := NewMySQLRunner(dir)
	rec := bindRecorder(r)
	r.reconcileOpenTail(tasks.Task{ID: "task-4"}, newExecutedGTID("mysql", sampleGTID+":1-9"))
	f, ok := rec.has("task-4", tasks.FindingOpenTailCutFailed, "mysql-bin.000001")
	if !ok {
		t.Fatalf("no OPEN_TAIL_CUT_FAILED: %v", rec.open)
	}
	if f.Severity != tasks.FindingCritical {
		t.Fatalf("severity %s, want CRITICAL", f.Severity)
	}
	// #338 QA P3: the detail carries the cut error and does not promise a resume.
	if !strings.Contains(f.Detail, "EROFS") || strings.Contains(f.Detail, "resuming inside it") {
		t.Fatalf("detail %q", f.Detail)
	}
}

// S12: the dump connection must time out instead of blocking forever.
func TestS12_DumpHasReadTimeout(t *testing.T) {
	cfg := buildSyncerConfig(tasks.Task{ID: "t"})
	if cfg.ReadTimeout != 45*time.Second {
		t.Fatalf("ReadTimeout %s, want 45s", cfg.ReadTimeout)
	}
	if cfg.ReadTimeout <= dumpHeartbeatPeriod {
		t.Fatalf("ReadTimeout %s must exceed heartbeat %s", cfg.ReadTimeout, dumpHeartbeatPeriod)
	}
}

// realReadTimeoutErr is the error go-mysql returns when the dump read hits
// ReadTimeout: packet.Conn wraps ErrBadConn and formats the net.Error into
// text, so errors.As(net.Error) no longer matches (#338 QA P1).
func realReadTimeoutErr(t *testing.T) error {
	t.Helper()
	client, server := net.Pipe()
	defer server.Close()
	defer client.Close()
	_, err := packet.NewConnWithTimeout(client, 20*time.Millisecond, 0, 0).ReadPacket()
	if err == nil {
		t.Fatal("silent pipe returned a packet")
	}
	var ne net.Error
	if errors.As(err, &ne) {
		t.Fatalf("go-mysql now keeps net.Error in the chain; revisit this test: %v", err)
	}
	return err
}

type stepClock struct{ at time.Time }

func (c *stepClock) now() time.Time { return c.at }

// S12: the dump read that fails after a silence as long as the read timeout is
// a DUMP_STALLED finding, whatever the error looks like; the next event resolves it.
func TestS12_DumpStallIsFinding(t *testing.T) {
	r := NewMySQLRunner(t.TempDir())
	clock := &stepClock{at: time.Unix(1_700_000_000, 0)}
	r.clock = clock.now
	rec := bindRecorder(r)
	stallErr := realReadTimeoutErr(t)
	st := &fakeStreamer{results: []streamResult{{event: xidAt(100)}, {err: stallErr}}}
	if _, err := r.nextEvent(context.Background(), st, "t12", tasks.SourceConfig{}, "mysql-bin.000003", 100, nil); err != nil {
		t.Fatal(err)
	}
	clock.at = clock.at.Add(dumpReadTimeout + time.Second)
	if _, err := r.nextEvent(context.Background(), st, "t12", tasks.SourceConfig{}, "mysql-bin.000003", 100, nil); err == nil {
		t.Fatal("stall must still return an error so the dump reconnects")
	}
	if _, ok := rec.has("t12", tasks.FindingDumpStalled, ""); !ok {
		t.Fatalf("no DUMP_STALLED for %q: %v", stallErr, rec.open)
	}
	// A new session (reconnect) that receives an event resolves it, also when
	// the finding was loaded from the store after a restart.
	next := &fakeStreamer{results: []streamResult{{event: xidAt(200)}}}
	if _, err := r.nextEvent(context.Background(), next, "t12", tasks.SourceConfig{}, "mysql-bin.000003", 100, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.has("t12", tasks.FindingDumpStalled, ""); ok {
		t.Fatal("DUMP_STALLED must resolve once events flow")
	}
}

// A connection that drops quickly (source restart, reset) is not a stall.
func TestS12_QuickDropIsNotStall(t *testing.T) {
	r := NewMySQLRunner(t.TempDir())
	clock := &stepClock{at: time.Unix(1_700_000_000, 0)}
	r.clock = clock.now
	rec := bindRecorder(r)
	st := &fakeStreamer{results: []streamResult{{event: xidAt(100)}, {err: realReadTimeoutErr(t)}}}
	_, _ = r.nextEvent(context.Background(), st, "q", tasks.SourceConfig{}, "f", 100, nil)
	clock.at = clock.at.Add(3 * time.Second)
	_, _ = r.nextEvent(context.Background(), st, "q", tasks.SourceConfig{}, "f", 100, nil)
	if _, ok := rec.has("q", tasks.FindingDumpStalled, ""); ok {
		t.Fatal("a 3s-old connection error is not a stall")
	}
}

type hangingSyncer struct {
	fakeSyncer
	release chan struct{}
}

func (h *hangingSyncer) Close() { <-h.release }

// #338 QA P2: Close of a dead dump must not hold the reconnect; go-mysql's
// Close dials the source to KILL the old thread and can wait for minutes.
func TestCloseDumpIsBounded(t *testing.T) {
	orig := dumpCloseTimeout
	dumpCloseTimeout = 50 * time.Millisecond
	defer func() { dumpCloseTimeout = orig }()
	h := &hangingSyncer{release: make(chan struct{})}
	defer close(h.release)
	r := NewMySQLRunner(t.TempDir())
	done := make(chan struct{})
	go func() {
		r.closeDump(tasks.Task{ID: "c"}, h)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("closeDump waited for a hanging syncer.Close")
	}
}

// #338 QA P2: our own KILL of a leftover dump thread is bounded too.
func TestKillDumpThreadIsBounded(t *testing.T) {
	origC, origIO := sourceConnectTimeout, sourceIOTimeout
	sourceConnectTimeout, sourceIOTimeout = 100*time.Millisecond, 100*time.Millisecond
	defer func() { sourceConnectTimeout, sourceIOTimeout = origC, origIO }()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // accept, never send a handshake
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	done := make(chan error, 1)
	go func() {
		done <- killMySQLDumpThread(tasks.SourceConfig{Host: "127.0.0.1", Port: uint16(port), User: "u"}, 42)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("kill against a silent server must fail")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("killMySQLDumpThread hung on a server that never answers")
	}
}
