// Package ownership is the pure task-ownership transition from ADR 0011.
// input: Step, under the three fairness assumptions in ADR 0011 §4.1
// output: liveness (a), (b), and (c). K counts backoff wastes and the deadline jumps between them (N9)
// pos: the bounded fair search. Random prefixes in invariant_test.go are safety only
// note: if this file changes, update this header and module README.md.
package ownership

import (
	"math/rand"
	"sort"
	"strings"
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
// again after refuse_until, on the RefusalDelay schedule, until the reader's
// horizon. The search may jump to every distinct deadline in (now, horizon],
// not only the reader's, so a non-reader can spend every claim that fits.
//
// While a fresh non-reader waits out a horizon H and the cap does not bind,
// the waste count is floor(log2(H/claim_interval+1))+1. After the delay hits
// refusal_retry_cap the gap is the cap and the count is the RefusalDelay
// iteration, not the log formula. K is that waste/jump DP, plus Acquire and
// DumpReady, plus the forced lock, stopped-owner, retry, and chain steps.
// The longest fair schedule has exactly K steps. K-1 does not fit it.
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
	t.Logf("shared reclaim H=1h formula=%d longest=%d wasted=%d", k, longest.steps, longest.wasted)
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
		if longest.steps != k {
			t.Fatalf("start %d steps %d != K %d phase %s storage %s", i, longest.steps, k, st.s.Phase, st.s.Params.Storage)
		}
		if i == 41 && (longest.steps != 8 || k != 8) {
			t.Fatalf("start 41 steps %d K %d phase %s storage %s owner %s refusals %d", longest.steps, k, st.s.Phase, st.s.Params.Storage, st.s.Lease.Owner, len(st.s.Refusal))
		}
		if k > 0 {
			if _, err := runSearch(st.s, st.now, k-1); err == nil {
				t.Fatalf("start %d still reached RUNNING within K-1 (%d)", i, k-1)
			}
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
	return budgetRec(s, now, 0)
}

func budgetRec(s State, now time.Time, depth int) int {
	if depth > 80 {
		return 0
	}
	refreshHeartbeats(&s, now)
	if runningNow(s, now) {
		return 0
	}
	if m, ok := forcedMove(s, now); ok {
		return 1 + budgetRec(m.s, m.at, depth+1)
	}
	if !InClaimSet(s, now) {
		return 0
	}
	return claimBound(s, now)
}

// claimBound is the waste/jump DP for an open claim set, plus Acquire and
// DumpReady. It calls RefusalDelay, including the shift cap, and does not call Step.
func claimBound(s State, now time.Time) int {
	p := s.Params.fill()
	pre := dpWaste(wastersOf(s, now), now.UnixNano(), readerHorizon(s, now).UnixNano(), p, map[string]int{})
	return pre + 2 // acquire + DumpReady
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
// RUNNING after K steps is a failure. The returned end is the longest one.
func searchAll(start State, now time.Time) (scheduleEnd, int, error) {
	k := budget(start, now)
	end, err := runSearch(start, now, k)
	return end, k, err
}

// searchBudget reports whether every fair schedule reaches RUNNING within limit.
func searchBudget(start State, now time.Time, limit int) error {
	_, err := runSearch(start, now, limit)
	return err
}

type memoHit struct {
	steps, wasted, choice int
	ok                    bool
}

type searcher struct {
	memo     map[string]memoHit
	misses   int
	overflow bool
	goal     State
	haveGoal bool
}

func runSearch(start State, now time.Time, limit int) (scheduleEnd, error) {
	s := start
	s.Trace = nil
	sr := &searcher{memo: map[string]memoHit{}}
	steps, wasted, ok := sr.longest(s, now)
	if sr.overflow {
		return scheduleEnd{}, errFair("fair search did not stay bounded")
	}
	if !ok {
		return scheduleEnd{}, errFair("no fair schedule reached RUNNING from " + string(s.Phase))
	}
	if steps > limit {
		return scheduleEnd{}, errFair("fair schedule still going after " + itoa(int64(limit)) + " steps, longest " + itoa(int64(steps)))
	}
	goal := sr.goal
	if !sr.haveGoal {
		goal = sr.witness(s, now)
	}
	return scheduleEnd{state: goal, phase: goal.Phase, owner: goal.Lease.Owner, steps: steps, wasted: wasted}, nil
}

func (sr *searcher) longest(s State, now time.Time) (int, int, bool) {
	refreshHeartbeats(&s, now)
	if runningNow(s, now) {
		return 0, 0, true
	}
	key := schedKey(s, now)
	if hit, seen := sr.memo[key]; seen {
		return hit.steps, hit.wasted, hit.ok
	}
	sr.misses++
	if sr.misses > 2000000 {
		sr.overflow = true
		return 0, 0, false
	}
	if m, forced := forcedMove(s, now); forced {
		ds, dw, ok := sr.longest(m.s, m.at)
		if !ok {
			return 0, 0, false
		}
		sr.memo[key] = memoHit{steps: ds + 1, wasted: dw + m.waste, choice: 0, ok: true}
		return ds + 1, dw + m.waste, true
	}
	if InClaimSet(s, now) {
		steps, wasted, goal, ok := realizeClaim(s, now)
		if !ok {
			return 0, 0, false
		}
		sr.goal = goal
		sr.haveGoal = true
		sr.memo[key] = memoHit{steps: steps, wasted: wasted, ok: true}
		return steps, wasted, true
	}
	// A cycle in the key space is not a fair path.
	sr.memo[key] = memoHit{}
	next := fairSuccessors(s, now)
	bestSteps, bestWaste, bestChoice := -1, 0, -1
	for i := range next {
		ds, dw, ok := sr.longest(next[i].s, next[i].now)
		if !ok {
			continue
		}
		total := ds + 1
		waste := dw + next[i].wasted
		if total > bestSteps || (total == bestSteps && waste > bestWaste) {
			bestSteps, bestWaste, bestChoice = total, waste, i
		}
	}
	if bestSteps < 0 {
		return 0, 0, false
	}
	sr.memo[key] = memoHit{steps: bestSteps, wasted: bestWaste, choice: bestChoice, ok: true}
	return bestSteps, bestWaste, true
}

func (sr *searcher) witness(s State, now time.Time) State {
	refreshHeartbeats(&s, now)
	if runningNow(s, now) {
		return s
	}
	hit := sr.memo[schedKey(s, now)]
	next := fairSuccessors(s, now)
	if hit.choice < 0 || hit.choice >= len(next) {
		return s
	}
	return sr.witness(next[hit.choice].s, next[hit.choice].now)
}

type fairError string

func (e fairError) Error() string { return string(e) }

func errFair(s string) error { return fairError(s) }

type move struct {
	s     State
	at    time.Time
	waste int
	label string
}

func forcedMove(s State, now time.Time) (move, bool) {
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
		return move{s: res.State, at: when, label: "lock-timeout"}, true
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
				return move{s: res.State, at: when, label: "chain-verified"}, true
			}
			return move{}, false
		}
		res := Step(s, Event{Kind: EventChainOwnerBack, Node: owner}, now)
		if res.Decision == DecisionApplied {
			return move{s: res.State, at: now, label: "chain-back"}, true
		}
		return move{}, false
	}
	if LeaseLive(s, now) && ownerStopped(s) && now.Before(s.Lease.ExpireAt) {
		return move{s: s, at: s.Lease.ExpireAt, label: "wait-ttl"}, true
	}
	if LeaseLive(s, now) && s.Lease.Owner != "" && s.Lease.Owner != eligibleReader(s, now) {
		if next, ok := yieldHolder(s, now); ok {
			return move{s: next, at: now, waste: 1, label: "yield-holder"}, true
		}
		if now.Before(s.Lease.ExpireAt) {
			return move{s: s, at: s.Lease.ExpireAt, label: "wait-holder"}, true
		}
	}
	if InStopSet(s, now) {
		res := Step(s, Event{Kind: EventStopExpired, Node: reader}, now)
		if res.Decision == DecisionApplied {
			return move{s: res.State, at: now, label: "stop-expired"}, true
		}
		return move{}, false
	}
	if s.Lease.Owner == eligibleReader(s, now) && LeaseLive(s, now) {
		if s.Phase == PhaseRetryBackoff && now.Before(s.RetryUntil) {
			return move{s: s, at: s.RetryUntil, label: "wait-retry"}, true
		}
		res := Step(s, Held(s, EventDumpReady, s.Lease.Owner), now)
		if res.Decision == DecisionApplied || res.Decision == DecisionIdempotent {
			return move{s: res.State, at: now, label: "dump"}, true
		}
	}
	return move{}, false
}

