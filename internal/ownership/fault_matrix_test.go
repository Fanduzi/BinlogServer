// Package ownership is the pure task-ownership transition from ADR 0011.
// input: Step and the §4.2 assertions that do not need a live mysqld
// output: one named test per fault-matrix row the pure model can express
// pos: ADR 0011 §4.2, as state transitions rather than SIGSTOP processes
// note: if this file changes, update this header and module README.md.
package ownership

import (
	"testing"
	"time"
)

func TestFaultPhase_AcquireCommittedPeerTakesOver(t *testing.T) {
	// Acquire committed, dump not open. SIGSTOP, kill -9, and a meta partition
	// all end in a peer claim or a yield. None of them is FAILED + desired STOP.
	for _, name := range []string{"sigstop", "kill9", "partition"} {
		t.Run(name, func(t *testing.T) {
			s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
			s.Phase = PhaseStarting
			if name == "partition" {
				s.Partitioned = true
				if HealthCode(s, t0) != 503 {
					t.Fatal("partition kept health 200")
				}
			}
			later := t0.Add(s.Params.LeaseTTL)
			s = expireLease(s, later)
			s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, later)
			if s.Phase == PhaseFailed || s.Desired == DesiredStop {
				t.Fatalf("peer claim: phase %s desired %s", s.Phase, s.Desired)
			}
			if s.Phase != PhaseStarting || s.Lease.Owner != "n2" {
				t.Fatalf("phase %s owner %s", s.Phase, s.Lease.Owner)
			}
		})
	}
}

func TestFaultPhase_DumpOpenPeerTakesOverOneDump(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	old := s.Lease.Epoch
	later := t0.Add(s.Params.LeaseTTL)
	s = expireLease(s, later)
	s.Partitioned = true
	if HealthCode(s, later) != 503 {
		t.Fatal("partition during dump was 200")
	}
	s.Partitioned = false
	s = applied(t, s, Event{Kind: EventEpochLost, Node: "n1", Epoch: old, RowGeneration: s.RowGeneration, Cause: CauseLeaseUnreadable}, later)
	if s.Desired != DesiredRun || s.Phase == PhaseFailed {
		t.Fatalf("yield: phase %s desired %s", s.Phase, s.Desired)
	}
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, later)
	s = applied(t, s, Held(s, EventDumpReady, "n2"), later)
	if s.Phase != PhaseRunning || s.Lease.Owner != "n2" {
		t.Fatalf("one dump: phase %s owner %s", s.Phase, s.Lease.Owner)
	}
}

func TestFaultPhase_LockHeldReleasedWithinIdleKill(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	s = applied(t, s, Event{Kind: EventLock, Node: "n1"}, t0)
	// idle_kill is 5s. The timeout is not the 15s TTL.
	at := t0.Add(s.Params.IdleKill)
	res := step(t, s, Event{Kind: EventMetaLockTimeout, Node: "n2"}, at)
	if res.State.Phase != PhaseRunning || res.LocksTaskLease || HealthCode(res.State, at) != 503 {
		t.Fatalf("phase %s locks %v health %d", res.State.Phase, res.LocksTaskLease, HealthCode(res.State, at))
	}
	if res.State.HealthStatus != "HEALTHY" {
		t.Fatal("last health row was rewritten")
	}
	if !res.State.Lock.Since.IsZero() || res.State.Lock.Held {
		t.Fatal("lock survived the server-side idle kill")
	}
	// kill -9 drops the connection with the process. The peer still waits out the TTL, not a second idle_kill.
	later := t0.Add(s.Params.LeaseTTL)
	dead := expireLease(res.State, later)
	dead = applied(t, dead, Event{Kind: EventAcquire, Node: "n2"}, later)
	if dead.Lease.Owner != "n2" || dead.Desired == DesiredStop {
		t.Fatalf("peer after kill: owner %s desired %s", dead.Lease.Owner, dead.Desired)
	}
}

