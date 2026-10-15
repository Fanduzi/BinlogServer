// Package replication provides module-level functionality for replication.
// input: none yet (red-commit stub)
// output: the socket liveness API surface the #383 tests compile against; no behaviour
// pos: ADR 0009 phase 2 P2-4 (docs/design/0009-phase2-health-evaluator.md §18, #383 QA)
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"net"
	"sync/atomic"
	"time"

	"binlog_server/internal/tasks"
)

// WithDumpTimeouts sets the dump heartbeat and read timeout (stub).
func WithDumpTimeouts(heartbeat, readTimeout time.Duration) RunnerOption {
	return func(*MySQLRunner) {}
}

// BindLiveness binds the per-task liveness clock (stub).
func (r *MySQLRunner) BindLiveness(clock func(taskID string) *tasks.SourceLiveness) {}

type socketClock struct{ at atomic.Int64 }

func (c *socketClock) touch()          {}
func (c *socketClock) last() time.Time { return time.Time{} }

func (r *MySQLRunner) dumpDialer(taskID string) func(ctx context.Context, network, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext
}

func (r *MySQLRunner) socketLast(taskID string) time.Time { return time.Time{} }

// ProbeSource probes the source (stub).
func (r *MySQLRunner) ProbeSource(ctx context.Context, source tasks.SourceConfig) error { return nil }

func (r *MySQLRunner) dumpReadTimeout() time.Duration { return dumpReadTimeout }
