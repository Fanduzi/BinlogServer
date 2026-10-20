// Package ownership is the pure task-ownership transition from ADR 0011.
// input: Step, under the three fairness assumptions in ADR 0011 §4.1
// output: liveness (a), (b), and (c), with K counting wasted per_node claims (N9)
// pos: the bounded fair search. Random prefixes in invariant_test.go are safety only
// note: if this file changes, update this header and module README.md.
package ownership

import (
	"math/rand"
	"testing"
	"time"
)

// Check method (ADR 0011 §4.1, review N9):
//
// Safety is a seeded random walk (seed 20261020) plus a bounded exhaustive
// walk of a small alphabet. Every prefix must satisfy Check and CheckStep.
// Those walks are not asked to reach RUNNING.
//
// Liveness is a bounded forward search from each state the safety walk
// reaches, and from the hand-built fixtures below (including "directory
// gone, heartbeat alive"). The three fairness assumptions are premises:
//
//  1. A node that is not stopped eventually takes a step.
//  2. A task_leases row lock is released within one idle_kill step. The
//     search fires MetaLockTimeout on that step, not after the lease TTL.
//  3. A node that can read the directory, whose open-ended refusal is clear,
//     and which is not stopped, is the reader the search schedules. An
//     expiring refusal costs one wait step, up to refusal_retry_cap.
//
// One wasted per_node claim is Acquire plus the TailUnreadable (or EpochLost,
// when the node is the verified chain owner) that releases it. A live owner
// that cannot read yields with that same release and no extra Acquire. Either
// way it counts as one step. N in K is how many such claims are still possible
// at the start of the search, which is at most the number of nodes.
//
//	K = claim + DumpReady + N
//	    + 1 if a row lock is held          (idle_kill)
//	    + 1 if the live owner is stopped   (wait out lease_expire_at, the TTL term of the RTO formula)
//	    + 1 if the reader has an expiring refusal
//	    + 1 if the reader is inside a legal retry window
//	    + 1 or 2 if the row is FAILED/CHAIN_OWNER_GONE and the chain owner can read
//
// ChainOwnerBack is not taken when the directory cannot be read. That fixture
// must stay FAILED. Terminal FAILED (auth, log_bin, legacy) waits for
// OperatorStart and is outside (a), because claim does not pull FAILED.
// A node whose refusal is NULL is not eligible until some other DumpReady
// or an OperatorStart deletes it; if no eligible reader remains, (a) does
// not apply.

func TestLivenessBudgetCountsWastedClaims(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", false, t0), peer("n3", false, t0)})
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	// Drop the live owner so the claim set is open and both peers can waste a claim.
	s = applied(t, s, heldCause(s, EventEpochLost, "n1", CauseENOENT), later)
	n := wastedPossible(s, later)
	if n != 2 {
		t.Fatalf("wasted possible %d, want 2", n)
	}
	got, _, steps, wasted := fair(s, later)
	k := budget(s, later)
	if got.Phase != PhaseRunning || got.Lease.Owner != "n1" {
		t.Fatalf("phase %s owner %s steps %d K %d wasted %d", got.Phase, got.Lease.Owner, steps, k, wasted)
	}
	if wasted != 2 || steps > k {
		t.Fatalf("steps %d wasted %d K %d", steps, wasted, k)
	}
	// N9: K without the N wasted claims is too small for this schedule.
	if steps <= k-n {
		t.Fatalf("schedule fit in K-N (%d <= %d); N is not actually in the budget", steps, k-n)
	}
}

func TestLivenessDumpReadyClosesTailWithinSweep(t *testing.T) {
	s := yieldTail(t, "n1", "n2")
	if !findOpen(s, FindingTail, s.TaskID) {
		t.Fatal("tail finding was not open")
	}
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n1"}, s.TailSince)
	res := step(t, s, Held(s, EventDumpReady, "n1"), s.TailSince)
	if findOpen(res.State, FindingTail, s.TaskID) {
		t.Fatal("DumpReady left the tail finding open")
	}
	for _, f := range res.Closed {
		if f.Name == FindingTail {
			goto swept
		}
	}
	t.Fatal("DumpReady did not close the tail finding")
swept:
	after := applied(t, res.State, Event{Kind: EventSweep, Node: "n1"}, s.TailSince.Add(s.Params.Sweep))
	if findOpen(after, FindingTail, s.TaskID) {
		t.Fatal("sweep reopened the tail finding")
	}
}