func TestFaultPhase_TailUnreadableBlocksUntilDumpReady(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", false, t0)})
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, later)
	s = applied(t, s, heldCause(s, EventTailUnreadable, "n2", CauseSegmentNotOnWorker), later)
	if s.Phase != PhaseLeaseDegraded || !s.Refusal["n2"].OpenEnded {
		t.Fatalf("phase %s open %v", s.Phase, s.Refusal["n2"].OpenEnded)
	}
	// Restart during the refusal, and a partition, both leave the row and the refusal.
	s.Partitioned = true
	if HealthCode(s, later) != 503 || s.Phase != PhaseLeaseDegraded {
		t.Fatal("partition changed the yielded row or stayed 200")
	}
	s.Partitioned = false
	if step(t, s, Event{Kind: EventAcquire, Node: "n2"}, later.Add(time.Minute)).Decision == DecisionApplied {
		t.Fatal("restart reclaimed inside a NULL refusal")
	}
	// The owner still has the directory.
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n1"}, later.Add(time.Minute))
	s = applied(t, s, Held(s, EventDumpReady, "n1"), later.Add(time.Minute))
	if len(s.Refusal) != 0 || findOpen(s, FindingTail, s.TaskID) {
		t.Fatal("DumpReady left the refusal or the tail finding")
	}
}

func TestFaultPhase_SharedTailRetriesOnCap(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", false, t0)})
	s.Params.Storage = StorageShared
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, later)
	s = applied(t, s, heldCause(s, EventTailUnreadable, "n2", CauseSegmentNotOnWorker), later)
	if s.Refusal["n2"].OpenEnded || s.Desired != DesiredRun {
		t.Fatal("shared TailUnreadable wrote a NULL refusal or STOP")
	}
	if step(t, s, Event{Kind: EventAcquire, Node: "n2"}, later).Decision == DecisionApplied {
		t.Fatal("acquired inside the refusal window")
	}
	when := s.Refusal["n2"].Until
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, when)
	if s.Lease.Owner != "n2" {
		t.Fatal("shared refusal did not expire")
	}
}

func TestFaultPhase_EvalReadDoesNotMoveChain(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	owner := s.Chain.Owner
	res := step(t, s, Event{Kind: EventChainVerified, Node: "n1", ReadOK: false}, t0)
	if res.State.Chain.Owner != owner || !res.State.Chain.VerifiedAt.Equal(s.Chain.VerifiedAt) {
		t.Fatal("failed evaluation wrote task_chain")
	}
	res = step(t, s, Event{Kind: EventEvalDirFailed, Node: "n1"}, t0)
	if res.State.Chain.Owner != owner {
		t.Fatal("EvalDirFailed wrote task_chain")
	}
}

func TestFaultPhase_OperatorStartIsOneTransaction(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	s = applied(t, s, Event{Kind: EventOperatorStop, Node: "n1"}, t0)
	s = applied(t, s, Held(s, EventStopConfirmed, "n1"), t0)
	// Meta is down: 503, row unchanged. Never 400, never an ownerless STARTING.
	res := step(t, s, Event{Kind: EventOperatorStart, Node: "n1", MetaDown: true}, t0)
	if res.HTTP != 503 || res.State.Phase != s.Phase || res.State.Lease.Owner != s.Lease.Owner || res.State.RowGeneration != s.RowGeneration {
		t.Fatalf("meta down: http %d phase %s", res.HTTP, res.State.Phase)
	}
	res = step(t, s, Event{Kind: EventOperatorStart, Node: "n1"}, t0)
	if res.HTTP != 204 || res.State.Phase != PhaseStarting || res.State.Lease.Owner != "n1" || res.State.RunID != "" {
		t.Fatalf("start: http %d phase %s owner %q run %q", res.HTTP, res.State.Phase, res.State.Lease.Owner, res.State.RunID)
	}
	if res.HTTP == 400 {
		t.Fatal("400")
	}
}