func fairSuccessors(s State, now time.Time) []searchFrame {
	var out []searchFrame
	add := func(next State, at time.Time, w int, label string) {
		out = append(out, searchFrame{s: next, now: at, wasted: w, label: label})
	}
	if m, ok := forcedMove(s, now); ok {
		add(m.s, m.at, m.waste, m.label)
		return out
	}
	if !InClaimSet(s, now) {
		return out
	}
	reader := eligibleReader(s, now)
	seen := map[string]bool{}
	for _, node := range s.Nodes {
		if !canWasteNow(s, now, node.ID) {
			continue
		}
		sig := wasterSig(s, node.ID)
		if seen[sig] {
			continue
		}
		seen[sig] = true
		if next, ok := wasteNode(s, now, node.ID); ok {
			add(next, now, 1, "waste-"+node.ID)
		}
	}
	horizon := readerHorizon(s, now)
	if horizon.After(now) {
		for _, when := range jumpTimes(s, now, horizon) {
			add(s, when, 0, "jump")
		}
		return out
	}
	if reader != "" && !RefusalBlocks(s, reader, now) {
		res := Step(s, Event{Kind: EventAcquire, Node: reader}, now)
		if res.Decision == DecisionApplied {
			add(res.State, now, 0, "acquire-"+reader)
		}
	}
	return out
}

