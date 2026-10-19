// Package tasks provides module-level functionality for tasks.
// input: metadata store observations from four places (the bounded /api/health read, a ping every metaPingInterval (1s) on its own goroutine, the evaluator's bounded sweep calls (the _health lease, the expired-lease list), and evaluator passes that fail on a timeout or connection error), the optional MetaProbe of the task store, and the evaluator's node id
// output: META_UNREACHABLE (S17, CRITICAL, task _cluster, subject the node id) through SignalFailure once failures have lasted meta_unreachable_after (default 10s) from the first failure, dated when the failing ping or read was sent; a streak that reaches the threshold only at its first answer opens and closes at once, so any outage of meta_unreachable_after plus one ping interval is recorded (§25.8 R2); the matching SignalSuccess at the first answer (for an evaluator-owned signal only after the evaluator has seen the OPEN row stored, then dated at the first answer, so an outage always leaves an OPEN at its start and a CLOSE at its end); a bounded, single-flight read of the open findings for HealthSummary (healthReadBudget 1s from entry, the handler returns at the budget even when the driver ignores ctx)
// pos: ADR 0009 phase 2, docs/design/0009-phase2-health-evaluator.md §25 and §25.8 (#427, #430 QA r1)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"binlog_server/internal/health"
)

// DefaultMetaUnreachableAfter is how long meta failures last before
// META_UNREACHABLE opens, so one slow query does not open it.
const DefaultMetaUnreachableAfter = 10 * time.Second

// healthReadBudget bounds every metadata read of one HealthSummary.
const healthReadBudget = time.Second

// metaPingInterval is how often each node pings the metadata store
// (design §25.8 R1: an outage is seen within one interval of its start).
const metaPingInterval = time.Second

// metaPingTimeout bounds one ping.
const metaPingTimeout = time.Second

// sweepMetaTimeout bounds each metadata call of the evaluator's sweep (the
// _health lease, the expired-lease list, the _cluster open rows), so a
// hanging meta cannot hold the loop (§25.8 R1).
const sweepMetaTimeout = 2 * healthReadBudget

// MetaProbe is an optional TaskStore method set: a bounded ping, and a
// classifier that tells "the store did not answer" from an SQL error.
type MetaProbe interface {
	PingMeta(ctx context.Context) error
	MetaUnreachable(err error) bool
}

// errHealthReadTimeout is a /api/health read that did not answer in budget.
var errHealthReadTimeout = fmt.Errorf("findings store read did not answer within %s: %w", healthReadBudget, context.DeadlineExceeded)

// metaReach is this node's view of whether the metadata store answers.
type metaReach struct {
	mu          sync.Mutex
	node        string
	after       time.Duration
	since       time.Time // first failure of the current streak; zero while reachable
	lastErr     string
	opened      bool      // a failure was signalled for this streak
	recovering  bool      // answered again; the success is held until the OPEN is stored
	recoveredAt time.Time // the first answer of a held success (the CLOSE's fact time)
	known       bool      // a success has been signalled since start
	pinging     bool
	pingStart   time.Time // when the ping still out was sent
}

func (s *Scheduler) metaProbe() MetaProbe {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, _ := s.store.(MetaProbe)
	return p
}

// setMetaReachNode names the subject (the evaluator's node id).
func (s *Scheduler) setMetaReachNode(node string, after time.Duration) {
	r := &s.metaReach
	r.mu.Lock()
	defer r.mu.Unlock()
	if node != "" {
		r.node = node
	}
	if after > 0 {
		r.after = after
	}
}

func (r *metaReach) subjectLocked(fallback string) string {
	if r.node != "" {
		return r.node
	}
	if fallback != "" {
		return fallback
	}
	return "local"
}

// noteMetaReach records one observation made now.
func (s *Scheduler) noteMetaReach(err error, countSuccess bool) {
	s.noteMetaReachAt(err, countSuccess, time.Time{})
}

