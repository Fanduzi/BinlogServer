// Package ownership is the pure task-ownership transition from ADR 0011.
// input: Step and Check only. No helper from FairStepBound
// output: the observed fair-schedule length beside FairStepBound
// pos: the Step search and the adversarial walks that check the upper bound
// note: if this file changes, update this header and module README.md.
package ownership

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"
)

// obsStateCap is the exhaustive-search limit. Past it the fixture is checked
// with a seeded walk instead of a full schedule tree.
const obsStateCap = 200000

type obsResult struct {
	steps int
	state State
	at    time.Time
	ok    bool
	over  bool
}

type obsMove struct {
	s        State
	at       time.Time
	kind     int
	attempts int
}

const (
	moveForced = iota
	moveWaste
	moveJump
	moveAcquire
	moveDump
)

type obsHit struct {
	n      int
	choice int
	ok     bool
}

type observer struct {
	memo map[string]obsHit
	over bool
}

func observeFair(start State, now time.Time) obsResult {
	sr := &observer{memo: map[string]obsHit{}}
	steps, ok := sr.rec(start, now, 0)
	if sr.over {
		return obsResult{over: true}
	}
	if !ok {
		return obsResult{}
	}
	end, at := sr.witness(start, now, 0)
	return obsResult{steps: steps, state: end, at: at, ok: true}
}

func (sr *observer) rec(s State, now time.Time, depth int) (int, bool) {
	if sr.over {
		return 0, false
	}
	if depth > 400 {
		sr.over = true
		return 0, false
	}
	s = obsPrep(s, now)
	if obsRunning(s, now) {
		return 0, true
	}
	key := obsKey(s, now)
	if hit, seen := sr.memo[key]; seen {
		return hit.n, hit.ok
	}
	if len(sr.memo) >= obsStateCap {
		sr.over = true
		return 0, false
	}
	// A re-entry while this key is open is a cycle, not a fair path.
	sr.memo[key] = obsHit{choice: -1}
	moves := fairMoves(s, now)
	best, choice := -1, -1
	for i := range moves {
		n, ok := sr.rec(moves[i].s, moves[i].at, depth+1)
		if sr.over {
			return 0, false
		}
		if ok && n+1 > best {
			best, choice = n+1, i
		}
	}
	if best < 0 {
		return 0, false
	}
	sr.memo[key] = obsHit{n: best, choice: choice, ok: true}
	return best, true
}

func (sr *observer) witness(s State, now time.Time, depth int) (State, time.Time) {
	s = obsPrep(s, now)
	if obsRunning(s, now) || depth > 400 {
		return s, now
	}
	hit, seen := sr.memo[obsKey(s, now)]
	if !seen || !hit.ok {
		return s, now
	}
	moves := fairMoves(s, now)
	if hit.choice < 0 || hit.choice >= len(moves) {
		return s, now
	}
	return sr.witness(moves[hit.choice].s, moves[hit.choice].at, depth+1)
}

func obsPrep(s State, now time.Time) State {
	nodes := make([]Node, len(s.Nodes))
	copy(nodes, s.Nodes)
	s.Nodes = nodes
	for i := range s.Nodes {
		if !s.Nodes[i].Stopped {
			s.Nodes[i].HeartbeatAt = now
		}
	}
	s.trace = nil
	return s
}

