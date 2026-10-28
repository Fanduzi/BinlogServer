// Package ownership is the pure task-ownership transition from ADR 0011.
// input: Step
// output: one named test per §4.1 trajectory the pure model can run
// pos: the table-driven QA traces in ADR 0011 §4.1
// note: if this file changes, update this header and module README.md.
package ownership

import (
	"testing"
	"time"
)

func TestTrajectory_GraceYieldThenClaim(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	epoch := s.Lease.Epoch
	lost := heldCause(s, EventEpochLost, "n1", CauseLeaseUnreadable)
	s = applied(t, s, lost, t0)
	if s.Phase != PhaseLeaseDegraded || s.Desired != DesiredRun || s.Lease.Owner != "" || s.Lease.Epoch != epoch {
		t.Fatalf("yield: phase %s desired %s owner %q epoch %d", s.Phase, s.Desired, s.Lease.Owner, s.Lease.Epoch)
	}
	if !InClaimSet(s, t0) {
		t.Fatal("yielded row is not claimable")
	}
	s = applied(t, s, Event{Kind: EventLeaseExpired, Node: "n2"}, t0)
	if s.Phase != PhaseStarting || s.Lease.Epoch != epoch+1 || s.Lease.Owner != "n2" {
		t.Fatalf("claim: phase %s epoch %d owner %s", s.Phase, s.Lease.Epoch, s.Lease.Owner)
	}
	// One lease, so one dump. The old owner's DumpReady does not open a second.
	stale := Event{Kind: EventDumpReady, Node: "n1", Epoch: epoch, RowGeneration: s.RowGeneration}
	res := step(t, s, stale, t0)
	if res.Decision != DecisionDiscarded || res.State.Lease.Owner != "n2" || res.State.Phase != PhaseStarting {
		t.Fatalf("stale dump: %s phase %s", res.Decision, res.State.Phase)
	}
}

func TestTrajectory_WriteFencedStaysClaimable(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	e := heldCause(s, EventEpochLost, "n1", CauseWriteFenced)
	s = applied(t, s, e, t0)
	if s.Phase != PhaseLeaseDegraded || s.Desired != DesiredRun || !InClaimSet(s, t0) {
		t.Fatalf("fenced yield: phase %s desired %s claim %v", s.Phase, s.Desired, InClaimSet(s, t0))
	}
	if s.Phase == PhaseStopped {
		t.Fatal("WRITE_FENCED must not land on STOPPED")
	}
}

func TestTrajectory_OldEpochENOENTDoesNotTouchNewOwner(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	oldEpoch := s.Lease.Epoch
	s = expireLease(s, t0.Add(time.Hour))
	now := t0.Add(time.Hour)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, now)
	s = applied(t, s, Held(s, EventDumpReady, "n2"), now)
	if s.Phase != PhaseRunning || s.Lease.Owner != "n2" {
		t.Fatalf("new owner: %s %s", s.Phase, s.Lease.Owner)
	}
	enoent := Event{Kind: EventEpochLost, Node: "n1", Epoch: oldEpoch, RowGeneration: s.RowGeneration, Cause: CauseENOENT}
	res := step(t, s, enoent, now)
	if res.Decision != DecisionDiscarded || res.State.Phase != PhaseRunning || res.State.Lease.Owner != "n2" || res.State.Desired != DesiredRun {
		t.Fatalf("enoent: decision %s phase %s desired %s", res.Decision, res.State.Phase, res.State.Desired)
	}
	if _, ok := res.State.Refusal["n1"]; ok {
		t.Fatal("newer epoch must not write a refusal")
	}
}

func TestTrajectory_MissingSealedOpensAndStrangerCannotClose(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	s = applied(t, s, Event{Kind: EventChainVerified, Node: "n1", ReadOK: true, MissingSealed: true}, t0)
	if !findOpen(s, FindingMissing, s.TaskID) {
		t.Fatal("SEGMENT_MISSING did not open")
	}
	res := step(t, s, Event{Kind: EventChainVerified, Node: "n2", ReadOK: true, MissingSealed: false}, t0)
	if res.Decision == DecisionApplied {
		t.Fatal("n2 does not hold the lease and is not the chain owner")
	}
	if !findOpen(res.State, FindingMissing, s.TaskID) {
		t.Fatal("unverified node closed SEGMENT_MISSING")
	}
	// n2 takes over, but has not verified. The finding stays open.
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, later)
	if closedMissing(step(t, s, Event{Kind: EventEvalDirFailed, Node: "n2"}, later)) {
		t.Fatal("failed directory read closed SEGMENT_MISSING")
	}
}

