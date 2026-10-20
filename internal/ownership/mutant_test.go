// Package ownership is the pure task-ownership transition from ADR 0011.
// input: Step, Check, and CheckStep
// output: one named assertion for each mutant the safety walk used to miss
// pos: the guards at a live lease, a stale epoch, ChainOwnerGone, and invariant 7
// note: if this file changes, update this header and module README.md.
package ownership

import (
	"testing"
	"time"
)

func TestMutantM19_StartCannotStealLiveLease(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	s = applied(t, s, heldCause(s, EventRetryableError, "n1", CauseLockWait), t0)
	if s.Phase != PhaseRetryBackoff || !LeaseLive(s, t0) {
		t.Fatalf("phase %s live %v", s.Phase, LeaseLive(s, t0))
	}
	res := step(t, s, Event{Kind: EventOperatorStart, Node: "n2"}, t0)
	if res.HTTP != 409 || res.State.Lease.Owner != "n1" || res.State.Lease.Epoch != s.Lease.Epoch {
		t.Fatalf("http %d owner %s epoch %d", res.HTTP, res.State.Lease.Owner, res.State.Lease.Epoch)
	}
	stolen := s.clone()
	stolen.Lease.Owner = "n2"
	stolen.Lease.Epoch++
	if v := CheckStep(s, stolen, Event{Kind: EventOperatorStart, Node: "n2"}, Result{Decision: DecisionApplied, State: stolen}, t0); !hasInv(v, "3") {
		t.Fatalf("stolen live lease was not invariant 3: %+v", v)
	}
}

func TestMutantM10_StopExpiredRequiresDeadLease(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	s = applied(t, s, Event{Kind: EventOperatorStop, Node: "n1"}, t0)
	if !LeaseLive(s, t0) || s.Phase != PhaseStopping {
		t.Fatalf("phase %s live %v", s.Phase, LeaseLive(s, t0))
	}
	res := step(t, s, Event{Kind: EventStopExpired, Node: "n2"}, t0)
	if res.Decision == DecisionApplied || res.State.Phase != PhaseStopping || res.State.Lease.Owner != "n1" {
		t.Fatalf("decision %s phase %s owner %s", res.Decision, res.State.Phase, res.State.Lease.Owner)
	}
}

func TestMutantM30_StopConfirmedChecksEpoch(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	s = applied(t, s, Event{Kind: EventOperatorStop, Node: "n1"}, t0)
	stale := Held(s, EventStopConfirmed, "n1")
	stale.Epoch--
	res := step(t, s, stale, t0)
	if res.Decision == DecisionApplied || res.State.Phase != PhaseStopping || res.State.RowGeneration != s.RowGeneration {
		t.Fatalf("decision %s phase %s gen %d", res.Decision, res.State.Phase, res.State.RowGeneration)
	}
}

func TestMutantM26_RenewChecksEpoch(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	expire := s.Lease.ExpireAt
	stale := Event{Kind: EventRenew, Node: "n1", Epoch: s.Lease.Epoch - 1}
	res := step(t, s, stale, t0)
	if res.Decision == DecisionApplied || !res.State.Lease.ExpireAt.Equal(expire) {
		t.Fatalf("decision %s expire %s", res.Decision, res.State.Lease.ExpireAt)
	}
}

func TestMutantM29_ChainOwnerGoneOnlyFromDegradedTail(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	goneAt := t0.Add(EffectiveNodeGone(s.Params) + time.Second)
	res := step(t, s, Event{Kind: EventChainOwnerGone, Node: "_health"}, goneAt)
	if res.Decision == DecisionApplied || res.State.Phase != PhaseRunning || res.State.FailedReason != "" {
		t.Fatalf("decision %s phase %s reason %s", res.Decision, res.State.Phase, res.State.FailedReason)
	}
	// A hand-built RUNNING → FAILED transition is invariant 2, even if Step refused it.
	bad := s.clone()
	bad.Phase = PhaseFailed
	bad.FailedReason = ReasonChainOwnerGone
	bad.FailedAt = goneAt
	if v := CheckStep(s, bad, Event{Kind: EventChainOwnerGone, Node: "_health"}, Result{Decision: DecisionApplied, State: bad}, goneAt); !hasInv(v, "2") {
		t.Fatalf("RUNNING to FAILED was not invariant 2: %+v", v)
	}
}