// noteMetaReachAt records one observation: err nil is an answer; an error
// counts only when the probe classifies it as unreachable. countSuccess
// false records failures only (an evaluator pass that worked proves little
// about the pool the next caller gets, and the ping already counts). at is
// when the observation started (a failed ping: when it was sent, so the
// streak starts at the outage, not one timeout later; design §25.8 R2);
// zero is now.
func (s *Scheduler) noteMetaReachAt(err error, countSuccess bool, at time.Time) {
	if s == nil {
		return
	}
	p := s.metaProbe()
	if p == nil {
		return // no metadata store: nothing to reach
	}
	if err != nil && !p.MetaUnreachable(err) {
		return
	}
	if err == nil && !countSuccess {
		return
	}
	e := s.healthEval.Load()
	now := time.Now()
	if e != nil {
		now = e.cfg.Now()
	}
	if at.IsZero() || at.After(now) {
		at = now
	}
	r := &s.metaReach
	r.mu.Lock()
	subject := r.subjectLocked(s.clusterWorkerID)
	after := r.after
	if after <= 0 {
		after = DefaultMetaUnreachableAfter
	}
	if err != nil {
		if r.since.IsZero() || at.Before(r.since) {
			r.since = at
		}
		r.recovering = false
		r.lastErr = err.Error()
		if now.Sub(r.since) < after {
			r.mu.Unlock()
			return
		}
		r.opened = true
		f := r.findingLocked(subject)
		r.mu.Unlock()
		s.SignalFailure(f)
		return
	}
	// The first answer after a streak. A streak that lasted
	// meta_unreachable_after from its first failure is an outage even when
	// no failure observation landed past the threshold (a 12s pause between
	// two pings, §25.8 R2): open it now at the first failure, close below.
	if !r.opened && !r.since.IsZero() && at.Sub(r.since) >= after {
		r.opened = true
		f := r.findingLocked(subject)
		r.mu.Unlock()
		s.SignalFailure(f)
		r.mu.Lock()
	}
	r.since, r.lastErr = time.Time{}, ""
	switch {
	case r.opened && e.ownsSignal(ClusterFindingTask, FindingMetaUnreachable):
		// The evaluator could not store the OPEN while meta was away; it
		// is still failing in the signal book, so the next pass opens it,
		// and settleMetaReach signals the success, at this answer, once
		// the OPEN is stored.
		r.opened, r.recovering, r.recoveredAt = false, true, at
		r.mu.Unlock()
		e.MarkDirty(ClusterFindingTask, ReasonSignal)
	case r.opened || !r.known:
		// The legacy book keeps the open row and its resolve in order;
		// !known closes a row a crashed process left open.
		r.opened, r.known = false, true
		r.mu.Unlock()
		s.SignalSuccess(ClusterFindingTask, FindingMetaUnreachable, subject)
	default:
		r.mu.Unlock()
	}
}

// findingLocked is the META_UNREACHABLE of the current streak.
func (r *metaReach) findingLocked(subject string) Finding {
	return Finding{TaskID: ClusterFindingTask, Kind: FindingMetaUnreachable, Subject: subject, Severity: FindingCritical, FirstSeen: r.since,
		Detail: fmt.Sprintf("this node has not reached the metadata store since %s: %s; findings and health are served from memory until it answers",
			r.since.UTC().Format(time.RFC3339), r.lastErr)}
}

// metaReachFinding is the open META_UNREACHABLE while the evaluator owns it
// and cannot store it (the store is the thing that does not answer), so
// /api/health and /api/findings list it from memory (§25 D2). The legacy
// book lists its own copy.
func (s *Scheduler) metaReachFinding() (Finding, bool) {
	e := s.healthEval.Load()
	if !e.ownsSignal(ClusterFindingTask, FindingMetaUnreachable) {
		return Finding{}, false
	}
	r := &s.metaReach
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.opened || r.since.IsZero() {
		return Finding{}, false
	}
	return Finding{TaskID: ClusterFindingTask, Kind: FindingMetaUnreachable, Subject: r.subjectLocked(s.clusterWorkerID), Severity: FindingCritical,
		FirstSeen: r.since, LastSeen: e.cfg.Now(), Count: 1,
		Detail: fmt.Sprintf("this node has not reached the metadata store since %s: %s", r.since.UTC().Format(time.RFC3339), r.lastErr)}, true
}

// settleMetaReach runs on each evaluated _cluster open set: once the OPEN
// of a recovered outage is stored, the held success is signalled and the
// next pass closes it.
func (e *Evaluator) settleMetaReach(open []health.Finding) {
	if e == nil || e.s == nil {
		return
	}
	r := &e.s.metaReach
	r.mu.Lock()
	if !r.recovering {
		r.mu.Unlock()
		return
	}
	subject := r.subjectLocked(e.s.clusterWorkerID)
	stored := false
	for _, f := range open {
		if f.Kind == health.MetaUnreachable && f.Subject == subject {
			stored = true
		}
	}
	recoveredAt := r.recoveredAt
	if stored {
		r.recovering, r.known, r.recoveredAt = false, true, time.Time{}
	}
	r.mu.Unlock()
	if stored {
		// The CLOSE's fact time is the first answer, not this pass (§25.8 R3).
		e.signalAt(ClusterFindingTask, FindingMetaUnreachable, subject, true, "", recoveredAt)
		return
	}
	e.MarkDirty(ClusterFindingTask, ReasonSignal) // this pass stores the OPEN; look again after it
}

// runMetaProbe pings the metadata store every interval until ctx ends. It
// is its own goroutine, not a branch of the evaluator loop: a metadata
// call that hangs there (a lease renew against a SIGSTOPped meta) must not
// stop this node from observing the outage (design §25.8 R1).
func (s *Scheduler) runMetaProbe(ctx context.Context, interval, timeout time.Duration) {
	if s == nil || s.metaProbe() == nil {
		return
	}
	if interval <= 0 {
		interval = metaPingInterval
	}
	if timeout <= 0 {
		timeout = metaPingTimeout
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.probeMeta(timeout)
		}
	}
}

