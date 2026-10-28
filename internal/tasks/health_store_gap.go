// Package tasks provides module-level functionality for tasks.
// input: health-store reads and writes that return healthstore.ErrSchemaAbsent, and the seal-fact probe's schema version
// output: an in-memory CRITICAL HEALTH_STORE_UNAVAILABLE (task _cluster, subject this node's id) while migration 000009 cannot be used; one WARN on open and one on close; after the store answers, standalone OPEN then CLOSE transitions (fact times are the first absence and the first successful write) or, in cluster, one resolved legacy row and one FINDING_RESOLVED
// pos: ADR 0009 phase 2, docs/design/0009-phase2-health-evaluator.md §27 (#436)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"

	"go.uber.org/zap"
)

// healthGap is this process's view of whether migration 000009 can be used.
// The finding is served from memory for the same reason META_UNREACHABLE is:
// the store that would hold it is the thing that is missing (design §27).
type healthGap struct {
	mu          sync.Mutex
	open        bool
	recovering  bool
	since       time.Time
	recoveredAt time.Time
	ver         int
	known       bool
	logged      bool
	subject     string
	reason      string
}

// healthStoreReason is the /api/health sentence for a missing 000009.
// A probed schema below 9 names that version; otherwise the tables themselves.
func healthStoreReason(ver int, known bool) string {
	if known && ver > 0 && ver < 9 {
		return fmt.Sprintf("HEALTH_STORE_UNAVAILABLE: schema %d < 9, run ./migrate up", ver)
	}
	return "HEALTH_STORE_UNAVAILABLE: health tables are absent; run ./migrate up to 000009"
}

// probeHealthStore notices a missing or returned 000009 without waiting for
// a task sweep. Run calls it at start (design §27 D3).
func (e *Evaluator) probeHealthStore(ctx context.Context) {
	if e == nil || e.cfg.Store == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c, cancel := context.WithTimeout(ctx, sweepMetaTimeout)
	_, err := e.cfg.Store.ListHealth(c)
	cancel()
	switch {
	case errors.Is(err, healthstore.ErrSchemaAbsent):
		e.noteHealthStoreAbsent(err)
	case err == nil:
		e.noteHealthStoreWritable()
	}
}

// noteHealthStoreAbsent records that the health store cannot be used.
// a task sweep. Run calls it at start (design §27 D2).
func (e *Evaluator) noteHealthStoreAbsent(err error) {
	if e == nil || e.s == nil {
		return
	}
	e.schemaAbsent.Store(true)
	ver, known := e.healthSchemaVersion()
	now := e.cfg.Now()
	subject := e.s.healthSubject()
	g := &e.s.healthGap
	g.mu.Lock()
	if g.since.IsZero() {
		g.since = now
	}
	if known {
		g.ver, g.known = ver, true
	}
	g.reason = healthStoreReason(g.ver, g.known)
	if g.subject == "" {
		g.subject = subject
	}
	g.open = true
	g.recovering = false
	first := !g.logged
	g.logged = true
	reason, subject, since := g.reason, g.subject, g.since
	g.mu.Unlock()
	if first {
		zap.L().Warn("HEALTH_STORE_UNAVAILABLE opened", zap.String("subject", subject), zap.Time("since", since), zap.String("reason", reason))
	}
}

// noteHealthStoreWritable runs on a successful health-store read or write
// after a gap. Standalone keeps the failure until the OPEN is stored, then
// closes it at this moment. Cluster writes one resolved legacy row.
func (e *Evaluator) noteHealthStoreWritable() {
	if e == nil || e.s == nil {
		return
	}
	e.schemaAbsent.Store(false)
	g := &e.s.healthGap
	g.mu.Lock()
	if !g.open {
		g.mu.Unlock()
		return
	}
	since, subject, reason := g.since, g.subject, g.reason
	if subject == "" {
		g.mu.Unlock()
		subject = e.s.healthSubject()
		g.mu.Lock()
		if !g.open {
			g.mu.Unlock()
			return
		}
		g.subject = subject
		since = g.since
	}
	if reason == "" {
		reason = healthStoreReason(g.ver, g.known)
		g.reason = reason
	}
	at := e.cfg.Now()
	g.open = false
	g.recoveredAt = at
	g.recovering = !e.cfg.Cluster
	g.mu.Unlock()
	if e.cfg.Cluster {
		e.recordHealthStoreClosed(subject, since, at, reason)
		zap.L().Warn("HEALTH_STORE_UNAVAILABLE closed", zap.String("subject", subject), zap.Time("since", since), zap.Time("at", at))
		return
	}
	e.signalAt(ClusterFindingTask, FindingHealthStoreUnavailable, subject, false, reason, since)
}