func TestMutantM33_Invariant7IgnoresNoProgressFlag(t *testing.T) {
	s := New()
	s.Phase = PhaseLeaseDegraded
	s.Desired = DesiredRun
	s.Lease = Lease{Present: true, ExpireAt: t0}
	s.UnownedSince = t0
	s.NoProgress = false
	s.Findings = nil
	at := t0.Add(s.Params.Sweep)
	if !ProgressOverdue(s, at) {
		t.Fatal("clock did not call the stall overdue")
	}
	if v := Check(s, at); !hasInv(v, "7") {
		t.Fatalf("invariant 7 trusted the cleared flag: %+v", v)
	}
	if v := Check(s, t0.Add(s.Params.Sweep-time.Second)); hasInv(v, "7") {
		t.Fatalf("invariant 7 fired before one sweep: %+v", v)
	}
}

func TestMutantM34_NoProgressOpensAtOneSweep(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	s = applied(t, s, heldCause(s, EventEpochLost, "n1", CauseENOENT), later)
	if findOpen(s, FindingNoProgress, s.TaskID) {
		t.Fatal("opened at the moment the owner left")
	}
	early := later.Add(s.Params.Sweep - time.Second)
	sEarly := applied(t, s, Event{Kind: EventSweep, Node: "n2"}, early)
	if findOpen(sEarly, FindingNoProgress, s.TaskID) {
		t.Fatal("opened before one sweep")
	}
	at := later.Add(s.Params.Sweep)
	s = applied(t, s, Event{Kind: EventSweep, Node: "n2"}, at)
	if !findOpen(s, FindingNoProgress, s.TaskID) {
		t.Fatal("still closed after one sweep")
	}
}

func TestMutantM04_RefusalDelayDoubles(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	s.Params.Storage = StorageShared
	s = applied(t, s, heldCause(s, EventEpochLost, "n1", CauseEROFS), t0)
	first := s.Refusal["n1"].Until.Sub(t0)
	when := s.Refusal["n1"].Until
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n1"}, when)
	s = applied(t, s, heldCause(s, EventEpochLost, "n1", CauseEROFS), when)
	second := s.Refusal["n1"].Until.Sub(when)
	if second <= first {
		t.Fatalf("delay %s then %s, want a longer second wait", first, second)
	}
}

func TestMutantF03_FirstRefusalIsClaimInterval(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	s.Params.Storage = StorageShared
	if s.Params.ClaimInterval != 2*time.Second {
		t.Fatalf("claim interval %s", s.Params.ClaimInterval)
	}
	s = applied(t, s, heldCause(s, EventEpochLost, "n1", CauseEROFS), t0)
	first := s.Refusal["n1"].Until.Sub(t0)
	if first != s.Params.ClaimInterval {
		t.Fatalf("first refusal %s, want %s", first, s.Params.ClaimInterval)
	}
}

func TestMutantF04_LeaseTTLIsExact(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	want := t0.Add(s.Params.LeaseTTL)
	if !s.Lease.ExpireAt.Equal(want) {
		t.Fatalf("expire %s, want %s", s.Lease.ExpireAt, want)
	}
}

func TestMutantF09_StartClearsFailedReason(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	s = applied(t, s, heldCause(s, EventTerminalError, "n1", CauseSourceAuth), t0)
	if s.FailedReason == "" || s.Phase != PhaseFailed {
		t.Fatalf("phase %s reason %s", s.Phase, s.FailedReason)
	}
	s.RetryAttempt = 4
	s.LastError = "stuck"
	before := s.SpecRevision
	res := step(t, s, Event{Kind: EventOperatorStart, Node: "n1"}, t0)
	if res.State.FailedReason != "" || !res.State.FailedAt.IsZero() || res.State.RetryAttempt != 0 || res.State.LastError != "" {
		t.Fatalf("reason %q at %s attempt %d err %q", res.State.FailedReason, res.State.FailedAt, res.State.RetryAttempt, res.State.LastError)
	}
	if res.State.SpecRevision <= before {
		t.Fatalf("spec %d -> %d", before, res.State.SpecRevision)
	}
}