func closedMissing(res Result) bool {
	for _, f := range res.Closed {
		if f.Name == FindingMissing {
			return true
		}
	}
	return false
}

func TestTrajectory_PerNodeRestartKeepsNullRefusal(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", false, t0)})
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, later)
	s = applied(t, s, heldCause(s, EventTailUnreadable, "n2", CauseSegmentNotOnWorker), later)
	if s.Desired != DesiredRun || s.Chain.Owner != "n1" || s.Phase != PhaseLeaseDegraded {
		t.Fatalf("yield: desired %s chain %s phase %s", s.Desired, s.Chain.Owner, s.Phase)
	}
	r := s.Refusal["n2"]
	if !r.OpenEnded {
		t.Fatal("per_node refuse_until must be NULL")
	}
	// Restart: process memory is gone, the meta row is this same state.
	res := step(t, s, Event{Kind: EventAcquire, Node: "n2"}, later.Add(time.Second))
	if res.Decision == DecisionApplied {
		t.Fatal("NULL refusal survived restart and still acquired")
	}
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n1"}, later.Add(time.Second))
	s = applied(t, s, Held(s, EventDumpReady, "n1"), later.Add(time.Second))
	if s.Phase != PhaseRunning || len(s.Refusal) != 0 || findOpen(s, FindingTail, s.TaskID) {
		t.Fatalf("dump ready: phase %s refusals %d tail %v", s.Phase, len(s.Refusal), findOpen(s, FindingTail, s.TaskID))
	}
	// The peer can try the next takeover.
	again := later.Add(2 * time.Hour)
	s = expireLease(s, again)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, again)
	if s.Lease.Owner != "n2" {
		t.Fatal("peer was excluded after DumpReady")
	}
}

func TestTrajectory_FailedReadDoesNotStealChain(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", false, t0)})
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, later)
	chain := s.Chain.Owner
	res := step(t, s, Event{Kind: EventChainVerified, Node: "n2", ReadOK: false}, later)
	if res.State.Chain.Owner != chain || res.Decision == DecisionApplied {
		t.Fatalf("failed read wrote chain %s", res.State.Chain.Owner)
	}
	s = applied(t, res.State, heldCause(res.State, EventTailUnreadable, "n2", CauseSegmentNotOnWorker), later)
	if s.Phase == PhaseFailed || s.Desired != DesiredRun || s.Chain.Owner != "n1" {
		t.Fatalf("tail became phase %s desired %s chain %s", s.Phase, s.Desired, s.Chain.Owner)
	}
}

func TestOwnerlessStartDuringStopKeepsTheLease(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	s = applied(t, s, Event{Kind: EventOperatorStop, Node: "n1"}, t0)
	epoch, spec, owner := s.Lease.Epoch, s.SpecRevision, s.Lease.Owner
	res := step(t, s, Event{Kind: EventOperatorStart, Node: "control", Ownerless: true}, t0)
	if res.HTTP != 204 || res.Decision != DecisionApplied || res.State.Phase != PhaseStopping || res.State.Desired != DesiredRun {
		t.Fatalf("http %d decision %s phase %s desired %s", res.HTTP, res.Decision, res.State.Phase, res.State.Desired)
	}
	if res.State.Lease.Epoch != epoch || res.State.Lease.Owner != owner || res.State.SpecRevision != spec+1 || res.State.StoppedByOperator {
		t.Fatalf("epoch %d owner %s spec %d -> %d stopped %v", res.State.Lease.Epoch, res.State.Lease.Owner, spec, res.State.SpecRevision, res.State.StoppedByOperator)
	}
	if step(t, s, Event{Kind: EventOperatorStart, Node: "n2"}, t0).HTTP != 409 {
		t.Fatal("a worker start on STOPPING was accepted")
	}
}

