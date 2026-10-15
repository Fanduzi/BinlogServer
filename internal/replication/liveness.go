// Package replication provides module-level functionality for replication.
// input: the scheduler's per-task source liveness clock (BindLiveness) and a task's source address
// output: a dump Dialer whose connection touches the task's clock, and this session's socket clock, on every read that returned bytes (events and heartbeats, measured at the socket, not at event consumption); ProbeSource, which connects to the source and reads its greeting within the caller's deadline
// pos: ADR 0009 P2-4 source liveness (docs/design/0009-phase2-health-evaluator.md §18, #383 QA)
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"binlog_server/internal/tasks"
)

// BindLiveness connects the runner to the scheduler's liveness clocks.
func (r *MySQLRunner) BindLiveness(clock func(taskID string) *tasks.SourceLiveness) {
	r.livenessMu.Lock()
	r.liveness = clock
	r.livenessMu.Unlock()
}

func (r *MySQLRunner) livenessClock(taskID string) *tasks.SourceLiveness {
	r.livenessMu.Lock()
	clock := r.liveness
	r.livenessMu.Unlock()
	if clock == nil {
		return nil
	}
	return clock(taskID)
}

// socketClock is the last read of the task's current dump connection.
type socketClock struct{ at atomic.Int64 }

func (c *socketClock) touch() { c.at.Store(time.Now().UnixNano()) }

func (c *socketClock) last() time.Time {
	if n := c.at.Load(); n > 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

// livenessConn touches the clocks on every read that returned bytes.
type livenessConn struct {
	net.Conn
	clock  *tasks.SourceLiveness
	socket *socketClock
}

func (c *livenessConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.socket.touch()
		if c.clock != nil {
			c.clock.Touch()
		}
	}
	return n, err
}

// dumpDialer dials the source for a dump of taskID; every connection is a
// new session with its own socket clock.
func (r *MySQLRunner) dumpDialer(taskID string) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		var d net.Dialer
		conn, err := d.DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		sc := &socketClock{}
		r.sockets.Store(taskID, sc)
		return &livenessConn{Conn: conn, clock: r.livenessClock(taskID), socket: sc}, nil
	}
}

// socketLast is the last byte read on the task's current dump connection
// (zero: no dialed connection, as with a fake syncer in tests).
func (r *MySQLRunner) socketLast(taskID string) time.Time {
	if v, ok := r.sockets.Load(taskID); ok {
		return v.(*socketClock).last()
	}
	return time.Time{}
}

// ProbeSource connects to the source and reads the first byte of its
// greeting within ctx's deadline (the scheduler bounds it). A black-holed
// source accepts the connection and sends nothing: that is a failure.
func (r *MySQLRunner) ProbeSource(ctx context.Context, source tasks.SourceConfig) error {
	addr := net.JoinHostPort(source.Host, fmt.Sprint(source.Port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("probe %s: %w", addr, err)
	}
	defer conn.Close()
	dl, ok := ctx.Deadline()
	if !ok {
		dl = time.Now().Add(sourceConnectTimeout)
	}
	if err := conn.SetReadDeadline(dl); err != nil {
		return fmt.Errorf("probe %s: %w", addr, err)
	}
	var b [1]byte
	if _, err := conn.Read(b[:]); err != nil {
		return fmt.Errorf("probe %s: no greeting: %w", addr, err)
	}
	return nil
}