func TestMutantF11_SecondLockIsSameNodeInsideLockWait(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	s.Params.LockWait = time.Second
	s = applied(t, s, Event{Kind: EventLock, Node: "n1"}, t0)
	early := t0.Add(time.Second)
	stalled := applied(t, s, Event{Kind: EventMetaLockTimeout, Node: "n2"}, early)
	at := t0.Add(s.Params.IdleKill)
	once := applied(t, stalled, Event{Kind: EventLock, Node: "n2"}, at)
	if !once.MetaLockStalled || once.LockRecoveries != 1 {
		t.Fatalf("first take stalled %v recoveries %d", once.MetaLockStalled, once.LockRecoveries)
	}
	late := applied(t, once, Event{Kind: EventLock, Node: "n2"}, at.Add(s.Params.LockWait+time.Millisecond))
	if !late.MetaLockStalled || late.LockRecoveries != 1 {
		t.Fatalf("late take closed the stall: stalled %v recoveries %d", late.MetaLockStalled, late.LockRecoveries)
	}
	freed := t0.Add(2 * s.Params.IdleKill)
	other := applied(t, once, Event{Kind: EventLock, Node: "n1"}, freed)
	if !other.MetaLockStalled {
		t.Fatal("a different node closed META_LOCK_STALLED")
	}
}

func TestMutantF12_NoProgressBoundaryIsStrict(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	renewed := t0.Add(s.Params.NoProgressAfter - s.Params.LeaseTTL + time.Second)
	s = applied(t, s, Event{Kind: EventRenew, Node: "n1", Epoch: s.Lease.Epoch}, renewed)
	at := t0.Add(s.Params.NoProgressAfter)
	if !LeaseLive(s, at) {
		t.Fatal("fixture let the lease die on the boundary")
	}
	exact := applied(t, s, Event{Kind: EventSweep, Node: "n2"}, at)
	if findOpen(exact, FindingNoProgress, s.TaskID) {
		t.Fatal("TASK_NO_PROGRESS opened at exactly no_progress_after")
	}
	open := applied(t, s, Event{Kind: EventSweep, Node: "n2"}, at.Add(time.Nanosecond))
	if !findOpen(open, FindingNoProgress, s.TaskID) {
		t.Fatal("TASK_NO_PROGRESS stayed closed after no_progress_after")
	}
	hand := exact
	hand.Findings = nil
	hand.NoProgress = false
	if v := Check(hand, at); hasInv(v, "7") {
		t.Fatalf("checker fired on the boundary: %+v", v)
	}
	hand.NoProgress = false
	if v := Check(hand, at.Add(time.Nanosecond)); !hasInv(v, "7") {
		t.Fatal("checker stayed quiet after the boundary")
	}
}

func TestMutantF15_RetryUntilBoundaryIsStrict(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	s = applied(t, s, heldCause(s, EventRetryableError, "n1", CauseLockWait), t0)
	deadline := s.RetryUntil
	s.Lease.ExpireAt = deadline
	s.UnownedSince = deadline.Add(-s.Params.Sweep)
	s.Findings = nil
	s.NoProgress = false
	early := applied(t, s, Event{Kind: EventSweep, Node: "n2"}, deadline.Add(-time.Nanosecond))
	if findOpen(early, FindingNoProgress, s.TaskID) || !LegalRetry(s, deadline.Add(-time.Nanosecond)) {
		t.Fatal("retry window closed before retry_until")
	}
	at := applied(t, s, Event{Kind: EventSweep, Node: "n2"}, deadline)
	if !findOpen(at, FindingNoProgress, s.TaskID) || LegalRetry(s, deadline) {
		t.Fatal("retry window still open at retry_until")
	}
}

func TestMutantF16_NodeGoneBoundaryIsStrict(t *testing.T) {
	s := yieldTail(t, "n1", "n2")
	limit := EffectiveNodeGone(s.Params)
	s = heartbeat(s, "n1", s.TailSince)
	at := s.TailSince.Add(limit)
	res := step(t, s, Event{Kind: EventChainOwnerGone, Node: "_health"}, at)
	if res.Decision == DecisionApplied || res.State.Phase == PhaseFailed {
		t.Fatalf("gone at exactly node_gone_after: %s phase %s", res.Decision, res.State.Phase)
	}
	res = step(t, s, Event{Kind: EventChainOwnerGone, Node: "_health"}, at.Add(time.Nanosecond))
	if res.State.Phase != PhaseFailed || res.State.FailedReason != ReasonChainOwnerGone {
		t.Fatalf("not gone just after node_gone_after: phase %s reason %s", res.State.Phase, res.State.FailedReason)
	}
}

