// Package tasks provides module-level functionality for tasks.
// input: bytes read from the source at the socket (the runner's dump connection, through SourceLiveness.Touch), failed connection attempts (runner SOURCE_UNREACHABLE errors and probe failures), and a SourceProber the runner provides
// output: the one source liveness clock per task (last byte read, latest failed attempt and why, since when the clock runs) that DUMP_STALLED and SOURCE_UNREACHABLE are derived from; a probe loop that, for every task with desired RUN this node drives and no read for a probe period, connects to the source and reads its greeting (bounded) whatever the task state, so the clock keeps running through STARTING and RETRY_BACKOFF
// pos: ADR 0009 phase 2 P2-4 (docs/design/0009-phase2-health-evaluator.md §18, #383 QA)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"binlog_server/internal/health"
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

type livenessEntry struct {
	clock       SourceLiveness
	since       time.Time
	lastFailure time.Time
	failure     string
	probing     bool
}

type livenessBook struct {
	mu           sync.Mutex
	m            map[string]*livenessEntry
	probeEvery   time.Duration
	probeTimeout time.Duration
	prober       SourceProber
	now          func() time.Time
}

func newLivenessBook() *livenessBook {
	return &livenessBook{m: map[string]*livenessEntry{}, probeEvery: DefaultProbeEvery, probeTimeout: DefaultProbeTimeout, now: time.Now}
}

// WithSourceLiveness sets the probe period and timeout (production 10s, 5s).
func WithSourceLiveness(probeEvery, probeTimeout time.Duration) Option {
	return func(s *Scheduler) {
		if probeEvery > 0 {
			s.liveness.probeEvery = probeEvery
		}
		if probeTimeout > 0 {
			s.liveness.probeTimeout = probeTimeout
		}
	}
}

func (b *livenessBook) entry(taskID string) *livenessEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.m[taskID]
	if e == nil {
		e = &livenessEntry{since: b.now()}
		b.m[taskID] = e
	}
	return e
}

// SourceLiveness is the task's clock, created on first use (the clock then
// runs from now). The runner touches it from its socket reads.
func (s *Scheduler) SourceLiveness(taskID string) *SourceLiveness {
	return &s.liveness.entry(strings.TrimSpace(taskID)).clock
}

// LivenessSnapshot reads the task's clock; false when it has none.
func (s *Scheduler) LivenessSnapshot(taskID string) (LivenessSnapshot, bool) {
	b := s.liveness
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.m[taskID]
	if e == nil {
		return LivenessSnapshot{}, false
	}
	read := e.clock.LastRead()
	if read.Before(e.since) {
		read = e.since
	}
	return LivenessSnapshot{LastRead: read, LastFailure: e.lastFailure, FailureDetail: e.failure}, true
}

// forgetLiveness drops the clock: the task is not meant to run, or it was
// just started and its silence counts from now.
func (s *Scheduler) forgetLiveness(taskID string) {
	s.liveness.mu.Lock()
	delete(s.liveness.m, taskID)
	s.liveness.mu.Unlock()
}

func (s *Scheduler) noteSourceRead(taskID string, at time.Time) {
	s.liveness.entry(taskID).clock.touchAt(at)
}

func (s *Scheduler) noteSourceFailure(taskID string, at time.Time, err error) {
	e := s.liveness.entry(taskID)
	s.liveness.mu.Lock()
	if at.After(e.lastFailure) {
		e.lastFailure = at
		if err != nil {
			e.failure = err.Error()
		}
	}
	s.liveness.mu.Unlock()
}

// noteRunnerError: a SOURCE_UNREACHABLE run error is a failed attempt.
func (s *Scheduler) noteRunnerError(taskID string, err error) {
	if err != nil && IsSourceUnreachable(err) {
		s.noteSourceFailure(taskID, s.liveness.now(), err)
	}
}

// StartSourceLiveness runs the probe loop until ctx ends.
func (s *Scheduler) StartSourceLiveness(ctx context.Context) {
	go s.RunSourceLiveness(ctx)
}

