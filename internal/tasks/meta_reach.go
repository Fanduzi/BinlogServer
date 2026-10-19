// Package tasks provides module-level functionality for tasks.
// input: metadata store observations from three places (the bounded /api/health read, the evaluator's LivenessTick ping, and evaluator passes that fail on a timeout or connection error), the optional MetaProbe of the task store, and the evaluator's node id
// output: META_UNREACHABLE (S17, CRITICAL, task _cluster, subject the node id) through SignalFailure once failures have lasted meta_unreachable_after (default 10s) with FirstSeen at the first failure; the matching SignalSuccess on the next answer (for an evaluator-owned signal only after the evaluator has seen the OPEN row stored, so an outage always leaves an OPEN and a CLOSE); a bounded, single-flight read of the open findings for HealthSummary (healthReadBudget 1s from entry, the handler returns at the budget even when the driver ignores ctx)
// pos: ADR 0009 phase 2, docs/design/0009-phase2-health-evaluator.md §25 (#427)
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

// metaPingTimeout bounds one LivenessTick ping.
const metaPingTimeout = time.Second

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
	mu         sync.Mutex
	node       string
	after      time.Duration
	since      time.Time // first failure of the current streak; zero while reachable
	lastErr    string
	opened     bool // a failure was signalled for this streak
	recovering bool // answered again; the success is held until the OPEN is stored
	known      bool // a success has been signalled since start
	pinging    bool
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

// noteMetaReach records one observation: err nil is an answer; an error
// counts only when the probe classifies it as unreachable. countSuccess
// false records failures only (an evaluator pass that worked proves little
// about the pool the next caller gets, and the ping already counts).
func (s *Scheduler) noteMetaReach(err error, countSuccess bool) {
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
	r := &s.metaReach
	r.mu.Lock()
	subject := r.subjectLocked(s.clusterWorkerID)
	if err != nil {
		if r.since.IsZero() {
			r.since = now
		}
		r.recovering = false
		r.lastErr = err.Error()
		after := r.after
		if after <= 0 {
			after = DefaultMetaUnreachableAfter
		}
		if now.Sub(r.since) < after {
			r.mu.Unlock()
			return
		}
		r.opened = true
		f := Finding{TaskID: ClusterFindingTask, Kind: FindingMetaUnreachable, Subject: subject, Severity: FindingCritical, FirstSeen: r.since,
			Detail: fmt.Sprintf("this node has not reached the metadata store since %s: %s; findings and health are served from memory until it answers",
				r.since.UTC().Format(time.RFC3339), r.lastErr)}
		r.mu.Unlock()
		s.SignalFailure(f)
		return
	}
	r.since, r.lastErr = time.Time{}, ""
	switch {
	case r.opened && e.ownsSignal(ClusterFindingTask, FindingMetaUnreachable):
		// The evaluator could not store the OPEN while meta was away; it
		// is still failing in the signal book, so the next pass opens it,
		// and settleMetaReach signals the success once the OPEN is stored.
		r.opened, r.recovering = false, true
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
	if stored {
		r.recovering, r.known = false, true
	}
	r.mu.Unlock()
	if stored {
		e.Signal(ClusterFindingTask, FindingMetaUnreachable, subject, true, "")
		return
	}
	e.MarkDirty(ClusterFindingTask, ReasonSignal) // this pass stores the OPEN; look again after it
}

// probeMeta pings the metadata store once (LivenessTick). The ping runs on
// its own goroutine with its own timer, so a driver that ignores ctx cannot
// hold the evaluator loop; while one ping is still out, the next tick
// counts as a timeout instead of stacking another.
func (s *Scheduler) probeMeta() {
	if s == nil {
		return
	}
	p := s.metaProbe()
	if p == nil {
		return
	}
	r := &s.metaReach
	r.mu.Lock()
	if r.pinging {
		r.mu.Unlock()
		s.noteMetaReach(fmt.Errorf("metadata ping still waiting after %s: %w", metaPingTimeout, context.DeadlineExceeded), false)
		return
	}
	r.pinging = true
	r.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), metaPingTimeout)
		err := p.PingMeta(ctx)
		cancel()
		r.mu.Lock()
		r.pinging = false
		r.mu.Unlock()
		done <- err
	}()
	t := time.NewTimer(metaPingTimeout + 100*time.Millisecond)
	defer t.Stop()
	select {
	case err := <-done:
		s.noteMetaReach(err, true)
	case <-t.C:
		s.noteMetaReach(fmt.Errorf("metadata ping timed out after %s: %w", metaPingTimeout, context.DeadlineExceeded), false)
	}
}

// healthRead is one in-flight open-findings read shared by concurrent
// HealthSummary calls.
type healthRead struct {
	done chan struct{}
	open []Finding
	err  error
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
		rd = &healthRead{done: make(chan struct{})}
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
			s.noteMetaReach(rd.err, true)
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
		s.noteMetaReach(errHealthReadTimeout, false)
		return nil, errHealthReadTimeout, true
	case <-ctx.Done():
		return nil, ctx.Err(), true
	}
}

// memoryOpenFindings is what this process holds without reading a store:
// the legacy book's open findings, the evaluator's unsaved open sets, and
// FINDINGS_PERSIST_FAILED.
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
	return mergeFindings(nil, local, 1000)
}

// isHealthReadTimeout reports errHealthReadTimeout.
func isHealthReadTimeout(err error) bool { return errors.Is(err, errHealthReadTimeout) }
