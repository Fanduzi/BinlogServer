// Package ownership is the pure task-ownership transition from ADR 0011.
// input: Step, under the three fairness assumptions in ADR 0011 §4.1
// output: liveness (a), (b), and (c) checked against FairStepBound
// pos: the fair-start set. The Step search that checks the bound is in observe_test.go
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
// Liveness is a bounded search of every fair schedule, not one witness path.
// The start set is every distinct premise state reached by 60 biased walks
// (both storages, Start first, Stop and Delete usually rewritten to Acquire).
// From each of those states, every schedule allowed by the three fairness
// assumptions must reach RUNNING. The three assumptions are premises:
//
//  1. A node that is not stopped eventually takes a step.
//  2. A task_leases row lock is released within one idle_kill step. The
//     search fires MetaLockTimeout on that step, not after the lease TTL.
//  3. A node that can read the directory, whose open-ended refusal is clear,
//     and which is not stopped, is the reader the search schedules. An
//     expiring refusal costs one wait step, up to refusal_retry_cap.
//
// One wasted claim is Acquire plus the TailUnreadable (or EpochLost, when the
// node is the verified chain owner) that releases it. A live owner that cannot
// read yields with that same release and no extra Acquire. Either way it counts
// as one step. per_node NULL refusals do not expire. A dated refusal can be
// claimed again after refuse_until.
//
// FairStepBound is an upper bound on those steps, not the schedule length.
// The search in observe_test.go only calls Step. Where it finishes, the
// longest schedule is at most the bound. A bound lowered by one fails on a
// fixture whose observed length equals the bound. Past the state cap, a
// seeded walk has to stay under the same bound.
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
	n := 0
	for _, node := range s.Nodes {
		if node.CanRead || node.Stopped {
			continue
		}
		if r, ok := s.Refusal[node.ID]; ok && (r.OpenEnded || r.Until.After(later)) {
			continue
		}
		n++
	}
	if n != 2 {
		t.Fatalf("wasted possible %d, want 2", n)
	}
	end := observeFair(s, later)
	bound := FairStepBound(s, later)
	if end.over || !end.ok {
		t.Fatalf("search over=%v ok=%v", end.over, end.ok)
	}
	if end.state.Phase != PhaseRunning || end.state.Lease.Owner != "n1" {
		t.Fatalf("phase %s owner %s steps %d bound %d", end.state.Phase, end.state.Lease.Owner, end.steps, bound)
	}
	if end.steps > bound {
		t.Fatalf("steps %d exceed bound %d", end.steps, bound)
	}
	if end.steps < n+2 {
		t.Fatalf("schedule %d did not include %d wastes plus acquire and dump", end.steps, n)
	}
}

func TestLivenessSharedReclaimFitsInK(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0), peer("n3", false, t0)})
	s.Params.Storage = StorageShared
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n1"}, later)
	s = applied(t, s, heldCause(s, EventEpochLost, "n1", CauseEROFS), later)
	s.Refusal["n1"] = Refusal{Reason: RefusalEpochLost, Until: later.Add(time.Hour), Attempts: 1}
	base := 3 // acquire, dump, and one waste, before any reclaim
	end := observeFair(s, later)
	bound := FairStepBound(s, later)
	got := end.steps
	if end.over {
		got, _ = walkMax(s, later, 32)
	} else if !end.ok || end.state.Phase != PhaseRunning {
		t.Fatalf("search ok=%v phase %s", end.ok, end.state.Phase)
	} else if got <= base {
		t.Fatalf("schedule %d did not reclaim on top of %d", got, base)
	}
	if got > bound {
		t.Fatalf("steps %d exceed bound %d", got, bound)
	}
	t.Logf("shared reclaim H=1h bound=%d steps=%d over=%v", bound, got, end.over)
}