// recordHealthStoreClosed writes one already-resolved legacy row and relies
// on findingEvented to emit FINDING_RESOLVED. Nothing is written while the
// gap is open (design §27 D3).
func (e *Evaluator) recordHealthStoreClosed(subject string, since, at time.Time, reason string) {
	if e == nil || e.s == nil {
		return
	}
	f := Finding{TaskID: ClusterFindingTask, Kind: FindingHealthStoreUnavailable, Subject: subject, Severity: FindingCritical,
		Detail: reason, FirstSeen: since, LastSeen: at, Count: 1}
	e.s.findings.recordClosed(f, at)
}

// settleHealthStore runs on an evaluated _cluster open set. Once the OPEN
// of a recovered gap is stored, the held success is signalled and the next
// pass closes it at the first successful write.
func (e *Evaluator) settleHealthStore(open []health.Finding) {
	if e == nil || e.s == nil || e.cfg.Cluster {
		return
	}
	g := &e.s.healthGap
	g.mu.Lock()
	if !g.recovering {
		g.mu.Unlock()
		return
	}
	subject := g.subject
	recoveredAt := g.recoveredAt
	stored := false
	for _, f := range open {
		if f.Kind == health.HealthStoreUnavailable && f.Subject == subject {
			stored = true
		}
	}
	if stored {
		g.recovering = false
	}
	g.mu.Unlock()
	if !stored {
		e.MarkDirty(ClusterFindingTask, ReasonSignal)
		return
	}
	e.signalAt(ClusterFindingTask, FindingHealthStoreUnavailable, subject, true, "", recoveredAt)
	zap.L().Warn("HEALTH_STORE_UNAVAILABLE closed", zap.String("subject", subject), zap.Time("at", recoveredAt))
}

// healthSchemaVersion is the schema_migrations version from the seal-fact
// probe. A probe error does not block the finding (design §27 D2).
func (e *Evaluator) healthSchemaVersion() (int, bool) {
	if e == nil || e.s == nil {
		return 0, false
	}
	e.s.mu.Lock()
	store := e.s.store
	e.s.mu.Unlock()
	p, ok := store.(sealFactsProber)
	if !ok {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ver, _, err := p.ProbeSealFacts(ctx)
	if err != nil {
		return 0, false
	}
	return ver, true
}

// healthSubject is the node id META_UNREACHABLE uses, or "local".
func (s *Scheduler) healthSubject() string {
	if s == nil {
		return "local"
	}
	s.metaReach.mu.Lock()
	defer s.metaReach.mu.Unlock()
	return s.metaReach.subjectLocked(s.clusterWorkerID)
}

// healthStoreFinding is the open HEALTH_STORE_UNAVAILABLE while it cannot
// be stored, and while standalone is still writing the OPEN after recovery.
func (s *Scheduler) healthStoreFinding() (Finding, bool) {
	if s == nil {
		return Finding{}, false
	}
	e := s.healthEval.Load()
	if e == nil {
		return Finding{}, false
	}
	g := &s.healthGap
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.open && !g.recovering {
		return Finding{}, false
	}
	now := time.Now()
	if e.cfg.Now != nil {
		now = e.cfg.Now()
	}
	subject := g.subject
	if subject == "" {
		subject = "local"
	}
	reason := g.reason
	if reason == "" {
		reason = healthStoreReason(g.ver, g.known)
	}
	return Finding{TaskID: ClusterFindingTask, Kind: FindingHealthStoreUnavailable, Subject: subject, Severity: FindingCritical,
		FirstSeen: g.since, LastSeen: now, Count: 1, Detail: reason}, true
}

// healthStoreDegradedReason is the extra /api/health sentence, present
// while the finding is served from memory.
func (s *Scheduler) healthStoreDegradedReason() (string, bool) {
	f, ok := s.healthStoreFinding()
	if !ok {
		return "", false
	}
	if f.Detail != "" {
		return f.Detail, true
	}
	return healthStoreReason(0, false), true
}