func runningNow(s State, now time.Time) bool {
	if s.Lock.Held {
		return false
	}
	return s.Phase == PhaseRunning && s.Desired == DesiredRun && LeaseLive(s, now) && eligibleNode(s, s.Lease.Owner, now)
}

func readerHorizon(s State, now time.Time) time.Time {
	reader := eligibleReader(s, now)
	if r, ok := s.Refusal[reader]; ok && !r.OpenEnded && r.Until.After(now) {
		return r.Until
	}
	return now
}

func jumpTimes(s State, now, horizon time.Time) []time.Time {
	seen := map[int64]bool{}
	var out []time.Time
	add := func(t time.Time) {
		if !t.After(now) || t.After(horizon) {
			return
		}
		ns := t.UnixNano()
		if seen[ns] {
			return
		}
		seen[ns] = true
		out = append(out, t)
	}
	add(horizon)
	for _, node := range s.Nodes {
		if node.CanRead || node.Stopped {
			continue
		}
		r, ok := s.Refusal[node.ID]
		if !ok || r.OpenEnded {
			continue
		}
		add(r.Until)
	}
	return out
}

func wasterSig(s State, id string) string {
	r := s.Refusal[id]
	once := 0
	if wasteStaysOpen(s, id) {
		once = 1
	}
	chain := 0
	if s.Chain.Verified() && s.Chain.Owner == id {
		chain = 1
	}
	open := 0
	if r.OpenEnded {
		open = 1
	}
	return itoa(int64(r.Attempts)) + "|" + itoa(r.Until.UnixNano()) + "|" + itoa(int64(open)) + "|" + itoa(int64(once)) + "|" + itoa(int64(chain))
}

type waster struct {
	id    string
	att   int
	ready int64
	once  bool
	spent bool
	chain bool
}

func wastersOf(s State, now time.Time) []waster {
	var out []waster
	for _, node := range s.Nodes {
		if node.CanRead || node.Stopped {
			continue
		}
		r := s.Refusal[node.ID]
		if r.OpenEnded {
			continue
		}
		ready := now.UnixNano()
		if r.Until.After(now) {
			ready = r.Until.UnixNano()
		}
		out = append(out, waster{
			id:    node.ID,
			att:   r.Attempts,
			ready: ready,
			once:  wasteStaysOpen(s, node.ID),
			chain: s.Chain.Verified() && s.Chain.Owner == node.ID,
		})
	}
	sortWasters(out)
	return out
}