func TestFaultPhase_StartDuringBackoffStillOneTransaction(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	s = applied(t, s, heldCause(s, EventRetryableError, "n1", CauseLockWait), t0)
	if s.Phase != PhaseRetryBackoff {
		t.Fatal(s.Phase)
	}
	res := step(t, s, Event{Kind: EventOperatorStart, Node: "n1"}, t0)
	if res.HTTP != 204 || res.State.Phase != PhaseStarting || res.State.Lease.Owner != "n1" {
		t.Fatalf("start during backoff: http %d phase %s owner %s", res.HTTP, res.State.Phase, res.State.Lease.Owner)
	}
}

func TestFault_ContinuousYieldIsBounded(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", false, t0), peer("n3", false, t0)})
	start := s.Lease.Epoch
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	for _, id := range []string{"n2", "n3"} {
		s = applied(t, s, Event{Kind: EventAcquire, Node: id}, later)
		s = applied(t, s, heldCause(s, EventTailUnreadable, id, CauseSegmentNotOnWorker), later)
		if step(t, s, Event{Kind: EventAcquire, Node: id}, later).Decision == DecisionApplied {
			t.Fatalf("%s acquired twice", id)
		}
	}
	if s.Lease.Epoch-start > 2 || s.Chain.Owner != "n1" || s.Desired == DesiredStop || s.Phase == PhaseFailed {
		t.Fatalf("epoch %d->%d chain %s desired %s phase %s", start, s.Lease.Epoch, s.Chain.Owner, s.Desired, s.Phase)
	}
	if !findOpen(s, FindingTail, s.TaskID) || HealthCode(s, later) != 503 {
		t.Fatal("tail CRITICAL not open")
	}
}

func TestFault_CopyDirectoryThenStart(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", false, t0)})
	s = applied(t, s, Event{Kind: EventChainVerified, Node: "n1", ReadOK: true, MissingSealed: true}, t0)
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, later)
	s = applied(t, s, heldCause(s, EventTailUnreadable, "n2", CauseSegmentNotOnWorker), later)
	// Copying the directory is CanRead recovering. The NULL row stays until
	// DumpReady or Start, but it no longer blocks Acquire.
	for i := range s.Nodes {
		if s.Nodes[i].ID == "n2" {
			s.Nodes[i].CanRead = true
		}
	}
	if step(t, s, Event{Kind: EventAcquire, Node: "n2"}, later).Decision != DecisionApplied {
		t.Fatal("recovered directory still blocked Acquire")
	}
	if _, ok := s.Refusal["n2"]; !ok || !s.Refusal["n2"].OpenEnded {
		t.Fatal("copy deleted the NULL row")
	}
	res := step(t, s, Event{Kind: EventOperatorStart, Node: "n2"}, later)
	if res.HTTP != 204 || len(res.State.Refusal) != 0 {
		t.Fatalf("start: http %d refusals %d", res.HTTP, len(res.State.Refusal))
	}
	// Violation still holds, so the directory finding is not closed.
	verified := step(t, res.State, Event{Kind: EventChainVerified, Node: "n2", ReadOK: true, MissingSealed: true}, later)
	for _, f := range verified.Closed {
		if f.Name == FindingMissing {
			t.Fatal("closed a finding whose fact still holds")
		}
	}
	ran := applied(t, verified.State, Held(verified.State, EventDumpReady, "n2"), later)
	if ran.Phase != PhaseRunning || ran.Lease.Owner != "n2" {
		t.Fatalf("dump: %s %s", ran.Phase, ran.Lease.Owner)
	}
}