// RunSourceLiveness is the probe loop; it returns when ctx ends.
func (s *Scheduler) RunSourceLiveness(ctx context.Context) {
	t := time.NewTicker(s.liveness.probeEvery / 2)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.probeSilentSources(ctx)
		}
	}
}

// probeSilentSources probes, at most once at a time per task, every task
// with desired RUN this node drives whose source sent nothing for a probe
// period.
func (s *Scheduler) probeSilentSources(ctx context.Context) {
	b := s.liveness
	b.mu.Lock()
	prober := b.prober
	b.mu.Unlock()
	if prober == nil {
		return
	}
	now := b.now()
	s.mu.Lock()
	var due []Task
	for id, task := range s.tasks {
		if task.DesiredRun != TaskDesiredRun {
			continue
		}
		if s.leaseManager != nil && task.OwnerWorkerID != s.clusterWorkerID {
			continue
		}
		due = append(due, s.tasks[id])
	}
	s.mu.Unlock()
	for _, task := range due {
		e := b.entry(task.ID)
		b.mu.Lock()
		read := e.clock.LastRead()
		if read.Before(e.since) {
			read = e.since
		}
		if e.probing || now.Sub(read) < b.probeEvery {
			b.mu.Unlock()
			continue
		}
		e.probing = true
		b.mu.Unlock()
		go func(task Task, e *livenessEntry) {
			pctx, cancel := context.WithTimeout(ctx, b.probeTimeout)
			err := prober.ProbeSource(pctx, task.Source)
			cancel()
			if ctx.Err() == nil {
				if err == nil {
					e.clock.touchAt(b.now())
				} else {
					s.noteSourceFailure(task.ID, b.now(), err)
				}
			}
			b.mu.Lock()
			e.probing = false
			b.mu.Unlock()
		}(task, e)
	}
}

func (s *Scheduler) hasRunner() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runner != nil
}

// livenessPhase names where a clock stands against the thresholds; the
// evaluator re-evaluates a task when it changes, so a finding follows the
// clock within one LivenessTick, not one sweep.
func livenessPhase(snap LivenessSnapshot, now time.Time, stalledAfter, unreachableAfter time.Duration) string {
	switch {
	case snap.LastFailure.After(snap.LastRead) && snap.LastFailure.Sub(snap.LastRead) >= unreachableAfter:
		return "unreachable"
	case now.Sub(snap.LastRead) >= stalledAfter:
		return "stalled"
	}
	return "live"
}

// markLivenessCrossings marks dirty every task whose liveness phase changed
// since the last tick.
func (e *Evaluator) markLivenessCrossings() {
	if e.s == nil {
		return
	}
	now := e.cfg.Now()
	stalledAfter, unreachableAfter := e.cfg.DumpStalledAfter, e.cfg.UnreachableAfter
	if stalledAfter <= 0 {
		stalledAfter = health.DefaultDumpStalledAfter
	}
	noProgressAfter := e.cfg.NoProgressAfter
	if noProgressAfter <= 0 {
		noProgressAfter = health.DefaultNoProgressAfter
	}
	for _, t := range e.responsible() {
		snap, ok := e.s.LivenessSnapshot(t.ID)
		if !ok {
			continue
		}
		ua := unreachableAfter
		if d, ok := e.cfg.UnreachableAfterTask[t.ID]; ok && d > 0 {
			ua = d
		}
		if ua <= 0 {
			ua = health.DefaultUnreachableAfter
		}
		ph := livenessPhase(snap, now, stalledAfter, ua)
		if idle, ok := e.s.runProgressIdle(t.ID, now); ok && idle >= noProgressAfter {
			ph += "+no-progress" // TASK_NO_PROGRESS opens within one tick (§24.9)
		}
		e.mu.Lock()
		if e.phase == nil {
			e.phase = map[string]string{}
		}
		changed := e.phase[t.ID] != ph
		e.phase[t.ID] = ph
		e.mu.Unlock()
		if changed {
			e.MarkDirty(t.ID, ReasonLiveness)
		}
	}
}
