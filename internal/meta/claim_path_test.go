// Package meta provides module-level functionality for meta.
// input: the claim SQL and the finding upsert
// output: proof that claim does not read worker_registrations and the finding upsert does not lock task_leases
// pos: ADR 0011 PR-2 claim-path assertion
// note: if this file changes, update this header and module README.md.
package meta

import (
	"strings"
	"testing"
)

func TestClaimSQLDoesNotReadWorkerRegistrations(t *testing.T) {
	for _, q := range []string{listTasksWithExpiredLeaseSQL, listStartingUnownedTaskSQL, listTaskIDsWithExpiredOwnerLeaseSQL} {
		if strings.Contains(strings.ToLower(q), "worker_registrations") {
			t.Fatalf("claim SQL reads worker_registrations:\n%s", q)
		}
	}
}

func TestFindingUpsertDoesNotLockTaskLeases(t *testing.T) {
	if strings.Contains(strings.ToLower(upsertFindingSQL), "task_leases") {
		t.Fatalf("finding upsert locks task_leases:\n%s", upsertFindingSQL)
	}
}