func TestFault_SharedUnmountYieldsAndComesBack(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	s.Params.Storage = StorageShared
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	for i := range s.Nodes {
		if s.Nodes[i].ID == "n1" {
			s.Nodes[i].CanRead = false
		}
	}
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, later)
	// n2 also cannot see it once the mount is gone; use n2 as the node that noticed.
	s = applied(t, s, heldCause(s, EventTailUnreadable, "n2", CauseSegmentNotOnWorker), later)
	if s.Desired == DesiredStop || s.Phase == PhaseFailed || s.Refusal["n2"].OpenEnded {
		t.Fatalf("shared unmount: phase %s desired %s null %v", s.Phase, s.Desired, s.Refusal["n2"].OpenEnded)
	}
	if HealthCode(s, later) != 503 {
		t.Fatal("unmount stayed 200")
	}
	when := s.Refusal["n2"].Until
	for i := range s.Nodes {
		s.Nodes[i].CanRead = true
	}
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n1"}, when)
	s = applied(t, s, Held(s, EventDumpReady, "n1"), when)
	if s.Phase != PhaseRunning {
		t.Fatal(s.Phase)
	}
}

func TestFault_PerNodeOwnerContinuesAfterStop(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", false, t0)})
	epoch := s.Lease.Epoch
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, later)
	s = applied(t, s, heldCause(s, EventTailUnreadable, "n2", CauseSegmentNotOnWorker), later)
	// SIGCONT: the original owner claims. Release cleared the owner, so the epoch grows.
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n1"}, later)
	if s.Lease.Epoch <= epoch || s.Lease.Owner != "n1" {
		t.Fatalf("epoch %d -> %d owner %s", epoch, s.Lease.Epoch, s.Lease.Owner)
	}
	s = applied(t, s, Held(s, EventDumpReady, "n1"), later)
	if s.Phase != PhaseRunning || s.Desired == DesiredStop {
		t.Fatalf("phase %s desired %s", s.Phase, s.Desired)
	}
}

func TestFaultRow1_IdleKillBoundsTheLock(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	s = renewLease(t, s, t0)
	start := RTOStart(s.Lease)
	s = applied(t, s, Event{Kind: EventLock, Node: "n1"}, t0)
	at := t0.Add(s.Params.IdleKill)
	s = applied(t, s, Event{Kind: EventMetaLockTimeout, Node: "n2"}, at)
	if HealthCode(s, at) != 503 || s.Phase != PhaseRunning {
		t.Fatalf("during wait: health %d phase %s", HealthCode(s, at), s.Phase)
	}
	later := s.Lease.ExpireAt
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, later)
	s = applied(t, s, Held(s, EventDumpReady, "n2"), later)
	if s.Phase != PhaseRunning || s.Lease.Owner != "n2" {
		t.Fatal("RTO did not end at the new owner's DumpReady")
	}
	if s.ProgressAt.Sub(start) > RTOBound(s.Params) {
		t.Fatalf("RTO %s exceeds %s", s.ProgressAt.Sub(start), RTOBound(s.Params))
	}
}

func TestFaultRow2_HealthHolderLockIsRetryNotFailed(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	epoch := s.Lease.Epoch
	// The _health holder, not the task owner, sits on the row lock.
	s = applied(t, s, Event{Kind: EventLock, Node: "_health"}, t0)
	s = applied(t, s, heldCause(s, EventRetryableError, "n1", CauseLockWait), t0)
	if s.Phase == PhaseFailed || s.Lease.Epoch != epoch || s.Lease.Owner != "n1" || s.Desired != DesiredRun {
		t.Fatalf("phase %s epoch %d owner %s desired %s", s.Phase, s.Lease.Epoch, s.Lease.Owner, s.Desired)
	}
}

