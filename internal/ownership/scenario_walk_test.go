// Package ownership is the pure task-ownership transition from ADR 0011.
// input: Step and the pure predicates in spec.go
// output: one named test per §4.3 walk the model can decide
// pos: ADR 0011 §4.3 GAP rows, closed by the revised rules
// note: if this file changes, update this header and module README.md.
package ownership

import (
	"testing"
	"time"
)

func TestScenarioS2_PerNodeEpochIsCapped(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", false, t0), peer("n3", false, t0)})
	start := s.Lease.Epoch
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	for _, id := range []string{"n2", "n3"} {
		s = applied(t, s, Event{Kind: EventAcquire, Node: id}, later)
		s = applied(t, s, heldCause(s, EventTailUnreadable, id, CauseSegmentNotOnWorker), later)
		if !s.Refusal[id].OpenEnded {
			t.Fatalf("%s refusal is not NULL", id)
		}
	}
	if s.Lease.Epoch-start > int64(len(s.Nodes)-1) {
		t.Fatalf("epoch grew by %d", s.Lease.Epoch-start)
	}
	// DumpReady deletes the NULL rows, so the next takeover can use them.
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n1"}, later)
	s = applied(t, s, Held(s, EventDumpReady, "n1"), later)
	if len(s.Refusal) != 0 {
		t.Fatal("DumpReady left refusals")
	}
	again := later.Add(time.Hour)
	s = expireLease(s, again)
	if step(t, s, Event{Kind: EventAcquire, Node: "n2"}, again).Decision != DecisionApplied {
		t.Fatal("peer stayed excluded after DumpReady")
	}
}

func TestScenarioS3b_HeartbeatWithoutDirectory(t *testing.T) {
	s := yieldTail(t, "n1", "n2")
	goneAt := s.TailSince.Add(EffectiveNodeGone(s.Params) + time.Second)
	s = heartbeat(s, "n1", s.TailSince)
	s = applied(t, s, Event{Kind: EventChainOwnerGone, Node: "_health"}, goneAt)
	epoch := s.Lease.Epoch
	when := goneAt.Add(time.Second)
	s = heartbeat(s, "n1", when)
	res := step(t, s, Event{Kind: EventChainVerified, Node: "n1", ReadOK: false}, when)
	res = step(t, res.State, Event{Kind: EventChainOwnerBack, Node: "n1"}, when)
	if res.State.Phase != PhaseFailed || res.State.Lease.Epoch != epoch || !findOpen(res.State, FindingTail, s.TaskID) {
		t.Fatalf("phase %s epoch %d", res.State.Phase, res.State.Lease.Epoch)
	}
	if InClaimSet(res.State, when) {
		t.Fatal("claim pulled FAILED")
	}
}

func TestScenarioS15b_StartOnPendingDelete(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	s = applied(t, s, Event{Kind: EventOperatorDelete, Node: "n1"}, t0)
	if s.Phase != PhaseStopping || !s.PendingDelete {
		t.Fatalf("delete: phase %s pending %v", s.Phase, s.PendingDelete)
	}
	res := step(t, s, Event{Kind: EventOperatorStart, Node: "n1", ChangesSpec: true}, t0)
	if res.HTTP != 409 || res.State.Phase != PhaseStopping || res.State.RowGeneration != s.RowGeneration {
		t.Fatalf("start: http %d phase %s", res.HTTP, res.State.Phase)
	}
}

func TestScenarioS16_DeadHolderStopAndDelete(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	s = applied(t, s, Event{Kind: EventOperatorStop, Node: "n1"}, t0)
	if s.Phase != PhaseStopping || s.Desired != DesiredStop {
		t.Fatalf("stop: %s %s", s.Phase, s.Desired)
	}
	if step(t, s, Event{Kind: EventOperatorStart, Node: "n2"}, t0).HTTP != 409 {
		t.Fatal("start on STOPPING")
	}
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	s = applied(t, s, Event{Kind: EventStopExpired, Node: "n2"}, later)
	if s.Phase != PhaseStopped || s.Desired != DesiredStop || s.Deleted {
		t.Fatalf("stop expired: phase %s deleted %v", s.Phase, s.Deleted)
	}
	// A delete whose lease is already gone removes the row in the same step.
	live := boot(t, "n1", t0, nil)
	live = expireLease(live, later)
	gone := applied(t, live, Event{Kind: EventOperatorDelete, Node: "n2"}, later)
	if !gone.Deleted {
		t.Fatal("expired delete left the row")
	}
}

func TestScenarioS17_DefaultSweepStaysHealthy(t *testing.T) {
	if got := EffectiveStaleAfter(DefaultParams()); got != 125*time.Second {
		t.Fatalf("stale_after %s, want 125s", got)
	}
	s := boot(t, "n1", t0, nil)
	s = applied(t, s, Event{Kind: EventSweep, Node: "n1"}, t0)
	if HealthCode(s, t0.Add(60*time.Second)) != 200 {
		t.Fatal("healthy cluster inside one sweep returned 503")
	}
	if HealthCode(s, t0.Add(125*time.Second+time.Millisecond)) != 503 {
		t.Fatal("evaluated_at older than 125s stayed 200")
	}
}

