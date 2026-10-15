// Package replication provides module-level functionality for replication.
// input: local TCP listeners that answer, black-hole or refuse, and a dump whose last consumed event is newer than its last socket read
// output: proof that the dump connection touches the liveness clock on every read with bytes, that ProbeSource needs the greeting (a black hole that accepts but never writes is a failure), and that a stall is judged from the socket clock, not from event consumption (#383 QA F1)
// pos: ADR 0009 P2-4 source liveness unit tests (CI runs them; liveness_e2e_test.go needs a real source)
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"net"
	"testing"
	"time"

	"binlog_server/internal/tasks"
)

func listen(t *testing.T, serve func(net.Conn)) (*net.TCPAddr, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr), func() { ln.Close() }
}

func TestProbeSourceNeedsTheGreeting(t *testing.T) {
	r := NewMySQLRunner(t.TempDir())
	greet, stopG := listen(t, func(c net.Conn) { c.Write([]byte{0x4a}); time.Sleep(time.Second); c.Close() })
	defer stopG()
	hole, stopH := listen(t, func(c net.Conn) { time.Sleep(2 * time.Second); c.Close() })
	defer stopH()
	refused, stopR := listen(t, func(c net.Conn) {})
	stopR()
	probe := func(a *net.TCPAddr) error {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		return r.ProbeSource(ctx, tasks.SourceConfig{Host: "127.0.0.1", Port: uint16(a.Port)})
	}
	if err := probe(greet); err != nil {
		t.Fatalf("answering source: %v", err)
	}
	start := time.Now()
	if err := probe(hole); err == nil {
		t.Fatal("a black hole that accepts and never writes passed the probe")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("probe ignored its deadline: %s", d)
	}
	if err := probe(refused); err == nil {
		t.Fatal("a refused connection passed the probe")
	}
}

func TestDumpConnectionTouchesTheClock(t *testing.T) {
	r := NewMySQLRunner(t.TempDir())
	clock := &tasks.SourceLiveness{}
	r.BindLiveness(func(string) *tasks.SourceLiveness { return clock })
	addr, stop := listen(t, func(c net.Conn) {
		for i := 0; i < 3; i++ {
			c.Write([]byte("hb"))
			time.Sleep(50 * time.Millisecond)
		}
		c.Close()
	})
	defer stop()
	conn, err := r.dumpDialer("t1")(context.Background(), "tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	buf := make([]byte, 8)
	if _, err := conn.Read(buf); err != nil {
		t.Fatal(err)
	}
	first := clock.LastRead()
	if first.IsZero() || r.socketLast("t1").IsZero() {
		t.Fatal("a read with bytes did not touch the clocks")
	}
	time.Sleep(60 * time.Millisecond)
	if _, err := conn.Read(buf); err != nil {
		t.Fatal(err)
	}
	if !clock.LastRead().After(first) {
		t.Fatal("a later read did not move the clock")
	}
}

// #383 QA F1: under load go-mysql has read far ahead, so the last consumed
// event is recent while the socket has been silent for the read timeout.
// The stall is judged from the socket.
func TestStallIsJudgedAtTheSocket(t *testing.T) {
	r := NewMySQLRunner(t.TempDir())
	rec := bindRecorder(r)
	st := &fakeStreamer{results: []streamResult{{err: context.DeadlineExceeded}}}
	sc := &socketClock{}
	sc.at.Store(time.Now().Add(-r.dumpReadTimeout() - time.Second).UnixNano())
	r.sockets.Store("t1", sc)
	mark := r.recvMark("t1", st)
	mark.touch(time.Now()) // an event was consumed just now, from the read-ahead buffer
	_, err := r.nextEvent(context.Background(), st, "t1", tasks.SourceConfig{}, "mysql-bin.000001", 4, nil)
	if err == nil || !tasks.IsSourceUnreachable(err) {
		t.Fatalf("socket silent for the read timeout: err %v, want a stall", err)
	}
	if _, ok := rec.has("t1", tasks.FindingDumpStalled, ""); !ok {
		t.Fatal("no DUMP_STALLED from the runner (legacy path)")
	}
}