func TestFaultRow3_IdleKillInsideTTLDoesNotFailOver(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	epoch := s.Lease.Epoch
	expire := s.Lease.ExpireAt
	at := t0.Add(s.Params.IdleKill)
	if !expire.After(at) {
		t.Fatal("fixture expired the lease inside idle_kill")
	}
	s = applied(t, s, Event{Kind: EventLock, Node: "n1"}, t0)
	s = applied(t, s, Event{Kind: EventMetaLockTimeout, Node: "n2"}, at)
	if !s.Lease.ExpireAt.Equal(expire) || !s.Lease.ExpireAt.After(at) {
		t.Fatal("idle kill moved lease_expire_at")
	}
	s = applied(t, s, heldCause(s, EventRetryableError, "n1", CauseIdleKilled), at)
	if s.Phase == PhaseFailed || s.Lease.Epoch != epoch || s.Lease.Owner != "n1" {
		t.Fatalf("phase %s epoch %d owner %s", s.Phase, s.Lease.Epoch, s.Lease.Owner)
	}
	if step(t, s, Event{Kind: EventAcquire, Node: "n2"}, at).Decision == DecisionApplied {
		t.Fatal("peer took over inside the TTL")
	}
}

func TestFaultRow4_LateWriteAfterTwoTTLIsFenced(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	old := s.Lease.Epoch
	later := t0.Add(2 * s.Params.LeaseTTL)
	s = expireLease(s, later)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, later)
	s = applied(t, s, Held(s, EventDumpReady, "n2"), later)
	late := Event{Kind: EventDumpReady, Node: "n1", Epoch: old, RowGeneration: s.RowGeneration}
	res := step(t, s, late, later)
	if res.Decision != DecisionDiscarded || res.State.Lease.Owner != "n2" || res.State.Phase != PhaseRunning {
		t.Fatalf("late write: %s owner %s", res.Decision, res.State.Lease.Owner)
	}
}

func TestFaultRow5_AcquireAndStartingAreOneStep(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	later := t0.Add(time.Hour)
	before := expireLease(s, later)
	res := Step(before, Event{Kind: EventAcquire, Node: "n2"}, later)
	if res.Decision != DecisionApplied {
		t.Fatal(res.Decision)
	}
	if res.State.Lease.Epoch == before.Lease.Epoch && res.State.Phase == before.Phase {
		t.Fatal("claim neither kept the row nor moved it")
	}
	if res.State.Lease.Epoch != before.Lease.Epoch && res.State.Phase != PhaseStarting {
		t.Fatal("epoch moved while the phase stayed behind")
	}
	if res.State.Phase != PhaseStarting || res.State.Lease.Owner != "n2" {
		t.Fatalf("phase %s owner %s", res.State.Phase, res.State.Lease.Owner)
	}
}

func TestFaultRow6_ChainOwnerBackAfterNodeGone(t *testing.T) {
	s := yieldTail(t, "n1", "n2")
	goneAt := s.TailSince.Add(EffectiveNodeGone(s.Params) + time.Second)
	s = heartbeat(s, "n1", s.TailSince)
	s = applied(t, s, Event{Kind: EventChainOwnerGone, Node: "_health"}, goneAt)
	// Heartbeat alone does not leave FAILED.
	s = heartbeat(s, "n1", goneAt.Add(time.Second))
	if step(t, s, Event{Kind: EventChainOwnerBack, Node: "n1"}, goneAt.Add(time.Second)).Decision == DecisionApplied {
		t.Fatal("heartbeat alone ran ChainOwnerBack")
	}
	when := goneAt.Add(time.Second)
	s = applied(t, s, Event{Kind: EventChainVerified, Node: "n1", ReadOK: true}, when)
	s = applied(t, s, Event{Kind: EventChainOwnerBack, Node: "n1"}, when)
	if s.Phase != PhaseLeaseDegraded || s.FailedReason != "" {
		t.Fatalf("back: %s %s", s.Phase, s.FailedReason)
	}
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n1"}, when)
	s = applied(t, s, Held(s, EventDumpReady, "n1"), when)
	if s.Phase != PhaseRunning || s.Lease.Owner != "n1" {
		t.Fatalf("dump: %s %s", s.Phase, s.Lease.Owner)
	}
}

