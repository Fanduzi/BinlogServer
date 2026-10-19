// Package health provides module-level functionality for health.
// input: DefaultRegistry
// output: proof that HEALTH_STALE is Managed, WARNING, and not Confirm (design 0009 §28)
// pos: kind-table test for P2-6.1
// note: if this file changes, update this header and module README.md.
package health

import "testing"

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
