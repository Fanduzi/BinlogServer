// Package health provides module-level functionality for health.
// input: DefaultRegistry and the assurance kind map
// output: proof that HEALTH_STORE_UNAVAILABLE is Managed, CRITICAL, not Confirm, and not derived from an assurance kind (design 0009 §27)
// pos: kind-table test for #436
// note: if this file changes, update this header and module README.md.
package health

import "testing"

func TestHealthStoreUnavailableManaged(t *testing.T) {
	k := Kind("HEALTH_STORE_UNAVAILABLE")
	s := DefaultRegistry()[k]
	if !ManagedKinds()[k] || !s.Managed || s.Confirm || s.Severity != Critical {
		t.Fatalf("HEALTH_STORE_UNAVAILABLE managed=%v confirm=%v severity=%s, want Managed CRITICAL without Confirm (§27)", s.Managed, s.Confirm, s.Severity)
	}
	for ak, fk := range fromAssurance {
		if fk == k {
			t.Fatalf("HEALTH_STORE_UNAVAILABLE derived from assurance %s; it is a signal, not a chain finding", ak)
		}
	}
}