func TestFaultRow7_SameNodeRestartStaysDesiredRun(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	s.RegistrationLive = map[string]bool{"n1": true}
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n1"}, later)
	if s.Desired != DesiredRun || s.Phase == PhaseFailed || s.Lease.Owner != "n1" {
		t.Fatalf("phase %s desired %s owner %s", s.Phase, s.Desired, s.Lease.Owner)
	}
	// The same owner was still on the expired row, so the epoch does not bump (F9 is after Release).
	if s.RegistrationLive["n1"] && s.Lease.Owner != "n1" {
		t.Fatal("registration blocked the same node")
	}
}

func TestFaultRow8_ENOENTWhileHoldingIsEpochLost(t *testing.T) {
	for _, storage := range []Storage{StoragePerNode, StorageShared} {
		t.Run(string(storage), func(t *testing.T) {
			s := boot(t, "n1", t0, nil)
			s.Params.Storage = storage
			epoch := s.Lease.Epoch
			s = applied(t, s, heldCause(s, EventEpochLost, "n1", CauseENOENT), t0)
			if s.Phase == PhaseFailed || s.Desired == DesiredStop || s.Phase != PhaseLeaseDegraded || s.Lease.Epoch != epoch {
				t.Fatalf("phase %s desired %s epoch %d", s.Phase, s.Desired, s.Lease.Epoch)
			}
			r := s.Refusal["n1"]
			if r.OpenEnded || r.Until.IsZero() || r.Reason != RefusalEpochLost {
				t.Fatalf("refusal %+v", r)
			}
			// The read of the lease failing is the same event.
			s2 := boot(t, "n1", t0, nil)
			s2.Params.Storage = storage
			e := heldCause(s2, EventEpochLost, "n1", CauseLeaseUnreadable)
			e.LeaseUnreadable = true
			s2 = applied(t, s2, e, t0)
			if s2.Phase == PhaseFailed || s2.Desired != DesiredRun {
				t.Fatalf("unreadable lease: phase %s desired %s", s2.Phase, s2.Desired)
			}
		})
	}
}

func TestFaultRow9_CatalogGuardIsPR5(t *testing.T) {
	// F7 / fault row 9: a stale directory whose newest sealed segment disagrees
	// with the catalog must not become task_chain.owner. That comparison is
	// PR-5. PR-1's ChainVerified guard is still invariant 4 (the directory
	// read succeeded). This test names the row so PR-5 can flip the assertion.
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, later)
	res := step(t, s, Event{Kind: EventChainVerified, Node: "n2", ReadOK: true}, later)
	if res.Decision != DecisionApplied || res.State.Chain.Owner != "n2" {
		t.Fatalf("PR-1 guard rejected a successful read: %s owner %s", res.Decision, res.State.Chain.Owner)
	}
}

func TestFaultRow10_ThreeNodesOneDirectory(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", false, t0), peer("n3", false, t0)})
	start := s.Lease.Epoch
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	for _, id := range []string{"n2", "n3"} {
		s = applied(t, s, Event{Kind: EventAcquire, Node: id}, later)
		s = applied(t, s, heldCause(s, EventTailUnreadable, id, CauseSegmentNotOnWorker), later)
	}
	if s.Lease.Epoch-start != 2 {
		t.Fatalf("wasted epoch delta %d, want 2", s.Lease.Epoch-start)
	}
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n1"}, later)
	s = applied(t, s, Held(s, EventDumpReady, "n1"), later)
	if s.Phase != PhaseRunning || s.Lease.Owner != "n1" {
		t.Fatalf("reader: phase %s owner %s", s.Phase, s.Lease.Owner)
	}
}

