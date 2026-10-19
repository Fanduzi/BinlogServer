// Package health provides module-level functionality for health.
// input: DefaultRegistry
// output: proof that HEALTH_STALE is Managed, WARNING, and not Confirm (design 0009 §28)
// pos: kind-table test for P2-6.1
// note: if this file changes, update this header and module README.md.
package health

import (
	"testing"
	"time"
)

func TestHealthStaleManaged(t *testing.T) {
	k := HealthStale
	s := DefaultRegistry()[k]
	if !ManagedKinds()[k] || !s.Managed || s.Confirm || s.Severity != Warning || s.Derive == nil {
		t.Fatalf("HEALTH_STALE managed=%v confirm=%v severity=%s derive=%v, want Managed WARNING without Confirm (§28)", s.Managed, s.Confirm, s.Severity, s.Derive != nil)
	}
	if len(s.Scopes) != 1 || s.Scopes[0] != ScopeTaskState {
		t.Fatalf("scopes %v, want task.state", s.Scopes)
	}
}

func TestHealthStaleDerive(t *testing.T) {
	r := DefaultRegistry()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	after := 5 * time.Minute
	fresh := now.Add(-4 * time.Minute)
	old := now.Add(-after)
	f := &Facts{TaskID: "_cluster", Now: now, StaleAfter: after,
		Complete:          map[Scope]bool{ScopeTaskState: true},
		Produced:          map[Scope]bool{ScopeTaskState: true},
		HealthEvaluatedAt: map[string]time.Time{"fresh": fresh, "old": old, "_cluster": old, "_health": old},
	}
	ts, _ := r.Reconcile(nil, f)
	if len(ts) != 1 || ts[0].Op != OpOpen || ts[0].Subject != "old" || ts[0].Kind != HealthStale || ts[0].Instance.Severity != Warning {
		t.Fatalf("%+v", ts)
	}
	f.HealthEvaluatedAt = map[string]time.Time{"ahead": now.Add(time.Minute)}
	if ts, _ = r.Reconcile(nil, f); len(ts) != 0 {
		t.Fatalf("future evaluated_at opened %+v", ts)
	}
	f.HealthEvaluatedAt = map[string]time.Time{"new": {}}
	f.HealthKnownSince = map[string]time.Time{"new": now}
	if ts, _ = r.Reconcile(nil, f); len(ts) != 0 {
		t.Fatalf("first sight opened %+v", ts)
	}
	f.HealthKnownSince["new"] = now.Add(-after)
	ts, _ = r.Reconcile(nil, f)
	if len(ts) != 1 || ts[0].Subject != "new" {
		t.Fatalf("knownSince %+v", ts)
	}
	open := []Finding{{TaskID: "_cluster", Kind: HealthStale, Subject: "gone", Severity: Warning, Detail: "x"}}
	f.HealthEvaluatedAt = map[string]time.Time{}
	f.HealthKnownSince = nil
	ts, _ = r.Reconcile(open, f)
	if len(ts) != 1 || ts[0].Op != OpClose || ts[0].Subject != "gone" {
		t.Fatalf("close %+v", ts)
	}
	f.HealthEvaluatedAt = map[string]time.Time{"old": old}
	ts1, _ := r.Reconcile(nil, f)
	f.Now = now.Add(time.Minute)
	kept := []Finding{{TaskID: "_cluster", Kind: HealthStale, Subject: "old", Severity: Warning, Detail: ts1[0].Instance.Detail}}
	if ts2, _ := r.Reconcile(kept, f); len(ts2) != 0 {
		t.Fatalf("detail not stable: %+v", ts2)
	}
}