func fairMoves(s State, now time.Time) []obsMove {
	reader := obsReader(s, now)
	live := s.Lease.Present && s.Lease.Owner != "" && s.Lease.ExpireAt.After(now)
	if s.Lock.Held {
		idle := s.Params.IdleKill
		if idle <= 0 {
			idle = 5 * time.Second
		}
		when := now
		deadline := s.Lock.Since.Add(idle)
		if when.Before(deadline) {
			when = deadline
		}
		who := reader
		if who == "" {
			who = s.Lease.Owner
		}
		res := Step(s, Event{Kind: EventMetaLockTimeout, Node: who}, when)
		res.State.trace = nil
		return []obsMove{{s: res.State, at: when, kind: moveForced}}
	}
	if s.Phase == PhaseFailed && s.FailedReason == ReasonChainOwnerGone {
		owner := s.Chain.Owner
		if !s.Chain.VerifiedAt.After(s.FailedAt) {
			when := now
			if !when.After(s.FailedAt) {
				when = s.FailedAt.Add(time.Millisecond)
			}
			res := Step(s, Event{Kind: EventChainVerified, Node: owner, ReadOK: true}, when)
			if res.Decision != DecisionApplied {
				return nil
			}
			res.State.trace = nil
			return []obsMove{{s: res.State, at: when, kind: moveForced}}
		}
		res := Step(s, Event{Kind: EventChainOwnerBack, Node: owner}, now)
		if res.Decision != DecisionApplied {
			return nil
		}
		res.State.trace = nil
		return []obsMove{{s: res.State, at: now, kind: moveForced}}
	}
	if live && obsStopped(s, s.Lease.Owner) && now.Before(s.Lease.ExpireAt) {
		return []obsMove{{s: s, at: s.Lease.ExpireAt, kind: moveJump}}
	}
	if live && s.Lease.Owner != reader {
		if next, ok := obsYield(s, now); ok {
			return []obsMove{{s: next, at: now, kind: moveWaste}}
		}
		if now.Before(s.Lease.ExpireAt) {
			return []obsMove{{s: s, at: s.Lease.ExpireAt, kind: moveJump}}
		}
	}
	if s.Desired == DesiredStop && s.Phase == PhaseStopping && !live {
		who := reader
		if who == "" {
			who = s.Lease.Owner
		}
		res := Step(s, Event{Kind: EventStopExpired, Node: who}, now)
		if res.Decision != DecisionApplied {
			return nil
		}
		res.State.trace = nil
		return []obsMove{{s: res.State, at: now, kind: moveForced}}
	}
	if live && reader != "" && s.Lease.Owner == reader {
		if s.Phase == PhaseRetryBackoff && now.Before(s.RetryUntil) {
			return []obsMove{{s: s, at: s.RetryUntil, kind: moveJump}}
		}
		res := Step(s, Held(s, EventDumpReady, reader), now)
		if res.Decision == DecisionApplied || res.Decision == DecisionIdempotent {
			res.State.trace = nil
			return []obsMove{{s: res.State, at: now, kind: moveDump}}
		}
	}
	var out []obsMove
	seen := map[string]bool{}
	for _, node := range s.Nodes {
		if node.CanRead || node.Stopped {
			continue
		}
		sig := obsWasteSig(s, node.ID)
		if seen[sig] {
			continue
		}
		next, ok := obsWaste(s, now, node.ID)
		if !ok {
			continue
		}
		seen[sig] = true
		out = append(out, obsMove{s: next, at: now, kind: moveWaste, attempts: s.Refusal[node.ID].Attempts})
	}
	horizon := obsHorizon(s, now, reader)
	if horizon.After(now) {
		for _, when := range obsJumps(s, now, horizon) {
			out = append(out, obsMove{s: s, at: when, kind: moveJump})
		}
		return out
	}
	if reader != "" {
		res := Step(s, Event{Kind: EventAcquire, Node: reader}, now)
		if res.Decision == DecisionApplied {
			res.State.trace = nil
			out = append(out, obsMove{s: res.State, at: now, kind: moveAcquire})
		}
	}
	return out
}

func obsWaste(s State, now time.Time, id string) (State, bool) {
	res := Step(s, Event{Kind: EventAcquire, Node: id}, now)
	if res.Decision != DecisionApplied {
		return s, false
	}
	ev := Held(res.State, EventTailUnreadable, id)
	ev.Cause = CauseSegmentNotOnWorker
	y := Step(res.State, ev, now)
	if y.Decision == DecisionApplied {
		y.State.trace = nil
		return y.State, true
	}
	ev = Held(res.State, EventEpochLost, id)
	ev.Cause = CauseENOENT
	y = Step(res.State, ev, now)
	if y.Decision == DecisionApplied {
		y.State.trace = nil
		return y.State, true
	}
	return s, false
}