// probeMeta pings the metadata store once. The ping runs on its own
// goroutine with its own timer, so a driver that ignores ctx cannot hold
// the caller; while one ping is still out, the next tick counts as a
// timeout instead of stacking another. A failure is dated when its ping
// was sent, an answer when it arrived.
func (s *Scheduler) probeMeta(timeout time.Duration) {
	if s == nil {
		return
	}
	p := s.metaProbe()
	if p == nil {
		return
	}
	if timeout <= 0 {
		timeout = metaPingTimeout
	}
	clock := time.Now
	if e := s.healthEval.Load(); e != nil {
		clock = e.cfg.Now
	}
	r := &s.metaReach
	r.mu.Lock()
	if r.pinging {
		sent := r.pingStart
		r.mu.Unlock()
		s.noteMetaReachAt(fmt.Errorf("metadata ping still waiting after %s: %w", timeout, context.DeadlineExceeded), false, sent)
		return
	}
	start := clock()
	r.pinging, r.pingStart = true, start
	r.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := p.PingMeta(ctx)
		cancel()
		r.mu.Lock()
		r.pinging = false
		r.mu.Unlock()
		done <- err
	}()
	t := time.NewTimer(timeout + timeout/10)
	defer t.Stop()
	select {
	case err := <-done:
		if err != nil {
			s.noteMetaReachAt(err, true, start)
			return
		}
		s.noteMetaReachAt(nil, true, clock())
	case <-t.C:
		s.noteMetaReachAt(fmt.Errorf("metadata ping timed out after %s: %w", timeout, context.DeadlineExceeded), false, start)
	}
}

// healthRead is one in-flight open-findings read shared by concurrent
// HealthSummary calls.
type healthRead struct {
	done  chan struct{}
	start time.Time
	open  []Finding
	err   error
}

// boundedOpenFindings reads the open findings within healthReadBudget from
// entry. At most one read is in flight; callers that arrive meanwhile wait
// on it. timedOut reports that the budget ran out first.
func (s *Scheduler) boundedOpenFindings(ctx context.Context) (open []Finding, err error, timedOut bool) {
	t := time.NewTimer(healthReadBudget)
	defer t.Stop()
	s.healthReadMu.Lock()
	rd := s.healthReadCur
	if rd == nil {
		rd = &healthRead{done: make(chan struct{}), start: time.Now()}
		if e := s.healthEval.Load(); e != nil {
			rd.start = e.cfg.Now()
		}
		s.healthReadCur = rd
		go func() {
			rctx, cancel := context.WithTimeout(context.Background(), healthReadBudget)
			rd.open, rd.err = s.OpenFindings(rctx, 1000)
			cancel()
			s.healthReadMu.Lock()
			if s.healthReadCur == rd {
				s.healthReadCur = nil
			}
			s.healthReadMu.Unlock()
			close(rd.done)
			if rd.err != nil {
				s.noteMetaReachAt(rd.err, true, rd.start) // dated when the read was sent
				return
			}
			s.noteMetaReach(nil, true)
		}()
	}
	s.healthReadMu.Unlock()
	select {
	case <-rd.done:
		if errors.Is(rd.err, context.DeadlineExceeded) {
			// The read's own budget ran out first: same outcome as our timer.
			return nil, errHealthReadTimeout, true
		}
		return rd.open, rd.err, false
	case <-t.C:
		s.noteMetaReachAt(errHealthReadTimeout, false, rd.start)
		return nil, errHealthReadTimeout, true
	case <-ctx.Done():
		return nil, ctx.Err(), true
	}
}

// memoryOpenFindings is what this process holds without reading a store:
// the legacy book's open findings, the evaluator's unsaved open sets,
// FINDINGS_PERSIST_FAILED, and an open META_UNREACHABLE.
func (s *Scheduler) memoryOpenFindings() []Finding {
	b := s.findings
	b.mu.Lock()
	local := make([]Finding, 0, len(b.open))
	for _, f := range b.open {
		local = append(local, f)
	}
	b.mu.Unlock()
	if e := s.healthEval.Load(); e != nil && e.cfg.Mode == EvaluatorOn {
		e.mu.Lock()
		for _, u := range e.unsaved {
			for _, f := range u.open {
				local = append(local, Finding{TaskID: f.TaskID, Kind: FindingKind(f.Kind), Subject: f.Subject, Severity: FindingSeverity(f.Severity),
					Detail: f.Detail, FirstSeen: f.FirstSeen, LastSeen: f.LastSeen, Count: 1})
			}
		}
		e.mu.Unlock()
	}
	if f, ok := s.persistFailureFindingFrom(false); ok {
		local = append(local, f)
	}
	if f, ok := s.metaReachFinding(); ok {
		local = append(local, f)
	}
	return mergeFindings(nil, local, 1000)
}

// isHealthReadTimeout reports errHealthReadTimeout.
func isHealthReadTimeout(err error) bool { return errors.Is(err, errHealthReadTimeout) }