func TestTrajectory_StartAfterYieldAndRejectOnStopping(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", false, t0)})
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, later)
	s = applied(t, s, heldCause(s, EventTailUnreadable, "n2", CauseSegmentNotOnWorker), later)
	tail := s.TailSince
	res := step(t, s, Event{Kind: EventOperatorStart, Node: "n1"}, later)
	if res.HTTP != 204 || res.State.Phase != PhaseStarting || res.State.RunID != "" || len(res.State.Refusal) != 0 {
		t.Fatalf("start: http %d phase %s run %q refusals %d", res.HTTP, res.State.Phase, res.State.RunID, len(res.State.Refusal))
	}
	if !res.State.TailSince.Equal(tail) || !findOpen(res.State, FindingTail, s.TaskID) {
		t.Fatal("start cleared the tail fact")
	}
	stopped := applied(t, s, Event{Kind: EventOperatorStop, Node: "n1"}, later)
	res = step(t, stopped, Event{Kind: EventOperatorStart, Node: "n1"}, later)
	if res.HTTP != 409 || res.State.Phase != stopped.Phase || res.State.RowGeneration != stopped.RowGeneration {
		t.Fatalf("start on STOPPING: http %d phase %s", res.HTTP, res.State.Phase)
	}
}

func TestTrajectory_OwnerChangeDoesNotCloseDirectoryFinding(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	s = applied(t, s, Event{Kind: EventChainVerified, Node: "n1", ReadOK: true, MissingSealed: true}, t0)
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, later)
	res := step(t, s, Event{Kind: EventChainVerified, Node: "n2", ReadOK: true, MissingSealed: true}, later)
	if res.Decision != DecisionApplied || res.State.Chain.Owner != "n2" {
		t.Fatalf("verify: %s chain %s", res.Decision, res.State.Chain.Owner)
	}
	for _, f := range res.Closed {
		if f.Name == FindingMissing {
			t.Fatal("directory finding closed while the violation still holds")
		}
	}
	if !findOpen(res.State, FindingMissing, s.TaskID) {
		t.Fatal("finding dropped")
	}
	// Before n2 verifies, a sweep must not close it either.
	before := step(t, s, Event{Kind: EventSweep, Node: "n2"}, later)
	for _, f := range before.Closed {
		if f.Name == FindingMissing {
			t.Fatal("closed before ChainVerified")
		}
	}
}

func TestTrajectory_MetaLockTimeoutDoesNotLockTaskLeases(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	s = applied(t, s, Event{Kind: EventLock, Node: "n1"}, t0)
	res := step(t, s, Event{Kind: EventMetaLockTimeout, Node: "n2"}, t0.Add(5*time.Second))
	if res.State.Phase != PhaseRunning || res.LocksTaskLease {
		t.Fatalf("phase %s locks %v", res.State.Phase, res.LocksTaskLease)
	}
	for _, in := range res.Intents {
		if in.LocksTaskLease {
			t.Fatal("finding write locked task_leases")
		}
	}
	if !findOpen(res.State, FindingLockStalled, s.TaskID) || !res.State.Findings[0].OnCluster && !findOnCluster(res.State, FindingLockStalled) {
		t.Fatal("META_LOCK_STALLED not on _cluster")
	}
	if res.State.HealthStatus != "HEALTHY" || HealthCode(res.State, t0.Add(5*time.Second)) != 503 {
		t.Fatalf("status %s health %d", res.State.HealthStatus, HealthCode(res.State, t0))
	}
	// The timeout is idle_kill, not the lease TTL.
	if t0.Add(5*time.Second).Sub(t0) >= s.Params.LeaseTTL {
		t.Fatal("fixture waited a full TTL")
	}
}

func findOnCluster(s State, name string) bool {
	for _, f := range s.Findings {
		if f.Name == name && f.OnCluster {
			return true
		}
	}
	return false
}

func TestTrajectory_OtherWorkerClaimsWithoutRegistration(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	s.RegistrationLive = map[string]bool{"n1": true}
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	s = applied(t, s, Event{Kind: EventLeaseExpired, Node: "n2"}, later)
	if s.Lease.Owner != "n2" {
		t.Fatal("claim waited for the other worker's registration")
	}
}