func sortWasters(ws []waster) {
	sort.Slice(ws, func(i, j int) bool {
		if ws[i].att != ws[j].att {
			return ws[i].att < ws[j].att
		}
		if ws[i].ready != ws[j].ready {
			return ws[i].ready < ws[j].ready
		}
		if ws[i].once != ws[j].once {
			return !ws[i].once
		}
		if ws[i].spent != ws[j].spent {
			return !ws[i].spent
		}
		if ws[i].chain != ws[j].chain {
			return !ws[i].chain && ws[j].chain
		}
		return ws[i].id < ws[j].id
	})
}

func dpWaste(ws []waster, now, horizon int64, p Params, memo map[string]int) int {
	steps, _, _ := solveClaim(ws, now, horizon, p)
	_ = memo
	return steps
}

type claimMove struct {
	jump bool
	at   int64
	id   string
}

type reachMark struct {
	steps  int
	wasted int
}

type claimSolver struct {
	p         Params
	horizon   int64
	best      int
	bestWaste int
	bestPath  []claimMove
	seen      map[string]reachMark
	expanded  int
	capped    bool
}

func solveClaim(ws []waster, now, horizon int64, p Params) (int, int, []claimMove) {
	cs := &claimSolver{p: p, horizon: horizon, seen: map[string]reachMark{}}
	gSteps, gWaste, gPath := greedyClaim(ws, now, horizon, p)
	cs.best, cs.bestWaste, cs.bestPath = gSteps, gWaste, gPath
	cs.walk(append([]waster(nil), ws...), now, 0, 0, nil)
	return cs.best, cs.bestWaste, cs.bestPath
}

func (cs *claimSolver) walk(ws []waster, now int64, steps, wasted int, path []claimMove) {
	if cs.capped {
		return
	}
	cs.expanded++
	if cs.expanded > 2000000 {
		cs.capped = true
		return
	}
	upW, upJ := claimUpper(ws, now, cs.horizon, cs.p)
	if steps+upW+upJ < cs.best || (steps+upW+upJ == cs.best && wasted+upW <= cs.bestWaste) {
		return
	}
	key := dpKey(ws, now, cs.horizon)
	if prev, ok := cs.seen[key]; ok && (steps < prev.steps || (steps == prev.steps && wasted <= prev.wasted)) {
		return
	}
	cs.seen[key] = reachMark{steps: steps, wasted: wasted}
	if claimDone(ws, now, cs.horizon) {
		if steps > cs.best || (steps == cs.best && wasted > cs.bestWaste) {
			cs.best = steps
			cs.bestWaste = wasted
			cs.bestPath = append([]claimMove(nil), path...)
		}
		return
	}
	extend := func(move claimMove, next []waster, at int64, w int) {
		np := make([]claimMove, len(path)+1)
		copy(np, path)
		np[len(path)] = move
		cs.walk(next, at, steps+1, wasted+w, np)
	}
	for i := range ws {
		if ws[i].spent || ws[i].ready > now {
			continue
		}
		if i > 0 && wasterEq(ws[i], ws[i-1]) {
			continue
		}
		next := append([]waster(nil), ws...)
		id := next[i].id
		spendWaster(&next[i], now, cs.p)
		sortWasters(next)
		extend(claimMove{id: id}, next, now, 1)
	}
	if now >= cs.horizon {
		return
	}
	var jumps []int64
	seenJump := map[int64]bool{}
	for _, w := range ws {
		if w.spent || w.ready <= now || w.ready > cs.horizon || seenJump[w.ready] {
			continue
		}
		seenJump[w.ready] = true
		jumps = append(jumps, w.ready)
	}
	if !seenJump[cs.horizon] {
		jumps = append(jumps, cs.horizon)
	}
	sort.Slice(jumps, func(i, j int) bool { return jumps[i] < jumps[j] })
	for _, at := range jumps {
		extend(claimMove{jump: true, at: at}, ws, at, 0)
	}
}

