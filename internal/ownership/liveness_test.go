// Package ownership is the pure task-ownership transition from ADR 0011.
// input: Step, under the three fairness assumptions in ADR 0011 §4.1
// output: liveness (a), (b), and (c). K counts wasted claims and shared re-claims (N9)
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
// Liveness is a bounded search of every fair schedule, not one witness path.
// The start set is every distinct premise state reached by 60 biased walks
// (both storages, Start first, Stop and Delete usually rewritten to Acquire).
// From each of those states, every schedule allowed by the three fairness
// assumptions must be RUNNING within K. The three assumptions are premises:
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
// as one step. per_node NULL refusals are pruned: that node cannot claim again.
// A dated refusal (shared TailUnreadable, and every EpochLost) can be claimed
// again after it expires. K counts one such re-claim per node when a fair time
// jump — the reader's refusal, idle_kill, or a stopped owner's lease TTL —
// lands at or after refuse_until.
//
//	K = claim + DumpReady + N + re-claims
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
	longest, k, err := searchAll(s, later)
	if err != nil {
		t.Fatal(err)
	}
	if longest.phase != PhaseRunning || longest.owner != "n1" {
		t.Fatalf("phase %s owner %s steps %d K %d wasted %d", longest.phase, longest.owner, longest.steps, k, longest.wasted)
	}
	if longest.wasted != 2 || longest.steps != k {
		t.Fatalf("steps %d wasted %d K %d; K must be exactly the longest fair schedule", longest.steps, longest.wasted, k)
	}
	// N9: K without the N wasted claims is too small for the longest fair schedule.
	if longest.steps <= k-n {
		t.Fatalf("schedule fit in K-N (%d <= %d); N is not actually in the budget", longest.steps, k-n)
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
	base := 2 + wastedPossible(s, later)
	longest, k, err := searchAll(s, later)
	if err != nil {
		t.Fatal(err)
	}
	if k <= base {
		t.Fatalf("K %d does not count the shared re-claim on top of %d", k, base)
	}
	if longest.phase != PhaseRunning || longest.steps != k {
		t.Fatalf("steps %d K %d phase %s", longest.steps, k, longest.phase)
	}
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
	for i, st := range starts {
		longest, k, err := searchAll(st.s, st.now)
		if err != nil {
			t.Fatalf("start %d phase %s owner %s live %v lock %v reader %q claim %v tail %v refusals %d K %d: %v", i, st.s.Phase, st.s.Lease.Owner, LeaseLive(st.s, st.now), st.s.Lock.Held, eligibleReader(st.s, st.now), InClaimSet(st.s, st.now), !st.s.TailSince.IsZero(), len(st.s.Refusal), budget(st.s, st.now), err)
		}
		if longest.steps > k {
			t.Fatalf("start %d exceeded K: steps %d K %d", i, longest.steps, k)
		}
		if findOpen(st.s, FindingTail, st.s.TaskID) && findOpen(longest.state, FindingTail, st.s.TaskID) {
			t.Fatalf("start %d left the tail finding open", i)
		}
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

// extraWastes is the shared-mode (and dated EpochLost) re-claim term.
// A NULL per_node refusal does not expire, so it adds nothing. A dated refusal
// adds one more claim when the fair jump lands at or after refuse_until.
func extraWastes(s State, now time.Time) int {
	jump := fairJump(s, now)
	if !jump.After(now) {
		return 0
	}
	p := s.Params.fill()
	n := 0
	for _, node := range s.Nodes {
		if node.CanRead || node.Stopped {
			continue
		}
		r, ok := s.Refusal[node.ID]
		if ok && r.OpenEnded {
			continue
		}
		if ok && r.Until.After(now) {
			if !r.Until.After(jump) {
				n++
			}
			continue
		}
		if !canWasteNow(s, now, node.ID) || wasteStaysOpen(s, node.ID) {
			continue
		}
		attempts := r.Attempts + 1
		if attempts < 1 {
			attempts = 1
		}
		until := now.Add(RefusalDelay(attempts, p.ClaimInterval, p.RefusalRetryCap))
		if !until.After(jump) {
			n++
		}
	}
	return n
}

func fairJump(s State, now time.Time) time.Time {
	jump := now
	reader := eligibleReader(s, now)
	if r, ok := s.Refusal[reader]; ok && !r.OpenEnded && r.Until.After(jump) {
		jump = r.Until
	}
	if s.Lock.Held {
		if d := s.Lock.Since.Add(s.Params.IdleKill); d.After(jump) {
			jump = d
		}
	}
	if LeaseLive(s, now) && ownerStopped(s) && s.Lease.ExpireAt.After(jump) {
		jump = s.Lease.ExpireAt
	}
	if LegalRetry(s, now) && s.Lease.Owner == reader && s.RetryUntil.After(jump) {
		jump = s.RetryUntil
	}
	return jump
}

func canWasteNow(s State, now time.Time, id string) bool {
	node := findNode(s, id)
	if node == nil || node.CanRead || node.Stopped || RefusalBlocks(s, id, now) || !InClaimSet(s, now) {
		return false
	}
	return true
}

func wasteStaysOpen(s State, id string) bool {
	if s.Params.Storage != StoragePerNode {
		return false
	}
	return !(s.Chain.Verified() && s.Chain.Owner == id)
}

func ownerStopped(s State) bool {
	n := findNode(s, s.Lease.Owner)
	return n != nil && n.Stopped
}

func budget(s State, now time.Time) int {
	k := 2 + wastedPossible(s, now) + extraWastes(s, now) // claim + DumpReady + N + re-claims
	if s.Lock.Held {
		k++
		// idle_kill can land at or after lease_expire_at. The owner then has to
		// claim again, and every non-reader may waste a claim first.
		releaseAt := now
		if deadline := s.Lock.Since.Add(s.Params.IdleKill); deadline.After(now) {
			releaseAt = deadline
		}
		if LeaseLive(s, now) && !s.Lease.ExpireAt.After(releaseAt) {
			dead := s
			dead.Lease.ExpireAt = releaseAt
			k += wastedPossible(dead, releaseAt)
		}
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

type timedState struct {
	s   State
	now time.Time
}

type searchFrame struct {
	s         State
	now       time.Time
	steps     int
	wasted    int
	path      string
	label     string
	wastedIDs map[string]bool
}

type scheduleEnd struct {
	state  State
	phase  Phase
	owner  string
	steps  int
	wasted int
}

// searchAll explores every fair schedule. A schedule that is still not
// RUNNING after K steps is a failure. The returned end is the longest one,
// which is the schedule that spends the wasted claims.
func searchAll(start State, now time.Time) (scheduleEnd, int, error) {
	k := budget(start, now)
	queue := []searchFrame{{s: start, now: now}}
	var longest scheduleEnd
	seenGoal := false
	visited := 0
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		visited++
		if visited > 200000 {
			return scheduleEnd{}, k, errFair("fair search did not stay bounded")
		}
		refreshHeartbeats(&cur.s, cur.now)
		if runningNow(cur.s, cur.now) {
			if cur.steps > k {
				return scheduleEnd{}, k, errFair("fair schedule reached RUNNING past K")
			}
			seenGoal = true
			if cur.steps >= longest.steps {
				longest = scheduleEnd{state: cur.s, phase: cur.s.Phase, owner: cur.s.Lease.Owner, steps: cur.steps, wasted: cur.wasted}
			}
			continue
		}
		if cur.steps >= k {
			return scheduleEnd{}, k, errFair("fair schedule still " + string(cur.s.Phase) + " owner " + cur.s.Lease.Owner + " path " + cur.path + " after K")
		}
		next := fairSuccessors(cur.s, cur.now, cur.steps, cur.wasted, cur.wastedIDs)
		for i := range next {
			next[i].path = cur.path + ">" + next[i].label
		}
		if len(next) == 0 {
			return scheduleEnd{}, k, errFair("no fair move from " + string(cur.s.Phase) + " owner " + cur.s.Lease.Owner)
		}
		queue = append(queue, next...)
	}
	if !seenGoal {
		return scheduleEnd{}, k, errFair("no fair schedule reached RUNNING")
	}
	return longest, k, nil
}

type fairError string

func (e fairError) Error() string { return string(e) }

func errFair(s string) error { return fairError(s) }

func fairSuccessors(s State, now time.Time, steps, wasted int, already map[string]bool) []searchFrame {
	var out []searchFrame
	add := func(next State, at time.Time, w int, label, wastedID string) {
		ids := make(map[string]bool, len(already)+1)
		for id := range already {
			ids[id] = true
		}
		if wastedID != "" {
			ids[wastedID] = true
		}
		out = append(out, searchFrame{s: next, now: at, steps: steps + 1, wasted: wasted + w, label: label, wastedIDs: ids})
	}
	reader := eligibleReader(s, now)
	if reader == "" {
		reader = s.Lease.Owner
	}
	if s.Lock.Held {
		when := now
		deadline := s.Lock.Since.Add(s.Params.IdleKill)
		if when.Before(deadline) {
			when = deadline
		}
		res := Step(s, Event{Kind: EventMetaLockTimeout, Node: reader}, when)
		add(res.State, when, 0, "lock-timeout", "")
		return out
	}
	if s.Phase == PhaseFailed && s.FailedReason == ReasonChainOwnerGone {
		owner := s.Chain.Owner
		if !s.Chain.VerifiedAt.After(s.FailedAt) {
			when := now
			if !when.After(s.FailedAt) {
				when = s.FailedAt.Add(time.Millisecond)
			}
			res := Step(s, Event{Kind: EventChainVerified, Node: owner, ReadOK: true}, when)
			if res.Decision == DecisionApplied {
				add(res.State, when, 0, "chain-verified", "")
			}
			return out
		}
		res := Step(s, Event{Kind: EventChainOwnerBack, Node: owner}, now)
		if res.Decision == DecisionApplied {
			add(res.State, now, 0, "chain-back", "")
		}
		return out
	}
	if LeaseLive(s, now) && ownerStopped(s) && now.Before(s.Lease.ExpireAt) {
		add(s, s.Lease.ExpireAt, 0, "wait-ttl", "")
		return out
	}
	if LeaseLive(s, now) && s.Lease.Owner != "" && s.Lease.Owner != eligibleReader(s, now) {
		if next, ok := yieldHolder(s, now); ok {
			add(next, now, 1, "yield-holder", s.Lease.Owner)
			return out
		}
		if now.Before(s.Lease.ExpireAt) {
			add(s, s.Lease.ExpireAt, 0, "wait-holder", "")
			return out
		}
	}
	if InStopSet(s, now) {
		res := Step(s, Event{Kind: EventStopExpired, Node: reader}, now)
		if res.Decision == DecisionApplied {
			add(res.State, now, 0, "stop-expired", "")
		}
		return out
	}
	if InClaimSet(s, now) {
		for _, node := range s.Nodes {
			if already[node.ID] {
				r, ok := s.Refusal[node.ID]
				// per_node NULL refusals never expire. A dated refusal can be
				// claimed again once refuse_until has passed.
				if !ok || r.OpenEnded || r.Until.After(now) {
					continue
				}
			}
			if next, ok := wasteNode(s, now, node.ID); ok {
				add(next, now, 1, "waste-"+node.ID, node.ID)
			}
		}
		if r, ok := s.Refusal[reader]; ok && !r.OpenEnded && r.Until.After(now) {
			add(s, r.Until, 0, "wait-refusal", "")
		} else if reader != "" && !RefusalBlocks(s, reader, now) {
			res := Step(s, Event{Kind: EventAcquire, Node: reader}, now)
			if res.Decision == DecisionApplied {
				add(res.State, now, 0, "acquire-"+reader, "")
			}
		}
		return out
	}
	if s.Lease.Owner == eligibleReader(s, now) && LeaseLive(s, now) {
		if s.Phase == PhaseRetryBackoff && now.Before(s.RetryUntil) {
			add(s, s.RetryUntil, 0, "wait-retry", "")
			return out
		}
		res := Step(s, Held(s, EventDumpReady, s.Lease.Owner), now)
		if res.Decision == DecisionApplied || res.Decision == DecisionIdempotent {
			add(res.State, now, 0, "dump", "")
		}
		return out
	}
	return out
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

func wasteNode(s State, now time.Time, id string) (State, bool) {
	node := findNode(s, id)
	if node == nil || node.CanRead || node.Stopped || RefusalBlocks(s, id, now) || !InClaimSet(s, now) {
		return s, false
	}
	res := Step(s, Event{Kind: EventAcquire, Node: id}, now)
	if res.Decision != DecisionApplied {
		return s, false
	}
	ev := heldCause(res.State, EventTailUnreadable, id, CauseSegmentNotOnWorker)
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
	return s, false
}