func TestTrajectory_StaleGenerationRereadsButStaleEpochDiscards(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	oldEpoch := s.Lease.Epoch
	oldGen := s.RowGeneration
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, later)
	s = applied(t, s, Held(s, EventDumpReady, "n2"), later)
	// Generation happens to still match the number the old owner stored,
	// but the epoch does not. Both DumpReady and TerminalError are discarded.
	dump := Event{Kind: EventDumpReady, Node: "n1", Epoch: oldEpoch, RowGeneration: oldGen}
	res := step(t, s, dump, later)
	if res.Decision != DecisionDiscarded || res.State.Phase != PhaseRunning || res.State.Desired == DesiredStop || res.State.Lease.Owner != "n2" {
		t.Fatalf("dump replay: %s phase %s desired %s", res.Decision, res.State.Phase, res.State.Desired)
	}
	term := Event{Kind: EventTerminalError, Node: "n1", Epoch: oldEpoch, RowGeneration: s.RowGeneration, Cause: CauseSourceAuth}
	res = step(t, s, term, later)
	if res.Decision != DecisionDiscarded || res.State.Phase != PhaseRunning || res.State.Desired != DesiredRun {
		t.Fatalf("terminal replay: %s phase %s desired %s", res.Decision, res.State.Phase, res.State.Desired)
	}
	// Same epoch, stale generation: reread, do not apply.
	staleGen := Held(s, EventDumpReady, "n2")
	staleGen.RowGeneration = s.RowGeneration - 1
	res = step(t, s, staleGen, later)
	if res.Decision != DecisionReread || res.State.RowGeneration != s.RowGeneration {
		t.Fatalf("reread: %s gen %d", res.Decision, res.State.RowGeneration)
	}
}

func TestTrajectory_ChainOwnerReturnsWithoutStart(t *testing.T) {
	s := yieldTail(t, "n1", "n2")
	goneAt := s.TailSince.Add(EffectiveNodeGone(s.Params) + time.Second)
	s = heartbeat(s, "n1", s.TailSince)
	s = applied(t, s, Event{Kind: EventChainOwnerGone, Node: "_health"}, goneAt)
	if s.Phase != PhaseFailed || s.FailedReason != ReasonChainOwnerGone || s.Desired != DesiredRun {
		t.Fatalf("gone: %s %s %s", s.Phase, s.FailedReason, s.Desired)
	}
	backAt := goneAt.Add(time.Second)
	s = heartbeat(s, "n1", backAt)
	s = applied(t, s, Event{Kind: EventChainVerified, Node: "n1", ReadOK: true}, backAt)
	if !s.Chain.VerifiedAt.After(s.FailedAt) {
		t.Fatal("verified_at is not after failed_at")
	}
	epoch := s.Lease.Epoch
	s = applied(t, s, Event{Kind: EventChainOwnerBack, Node: "n1"}, backAt)
	if s.Phase != PhaseLeaseDegraded || s.FailedReason != "" || s.Lease.Epoch != epoch {
		t.Fatalf("back: phase %s reason %s epoch %d", s.Phase, s.FailedReason, s.Lease.Epoch)
	}
	s = applied(t, s, Event{Kind: EventLeaseExpired, Node: "n1"}, backAt)
	s = applied(t, s, Held(s, EventDumpReady, "n1"), backAt)
	if s.Phase != PhaseRunning || findOpen(s, FindingTail, s.TaskID) {
		t.Fatalf("resumed: %s tail %v", s.Phase, findOpen(s, FindingTail, s.TaskID))
	}
}