func claimDone(ws []waster, now, horizon int64) bool {
	if now < horizon {
		return false
	}
	for _, w := range ws {
		if !w.spent && w.ready <= now {
			return false
		}
	}
	return true
}

func claimUpper(ws []waster, now, horizon int64, p Params) (wastes, jumps int) {
	for i := range ws {
		wastes += claimsLeft(ws[i], now, horizon, p)
	}
	if now >= horizon {
		return wastes, 0
	}
	// Every future waste creates at most one new deadline, and every deadline
	// already pending can be visited on its own. The horizon is one of them.
	seen := map[int64]bool{}
	for _, w := range ws {
		if w.spent || w.ready <= now || w.ready > horizon || seen[w.ready] {
			continue
		}
		seen[w.ready] = true
		jumps++
	}
	if !seen[horizon] {
		jumps++
	}
	jumps += wastes
	return wastes, jumps
}

func claimsLeft(w waster, now, horizon int64, p Params) int {
	if w.spent {
		return 0
	}
	pos := now
	t := w.ready
	if t < pos {
		t = pos
	}
	att := w.att
	n := 0
	for t <= horizon {
		if pos >= horizon && t > pos {
			break
		}
		n++
		att++
		if att > maxRefusalAttempts {
			att = maxRefusalAttempts
		}
		if att < 1 {
			att = 1
		}
		next := t + int64(RefusalDelay(att, p.ClaimInterval, p.RefusalRetryCap))
		if pos >= horizon {
			break
		}
		t = next
		pos = t
	}
	return n
}

func greedyClaim(ws0 []waster, now, horizon int64, p Params) (int, int, []claimMove) {
	ws := append([]waster(nil), ws0...)
	var path []claimMove
	steps, wasted := 0, 0
	for {
		if now >= horizon {
			i := readyWaster(ws, now)
			if i < 0 {
				break
			}
			path = append(path, claimMove{id: ws[i].id})
			spendWaster(&ws[i], now, p)
			sortWasters(ws)
			steps++
			wasted++
			continue
		}
		if i := readyWaster(ws, now); i >= 0 {
			path = append(path, claimMove{id: ws[i].id})
			spendWaster(&ws[i], now, p)
			sortWasters(ws)
			steps++
			wasted++
		}
		next, ok := earliestDeadline(ws, now, horizon)
		if !ok {
			break
		}
		path = append(path, claimMove{jump: true, at: next})
		now = next
		steps++
	}
	return steps, wasted, path
}

func readyWaster(ws []waster, now int64) int {
	for i := range ws {
		if ws[i].spent || ws[i].ready > now {
			continue
		}
		if i > 0 && wasterEq(ws[i], ws[i-1]) {
			continue
		}
		return i
	}
	return -1
}

func earliestDeadline(ws []waster, now, horizon int64) (int64, bool) {
	if now >= horizon {
		return 0, false
	}
	next := horizon
	for _, w := range ws {
		if w.spent || w.ready <= now || w.ready > horizon {
			continue
		}
		if w.ready < next {
			next = w.ready
		}
	}
	return next, true
}

func realizeClaim(s State, now time.Time) (int, int, State, bool) {
	p := s.Params.fill()
	ws := wastersOf(s, now)
	horizon := readerHorizon(s, now)
	pre, wasted, path := solveClaim(ws, now.UnixNano(), horizon.UnixNano(), p)
	cur := s
	at := now
	got, gotW := 0, 0
	for _, m := range path {
		if m.jump {
			at = time.Unix(0, m.at).UTC()
			got++
			continue
		}
		next, ok := wasteNode(cur, at, m.id)
		if !ok {
			return 0, 0, State{}, false
		}
		cur, got, gotW = next, got+1, gotW+1
	}
	reader := eligibleReader(cur, at)
	if reader == "" || RefusalBlocks(cur, reader, at) || !InClaimSet(cur, at) {
		return 0, 0, State{}, false
	}
	res := Step(cur, Event{Kind: EventAcquire, Node: reader}, at)
	if res.Decision != DecisionApplied {
		return 0, 0, State{}, false
	}
	got++
	cur = res.State
	res = Step(cur, Held(cur, EventDumpReady, cur.Lease.Owner), at)
	if res.Decision != DecisionApplied && res.Decision != DecisionIdempotent {
		return 0, 0, State{}, false
	}
	got++
	if got != pre+2 || gotW != wasted || !runningNow(res.State, at) {
		return 0, 0, State{}, false
	}
	return got, gotW, res.State, true
}