func TestScenarioS18_ShortSweepDerivesNodeGone(t *testing.T) {
	p := DefaultParams()
	p.Sweep = 5 * time.Second
	p.LeaseTTL = 60 * time.Second
	if NodeGoneRejectsStart(p) {
		t.Fatal("derived node_gone rejected cluster start")
	}
	if got := EffectiveNodeGone(p); got != 70*time.Second {
		t.Fatalf("node_gone %s, want 70s", got)
	}
	s := boot(t, "n1", t0, []Node{peer("n2", false, t0)})
	s.Params.Sweep = 5 * time.Second
	s.Params.LeaseTTL = 60 * time.Second
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, later)
	s = applied(t, s, heldCause(s, EventTailUnreadable, "n2", CauseSegmentNotOnWorker), later)
	s = heartbeat(s, "n1", later)
	// Inside 70s the first yield must not be ChainOwnerGone.
	res := step(t, s, Event{Kind: EventChainOwnerGone, Node: "_health"}, later.Add(60*time.Second))
	if res.Decision == DecisionApplied || res.State.Phase == PhaseFailed {
		t.Fatal("first yield triggered ChainOwnerGone inside the TTL")
	}
}

func TestScenarioS19_MigrationPrecheckAndTrigger(t *testing.T) {
	if MigrationAllowed(Precheck{LiveRegistration: true, SuperOrTrust: true}) {
		t.Fatal("live registration was allowed to migrate")
	}
	if !MigrationAllowed(Precheck{LogBin: true}) {
		t.Fatal("binlog without SUPER was refused; least-privilege migrate does not create a routine")
	}
	if !MigrationAllowed(Precheck{LogBin: true, SuperOrTrust: true}) {
		t.Fatal("precheck rejected a ready account")
	}
	s := boot(t, "n1", t0, nil)
	gen := s.RowGeneration
	s = applied(t, s, Event{Kind: EventLegacyUpdate}, t0)
	if GenerationBumped(gen, s.RowGeneration) || !findOpen(s, FindingMixedVersion, "_cluster") {
		t.Fatal("legacy update bumped generation or did not open MIXED_VERSION")
	}
	// Renew writes task_leases, not backup_tasks, so it still applies.
	if step(t, s, Event{Kind: EventRenew, Node: "n1", Epoch: s.Lease.Epoch}, t0).Decision != DecisionApplied {
		t.Fatal("renew was refused")
	}
}

func TestScenarioS20_LongTransactionBlocksMigration(t *testing.T) {
	if MigrationAllowed(Precheck{LongTransaction: true, SuperOrTrust: true}) {
		t.Fatal("long transaction was allowed to migrate")
	}
	if SessionLockWaitTimeout != 10*time.Second {
		t.Fatal(SessionLockWaitTimeout)
	}
}

func TestScenarioS21_UnverifiedBackfillIsNotChainOwner(t *testing.T) {
	existing, inserted := BackfillChain(Chain{}, false, "n1", 4)
	if !inserted || existing.Verified() || existing.Owner != "n1" || existing.VerifiedEpoch != 4 {
		t.Fatalf("backfill %+v inserted %v", existing, inserted)
	}
	again, inserted := BackfillChain(existing, true, "n2", 9)
	if inserted || again.Owner != "n1" {
		t.Fatal("re-run inserted a second chain row")
	}
	s := boot(t, "n1", t0, nil)
	// Replace the verified chain with the unverified candidate.
	s.Chain = existing
	s.Findings = derive(s)
	e := heldCause(s, EventTailUnreadable, "n1", CauseSegmentNotOnWorker)
	s = applied(t, s, e, t0)
	if s.Phase != PhaseLeaseDegraded || s.Chain.Owner != "n1" || s.Chain.Verified() {
		t.Fatalf("unverified owner was treated as chain owner: phase %s verified %v", s.Phase, s.Chain.Verified())
	}
}

func TestScenarioS23_SideKillDefaultsOffAndNeverSendsKill(t *testing.T) {
	off := PlanSideKill(SideKillInput{})
	if off.Mode != "off" || off.CallProcedure || off.ClientSendsKill {
		t.Fatalf("default %+v", off)
	}
	armed := PlanSideKill(SideKillInput{
		Mode: "auto", Direct: true, Procedure: true, CanaryPassed: true,
		Flavor: "mysql", ProcessPrivilege: true, DataLocksPrivilege: true,
	})
	if !armed.CallProcedure || armed.ClientSendsKill {
		t.Fatalf("mysql auto %+v", armed)
	}
	proxy := armed
	_ = proxy
	behind := PlanSideKill(SideKillInput{
		Mode: "auto", Direct: false, Procedure: true, CanaryPassed: true,
		Flavor: "mysql", ProcessPrivilege: true, DataLocksPrivilege: true,
	})
	if behind.CallProcedure || behind.ClientSendsKill {
		t.Fatal("auto armed behind a proxy")
	}
	maria := PlanSideKill(SideKillInput{
		Mode: "auto", Direct: true, Procedure: true, CanaryPassed: true,
		Flavor: "mariadb", ProcessPrivilege: true,
	})
	if maria.CallProcedure {
		t.Fatal("MariaDB canary armed without a waiter")
	}
	maria.CallProcedure = PlanSideKill(SideKillInput{
		Mode: "auto", Direct: true, Procedure: true, CanaryPassed: true,
		Flavor: "mariadb", ProcessPrivilege: true, MariaDBWaiter: true,
	}).CallProcedure
	if !maria.CallProcedure {
		t.Fatal("MariaDB with a waiter stayed off")
	}
	noProc := PlanSideKill(SideKillInput{
		Mode: "auto", Direct: true, Procedure: false, CanaryPassed: true,
		Flavor: "mysql", ProcessPrivilege: true, DataLocksPrivilege: true,
	})
	if noProc.CallProcedure {
		t.Fatal("missing procedure armed side_kill")
	}
}