func TestFaultRow11_StopStartDeleteAndDeadHolder(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	epoch := s.Lease.Epoch
	stopped := applied(t, s, Event{Kind: EventOperatorStop, Node: "n1"}, t0)
	res := step(t, stopped, Event{Kind: EventAcquire, Node: "n2"}, t0)
	if res.Decision == DecisionApplied || res.State.Lease.Epoch != epoch {
		t.Fatal("claim did not roll back")
	}
	res = step(t, stopped, Event{Kind: EventOperatorStart, Node: "n2"}, t0)
	if res.HTTP != 409 || res.State.RowGeneration != stopped.RowGeneration {
		t.Fatal("start on STOPPING wrote the row")
	}
	running := boot(t, "n1", t0, nil)
	spec := running.SpecRevision
	res = step(t, running, Event{Kind: EventOperatorStart, Node: "n9"}, t0)
	if res.HTTP != 204 || res.State.SpecRevision != spec {
		t.Fatalf("repeat start http %d spec %d", res.HTTP, res.State.SpecRevision)
	}
	res = step(t, running, Event{Kind: EventOperatorDelete, Node: "n9"}, t0)
	if res.HTTP != 204 || res.State.Phase != PhaseStopping || !res.State.PendingDelete || res.State.Lease.Owner != "n1" {
		t.Fatalf("delete: phase %s pending %v owner %s", res.State.Phase, res.State.PendingDelete, res.State.Lease.Owner)
	}
	if step(t, res.State, Event{Kind: EventOperatorStart, Node: "n1"}, t0).HTTP != 409 {
		t.Fatal("start after pending_delete")
	}
	dead := expireLease(res.State, t0.Add(time.Hour))
	done := applied(t, dead, Event{Kind: EventStopExpired, Node: "n2"}, t0.Add(time.Hour))
	if !done.Deleted {
		t.Fatal("StopExpired left the row")
	}
	for _, in := range step(t, dead, Event{Kind: EventStopExpired, Node: "n2"}, t0.Add(time.Hour)).Intents {
		if in.Kind == IntentTombstone {
			return
		}
	}
	t.Fatal("per_node delete did not leave a tombstone")
}

func TestFaultRow12_LegacyUpdateDoesNotBumpGeneration(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	gen := s.RowGeneration
	s = applied(t, s, Event{Kind: EventLegacyUpdate}, t0)
	if s.RowGeneration != gen || s.Phase != PhaseRunning || !findOpen(s, FindingMixedVersion, "_cluster") {
		t.Fatalf("gen %d->%d phase %s mixed %v", gen, s.RowGeneration, s.Phase, findOpen(s, FindingMixedVersion, "_cluster"))
	}
	s = renewLease(t, s, t0)
	s = applied(t, s, Held(s, EventDumpReady, "n1"), t0)
	if s.Phase != PhaseRunning {
		t.Fatal("renew or dump was blocked by the trigger")
	}
}

func TestFaultRow13_ClockSkewAllowsEarlyClaim(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	old := s.Lease.Epoch
	// The clock jumps past TTL. Comparisons use the passed now.
	skew := t0.Add(s.Params.LeaseTTL + time.Second)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, skew)
	if s.Lease.Epoch != old+1 || s.Lease.Owner != "n2" {
		t.Fatalf("epoch %d owner %s", s.Lease.Epoch, s.Lease.Owner)
	}
	res := step(t, s, Event{Kind: EventDumpReady, Node: "n1", Epoch: old, RowGeneration: s.RowGeneration}, skew)
	if res.Decision != DecisionDiscarded || res.State.Phase == PhaseFailed || res.State.Desired == DesiredStop {
		t.Fatalf("old epoch: %s phase %s desired %s", res.Decision, res.State.Phase, res.State.Desired)
	}
}

func TestFaultRow14_IncompleteReadDoesNotCloseUnreadable(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	s = applied(t, s, Event{Kind: EventChainVerified, Node: "n1", ReadOK: true, Unreadable: true}, t0)
	if !findOpen(s, FindingUnreadable, s.TaskID) {
		t.Fatal("SEGMENT_UNREADABLE did not open")
	}
	res := step(t, s, Event{Kind: EventChainVerified, Node: "n1", ReadOK: true, Incomplete: true}, t0)
	if res.Decision == DecisionApplied || !findOpen(res.State, FindingUnreadable, s.TaskID) {
		t.Fatal("incomplete read closed SEGMENT_UNREADABLE")
	}
	for _, f := range res.Closed {
		if f.Name == FindingUnreadable {
			t.Fatal("closed")
		}
	}
}