func spendWaster(w *waster, now int64, p Params) {
	if w.once {
		w.spent = true
		return
	}
	w.att++
	if w.att > maxRefusalAttempts {
		w.att = maxRefusalAttempts
	}
	if w.att < 1 {
		w.att = 1
	}
	delay := RefusalDelay(w.att, p.ClaimInterval, p.RefusalRetryCap)
	w.ready = now + int64(delay)
}

func wasterEq(a, b waster) bool {
	return a.att == b.att && a.ready == b.ready && a.once == b.once && a.spent == b.spent && a.chain == b.chain
}

func dpKey(ws []waster, now, horizon int64) string {
	var b strings.Builder
	b.WriteString(itoa(now))
	b.WriteByte('|')
	b.WriteString(itoa(horizon))
	for _, w := range ws {
		b.WriteByte(';')
		b.WriteString(itoa(int64(w.att)))
		b.WriteByte(',')
		b.WriteString(itoa(w.ready))
		if w.once {
			b.WriteByte('o')
		}
		if w.spent {
			b.WriteByte('s')
		}
		if w.chain {
			b.WriteByte('c')
		}
	}
	return b.String()
}

// iteratedWastes is how many claims a fresh node fits into a horizon H.
// The next claim is at the previous claim plus RefusalDelay, so the cap and
// the shift saturation are included. The closed form floor(log2(H/claim+1))+1
// matches this only while every delay stays at or below the cap.
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

func schedKey(s State, now time.Time) string {
	var b strings.Builder
	b.WriteString(itoa(now.UnixNano()))
	b.WriteByte('|')
	b.WriteString(string(s.Phase))
	b.WriteByte('|')
	b.WriteString(string(s.Desired))
	b.WriteByte('|')
	b.WriteString(s.Lease.Owner)
	b.WriteByte('|')
	b.WriteString(itoa(s.Lease.ExpireAt.UnixNano()))
	b.WriteByte('|')
	if s.Lease.Present {
		b.WriteByte('1')
	}
	b.WriteByte('|')
	if s.Lock.Held {
		b.WriteString(s.Lock.HeldBy)
		b.WriteByte('@')
		b.WriteString(itoa(s.Lock.Since.UnixNano()))
	}
	b.WriteByte('|')
	b.WriteString(itoa(int64(s.RetryAttempt)))
	b.WriteByte('@')
	b.WriteString(itoa(s.RetryUntil.UnixNano()))
	b.WriteByte('|')
	b.WriteString(s.FailedReason)
	b.WriteByte('@')
	b.WriteString(itoa(s.FailedAt.UnixNano()))
	b.WriteByte('|')
	b.WriteString(s.Chain.Owner)
	b.WriteByte('@')
	b.WriteString(itoa(s.Chain.VerifiedAt.UnixNano()))
	if !s.TailSince.IsZero() {
		b.WriteString("|tail")
	}
	if s.PendingDelete {
		b.WriteString("|pend")
	}
	if s.Deleted {
		b.WriteString("|del")
	}
	b.WriteByte('|')
	b.WriteString(string(s.Params.Storage))
	b.WriteByte('|')
	b.WriteString(itoa(int64(s.Params.ClaimInterval)))
	b.WriteByte('|')
	b.WriteString(itoa(int64(s.Params.RefusalRetryCap)))
	var readers, others []string
	for _, node := range s.Nodes {
		r := s.Refusal[node.ID]
		sig := node.ID + "#" + boolBit(node.CanRead) + boolBit(node.Stopped) + "#" + itoa(int64(r.Attempts)) + "#" + itoa(r.Until.UnixNano()) + "#" + boolBit(r.OpenEnded)
		if node.Stopped {
			sig += "#" + itoa(node.HeartbeatAt.UnixNano())
		}
		if node.CanRead {
			readers = append(readers, sig)
			continue
		}
		sig += "#" + boolBit(wasteStaysOpen(s, node.ID)) + "#" + boolBit(s.Chain.Verified() && s.Chain.Owner == node.ID)
		others = append(others, sig)
	}
	sort.Strings(others)
	b.WriteString("|R")
	for _, sig := range readers {
		b.WriteByte(',')
		b.WriteString(sig)
	}
	b.WriteString("|N")
	for _, sig := range others {
		b.WriteByte(',')
		b.WriteString(sig)
	}
	return b.String()
}