func TestLivenessPerNodeClaimsAtMostOnce(t *testing.T) {
	const n = 4
	s := boot(t, "n1", t0, nil)
	s.Nodes = []Node{{ID: "n1", CanRead: true, HeartbeatAt: t0}}
	for i := 2; i <= n; i++ {
		s.Nodes = append(s.Nodes, Node{ID: nodeID(i - 1), CanRead: false, HeartbeatAt: t0})
	}
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	start := s.Lease.Epoch
	for _, node := range s.Nodes {
		if node.CanRead {
			continue
		}
		s = applied(t, s, Event{Kind: EventAcquire, Node: node.ID}, later)
		s = applied(t, s, heldCause(s, EventTailUnreadable, node.ID, CauseSegmentNotOnWorker), later)
		if step(t, s, Event{Kind: EventAcquire, Node: node.ID}, later.Add(time.Hour)).Decision == DecisionApplied {
			t.Fatalf("%s claimed twice while the tail was unread", node.ID)
		}
		if !s.Refusal[node.ID].OpenEnded {
			t.Fatalf("%s refusal is not NULL", node.ID)
		}
	}
	if s.Lease.Epoch-start != int64(n-1) {
		t.Fatalf("epoch delta %d, want %d", s.Lease.Epoch-start, n-1)
	}
	// shared refusals are never NULL, and they keep a finite deadline at the cap.
	shared := boot(t, "n1", t0, []Node{peer("n2", true, t0)})
	shared.Params.Storage = StorageShared
	shared = applied(t, shared, heldCause(shared, EventEpochLost, "n1", CauseEROFS), t0)
	if shared.Refusal["n1"].OpenEnded || shared.Refusal["n1"].Until.IsZero() {
		t.Fatal("shared EpochLost wrote NULL")
	}
	top := t0
	for i := 0; i < 12; i++ {
		when := shared.Refusal["n1"].Until
		if when.Sub(top) > shared.Params.RefusalRetryCap {
			t.Fatalf("gap %s exceeds the cap", when.Sub(top))
		}
		top = when
		shared = applied(t, shared, Event{Kind: EventAcquire, Node: "n1"}, when)
		shared = applied(t, shared, heldCause(shared, EventEpochLost, "n1", CauseEROFS), when)
		if shared.Refusal["n1"].OpenEnded {
			t.Fatal("cap wrote NULL")
		}
	}
}

func TestLivenessDirectoryGoneHeartbeatAliveStaysFailed(t *testing.T) {
	s := yieldTail(t, "n1", "n2")
	goneAt := s.TailSince.Add(EffectiveNodeGone(s.Params) + time.Second)
	s = heartbeat(s, "n1", s.TailSince)
	s = applied(t, s, Event{Kind: EventChainOwnerGone, Node: "_health"}, goneAt)
	epoch := s.Lease.Epoch
	// The owner is alive and the directory is not readable.
	for i := range s.Nodes {
		if s.Nodes[i].ID == "n1" {
			s.Nodes[i].CanRead = false
			s.Nodes[i].HeartbeatAt = goneAt.Add(time.Second)
			s.Nodes[i].Stopped = false
		}
	}
	res := step(t, s, Event{Kind: EventChainVerified, Node: "n1", ReadOK: false}, goneAt.Add(time.Second))
	res = step(t, res.State, Event{Kind: EventChainOwnerBack, Node: "n1"}, goneAt.Add(time.Second))
	if res.Decision == DecisionApplied || res.State.Phase != PhaseFailed || res.State.Lease.Epoch != epoch {
		t.Fatalf("decision %s phase %s epoch %d", res.Decision, res.State.Phase, res.State.Lease.Epoch)
	}
}

func TestLivenessFairSearchFromRandomStates(t *testing.T) {
	const seed = 20261020
	checked := 0
	for seq := 0; seq < 20; seq++ {
		rng := rand.New(rand.NewSource(seed + int64(seq) + 100))
		s := New()
		s.Nodes = []Node{
			{ID: "n1", CanRead: true, HeartbeatAt: t0},
			{ID: "n2", CanRead: rng.Intn(2) == 0, HeartbeatAt: t0},
			{ID: "n3", CanRead: false, HeartbeatAt: t0},
		}
		now := t0
		for i := 0; i < 12; i++ {
			e := randomEvent(rng, s, 3)
			now = now.Add(time.Duration(1+rng.Intn(10)) * time.Second)
			res := Step(s, e, now)
			if v := CheckStep(s, res.State, e, res, now); len(v) > 0 {
				t.Fatalf("safety seq %d: %+v", seq, v)
			}
			s = res.State
			if !livenessPremise(s, now) {
				continue
			}
			got, at, steps, _ := fair(s, now)
			if got.Phase != PhaseRunning {
				t.Fatalf("seq %d step %d: stayed %s owner %s failed %s steps %d K %d", seq, i, got.Phase, got.Lease.Owner, got.FailedReason, steps, budget(s, now))
			}
			if steps > budget(s, now) {
				t.Fatalf("seq %d exceeded K: steps %d K %d", seq, steps, budget(s, now))
			}
			if findOpen(s, FindingTail, s.TaskID) && findOpen(got, FindingTail, s.TaskID) {
				t.Fatal("tail finding still open after DumpReady")
			}
			_ = at
			checked++
		}
	}
	if checked < 10 {
		t.Fatalf("premise held for only %d states", checked)
	}
}