func TestMutantF02_ChainOwnerBackNeedsAFreshHeartbeat(t *testing.T) {
	s := yieldTail(t, "n1", "n2")
	goneAt := s.TailSince.Add(EffectiveNodeGone(s.Params) + time.Second)
	s = heartbeat(s, "n1", s.TailSince)
	s = applied(t, s, Event{Kind: EventChainOwnerGone, Node: "_health"}, goneAt)
	backAt := goneAt.Add(time.Second)
	s = heartbeat(s, "n1", time.Time{})
	s = applied(t, s, Event{Kind: EventChainVerified, Node: "n1", ReadOK: true}, backAt)
	res := step(t, s, Event{Kind: EventChainOwnerBack, Node: "n1"}, backAt)
	if res.Decision == DecisionApplied || res.State.Phase != PhaseFailed {
		t.Fatalf("back with a zero heartbeat: %s phase %s", res.Decision, res.State.Phase)
	}
}

func TestMutantF20_ChainOwnerBackAllowsTheExactBoundary(t *testing.T) {
	s := yieldTail(t, "n1", "n2")
	goneAt := s.TailSince.Add(EffectiveNodeGone(s.Params) + time.Second)
	s = heartbeat(s, "n1", s.TailSince)
	s = applied(t, s, Event{Kind: EventChainOwnerGone, Node: "_health"}, goneAt)
	backAt := goneAt.Add(time.Second)
	s = heartbeat(s, "n1", backAt.Add(-EffectiveNodeGone(s.Params)))
	s = applied(t, s, Event{Kind: EventChainVerified, Node: "n1", ReadOK: true}, backAt)
	res := step(t, s, Event{Kind: EventChainOwnerBack, Node: "n1"}, backAt)
	if res.State.Phase != PhaseLeaseDegraded || res.State.FailedReason != "" {
		t.Fatalf("back at exactly node_gone_after: phase %s reason %s", res.State.Phase, res.State.FailedReason)
	}
}

func TestMutantM13_RetryableErrorOnDeadLeaseIsEpochLost(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	later := t0.Add(s.Params.LeaseTTL)
	s = expireLease(s, later)
	res := step(t, s, heldCause(s, EventRetryableError, "n1", CauseLockWait), later)
	if res.State.Phase == PhaseRetryBackoff || res.State.Phase == PhaseFailed || res.State.Desired != DesiredRun {
		t.Fatalf("phase %s desired %s", res.State.Phase, res.State.Desired)
	}
	if res.State.Phase != PhaseLeaseDegraded {
		t.Fatalf("phase %s", res.State.Phase)
	}
}

func TestMutantM15_VerifiedChainOwnerDoesNotTailYield(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	res := step(t, s, heldCause(s, EventTailUnreadable, "n1", CauseSegmentNotOnWorker), t0)
	if res.Decision == DecisionApplied || res.State.Phase != PhaseRunning || !res.State.TailSince.IsZero() {
		t.Fatalf("decision %s phase %s tail %s", res.Decision, res.State.Phase, res.State.TailSince)
	}
}

func TestMutantM16_StallClosesOnSecondLockNotFirstSweep(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	s = applied(t, s, Event{Kind: EventLock, Node: "n1"}, t0)
	early := t0.Add(time.Second)
	res := step(t, s, Event{Kind: EventMetaLockTimeout, Node: "n2"}, early)
	if !res.State.Lock.Held || res.State.Lock.HeldBy != "n1" {
		t.Fatal("MetaLockTimeout cleared the lock before idle_kill")
	}
	if !res.State.MetaLockStalled {
		t.Fatal("stall was not recorded")
	}
	swept := applied(t, res.State, Event{Kind: EventSweep, Node: "n2"}, early)
	swept = applied(t, swept, Event{Kind: EventSweep, Node: "n2"}, early.Add(time.Second))
	if !swept.MetaLockStalled {
		t.Fatal("two sweeps closed META_LOCK_STALLED")
	}
	stolen := step(t, res.State, Event{Kind: EventLock, Node: "n2"}, early)
	if stolen.Decision == DecisionApplied || stolen.State.Lock.HeldBy != "n1" || !stolen.State.MetaLockStalled {
		t.Fatalf("observer took the stalled holder's lock: decision %s holder %s", stolen.Decision, stolen.State.Lock.HeldBy)
	}
	at := t0.Add(s.Params.IdleKill)
	once := applied(t, res.State, Event{Kind: EventLock, Node: "n2"}, at)
	if !once.MetaLockStalled || once.Lock.HeldBy != "n2" || once.LockRecoveries < 1 {
		t.Fatalf("first take closed the stall or missed the lock: stalled %v holder %s", once.MetaLockStalled, once.Lock.HeldBy)
	}
	twice := applied(t, once, Event{Kind: EventLock, Node: "n2"}, at.Add(time.Second))
	if twice.MetaLockStalled || HealthCode(twice, at.Add(time.Second)) == 503 && twice.HealthBlocked {
		t.Fatalf("second take left stalled %v blocked %v", twice.MetaLockStalled, twice.HealthBlocked)
	}
}