func addChainGoneStarts(t *testing.T, add func(State, time.Time)) {
	t.Helper()
	per := yieldTail(t, "n1", "n2")
	goneAt := per.TailSince.Add(EffectiveNodeGone(per.Params) + time.Second)
	per = heartbeat(per, "n1", per.TailSince)
	per = applied(t, per, Event{Kind: EventChainOwnerGone, Node: "_health"}, goneAt)
	add(per, goneAt)

	shared := boot(t, "n1", t0, []Node{peer("n2", false, t0), peer("n3", false, t0)})
	shared.Params.Storage = StorageShared
	later := t0.Add(time.Hour)
	shared = expireLease(shared, later)
	shared = applied(t, shared, Event{Kind: EventAcquire, Node: "n2"}, later)
	shared = applied(t, shared, heldCause(shared, EventTailUnreadable, "n2", CauseSegmentNotOnWorker), later)
	when := shared.TailSince.Add(EffectiveNodeGone(shared.Params) + time.Second)
	shared = heartbeat(shared, "n1", shared.TailSince)
	shared = applied(t, shared, Event{Kind: EventChainOwnerGone, Node: "_health"}, when)
	add(shared, when)
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

func TestLivenessAllFairSchedules(t *testing.T) {
	const seed = 20261020
	seen := map[string]bool{}
	var starts []timedState
	add := func(s State, now time.Time) {
		if !livenessPremise(s, now) {
			return
		}
		key := premiseKey(s)
		if seen[key] {
			return
		}
		seen[key] = true
		starts = append(starts, timedState{s: s, now: now})
	}
	for seq := 0; seq < 60; seq++ {
		rng := rand.New(rand.NewSource(seed + int64(seq) + 100))
		s := New()
		if seq%2 == 0 {
			s.Params.Storage = StorageShared
		}
		s.Nodes = []Node{
			{ID: "n1", CanRead: true, HeartbeatAt: t0},
			{ID: "n2", CanRead: rng.Intn(2) == 0, HeartbeatAt: t0},
			{ID: "n3", CanRead: false, HeartbeatAt: t0},
		}
		now := t0
		// Leave CREATED. The liveness premise starts once a reader exists.
		started := Step(s, Event{Kind: EventOperatorStart, Node: "n1"}, now)
		if v := CheckStep(s, started.State, Event{Kind: EventOperatorStart, Node: "n1"}, started, now); len(v) > 0 {
			t.Fatalf("start seq %d: %+v", seq, v)
		}
		s = started.State
		add(s, now)
		for i := 0; i < 24; i++ {
			e := randomEvent(rng, s, 3)
			// Stop and Delete are safety events. Keep most of the walk inside (a).
			if (e.Kind == EventOperatorStop || e.Kind == EventOperatorDelete) && rng.Intn(4) != 0 {
				e.Kind = EventAcquire
				e.Node = "n1"
			}
			now = now.Add(time.Duration(1+rng.Intn(40)) * time.Second)
			res := Step(s, e, now)
			if v := CheckStep(s, res.State, e, res, now); len(v) > 0 {
				t.Fatalf("safety seq %d: %+v", seq, v)
			}
			s = res.State
			add(s, now)
			if len(starts) >= 200 {
				break
			}
		}
		if len(starts) >= 200 {
			break
		}
	}
	addChainGoneStarts(t, add)
	if len(starts) < 80 {
		t.Fatalf("premise held for only %d states", len(starts))
	}
	goneStarts := 0
	for _, st := range starts {
		if st.s.Phase == PhaseFailed && st.s.FailedReason == ReasonChainOwnerGone {
			goneStarts++
		}
	}
	if goneStarts < 1 {
		t.Fatal("liveness start set has no FAILED/CHAIN_OWNER_GONE state")
	}
	tight, overflow := 0, 0
	for i, st := range starts {
		end := observeFair(st.s, st.now)
		bound := FairStepBound(st.s, st.now)
		if end.over || !end.ok {
			t.Fatalf("start %d phase %s over=%v ok=%v states %d violation %s stuck %v", i, st.s.Phase, end.over, end.ok, end.states, end.violation, end.stuck)
		}
		got := end.steps
		if got > bound {
			t.Fatalf("start %d steps %d exceed bound %d phase %s storage %s", i, got, bound, st.s.Phase, st.s.Params.Storage)
		}
		if !end.over && got == bound && bound > 0 {
			tight++
			if got <= bound-1 {
				t.Fatalf("start %d schedule fit in bound-1 (%d)", i, bound-1)
			}
		}
		if !end.over && end.state.Phase != PhaseFailed && findOpen(st.s, FindingTail, st.s.TaskID) && findOpen(end.state, FindingTail, st.s.TaskID) {
			t.Fatalf("start %d left the tail finding open", i)
		}
	}
	t.Logf("starts %d tight %d overflow %d", len(starts), tight, overflow)
}

func livenessPremise(s State, now time.Time) bool {
	if s.Deleted || s.Desired != DesiredRun || s.Phase == PhaseCreated || s.PendingDelete {
		return false
	}
	if s.Phase == PhaseFailed && s.FailedReason != ReasonChainOwnerGone {
		return false
	}
	if s.Phase == PhaseFailed && !obsCanRead(s, s.Chain.Owner) {
		return false
	}
	return obsReader(s, now) != ""
}

type timedState struct {
	s   State
	now time.Time
}

func iteratedWastes(h, claim, cap time.Duration) int {
	if h < 0 {
		return 0
	}
	t := time.Duration(0)
	att := 0
	n := 0
	for t <= h {
		n++
		att++
		if att > maxRefusalAttempts {
			att = maxRefusalAttempts
		}
		next := t + RefusalDelay(att, claim, cap)
		if next > h {
			break
		}
		t = next
	}
	return n
}

// logWastes is floor(log2(H/claim_interval+1))+1. It is the waste count only
// when RefusalDelay never returns the cap inside the horizon.
func logWastes(h, claim time.Duration) int {
	if claim <= 0 || h < 0 {
		return 0
	}
	n := int64(h/claim) + 1
	lg := 0
	for n > 1 {
		n >>= 1
		lg++
	}
	return lg + 1
}

func premiseKey(s State) string {
	tail := "0"
	if !s.TailSince.IsZero() {
		tail = "1"
	}
	lock := "0"
	if s.Lock.Held {
		lock = "1"
	}
	return string(s.Phase) + "|" + string(s.Desired) + "|" + s.Lease.Owner + "|" +
		itoa(s.Lease.Epoch) + "|" + itoa(s.RowGeneration) + "|" + itoa(int64(len(s.Refusal))) + "|" +
		itoa(int64(s.RetryAttempt)) + "|" + s.FailedReason + "|" + s.Chain.Owner + "|" + tail + "|" + lock
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func TestLivenessWasteFormula(t *testing.T) {
	claim := 2 * time.Second
	cap := 600 * time.Second
	if logWastes(4*time.Second, claim) != 2 || iteratedWastes(4*time.Second, claim, cap) != 2 {
		t.Fatalf("H=4s log %d iterated %d", logWastes(4*time.Second, claim), iteratedWastes(4*time.Second, claim, cap))
	}
	if logWastes(600*time.Second, claim) != 9 || iteratedWastes(600*time.Second, claim, cap) != 9 {
		t.Fatalf("H=600s log %d iterated %d, want 9", logWastes(600*time.Second, claim), iteratedWastes(600*time.Second, claim, cap))
	}
	// 1022s is the horizon where the 10th claim lands on H itself
	// (0+2+4+...+512 = 1022). A strict < would drop that claim.
	if logWastes(1022*time.Second, claim) != 10 || iteratedWastes(1022*time.Second, claim, cap) != 10 {
		t.Fatalf("H=1022s log %d iterated %d, want 10", logWastes(1022*time.Second, claim), iteratedWastes(1022*time.Second, claim, cap))
	}
	if logWastes(3600*time.Second, claim) != 11 {
		t.Fatalf("H=3600s log %d, want 11", logWastes(3600*time.Second, claim))
	}
	if iteratedWastes(3600*time.Second, claim, cap) != 14 {
		t.Fatalf("H=3600s iterated %d, want 14", iteratedWastes(3600*time.Second, claim, cap))
	}
}