func livenessPremise(s State, now time.Time) bool {
	if s.Deleted || s.Desired != DesiredRun || s.Phase == PhaseCreated || s.PendingDelete {
		return false
	}
	if s.Phase == PhaseFailed && s.FailedReason != ReasonChainOwnerGone {
		return false
	}
	if s.Phase == PhaseFailed && !eligibleNode(s, s.Chain.Owner, now) {
		return false
	}
	return eligibleReader(s, now) != ""
}

func eligibleNode(s State, id string, now time.Time) bool {
	n := findNode(s, id)
	if n == nil || !n.CanRead || n.Stopped {
		return false
	}
	if r, ok := s.Refusal[id]; ok && r.OpenEnded {
		return false
	}
	_ = now
	return true
}

func eligibleReader(s State, now time.Time) string {
	if owner := s.Lease.Owner; LeaseLive(s, now) && eligibleNode(s, owner, now) {
		return owner
	}
	for _, n := range s.Nodes {
		if eligibleNode(s, n.ID, now) {
			return n.ID
		}
	}
	return ""
}

func findNode(s State, id string) *Node {
	for i := range s.Nodes {
		if s.Nodes[i].ID == id {
			return &s.Nodes[i]
		}
	}
	return nil
}

func wastedPossible(s State, now time.Time) int {
	if !InClaimSet(s, now) && !(LeaseLive(s, now) && ownerStopped(s)) {
		// Peers cannot claim while a live owner will finish the run.
		if LeaseLive(s, now) && eligibleNode(s, s.Lease.Owner, now) {
			return 0
		}
	}
	n := 0
	for _, node := range s.Nodes {
		if node.CanRead || node.Stopped || RefusalBlocks(s, node.ID, now) {
			continue
		}
		n++
	}
	return n
}

func ownerStopped(s State) bool {
	n := findNode(s, s.Lease.Owner)
	return n != nil && n.Stopped
}

func budget(s State, now time.Time) int {
	k := 2 + wastedPossible(s, now) // claim + DumpReady + N
	if s.Lock.Held {
		k++
	}
	if LeaseLive(s, now) && ownerStopped(s) {
		k++
	}
	reader := eligibleReader(s, now)
	if r, ok := s.Refusal[reader]; ok && !r.OpenEnded && r.Until.After(now) {
		k++
	}
	if LegalRetry(s, now) && s.Lease.Owner == reader {
		k++
	}
	if s.Phase == PhaseFailed && s.FailedReason == ReasonChainOwnerGone {
		k += 2
	}
	if s.Phase == PhaseRunning && LeaseLive(s, now) && eligibleNode(s, s.Lease.Owner, now) && !s.Lock.Held {
		// Already running under an eligible owner: no claim and no dump required.
		return wastedPossible(s, now)
	}
	return k
}