func TestFaultRow15_DiskFullIsEpochLostWithCap(t *testing.T) {
	for _, cause := range []Cause{CauseENOSPC, CauseEROFS} {
		t.Run(string(cause), func(t *testing.T) {
			s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
			s = applied(t, s, heldCause(s, EventEpochLost, "n1", cause), t0)
			if s.Phase == PhaseFailed || s.Desired != DesiredRun || s.Phase != PhaseLeaseDegraded {
				t.Fatalf("phase %s desired %s", s.Phase, s.Desired)
			}
			if s.Refusal["n1"].OpenEnded {
				t.Fatal("disk full wrote a NULL refusal")
			}
			// Three losses escalate the warning.
			when := s.Refusal["n1"].Until
			s = applied(t, s, Event{Kind: EventAcquire, Node: "n1"}, when)
			s = applied(t, s, heldCause(s, EventEpochLost, "n1", cause), when)
			when = s.Refusal["n1"].Until
			s = applied(t, s, Event{Kind: EventAcquire, Node: "n1"}, when)
			s = applied(t, s, heldCause(s, EventEpochLost, "n1", cause), when)
			if !findOpen(s, FindingEpochLost, "n1") || HealthCode(s, when) != 503 {
				t.Fatal("third EpochLost did not go CRITICAL")
			}
			for _, f := range s.Findings {
				if f.Name == FindingEpochLost && f.Subject == "n1" && f.Severity != SeverityCritical {
					t.Fatalf("severity %s", f.Severity)
				}
			}
			// A peer that can write takes over, and DumpReady clears every refusal.
			s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, when)
			s = applied(t, s, Held(s, EventDumpReady, "n2"), when)
			if s.Phase != PhaseRunning || len(s.Refusal) != 0 {
				t.Fatalf("phase %s refusals %d", s.Phase, len(s.Refusal))
			}
		})
	}
}

func TestFaultRow16_GraceHoleStaysClaimable(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	epoch := s.Lease.Epoch
	// Grace is exhausted, TTL is not. The lease row is still the owner's until TTL,
	// but a WRITE_FENCED / lost lease with no successor yields into the claim set.
	s = applied(t, s, heldCause(s, EventEpochLost, "n1", CauseWriteFenced), t0)
	if s.Desired == DesiredStop || !InClaimSet(s, t0) || !Covered(s, t0) {
		t.Fatalf("phase %s desired %s claim %v", s.Phase, s.Desired, InClaimSet(s, t0))
	}
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, t0)
	if s.Lease.Epoch != epoch+1 || s.Phase != PhaseStarting {
		t.Fatalf("epoch %d phase %s", s.Lease.Epoch, s.Phase)
	}
}

func TestFaultRow17_UploadsDoNotChangeTheTransition(t *testing.T) {
	run := func(uploads bool) State {
		s := boot(t, "n1", t0, []Node{peer("n2", false, t0)})
		s.Params.Uploads = uploads
		later := t0.Add(time.Hour)
		s = expireLease(s, later)
		s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, later)
		return applied(t, s, heldCause(s, EventTailUnreadable, "n2", CauseSegmentNotOnWorker), later)
	}
	on := run(true)
	off := run(false)
	if on.Phase != off.Phase || on.Desired != off.Desired || on.Refusal["n2"].OpenEnded != off.Refusal["n2"].OpenEnded || on.Chain.Owner != off.Chain.Owner {
		t.Fatalf("uploads changed the transition: %+v vs %+v", on.Phase, off.Phase)
	}
}
