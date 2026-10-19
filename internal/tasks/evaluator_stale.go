// Package tasks provides module-level functionality for tasks.
// input: the task list (this process and the task store) and task_health.evaluated_at, aged against StoreClock.StoreNow when the store has one
// output: Facts for one _cluster pass that derives HEALTH_STALE only (task.state complete, no signals, no chain, no store scope) and the process-local knownSince of tasks that have no health row
// pos: ADR 0009 P2-6.1 (docs/design/0009-phase2-health-evaluator.md §28); only the _health holder runs this
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"sort"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
)

// staleAfter is health.stale_after. Zero config means five sweeps, so the
// threshold follows the sweep instead of a fixed 300s.
func (e *Evaluator) staleAfter() time.Duration {
	if e.cfg.StaleAfter > 0 {
		return e.cfg.StaleAfter
	}
	return 5 * e.cfg.Sweep
}

// staleFacts fills the _cluster pass (design §28). A list, health or clock
// error returns before any scope is marked complete, so the caller writes
// nothing. The age uses the store clock when the store implements it:
// MySQL stamps evaluated_at with UTC_TIMESTAMP(6).
func (e *Evaluator) staleFacts(ctx context.Context, facts *health.Facts) error {
	if e.cfg.Store == nil {
		return errors.New("no health store")
	}
	ctx, cancel := context.WithTimeout(ctx, sweepMetaTimeout)
	defer cancel()
	ids, err := e.staleSubjectIDs(ctx)
	if err != nil {
		e.noteStaleRead(err)
		return err
	}
	rows, err := e.cfg.Store.ListHealth(ctx)
	if err != nil {
		e.noteStaleRead(err)
		return err
	}
	now := facts.Now
	if clock, ok := e.cfg.Store.(healthstore.StoreClock); ok {
		storeNow, serr := clock.StoreNow(ctx)
		if serr != nil {
			e.noteStaleRead(serr)
			return serr
		}
		now = storeNow
		facts.Now = storeNow
	}
	byID := make(map[string]time.Time, len(rows))
	for _, h := range rows {
		byID[h.TaskID] = h.EvaluatedAt
	}
	evaluated := make(map[string]time.Time, len(ids))
	haveRow := make(map[string]bool, len(ids))
	for _, id := range ids {
		if ts, ok := byID[id]; ok && !ts.IsZero() {
			evaluated[id] = ts
			haveRow[id] = true
			continue
		}
		evaluated[id] = time.Time{}
	}
	facts.HealthEvaluatedAt = evaluated
	facts.HealthKnownSince = e.noteHealthKnown(ids, haveRow, now)
	facts.StaleAfter = e.staleAfter()
	facts.Complete[health.ScopeTaskState] = true
	facts.Produced = map[health.Scope]bool{health.ScopeTaskState: true}
	return nil
}

// staleSubjectIDs is every task the holder can see: memory union the task
// store. _cluster and _health are not subjects.
func (e *Evaluator) staleSubjectIDs(ctx context.Context) ([]string, error) {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if id == "" || id == ClusterFindingTask || id == HealthLeaseID || seen[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if e.s != nil {
		for _, t := range e.s.ListTasks() {
			add(t.ID)
		}
		e.s.mu.Lock()
		store := e.s.store
		e.s.mu.Unlock()
		if store != nil {
			listed, err := store.ListTasks(ctx)
			if err != nil {
				return nil, err
			}
			for _, t := range listed {
				add(t.ID)
			}
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// noteHealthKnown remembers when this holder first saw a task with no
// health row, and forgets a task that gained a row or left the list.
// The returned map is a copy for this pass.
func (e *Evaluator) noteHealthKnown(ids []string, haveRow map[string]bool, now time.Time) map[string]time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.healthKnown == nil {
		e.healthKnown = map[string]time.Time{}
	}
	keep := map[string]bool{}
	for _, id := range ids {
		if haveRow[id] {
			delete(e.healthKnown, id)
			continue
		}
		keep[id] = true
		if _, ok := e.healthKnown[id]; !ok {
			e.healthKnown[id] = now
		}
	}
	for id := range e.healthKnown {
		if !keep[id] {
			delete(e.healthKnown, id)
		}
	}
	out := make(map[string]time.Time, len(e.healthKnown))
	for id, at := range e.healthKnown {
		out[id] = at
	}
	return out
}

func (e *Evaluator) noteStaleRead(err error) {
	if e.s != nil {
		e.s.noteMetaReach(err, false)
	}
}