// fair runs one schedule. It returns the state, the clock, the step count,
// and how many wasted per_node claims it spent.
func fair(start State, now time.Time) (State, time.Time, int, int) {
	s := start
	k := budget(s, now)
	reader := eligibleReader(s, now)
	steps := 0
	wasted := 0
	for steps <= k+2 {
		refreshHeartbeats(&s, now)
		if runningNow(s, now) {
			s = Step(s, Event{Kind: EventSweep, Node: reader}, now).State
			return s, now, steps, wasted
		}
		if steps > k {
			break
		}
		if s.Lock.Held {
			if now.Before(s.Lock.Since.Add(s.Params.IdleKill)) {
				now = s.Lock.Since.Add(s.Params.IdleKill)
			}
			s = Step(s, Event{Kind: EventMetaLockTimeout, Node: reader}, now).State
			steps++
			continue
		}
		if s.Phase == PhaseFailed && s.FailedReason == ReasonChainOwnerGone {
			owner := s.Chain.Owner
			if !s.Chain.VerifiedAt.After(s.FailedAt) {
				when := now
				if !when.After(s.FailedAt) {
					when = s.FailedAt.Add(time.Millisecond)
				}
				now = when
				res := Step(s, Event{Kind: EventChainVerified, Node: owner, ReadOK: true}, now)
				if res.Decision != DecisionApplied {
					break
				}
				s = res.State
				steps++
				continue
			}
			res := Step(s, Event{Kind: EventChainOwnerBack, Node: owner}, now)
			if res.Decision != DecisionApplied {
				break
			}
			s = res.State
			steps++
			continue
		}
		if LeaseLive(s, now) && ownerStopped(s) {
			if now.Before(s.Lease.ExpireAt) {
				now = s.Lease.ExpireAt
				steps++
				continue
			}
		}
		// A live owner that cannot read the directory yields. That release is
		// one wasted claim. A stopped owner waits out the TTL instead.
		if LeaseLive(s, now) && s.Lease.Owner != reader {
			if next, ok := yieldHolder(s, now); ok {
				s = next
				wasted++
				steps++
				continue
			}
			if now.Before(s.Lease.ExpireAt) {
				now = s.Lease.ExpireAt
				steps++
				continue
			}
		}
		if InStopSet(s, now) {
			s = Step(s, Event{Kind: EventStopExpired, Node: reader}, now).State
			steps++
			continue
		}
		if InClaimSet(s, now) {
			if next, ok := wasteOne(s, now); ok {
				s = next
				wasted++
				steps++
				continue
			}
			if r, ok := s.Refusal[reader]; ok && !r.OpenEnded && r.Until.After(now) {
				now = r.Until
				steps++
				continue
			}
			res := Step(s, Event{Kind: EventAcquire, Node: reader}, now)
			if res.Decision != DecisionApplied {
				break
			}
			s = res.State
			steps++
			continue
		}
		if s.Lease.Owner == reader && LeaseLive(s, now) {
			if s.Phase == PhaseRetryBackoff && now.Before(s.RetryUntil) {
				now = s.RetryUntil
				steps++
				continue
			}
			res := Step(s, Held(s, EventDumpReady, reader), now)
			if res.Decision != DecisionApplied && res.Decision != DecisionIdempotent {
				break
			}
			s = res.State
			steps++
			continue
		}
		break
	}
	return s, now, steps, wasted
}

func runningNow(s State, now time.Time) bool {
	return s.Phase == PhaseRunning && s.Desired == DesiredRun && LeaseLive(s, now) && eligibleNode(s, s.Lease.Owner, now)
}

func refreshHeartbeats(s *State, now time.Time) {
	for i := range s.Nodes {
		if !s.Nodes[i].Stopped {
			s.Nodes[i].HeartbeatAt = now
		}
	}
}

func yieldHolder(s State, now time.Time) (State, bool) {
	owner := s.Lease.Owner
	if owner == "" || !LeaseLive(s, now) {
		return s, false
	}
	ev := heldCause(s, EventTailUnreadable, owner, CauseSegmentNotOnWorker)
	yielded := Step(s, ev, now)
	if yielded.Decision == DecisionApplied {
		return yielded.State, true
	}
	ev.Kind = EventEpochLost
	ev.Cause = CauseENOENT
	yielded = Step(s, ev, now)
	if yielded.Decision == DecisionApplied {
		return yielded.State, true
	}
	return s, false
}

func wasteOne(s State, now time.Time) (State, bool) {
	if !InClaimSet(s, now) {
		return s, false
	}
	for _, node := range s.Nodes {
		if node.CanRead || node.Stopped || RefusalBlocks(s, node.ID, now) {
			continue
		}
		res := Step(s, Event{Kind: EventAcquire, Node: node.ID}, now)
		if res.Decision != DecisionApplied {
			continue
		}
		ev := heldCause(res.State, EventTailUnreadable, node.ID, CauseSegmentNotOnWorker)
		yielded := Step(res.State, ev, now)
		if yielded.Decision == DecisionApplied {
			return yielded.State, true
		}
		ev.Kind = EventEpochLost
		ev.Cause = CauseENOENT
		ev.Epoch = res.State.Lease.Epoch
		ev.RowGeneration = res.State.RowGeneration
		yielded = Step(res.State, ev, now)
		if yielded.Decision == DecisionApplied {
			return yielded.State, true
		}
	}
	return s, false
}
