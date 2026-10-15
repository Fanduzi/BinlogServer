// Package tasks provides module-level functionality for tasks.
// input: none yet (red-commit stub)
// output: the source liveness API surface the #383 tests compile against; no behaviour
// pos: ADR 0009 phase 2 P2-4 (docs/design/0009-phase2-health-evaluator.md §18, #383 QA)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"sync/atomic"
	"time"
)

// Production liveness periods: a silent task's source is probed every
// DefaultProbeEvery, each probe bounded by DefaultProbeTimeout.
const (
	DefaultProbeEvery   = 10 * time.Second
	DefaultProbeTimeout = 5 * time.Second
)

// SourceLiveness is a task's hot clock: the last byte read from its source.
// Touch is lock-free and is called on every socket read that returned data.
type SourceLiveness struct {
	lastRead atomic.Int64 // unix nanos
}

// Touch records that a byte arrived from the source now.
func (l *SourceLiveness) Touch() { l.touchAt(time.Now()) }

func (l *SourceLiveness) touchAt(at time.Time) {
	n := at.UnixNano()
	for {
		old := l.lastRead.Load()
		if old >= n || l.lastRead.CompareAndSwap(old, n) {
			return
		}
	}
}

// LastRead is the last Touch (zero: none).
func (l *SourceLiveness) LastRead() time.Time {
	if n := l.lastRead.Load(); n > 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

// LivenessSnapshot is the clock as the evaluator reads it. LastRead is the
// last byte read, or since when the clock runs if nothing was read yet.
type LivenessSnapshot struct {
	LastRead, LastFailure time.Time
	FailureDetail         string
}

// SourceProber connects to a task's source and reads its first byte (the
// server greeting); the runner implements it.
type SourceProber interface {
	ProbeSource(ctx context.Context, source SourceConfig) error
}

// LivenessBinder is implemented by a runner that touches the clock on
// every socket read of a task's dump.
type LivenessBinder interface {
	BindLiveness(clock func(taskID string) *SourceLiveness)
}

// WithSourceLiveness sets the probe period and timeout (stub).
func WithSourceLiveness(probeEvery, probeTimeout time.Duration) Option {
	return func(*Scheduler) {}
}

// SourceLiveness returns the task's clock (stub: a fresh one).
func (s *Scheduler) SourceLiveness(taskID string) *SourceLiveness { return &SourceLiveness{} }

// LivenessSnapshot returns the task's clock (stub: none).
func (s *Scheduler) LivenessSnapshot(taskID string) (LivenessSnapshot, bool) {
	return LivenessSnapshot{}, false
}

func (s *Scheduler) noteSourceRead(taskID string, at time.Time)               {}
func (s *Scheduler) noteSourceFailure(taskID string, at time.Time, err error) {}

// StartSourceLiveness starts the probe loop (stub).
func (s *Scheduler) StartSourceLiveness(ctx context.Context)  {}
func (s *Scheduler) noteRunnerError(taskID string, err error) {}