func TestMutantM22_TailTimestampDoesNotMove(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	s.Params.Storage = StorageShared
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, later)
	s = applied(t, s, heldCause(s, EventTailUnreadable, "n2", CauseSegmentNotOnWorker), later)
	first := s.TailSince
	when := s.Refusal["n2"].Until
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, when)
	s = applied(t, s, heldCause(s, EventTailUnreadable, "n2", CauseSegmentNotOnWorker), when)
	if !s.TailSince.Equal(first) {
		t.Fatalf("tail moved from %s to %s", first, s.TailSince)
	}
}

func TestMutantM27_RetryBudgetBecomesEpochLost(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	for i := 0; i < RetryBudget; i++ {
		s = applied(t, s, heldCause(s, EventRetryableError, "n1", CauseLockWait), t0)
		if s.Phase != PhaseRetryBackoff {
			t.Fatalf("attempt %d phase %s", i, s.Phase)
		}
	}
	res := step(t, s, heldCause(s, EventRetryableError, "n1", CauseLockWait), t0)
	if res.State.Phase == PhaseRetryBackoff || res.State.Phase == PhaseFailed {
		t.Fatalf("past budget: %s", res.State.Phase)
	}
	if res.State.Phase != PhaseLeaseDegraded {
		t.Fatalf("phase %s", res.State.Phase)
	}
}

func TestMutantM02_PendingDeleteRejectsStart(t *testing.T) {
	s := New()
	s.Phase = PhaseStopped
	s.Desired = DesiredStop
	s.StoppedByOperator = true
	s.PendingDelete = true
	s.Lease.Owner = ""
	res := step(t, s, Event{Kind: EventOperatorStart, Node: "n1"}, t0)
	if res.HTTP != 409 || res.State.Phase != PhaseStopped || !res.State.PendingDelete {
		t.Fatalf("http %d phase %s pending %v", res.HTTP, res.State.Phase, res.State.PendingDelete)
	}
}

func TestMutantM05_StopOnCreatedIsRejected(t *testing.T) {
	s := New()
	res := step(t, s, Event{Kind: EventOperatorStop, Node: "n1"}, t0)
	if res.HTTP != 409 || res.State.Phase != PhaseCreated || res.State.Desired != DesiredRun {
		t.Fatalf("http %d phase %s desired %s", res.HTTP, res.State.Phase, res.State.Desired)
	}
}

func TestMutantM08_DumpReadyWhileStoppingIsDiscarded(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	s = applied(t, s, Event{Kind: EventOperatorStop, Node: "n1"}, t0)
	res := step(t, s, Held(s, EventDumpReady, "n1"), t0)
	if res.Decision == DecisionApplied || res.State.Phase != PhaseStopping || res.State.Desired != DesiredStop {
		t.Fatalf("decision %s phase %s desired %s", res.Decision, res.State.Phase, res.State.Desired)
	}
}

func TestMutantM24_SameOwnerExpiredLeaseKeepsEpoch(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	epoch := s.Lease.Epoch
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n1"}, later)
	if s.Lease.Epoch != epoch || s.Phase != PhaseStarting || s.Lease.Owner != "n1" {
		t.Fatalf("epoch %d -> %d phase %s owner %s", epoch, s.Lease.Epoch, s.Phase, s.Lease.Owner)
	}
}

func TestLeaseUnreadableDoesNotBecomeFailedOrRunning(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	term := heldCause(s, EventTerminalError, "n1", CauseSourceAuth)
	term.LeaseUnreadable = true
	got := step(t, s, term, t0)
	if got.State.Phase == PhaseFailed || got.State.Phase != PhaseLeaseDegraded || got.State.Desired != DesiredRun {
		t.Fatalf("terminal with unreadable lease: phase %s desired %s", got.State.Phase, got.State.Desired)
	}
	ready := Held(s, EventDumpReady, "n1")
	ready.LeaseUnreadable = true
	got = step(t, s, ready, t0)
	if got.State.Phase == PhaseRunning || got.State.Lease.Owner != "" {
		t.Fatalf("DumpReady with unreadable lease: phase %s owner %s", got.State.Phase, got.State.Lease.Owner)
	}
}