func TestTrajectory_HeartbeatWithoutDirectoryStaysFailed(t *testing.T) {
	s := yieldTail(t, "n1", "n2")
	goneAt := s.TailSince.Add(EffectiveNodeGone(s.Params) + time.Second)
	s = heartbeat(s, "n1", s.TailSince)
	s = applied(t, s, Event{Kind: EventChainOwnerGone, Node: "_health"}, goneAt)
	epoch := s.Lease.Epoch
	backAt := goneAt.Add(time.Second)
	s = heartbeat(s, "n1", backAt)
	res := step(t, s, Event{Kind: EventChainVerified, Node: "n1", ReadOK: false}, backAt)
	if res.Decision == DecisionApplied {
		t.Fatal("failed read verified the chain")
	}
	res = step(t, res.State, Event{Kind: EventChainOwnerBack, Node: "n1"}, backAt)
	if res.Decision == DecisionApplied || res.State.Phase != PhaseFailed || res.State.Lease.Epoch != epoch {
		t.Fatalf("back without verify: decision %s phase %s epoch %d", res.Decision, res.State.Phase, res.State.Lease.Epoch)
	}
	if !findOpen(res.State, FindingTail, s.TaskID) || HealthCode(res.State, backAt) != 503 {
		t.Fatal("CRITICAL tail finding closed")
	}
}

func TestTrajectory_OperatorRaces(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	epoch := s.Lease.Epoch
	// Claim transaction sees desired STOP: Acquire rolls back, epoch stays.
	stopping := applied(t, s, Event{Kind: EventOperatorStop, Node: "n1"}, t0)
	res := step(t, stopping, Event{Kind: EventAcquire, Node: "n2"}, t0)
	if res.Decision == DecisionApplied || res.State.Lease.Epoch != epoch {
		t.Fatalf("claim during stop: decision %s epoch %d", res.Decision, res.State.Lease.Epoch)
	}
	res = step(t, stopping, Event{Kind: EventOperatorStart, Node: "n2"}, t0)
	if res.HTTP != 409 || res.State.RowGeneration != stopping.RowGeneration || res.State.Phase != PhaseStopping {
		t.Fatalf("start on STOPPING: http %d", res.HTTP)
	}
	// Repeat Start with the same spec.
	running := boot(t, "n1", t0, nil)
	spec := running.SpecRevision
	res = step(t, running, Event{Kind: EventOperatorStart, Node: "n1"}, t0)
	if res.HTTP != 204 || res.Decision != DecisionIdempotent || res.State.SpecRevision != spec {
		t.Fatalf("repeat start: http %d spec %d -> %d", res.HTTP, spec, res.State.SpecRevision)
	}
	// Delete while the lease is live: STOPPING + pending_delete, owner stays.
	res = step(t, running, Event{Kind: EventOperatorDelete, Node: "n9"}, t0)
	if res.HTTP != 204 || res.State.Phase != PhaseStopping || !res.State.PendingDelete || res.State.Lease.Owner != "n1" || res.State.Lease.Epoch != running.Lease.Epoch {
		t.Fatalf("delete: http %d phase %s pending %v owner %s", res.HTTP, res.State.Phase, res.State.PendingDelete, res.State.Lease.Owner)
	}
	again := step(t, res.State, Event{Kind: EventOperatorStart, Node: "n1"}, t0)
	if again.HTTP != 409 || again.State.Phase != PhaseStopping {
		t.Fatalf("start after delete: http %d phase %s", again.HTTP, again.State.Phase)
	}
	// Holder is kill -9'd. After the TTL, StopExpired deletes the row.
	dead := expireLease(res.State, t0.Add(time.Hour))
	done := step(t, dead, Event{Kind: EventStopExpired, Node: "n2"}, t0.Add(time.Hour))
	if done.Decision != DecisionApplied || !done.State.Deleted {
		t.Fatalf("stop expired: decision %s deleted %v phase %s", done.Decision, done.State.Deleted, done.State.Phase)
	}
	late := step(t, done.State, Held(running, EventStopConfirmed, "n1"), t0.Add(time.Hour))
	if !late.ZeroRow || late.State.Phase != done.State.Phase {
		t.Fatalf("late stop confirmed: zero %v", late.ZeroRow)
	}
}

func yieldTail(t *testing.T, owner, peerID string) State {
	t.Helper()
	s := boot(t, owner, t0, []Node{peer(peerID, false, t0)})
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	s = applied(t, s, Event{Kind: EventAcquire, Node: peerID}, later)
	s = applied(t, s, heldCause(s, EventTailUnreadable, peerID, CauseSegmentNotOnWorker), later)
	return s
}

func heartbeat(s State, id string, at time.Time) State {
	for i := range s.Nodes {
		if s.Nodes[i].ID == id {
			s.Nodes[i].HeartbeatAt = at
		}
	}
	return s
}