func boolBit(v bool) string {
	if v {
		return "1"
	}
	return "0"
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

func TestLivenessWasteFormula(t *testing.T) {
	claim := 2 * time.Second
	cap := 600 * time.Second
	if logWastes(4*time.Second, claim) != 2 || iteratedWastes(4*time.Second, claim, cap) != 2 {
		t.Fatalf("H=4s log %d iterated %d", logWastes(4*time.Second, claim), iteratedWastes(4*time.Second, claim, cap))
	}
	if logWastes(600*time.Second, claim) != 9 || iteratedWastes(600*time.Second, claim, cap) != 9 {
		t.Fatalf("H=600s log %d iterated %d, want 9", logWastes(600*time.Second, claim), iteratedWastes(600*time.Second, claim, cap))
	}
	if logWastes(3600*time.Second, claim) != 11 {
		t.Fatalf("H=3600s log %d, want 11", logWastes(3600*time.Second, claim))
	}
	if iteratedWastes(3600*time.Second, claim, cap) != 14 {
		t.Fatalf("H=3600s iterated %d, want 14", iteratedWastes(3600*time.Second, claim, cap))
	}
}

func TestLivenessOneNonReaderRefused600(t *testing.T) {
	s, later := refusedReader(t, 1, 600*time.Second)
	longest, k, err := searchAll(s, later)
	if err != nil {
		t.Fatal(err)
	}
	if longest.steps != 20 || longest.wasted != 9 || k != 20 {
		t.Fatalf("steps %d wasted %d K %d, want 20 steps and 9 wastes", longest.steps, longest.wasted, k)
	}
	if searchBudget(s, later, 19) == nil {
		t.Fatal("K-1 held the 600s schedule")
	}
	t.Logf("non-readers=1 H=600s formula=%d longest=%d wasted=%d", k, longest.steps, longest.wasted)
}

func TestLivenessTwoAndThreeNonReaders(t *testing.T) {
	for _, n := range []int{2, 3} {
		s, later := refusedReader(t, n, 600*time.Second)
		longest, k, err := searchAll(s, later)
		if err != nil {
			t.Fatalf("%d non-readers: %v", n, err)
		}
		if longest.steps != k || longest.phase != PhaseRunning {
			t.Fatalf("%d non-readers steps %d K %d phase %s", n, longest.steps, k, longest.phase)
		}
		if k > 0 && searchBudget(s, later, k-1) == nil {
			t.Fatalf("%d non-readers held at K-1 %d", n, k-1)
		}
		t.Logf("non-readers=%d formula=%d longest=%d wasted=%d", n, k, longest.steps, longest.wasted)
	}
}

func refusedReader(t *testing.T, nonReaders int, hold time.Duration) (State, time.Time) {
	t.Helper()
	peers := make([]Node, nonReaders)
	for i := 0; i < nonReaders; i++ {
		peers[i] = peer(nodeID(i+1), false, t0)
	}
	s := boot(t, "n1", t0, peers)
	s.Params.Storage = StorageShared
	later := t0.Add(time.Hour)
	s = expireLease(s, later)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n1"}, later)
	s = applied(t, s, heldCause(s, EventEpochLost, "n1", CauseEROFS), later)
	s.Refusal["n1"] = Refusal{Reason: RefusalEpochLost, Until: later.Add(hold), Attempts: 1}
	return s, later
}