func obsYield(s State, now time.Time) (State, bool) {
	owner := s.Lease.Owner
	ev := Held(s, EventTailUnreadable, owner)
	ev.Cause = CauseSegmentNotOnWorker
	y := Step(s, ev, now)
	if y.Decision == DecisionApplied {
		y.State.trace = nil
		return y.State, true
	}
	ev = Held(s, EventEpochLost, owner)
	ev.Cause = CauseENOENT
	y = Step(s, ev, now)
	if y.Decision == DecisionApplied {
		y.State.trace = nil
		return y.State, true
	}
	return s, false
}

func obsWasteSig(s State, id string) string {
	r := s.Refusal[id]
	chain, open := byte('0'), byte('0')
	if s.Chain.Verified() && s.Chain.Owner == id {
		chain = '1'
	}
	if r.OpenEnded {
		open = '1'
	}
	return string([]byte{chain, open}) + strconv.Itoa(r.Attempts) + "/" + strconv.FormatInt(r.Until.UnixNano(), 10)
}

func obsJumps(s State, now, horizon time.Time) []time.Time {
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

func obsHorizon(s State, now time.Time, reader string) time.Time {
	if r, ok := s.Refusal[reader]; ok && !r.OpenEnded && r.Until.After(now) {
		return r.Until
	}
	return now
}

func obsReader(s State, now time.Time) string {
	if s.Lease.Present && s.Lease.Owner != "" && s.Lease.ExpireAt.After(now) && obsCanRead(s, s.Lease.Owner) {
		return s.Lease.Owner
	}
	for _, node := range s.Nodes {
		if obsCanRead(s, node.ID) {
			return node.ID
		}
	}
	return ""
}

func obsCanRead(s State, id string) bool {
	for i := range s.Nodes {
		if s.Nodes[i].ID != id {
			continue
		}
		if !s.Nodes[i].CanRead || s.Nodes[i].Stopped {
			return false
		}
		if r, ok := s.Refusal[id]; ok && r.OpenEnded {
			return false
		}
		return true
	}
	return false
}

func obsStopped(s State, id string) bool {
	for i := range s.Nodes {
		if s.Nodes[i].ID == id {
			return s.Nodes[i].Stopped
		}
	}
	return false
}

func obsRunning(s State, now time.Time) bool {
	if s.Lock.Held || s.Phase != PhaseRunning || s.Desired != DesiredRun {
		return false
	}
	if !s.Lease.Present || s.Lease.Owner == "" || !s.Lease.ExpireAt.After(now) {
		return false
	}
	return obsCanRead(s, s.Lease.Owner)
}

func obsKey(s State, now time.Time) string {
	var b strings.Builder
	b.Grow(160)
	put := func(v int64) {
		b.WriteString(strconv.FormatInt(v, 10))
		b.WriteByte('|')
	}
	put(now.UnixNano())
	b.WriteString(string(s.Phase))
	b.WriteByte('|')
	b.WriteString(string(s.Desired))
	b.WriteByte('|')
	b.WriteString(s.Lease.Owner)
	b.WriteByte('|')
	put(s.Lease.Epoch)
	put(s.Lease.ExpireAt.UnixNano())
	if s.Lease.Present {
		b.WriteByte('1')
	}
	b.WriteByte('|')
	put(s.RowGeneration)
	if s.Lock.Held {
		b.WriteString(s.Lock.HeldBy)
		put(s.Lock.Since.UnixNano())
	}
	b.WriteByte('|')
	put(int64(s.RetryAttempt))
	put(s.RetryUntil.UnixNano())
	b.WriteString(s.FailedReason)
	b.WriteByte('|')
	put(s.FailedAt.UnixNano())
	b.WriteString(s.Chain.Owner)
	b.WriteByte('|')
	put(s.Chain.VerifiedAt.UnixNano())
	put(s.TailSince.UnixNano())
	if s.PendingDelete {
		b.WriteByte('p')
	}
	if s.Deleted {
		b.WriteByte('d')
	}
	if s.MetaLockStalled {
		b.WriteByte('m')
	}
	b.WriteString(string(s.Params.Storage))
	b.WriteByte('|')
	put(int64(s.Params.ClaimInterval))
	put(int64(s.Params.RefusalRetryCap))
	for _, node := range s.Nodes {
		b.WriteString(node.ID)
		if node.CanRead {
			b.WriteByte('r')
		}
		if node.Stopped {
			b.WriteByte('s')
			put(node.HeartbeatAt.UnixNano())
		}
		r := s.Refusal[node.ID]
		put(int64(r.Attempts))
		put(r.Until.UnixNano())
		if r.OpenEnded {
			b.WriteByte('o')
		}
		if s.Chain.Verified() && s.Chain.Owner == node.ID {
			b.WriteByte('c')
		}
	}
	return b.String()
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

func freshClaim(t *testing.T, storage Storage) (State, time.Time) {
	t.Helper()
	s := boot(t, "n1", t0, []Node{peer("n2", false, t0)})
	s.Params.Storage = storage
	now := t0.Add(time.Hour)
	s = expireLease(s, now)
	return s, now
}

func j05Fixture(t *testing.T) (State, time.Time) {
	t.Helper()
	s, now := refusedReader(t, 1, 100*time.Second)
	s.Params.ClaimInterval = 3 * time.Second
	s.Params.RefusalRetryCap = 7 * time.Second
	return s, now
}

func j06Fixture(t *testing.T) (State, time.Time) {
	t.Helper()
	s, now := refusedReader(t, 1, 300*time.Second)
	s.Refusal["n2"] = Refusal{Reason: RefusalEpochLost, Until: now.Add(40 * time.Second), Attempts: 6}
	return s, now
}

func j11Fixture(t *testing.T) (State, time.Time) {
	t.Helper()
	// per_node chain owner cannot use TailUnreadable. EpochLost is dated, so
	// the node keeps claiming until H. Seven claims land on 126s: 15 steps.
	s, now := refusedReader(t, 1, 126*time.Second)
	s.Params.Storage = StoragePerNode
	s.Chain.Owner = "n2"
	s.Chain.VerifiedAt = now
	return s, now
}

func TestFairStepBoundObserved(t *testing.T) {
	// J06 hand count: stored attempts 6, so the next delays are 128s, 256s,
	// then 512s which is already past H=300s. n = 3. floor(300/600)+1 = 1,
	// so wastes ≤ 4. One deadline is already pending. jumps ≤ 4+1+1 = 6.
	// Acquire and DumpReady add 2. K = 12.
	s, now := j06Fixture(t)
	if got := FairStepBound(s, now); got != 12 {
		t.Fatalf("J06 bound %d, hand count is 12", got)
	}

	cases := []struct {
		name  string
		s     State
		now   time.Time
		obs   int
		tight bool
	}{}
	add := func(name string, s State, now time.Time, obs int, tight bool) {
		cases = append(cases, struct {
			name  string
			s     State
			now   time.Time
			obs   int
			tight bool
		}{name, s, now, obs, tight})
	}
	j5s, j5n := j05Fixture(t)
	add("J05 cap=7s H=100s", j5s, j5n, 33, false)
	j6s, j6n := j06Fixture(t)
	add("J06 attempts=6 H=300s", j6s, j6n, 7, false)
	j11s, j11n := j11Fixture(t)
	add("J11 per_node chain owner", j11s, j11n, 15, false)
	h0p, h0pn := freshClaim(t, StoragePerNode)
	add("H=0 per_node 1 non-reader", h0p, h0pn, 3, true)
	h0s, h0sn := freshClaim(t, StorageShared)
	add("H=0 shared 1 non-reader", h0s, h0sn, 3, true)
	one, oneNow := refusedReader(t, 1, 600*time.Second)
	add("1 non-reader H=600s", one, oneNow, 20, false)
	two, twoNow := refusedReader(t, 2, 600*time.Second)
	add("2 non-readers H=600s", two, twoNow, 37, false)
	short, shortNow := refusedReader(t, 2, 3*time.Second)
	add("2 non-readers H=3s", short, shortNow, 8, false)

	fmt.Println("fixture\tobs\tbound\tratio")
	for _, tc := range cases {
		end := observeFair(tc.s, tc.now)
		b := FairStepBound(tc.s, tc.now)
		if end.over || !end.ok {
			t.Fatalf("%s search over=%v ok=%v states capped at %d", tc.name, end.over, end.ok, obsStateCap)
		}
		if v := Check(end.state, end.at); len(v) > 0 {
			t.Fatalf("%s running state: %+v", tc.name, v)
		}
		if end.steps != tc.obs {
			t.Fatalf("%s observed %d, Step schedule is %d", tc.name, end.steps, tc.obs)
		}
		if end.steps > b {
			t.Fatalf("%s observed %d exceeds bound %d", tc.name, end.steps, b)
		}
		if tc.tight && end.steps != b {
			t.Fatalf("%s observed %d bound %d, equality is required here", tc.name, end.steps, b)
		}
		if end.steps == b && end.steps <= b-1 {
			t.Fatalf("%s schedule fit in bound-1 (%d)", tc.name, b-1)
		}
		ratio := 1.0
		if b > 0 {
			ratio = float64(end.steps) / float64(b)
		}
		fmt.Printf("%s\t%d\t%d\t%.3f\n", tc.name, end.steps, b, ratio)
		t.Logf("%s observed %d bound %d ratio %.3f", tc.name, end.steps, b, ratio)
	}
}

func TestFairStepBoundLargeConfigs(t *testing.T) {
	cases := []struct {
		name string
		n    int
		hold time.Duration
	}{
		{"3 non-readers H=600s", 3, 600 * time.Second},
		{"4 non-readers H=600s", 4, 600 * time.Second},
		{"1 non-reader H=1h", 1, time.Hour},
	}
	const seeds = 48
	fmt.Println("walk\tfixture\texhaustive\tmax\tbound\tratio\tseeds\tfinished")
	for _, tc := range cases {
		s, now := refusedReader(t, tc.n, tc.hold)
		b := FairStepBound(s, now)
		end := observeFair(s, now)
		if !end.over && end.ok {
			if end.steps > b {
				t.Fatalf("%s exhaustive %d exceeds bound %d", tc.name, end.steps, b)
			}
			ratio := float64(end.steps) / float64(b)
			fmt.Printf("walk\t%s\texhaustive\t%d\t%d\t%.3f\t0\t1\n", tc.name, end.steps, b, ratio)
			t.Logf("%s exhaustive %d bound %d ratio %.3f", tc.name, end.steps, b, ratio)
		}
		max, finished := walkMax(s, now, seeds)
		if max > b {
			t.Fatalf("%s walk %d exceeds bound %d", tc.name, max, b)
		}
		ratio := 0.0
		if b > 0 {
			ratio = float64(max) / float64(b)
		}
		how := "walk"
		if !end.over && end.ok {
			how = "exhaustive+walk"
		}
		fmt.Printf("walk\t%s\t%s\t%d\t%d\t%.3f\t%d\t%d\n", tc.name, how, max, b, ratio, seeds, finished)
		t.Logf("%s %s max %d bound %d ratio %.3f seeds %d finished %d", tc.name, how, max, b, ratio, seeds, finished)
	}
}

func TestReplayKnownSchedules(t *testing.T) {
	four, fourNow := refusedReader(t, 4, 600*time.Second)
	if got := replayFourNonReaders(t); got != 62 || got > FairStepBound(four, fourNow) {
		t.Fatalf("4 non-reader script %d, bound %d", got, FairStepBound(four, fourNow))
	}
	three, threeNow := refusedReader(t, 3, 600*time.Second)
	if got := replayThreeNonReaders(t); got != 52 || got > FairStepBound(three, threeNow) {
		t.Fatalf("3 non-reader script %d, bound %d", got, FairStepBound(three, threeNow))
	}
	fmt.Printf("replay\t4 non-readers script\t62\t%d\n", FairStepBound(four, fourNow))
	fmt.Printf("replay\t3 non-readers script\t52\t%d\n", FairStepBound(three, threeNow))
	t.Logf("replay 62 bound %d; replay 52 bound %d", FairStepBound(four, fourNow), FairStepBound(three, threeNow))
}

func walkMax(s State, now time.Time, seeds int) (max, finished int) {
	for seed := 0; seed < seeds; seed++ {
		n, done := walkOne(s, now, int64(20261020+seed*97))
		if n > max {
			max = n
		}
		if done {
			finished++
		}
	}
	n, done := greedyFair(s, now)
	if n > max {
		max = n
	}
	if done {
		finished++
	}
	return max, finished
}

func walkOne(start State, now time.Time, seed int64) (int, bool) {
	rng := rand.New(rand.NewSource(seed))
	s := start
	seen := map[string]int{}
	steps := 0
	for steps < 500 {
		s = obsPrep(s, now)
		if obsRunning(s, now) {
			return steps, true
		}
		key := obsKey(s, now)
		if seen[key] > 1 {
			return steps, false
		}
		seen[key]++
		moves := fairMoves(s, now)
		if len(moves) == 0 {
			return steps, false
		}
		m := pickWalk(rng, moves)
		s, now = m.s, m.at
		steps++
	}
	return steps, false
}

func pickWalk(rng *rand.Rand, moves []obsMove) obsMove {
	var wastes, jumps, rest []obsMove
	for _, m := range moves {
		switch m.kind {
		case moveWaste:
			wastes = append(wastes, m)
		case moveJump:
			jumps = append(jumps, m)
		default:
			rest = append(rest, m)
		}
	}
	roll := rng.Intn(10)
	if len(wastes) > 0 && roll < 8 {
		best := wastes[0]
		same := []obsMove{best}
		for _, w := range wastes[1:] {
			if w.attempts < best.attempts {
				best = w
				same = []obsMove{w}
				continue
			}
			if w.attempts == best.attempts {
				same = append(same, w)
			}
		}
		return same[rng.Intn(len(same))]
	}
	if len(jumps) > 0 && (roll < 9 || len(rest) == 0) {
		best := jumps[0]
		for _, j := range jumps[1:] {
			if j.at.Before(best.at) {
				best = j
			}
		}
		return best
	}
	if len(rest) > 0 {
		return rest[rng.Intn(len(rest))]
	}
	if len(wastes) > 0 {
		return wastes[rng.Intn(len(wastes))]
	}
	return jumps[0]
}

func greedyFair(start State, now time.Time) (int, bool) {
	s := start
	seen := map[string]int{}
	steps := 0
	for steps < 500 {
		s = obsPrep(s, now)
		if obsRunning(s, now) {
			return steps, true
		}
		key := obsKey(s, now)
		if seen[key] > 1 {
			return steps, false
		}
		seen[key]++
		moves := fairMoves(s, now)
		if len(moves) == 0 {
			return steps, false
		}
		s, now = pickGreedy(moves).s, pickGreedy(moves).at
		steps++
	}
	return steps, false
}

func pickGreedy(moves []obsMove) obsMove {
	bestWaste := -1
	for i := range moves {
		if moves[i].kind != moveWaste {
			continue
		}
		if bestWaste < 0 || moves[i].attempts < moves[bestWaste].attempts {
			bestWaste = i
		}
	}
	if bestWaste >= 0 {
		return moves[bestWaste]
	}
	bestJump := -1
	for i := range moves {
		if moves[i].kind != moveJump {
			continue
		}
		if bestJump < 0 || moves[i].at.Before(moves[bestJump].at) {
			bestJump = i
		}
	}
	if bestJump >= 0 {
		return moves[bestJump]
	}
	return moves[0]
}

type replayAct struct {
	id   string
	jump time.Duration
}

func replayThreeNonReaders(t *testing.T) int {
	t.Helper()
	s, later := refusedReader(t, 3, 600*time.Second)
	// One Step-realized 52-step schedule. It is a witness, not the maximum.
	return replayScript(t, s, later, []replayAct{
		{id: "n2"}, {id: "n3"},
		{jump: 2 * time.Second},
		{id: "n4"}, {id: "n2"},
		{jump: 4 * time.Second},
		{id: "n4"}, {id: "n3"},
		{jump: 6 * time.Second},
		{id: "n2"},
		{jump: 8 * time.Second},
		{id: "n3"},
		{jump: 14 * time.Second},
		{id: "n4"}, {id: "n2"},
		{jump: 16 * time.Second},
		{id: "n3"},
		{jump: 22 * time.Second},
		{id: "n4"},
		{jump: 30 * time.Second},
		{id: "n2"},
		{jump: 32 * time.Second},
		{id: "n3"},
		{jump: 38 * time.Second},
		{id: "n4"},
		{jump: 62 * time.Second},
		{id: "n2"},
		{jump: 64 * time.Second},
		{id: "n3"},
		{jump: 70 * time.Second},
		{id: "n4"},
		{jump: 126 * time.Second},
		{id: "n2"},
		{jump: 128 * time.Second},
		{id: "n3"},
		{jump: 134 * time.Second},
		{id: "n4"},
		{jump: 254 * time.Second},
		{id: "n2"},
		{jump: 256 * time.Second},
		{id: "n3"},
		{jump: 262 * time.Second},
		{id: "n4"},
		{jump: 510 * time.Second},
		{id: "n2"},
		{jump: 512 * time.Second},
		{id: "n3"},
		{jump: 518 * time.Second},
		{id: "n4"},
		{jump: 600 * time.Second},
	})
}

func replayFourNonReaders(t *testing.T) int {
	t.Helper()
	s, later := refusedReader(t, 4, 600*time.Second)
	return replayScript(t, s, later, []replayAct{
		{id: "n2"}, {id: "n3"}, {id: "n4"}, {id: "n5"},
		{jump: 2 * time.Second},
		{id: "n2"}, {id: "n3"}, {id: "n4"}, {id: "n5"},
		{jump: 6 * time.Second},
		{id: "n2"}, {id: "n3"}, {id: "n4"}, {id: "n5"},
		{jump: 14 * time.Second},
		{id: "n2"},
		{jump: 30 * time.Second},
		{id: "n3"}, {id: "n2"},
		{jump: 46 * time.Second},
		{id: "n3"},
		{jump: 62 * time.Second},
		{jump: 78 * time.Second},
		{id: "n4"},
		{jump: 94 * time.Second},
		{id: "n5"}, {id: "n4"}, {id: "n2"},
		{jump: 110 * time.Second},
		{id: "n5"}, {id: "n3"},
		{jump: 126 * time.Second},
		{id: "n4"},
		{jump: 142 * time.Second},
		{id: "n5"},
		{jump: 158 * time.Second},
		{id: "n2"},
		{jump: 174 * time.Second},
		{id: "n3"},
		{jump: 190 * time.Second},
		{id: "n4"},
		{jump: 206 * time.Second},
		{id: "n5"},
		{jump: 286 * time.Second},
		{id: "n2"},
		{jump: 302 * time.Second},
		{id: "n3"},
		{jump: 318 * time.Second},
		{id: "n4"},
		{jump: 334 * time.Second},
		{id: "n5"},
		{jump: 542 * time.Second},
		{id: "n2"},
		{jump: 558 * time.Second},
		{id: "n3"},
		{jump: 574 * time.Second},
		{id: "n4"},
		{jump: 590 * time.Second},
		{id: "n5"},
		{jump: 600 * time.Second},
	})
}

func replayScript(t *testing.T, s State, later time.Time, script []replayAct) int {
	t.Helper()
	at := later
	steps := 0
	for _, a := range script {
		if a.jump != 0 {
			at = later.Add(a.jump)
			steps++
			continue
		}
		next, ok := obsWaste(s, at, a.id)
		if !ok {
			t.Fatalf("waste %s at %s did not apply", a.id, at.Sub(later))
		}
		s = next
		steps++
	}
	reader := obsReader(s, at)
	res := Step(s, Event{Kind: EventAcquire, Node: reader}, at)
	if res.Decision != DecisionApplied {
		t.Fatalf("acquire %s", res.Decision)
	}
	steps++
	res = Step(res.State, Held(res.State, EventDumpReady, reader), at)
	if res.Decision != DecisionApplied && res.Decision != DecisionIdempotent {
		t.Fatalf("dump %s", res.Decision)
	}
	steps++
	if !obsRunning(res.State, at) {
		t.Fatal("schedule did not reach RUNNING")
	}
	return steps
}
