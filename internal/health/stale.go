// Package health provides module-level functionality for health.
// input: Facts.HealthEvaluatedAt, Facts.HealthKnownSince and Facts.StaleAfter for task _cluster
// output: HEALTH_STALE instances (WARNING, subject = task id) when a listed task's last health write, or the first time this holder saw it with no row, is at least health.stale_after old
// pos: ADR 0009 P2-6.1 (docs/design/0009-phase2-health-evaluator.md §28); pure, no I/O
// note: if this file changes, update this header and module README.md.
package health

import (
	"fmt"
	"sort"
	"time"
)

// healthStaleSpec is HEALTH_STALE (design §28): the _health holder writes it
// on _cluster. Managed, WARNING, task.state only, no Confirm.
func healthStaleSpec() KindSpec {
	s := stateSpec(HealthStale, Warning, ScopeTaskState, "F", "the task is evaluated again within health.stale_after")
	s.Derive = healthStaleDerive
	return s
}

// healthStaleDerive opens one instance per task whose health write is at
// least StaleAfter old. A zero EvaluatedAt means there is no row: the age
// starts at HealthKnownSince. "_cluster" and "_health" are not subjects
// (they match tasks.ClusterFindingTask and tasks.HealthLeaseID; this
// package does not import tasks). A nil HealthEvaluatedAt is not this pass.
func healthStaleDerive(f *Facts) []Instance {
	if f == nil || f.HealthEvaluatedAt == nil || f.StaleAfter <= 0 {
		return nil
	}
	ids := make([]string, 0, len(f.HealthEvaluatedAt))
	for id := range f.HealthEvaluatedAt {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []Instance
	for _, id := range ids {
		if id == "_cluster" || id == "_health" {
			continue
		}
		at := f.HealthEvaluatedAt[id]
		var ref time.Time
		var detail string
		if !at.IsZero() {
			ref = at
			detail = fmt.Sprintf("task %s has not been evaluated within health.stale_after (%s); last health write %s",
				id, f.StaleAfter, at.UTC().Format(time.RFC3339))
		} else {
			if f.HealthKnownSince != nil {
				ref = f.HealthKnownSince[id]
			}
			if ref.IsZero() {
				continue
			}
			detail = fmt.Sprintf("task %s has no task_health row; expected an evaluation since %s (health.stale_after %s)",
				id, ref.UTC().Format(time.RFC3339), f.StaleAfter)
		}
		if f.Now.Sub(ref) < f.StaleAfter {
			continue
		}
		out = append(out, Instance{Key: Key{Subject: id}, Detail: detail})
	}
	return out
}
