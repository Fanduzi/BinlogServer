// Package health provides module-level functionality for health.
// input: DefaultRegistry and Facts for a retention-marked orphan and for missing seal-fact columns
// output: proof that ORPHAN_ROW is Managed with Confirm, SEAL_FACTS_ABSENT is Managed without Confirm, the seal-fact finding opens in one pass and closes when the columns return, and removing object storage closes ORPHAN_ROW with CONFIG (design 0009 §26)
// pos: P2-5.4 red tests for the kind table
// note: if this file changes, update this header and module README.md.
package health

import (
	"strings"
	"testing"

	"binlog_server/internal/assurance"
)

func TestP254ManagedTable(t *testing.T) {
	r := DefaultRegistry()
	if s := r[OrphanRow]; !s.Managed || !s.Confirm {
		t.Fatalf("ORPHAN_ROW managed=%v confirm=%v, want both (§26.4)", s.Managed, s.Confirm)
	}
	if s := r[SealFactsAbsent]; !s.Managed || s.Confirm {
		t.Fatalf("SEAL_FACTS_ABSENT managed=%v confirm=%v, want managed without Confirm (§26.5)", s.Managed, s.Confirm)
	}
}

func TestP254SealFactsAbsentOpensAndClosesInOnePass(t *testing.T) {
	r := DefaultRegistry()
	f := &Facts{TaskID: "1", Now: t0, Store: &StoreFacts{SchemaVersion: 7, SealFactsAbsent: true},
		Complete: map[Scope]bool{ScopeStore: true}}
	ts, _ := r.Reconcile(nil, f)
	if len(ts) != 1 || ts[0].Op != OpOpen || ts[0].Kind != SealFactsAbsent || ts[0].Subject != "" || ts[0].Instance.Severity != Warning {
		t.Fatalf("open: %+v", ts)
	}
	if !strings.Contains(ts[0].Instance.Detail, "7") {
		t.Fatalf("detail should name the schema version: %q", ts[0].Instance.Detail)
	}
	open := []Finding{{TaskID: "1", Kind: SealFactsAbsent, Severity: Warning, Detail: ts[0].Instance.Detail}}
	f.Store.SealFactsAbsent = false
	ts, _ = r.Reconcile(open, f)
	if len(ts) != 1 || ts[0].Op != OpClose || ts[0].Kind != SealFactsAbsent {
		t.Fatalf("close: %+v", ts)
	}
}

func TestP254OrphanConfirmsThenOpens(t *testing.T) {
	r := DefaultRegistry()
	f := &Facts{TaskID: "1", Now: t0, Chain: &ChainFacts{Violations: []assurance.Violation{
		{Kind: assurance.I6OrphanRow, Segments: []string{"mysql-bin.000005"}, Detail: "neither copy"},
	}}, Complete: map[Scope]bool{ScopeChainLocal: true, ScopeChainObjects: true}}
	if ts, _ := r.Reconcile(nil, f); len(ts) != 0 {
		t.Fatalf("first pass opened %+v", ts)
	}
	f.Confirmed = r.Candidates(f)
	ts, _ := r.Reconcile(nil, f)
	if len(ts) != 1 || ts[0].Op != OpOpen || ts[0].Kind != OrphanRow || ts[0].Subject != "mysql-bin.000005" {
		t.Fatalf("second pass: %+v", ts)
	}
}

func TestP254OrphanClosesWhenObjectStorageRemoved(t *testing.T) {
	r := DefaultRegistry()
	open := []Finding{{TaskID: "1", Kind: OrphanRow, Subject: "mysql-bin.000005", Severity: Warning}}
	f := &Facts{TaskID: "1", Now: t0, NoObjectStorage: true, ObjectConfigAudit: "was abc",
		Complete: map[Scope]bool{ScopeChainLocal: true}}
	ts, _ := r.Reconcile(open, f)
	if len(ts) != 1 || ts[0].Op != OpClose || ts[0].Reason != ReasonConfig {
		t.Fatalf("transitions %+v, want CONFIG close", ts)
	}
}
