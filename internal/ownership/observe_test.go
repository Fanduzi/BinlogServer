// Package ownership is the pure task-ownership transition from ADR 0011.
// input: Step and Check only. No helper from FairStepBound
// output: the observed fair-schedule length, and bottom-SCC terminality. Time in the key keeps each Step boundary exact and saturates only strictly past them. The operator FAILED table is an ADR literal
// pos: the Step search and the adversarial walks that check the upper bound
// note: if this file changes, update this header and module README.md.
package ownership

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// obsStateCap is the exhaustive-search limit. Past it the fixture is checked
// with a seeded walk instead of a full schedule tree.
const obsStateCap = 200000

// obsSeedBudget is the per-seed clock. A livelock that never repeats a key
// fails with the seed number instead of running until the test timeout.
const obsSeedBudget = 30 * time.Second

type obsResult struct {
	steps     int
	states    int
	state     State
	at        time.Time
	ok        bool
	over      bool
	timedOut  bool
	stuck     bool
	violation string
	bottoms   map[string]int
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

type obsNode struct {
	running  bool
	failed   bool
	reason   string
	declared bool
}

type observer struct {
	memo      map[string]obsHit
	node      map[string]obsNode
	edge      map[string][]string
	over      bool
	timedOut  bool
	deadline  time.Time
	stuck     bool
	violation string
	stuckAt   time.Time
	stuckSnap State
	kinds     map[string]int
}

func observeFair(start State, now time.Time) obsResult {
	sr := &observer{
		memo:     map[string]obsHit{},
		node:     map[string]obsNode{},
		edge:     map[string][]string{},
		kinds:    map[string]int{},
		deadline: time.Now().Add(obsSeedBudget),
	}
	steps, ok := sr.rec(start, now, 0)
	states := len(sr.memo)
	if sr.timedOut {
		return obsResult{over: true, timedOut: true, states: states}
	}
	if sr.over {
		return obsResult{over: true, states: states}
	}
	if msg := sr.audit(); msg != "" {
		sr.stuck = true
		sr.violation = msg
	}
	if sr.stuck || !ok {
		return obsResult{stuck: true, violation: sr.violation, states: states, state: sr.stuckSnap, at: sr.stuckAt, bottoms: sr.kinds}
	}
	end, at := sr.witness(start, now, 0)
	return obsResult{steps: steps, states: states, state: end, at: at, ok: true, bottoms: sr.kinds}
}

func (sr *observer) rec(s State, now time.Time, depth int) (int, bool) {
	if sr.over {
		return 0, false
	}
	if !sr.deadline.IsZero() && time.Now().After(sr.deadline) {
		sr.over = true
		sr.timedOut = true
		return 0, false
	}
	if depth > obsStateCap {
		sr.over = true
		return 0, false
	}
	s = obsPrep(s, now)
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
	sr.note(key, s, now)
	if runningEnd(s, now) {
		sr.memo[key] = obsHit{n: 0, choice: -1, ok: true}
		return 0, true
	}
	moves := fairMoves(s, now)
	for i := range moves {
		child := obsPrep(moves[i].s, moves[i].at)
		sr.edge[key] = append(sr.edge[key], obsKey(child, moves[i].at))
	}
	if len(moves) == 0 {
		if s.Phase == PhaseFailed && adrOperatorFailed(s.FailedReason) {
			sr.memo[key] = obsHit{n: 0, choice: -1, ok: true}
			return 0, true
		}
		sr.stuck = true
		sr.stuckSnap = s
		sr.stuckAt = now
		return 0, false
	}
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
	if runningEnd(s, now) || depth > obsStateCap {
		return s, now
	}
	if s.Phase == PhaseFailed && adrOperatorFailed(s.FailedReason) && len(fairMoves(s, now)) == 0 {
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

func (sr *observer) note(key string, s State, now time.Time) {
	sr.node[key] = obsNode{
		running:  runningEnd(s, now),
		failed:   s.Phase == PhaseFailed,
		reason:   s.FailedReason,
		declared: FairTerminal(s, now),
	}
}

func (sr *observer) audit() string {
	if len(sr.node) == 0 {
		return ""
	}
	keys := make([]string, 0, len(sr.node))
	index := map[string]int{}
	for k := range sr.node {
		index[k] = len(keys)
		keys = append(keys, k)
	}
	n := len(keys)
	adj := make([][]int, n)
	outN := make([]int, n)
	for from, tos := range sr.edge {
		i, ok := index[from]
		if !ok {
			continue
		}
		seen := map[int]bool{}
		for _, to := range tos {
			j, ok := index[to]
			if !ok || seen[j] {
				continue
			}
			seen[j] = true
			adj[i] = append(adj[i], j)
			outN[i]++
		}
	}
	comp := tarjan(adj)
	cid := make([]int, n)
	for c, members := range comp {
		for _, m := range members {
			cid[m] = c
		}
	}
	bottom := make([]bool, len(comp))
	for i := range bottom {
		bottom[i] = true
	}
	for i := 0; i < n; i++ {
		for _, j := range adj[i] {
			if cid[j] != cid[i] {
				bottom[cid[i]] = false
			}
		}
	}
	inBottom := make([]bool, n)
	for c, members := range comp {
		if !bottom[c] {
			continue
		}
		for _, m := range members {
			inBottom[m] = true
		}
		if msg := componentOK(members, keys, sr, adj); msg != "" {
			return msg
		}
		sr.kinds[bottomKind(members, keys, sr)]++
	}
	for i, k := range keys {
		nd := sr.node[k]
		if nd.running && !nd.declared {
			return "RUNNING sink is not declared"
		}
		if nd.declared && (!inBottom[i] || outN[i] > 0) {
			return "declared terminal is not absorbing"
		}
		if inBottom[i] && !nd.running && outN[i] == 0 && !nd.declared {
			return "absorbing state is not declared"
		}
	}
	return ""
}

func componentOK(members []int, keys []string, sr *observer, adj [][]int) string {
	run := false
	failed := 0
	reason := ""
	self := false
	for _, m := range members {
		nd := sr.node[keys[m]]
		if nd.running {
			run = true
		}
		if nd.failed {
			failed++
			reason = nd.reason
		}
		for _, j := range adj[m] {
			if j == m {
				self = true
			}
		}
	}
	if run {
		return ""
	}
	// adrOperatorFailed is the acceptance table. FairTerminal is only the
	// declaration checked in audit, not this predicate.
	if len(members) == 1 && failed == 1 && adrOperatorFailed(reason) && !self {
		return ""
	}
	return "bottom scc is not RUNNING or a single operator FAILED"
}

func bottomKind(members []int, keys []string, sr *observer) string {
	for _, m := range members {
		if sr.node[keys[m]].running {
			return "running"
		}
	}
	if len(members) == 1 && sr.node[keys[members[0]]].failed {
		return sr.node[keys[members[0]]].reason
	}
	return "other"
}

// adrOperatorFailed is the operator-required FAILED table in ADR 0011 §4.1(a):
// SOURCE_AUTH, LOG_BIN_OFF, LEGACY_UNKNOWN, CHAIN_OWNER_GONE.
// It is a literal copy of that sentence, not operatorFailed.
func adrOperatorFailed(reason string) bool {
	switch reason {
	case "SOURCE_AUTH", "LOG_BIN_OFF", "LEGACY_UNKNOWN", "CHAIN_OWNER_GONE":
		return true
	default:
		return false
	}
}

func tarjan(adj [][]int) [][]int {
	n := len(adj)
	index := make([]int, n)
	low := make([]int, n)
	for i := range index {
		index[i] = -1
	}
	var stack []int
	on := make([]bool, n)
	var comp [][]int
	var dfs func(int)
	next := 0
	dfs = func(v int) {
		index[v] = next
		low[v] = next
		next++
		stack = append(stack, v)
		on[v] = true
		for _, w := range adj[v] {
			if index[w] < 0 {
				dfs(w)
				if low[w] < low[v] {
					low[v] = low[w]
				}
			} else if on[w] && index[w] < low[v] {
				low[v] = index[w]
			}
		}
		if low[v] != index[v] {
			return
		}
		var members []int
		for {
			w := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			on[w] = false
			members = append(members, w)
			if w == v {
				break
			}
		}
		comp = append(comp, members)
	}
	for v := range adj {
		if index[v] < 0 {
			dfs(v)
		}
	}
	return comp
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
		if next, ok := advanceTime(s, now); ok {
			return []obsMove{{s: s, at: next, kind: moveJump}}
		}
		who := reader
		if who == "" {
			who = s.Lease.Owner
		}
		res := Step(s, Event{Kind: EventMetaLockTimeout, Node: who}, now)
		res.State.trace = nil
		return []obsMove{{s: res.State, at: now, kind: moveForced}}
	}
	if s.Phase == PhaseFailed && s.FailedReason == ReasonChainOwnerGone {
		if chainProgress(s, now) {
			return chainRecovery(s, now)
		}
		if next, ok := advanceTime(s, now); ok && chainProgress(s, next) {
			return timeMove(s, now)
		}
		return nil
	}
	if live && s.Lease.Owner != reader {
		owner := s.node(s.Lease.Owner)
		if owner != nil && !owner.Stopped {
			if next, ok := obsYield(s, now); ok {
				return []obsMove{{s: next, at: now, kind: moveWaste}}
			}
		}
		if now.Before(s.Lease.ExpireAt) {
			return timeMove(s, now)
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
	if s.Phase == PhaseRetryBackoff && now.Before(s.RetryUntil) {
		return timeMove(s, now)
	}
	var out []obsMove
	// progress step
	if live && reader != "" && s.Lease.Owner == reader {
		res := Step(s, Held(s, EventDumpReady, reader), now)
		if res.Decision == DecisionApplied || res.Decision == DecisionIdempotent {
			res.State.trace = nil
			return []obsMove{{s: res.State, at: now, kind: moveDump}}
		}
	}
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
	// sweep step
	if gone := Step(s, Event{Kind: EventChainOwnerGone, Node: "_health"}, now); gone.Decision == DecisionApplied {
		gone.State.trace = nil
		out = append(out, obsMove{s: gone.State, at: now, kind: moveForced})
	}
	if reader != "" && !(live && s.Lease.Owner == reader) {
		res := Step(s, Event{Kind: EventAcquire, Node: reader}, now)
		if res.Decision == DecisionApplied {
			res.State.trace = nil
			out = append(out, obsMove{s: res.State, at: now, kind: moveAcquire})
		}
	}
	if len(out) == 0 || readerWaiting(s, now, reader) {
		out = append(out, timeMove(s, now)...)
	}
	return out
}

func chainRecovery(s State, now time.Time) []obsMove {
	owner := s.Chain.Owner
	readOK := false
	if n := s.node(owner); n != nil {
		readOK = n.CanRead
	}
	var out []obsMove
	ver := Step(s, Event{Kind: EventChainVerified, Node: owner, ReadOK: readOK}, now)
	if ver.Decision == DecisionApplied {
		ver.State.trace = nil
		out = append(out, obsMove{s: ver.State, at: now, kind: moveForced})
	}
	back := Step(s, Event{Kind: EventChainOwnerBack, Node: owner}, now)
	if back.Decision == DecisionApplied {
		back.State.trace = nil
		out = append(out, obsMove{s: back.State, at: now, kind: moveForced})
	}
	return out
}

func chainProgress(s State, now time.Time) bool {
	for _, m := range chainRecovery(s, now) {
		if m.s.Phase != PhaseFailed || m.s.Chain.VerifiedAt.After(s.FailedAt) {
			return true
		}
	}
	return false
}

func advanceTime(s State, now time.Time) (time.Time, bool) {
	next := NextDeadline(s, now)
	if !next.After(now) {
		return now, false
	}
	return next, true
}

func timeMove(s State, now time.Time) []obsMove {
	next, ok := advanceTime(s, now)
	if !ok {
		return nil
	}
	return []obsMove{{s: s, at: next, kind: moveJump}}
}

func readerWaiting(s State, now time.Time, reader string) bool {
	if reader == "" {
		return false
	}
	r, ok := s.Refusal[reader]
	return ok && !r.OpenEnded && r.Until.After(now)
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

func obsReader(s State, now time.Time) string {
	return fairReader(s, now)
}

func obsCanRead(s State, id string) bool {
	return nodeReady(s, id)
}

// runningEnd is the search's RUNNING sink. It does not call fairRunning, so
// a mutant that edits the declaration cannot also move this sink.
func runningEnd(s State, now time.Time) bool {
	if s.Lock.Held || s.Phase != PhaseRunning || s.Desired != DesiredRun {
		return false
	}
	if !LeaseLive(s, now) {
		return false
	}
	n := s.node(s.Lease.Owner)
	return n != nil && n.CanRead && !n.Stopped
}

func obsRunning(s State, now time.Time) bool {
	return fairRunning(s, now)
}

func obsFind(s State, id string) *Node {
	for i := range s.Nodes {
		if s.Nodes[i].ID == id {
			return &s.Nodes[i]
		}
	}
	return nil
}

func obsKey(s State, now time.Time) string {
	p := s.Params.fill()
	var b strings.Builder
	b.Grow(160)
	put := func(v int64) {
		b.WriteString(strconv.FormatInt(v, 10))
		b.WriteByte('|')
	}
	rel := func(field string, t time.Time) {
		writeRegion(&b, field, t, now, p)
	}
	b.WriteString(string(s.Phase))
	b.WriteByte('|')
	b.WriteString(string(s.Desired))
	b.WriteByte('|')
	b.WriteString(s.Lease.Owner)
	b.WriteByte('|')
	if s.Lease.Present {
		b.WriteByte('1')
	}
	rel("expire", s.Lease.ExpireAt)
	rel("acquired", s.Lease.AcquiredAt)
	put(s.SpecRevision)
	put(s.FailedSpec)
	if s.Lock.Held {
		b.WriteString(s.Lock.HeldBy)
		rel("lockSince", s.Lock.Since)
	}
	b.WriteString(s.LockObserver)
	b.WriteByte('|')
	put(int64(clampRetry(s.RetryAttempt)))
	rel("retry", s.RetryUntil)
	b.WriteString(s.FailedReason)
	b.WriteByte('|')
	rel("failedAt", s.FailedAt)
	rel("unowned", s.UnownedSince)
	rel("progress", s.ProgressAt)
	rel("evaluated", s.EvaluatedAt)
	b.WriteString(s.Chain.Owner)
	b.WriteByte('|')
	if s.Chain.Verified() {
		b.WriteByte('v')
	}
	if s.Chain.MissingSealed {
		b.WriteByte('m')
	}
	if s.Chain.Unreadable {
		b.WriteByte('u')
	}
	put(int64(timeOrder(s.Chain.VerifiedAt, s.FailedAt)))
	if !s.TailSince.IsZero() {
		b.WriteByte('t')
	}
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
	put(int64(p.LeaseTTL))
	put(int64(p.ClaimInterval))
	put(int64(p.RefusalRetryCap))
	put(int64(p.IdleKill))
	put(int64(p.LockWait))
	put(int64(p.Sweep))
	put(int64(p.NoProgressAfter))
	put(int64(EffectiveNodeGone(p)))
	put(int64(EffectiveStaleAfter(p)))
	put(int64(s.LockRecoveries))
	for _, node := range s.Nodes {
		b.WriteString(node.ID)
		if node.CanRead {
			b.WriteByte('r')
		}
		if node.Stopped {
			b.WriteByte('s')
		}
		rel("heartbeat", node.HeartbeatAt)
		r := s.Refusal[node.ID]
		b.WriteString(r.Reason)
		b.WriteByte('|')
		put(int64(clampAttempts(r.Attempts)))
		rel("until", r.Until)
		if r.OpenEnded {
			b.WriteByte('o')
		}
		if s.Chain.Verified() && s.Chain.Owner == node.ID {
			b.WriteByte('c')
		}
	}
	return b.String()
}

func timeOrder(a, b time.Time) int {
	if a.IsZero() || b.IsZero() {
		return 0
	}
	if a.After(b) {
		return 1
	}
	if a.Before(b) {
		return -1
	}
	return 0
}

func clampAttempts(n int) int {
	if n > maxRefusalAttempts {
		return maxRefusalAttempts + 1
	}
	return n
}

func clampRetry(n int) int {
	if n > RetryBudget {
		return RetryBudget
	}
	return n
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

// qaLockFixture is the r6 counterexample: a refused non-reader, then a row
// lock. idle_kill lands after the refusal, so the node claims again.
func qaLockFixture(t *testing.T) (State, time.Time) {
	t.Helper()
	s, now := freshClaim(t, StorageShared)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, now)
	ev := Held(s, EventTailUnreadable, "n2")
	ev.Cause = CauseSegmentNotOnWorker
	s = applied(t, s, ev, now)
	s = applied(t, s, Event{Kind: EventLock, Node: "n1"}, now)
	return s, now
}

// stoppedOwnerFixture waits out a stopped holder's TTL. The non-reader whose
// refusal sits inside that TTL claims after the wait.
func stoppedOwnerFixture(t *testing.T) (State, time.Time) {
	t.Helper()
	now := t0.Add(time.Hour)
	s := boot(t, "n1", t0, []Node{
		peer("n2", true, t0),
		peer("n3", false, t0),
		peer("n4", false, t0),
	})
	s.Lease.Present = true
	s.Lease.Owner = "n1"
	s.Lease.ExpireAt = now.Add(15 * time.Second)
	s.Phase = PhaseRunning
	s.Nodes[0].Stopped = true
	s.Nodes[0].HeartbeatAt = now
	// The stopped holder is not the chain owner. The live reader is.
	s.Chain.Owner = "n2"
	s.Chain.VerifiedAt = now
	s.Refusal["n4"] = Refusal{Reason: RefusalEpochLost, Until: now.Add(10 * time.Second), Attempts: 1}
	return s, now
}

// twoReadersFixture has two dated readers. The search follows the first
// reader's horizon. A window that keeps only the shorter refusal undercounts.
func twoReadersFixture(t *testing.T) (State, time.Time) {
	t.Helper()
	now := t0.Add(time.Hour)
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0), peer("n3", false, t0)})
	s = expireLease(s, now)
	s.Params.Storage = StorageShared
	s.Refusal["n1"] = Refusal{Reason: RefusalEpochLost, Until: now.Add(30 * time.Second), Attempts: 1}
	s.Refusal["n2"] = Refusal{Reason: RefusalEpochLost, Until: now.Add(time.Second), Attempts: 1}
	return s, now
}

// retryBackoffFixture jumps to RetryUntil after the lease expires. The
// non-reader refused until a point between those two clocks claims there.
func retryBackoffFixture(t *testing.T) (State, time.Time) {
	t.Helper()
	s := boot(t, "n1", t0, []Node{peer("n2", false, t0)})
	now := t0.Add(time.Hour)
	s.Lease.ExpireAt = now.Add(15 * time.Second)
	s = applied(t, s, Held(s, EventRetryableError, "n1"), now)
	s.Lease.ExpireAt = now.Add(5 * time.Second)
	s.RetryUntil = now.Add(10 * time.Second)
	s.Refusal["n2"] = Refusal{Reason: RefusalEpochLost, Until: now.Add(6 * time.Second), Attempts: 1}
	return s, now
}

// untilJustAfterHorizon is one nanosecond past the reader. The non-reader
// never claims. The schedule is the window-end jump, Acquire, and DumpReady.
func untilJustAfterHorizon(t *testing.T) (State, time.Time) {
	t.Helper()
	s, now := freshClaim(t, StorageShared)
	s.Refusal["n1"] = Refusal{Reason: RefusalEpochLost, Until: now.Add(2 * time.Second), Attempts: 1}
	s.Refusal["n2"] = Refusal{Reason: RefusalEpochLost, Until: now.Add(2*time.Second + time.Nanosecond), Attempts: 1}
	return s, now
}

// untilAtHorizon puts the non-reader's refusal on the window endpoint.
// That claim is inside the window.
func untilAtHorizon(t *testing.T) (State, time.Time) {
	t.Helper()
	s, now := freshClaim(t, StorageShared)
	s.Refusal["n1"] = Refusal{Reason: RefusalEpochLost, Until: now.Add(2 * time.Second), Attempts: 1}
	s.Refusal["n2"] = Refusal{Reason: RefusalEpochLost, Until: now.Add(2 * time.Second), Attempts: 0}
	return s, now
}

// idleKillFixture holds the row lock for longer than the default 5s.
// Both non-readers become ready in that gap.
func idleKillFixture(t *testing.T) (State, time.Time) {
	t.Helper()
	now := t0.Add(time.Hour)
	s := boot(t, "n1", t0, []Node{peer("n2", false, t0), peer("n3", false, t0)})
	s = expireLease(s, now)
	s.Params.Storage = StoragePerNode
	s.Params.IdleKill = 8 * time.Second
	s.Lock = Lock{Held: true, HeldBy: "n1", Since: now}
	s.Refusal["n2"] = Refusal{Reason: RefusalEpochLost, Until: now.Add(6 * time.Second), Attempts: 1}
	s.Refusal["n3"] = Refusal{Reason: RefusalEpochLost, Until: now.Add(6 * time.Second), Attempts: 1}
	return s, now
}

// stoppedNonReaderOwnerFixture is a stopped holder who cannot read.
// The lease TTL is the clock that lets the refused non-readers claim.
func stoppedNonReaderOwnerFixture(t *testing.T) (State, time.Time) {
	t.Helper()
	now := t0.Add(time.Hour)
	s := boot(t, "n1", t0, []Node{peer("n2", true, t0), peer("n3", false, t0), peer("n4", false, t0)})
	s.Nodes[0].Stopped = true
	s.Nodes[0].CanRead = false
	s.Nodes[0].HeartbeatAt = now
	// The stopped holder is not the chain owner. The live reader is.
	s.Chain.Owner = "n2"
	s.Chain.VerifiedAt = now
	s.Lease.Present = true
	s.Lease.Owner = "n1"
	s.Lease.ExpireAt = now.Add(15 * time.Second)
	s.Phase = PhaseRunning
	s.Params.Storage = StoragePerNode
	s.Refusal["n3"] = Refusal{Reason: RefusalEpochLost, Until: now.Add(10 * time.Second), Attempts: 1}
	s.Refusal["n4"] = Refusal{Reason: RefusalEpochLost, Until: now.Add(10 * time.Second), Attempts: 1}
	return s, now
}

// stoppedChainOwnerFixture is QA's stopped chain owner. n2 verified the
// chain, lost the epoch, and was then stopped. n3's yield wrote the tail
// timestamp. n1 can read but is refused past node_gone, so the sweep's
// ChainOwnerGone is the fair end.
func stoppedChainOwnerFixture(t *testing.T) (State, time.Time) {
	t.Helper()
	now := t0
	s := boot(t, "n1", now, []Node{peer("n2", true, now), peer("n3", false, now)})
	s.Params.Storage = StorageShared
	s = expireLease(s, now)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, now)
	s = applied(t, s, Event{Kind: EventChainVerified, Node: "n2", ReadOK: true}, now)
	s = applied(t, s, Held(s, EventEpochLost, "n2"), now)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n3"}, now)
	ev := Held(s, EventTailUnreadable, "n3")
	ev.Cause = CauseSegmentNotOnWorker
	s = applied(t, s, ev, now)
	for i := range s.Nodes {
		if s.Nodes[i].ID == "n2" {
			s.Nodes[i].Stopped = true
			s.Nodes[i].HeartbeatAt = now
		}
	}
	gone := EffectiveNodeGone(s.Params)
	s.Refusal["n1"] = Refusal{Reason: RefusalEpochLost, Until: now.Add(gone + 2*time.Second), Attempts: 1}
	return s, now
}

// retryAfterExpiry is a second RetryableError whose RetryUntil is later
// than the lease. The search clock is in that gap.
func retryAfterExpiry(t *testing.T) (State, time.Time) {
	t.Helper()
	s := boot(t, "n1", t0, []Node{peer("n2", false, t0)})
	s.Params.LeaseTTL = 3 * time.Second
	s.Lease.ExpireAt = t0.Add(3 * time.Second)
	s = applied(t, s, Held(s, EventRetryableError, "n1"), t0)
	s = applied(t, s, Held(s, EventRetryableError, "n1"), t0.Add(2*time.Second))
	return s, t0.Add(4 * time.Second)
}

func j11Fixture(t *testing.T) (State, time.Time) {
	t.Helper()
	// per_node chain owner cannot use TailUnreadable. EpochLost is dated, so
	// the node keeps claiming until the directory loss becomes
	// CHAIN_OWNER_GONE. Seven claims land on 126s: 14 steps.
	s, now := refusedReader(t, 1, 126*time.Second)
	s.Params.Storage = StoragePerNode
	s.Chain.Owner = "n2"
	s.Chain.VerifiedAt = now
	return s, now
}

func TestSaturatedBackoffRepeatsKey(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	now := t0
	seen := map[string]bool{}
	for i := 0; i < 80; i++ {
		s = obsPrep(s, now)
		key := obsKey(s, now)
		if seen[key] {
			return
		}
		seen[key] = true
		if LeaseLive(s, now) && s.Lease.Owner != "" {
			ev := Held(s, EventEpochLost, s.Lease.Owner)
			ev.Cause = CauseENOENT
			res := Step(s, ev, now)
			if res.Decision != DecisionApplied {
				t.Fatalf("epoch lost %s %s", res.Decision, res.Detail)
			}
			s = res.State
			until := s.Refusal["n1"].Until
			if !until.After(now) {
				t.Fatal("epoch lost did not write a deadline")
			}
			now = until
			continue
		}
		res := Step(s, Event{Kind: EventAcquire, Node: "n1"}, now)
		if res.Decision != DecisionApplied {
			t.Fatalf("acquire %s %s", res.Decision, res.Detail)
		}
		s = res.State
	}
	t.Fatal("saturated backoff did not repeat a key")
}

func TestTimeKeyDeterminesSuccessors(t *testing.T) {
	mustShare := func(a, b State, an, bn time.Time) {
		t.Helper()
		a, b = obsPrep(a, an), obsPrep(b, bn)
		if obsKey(a, an) != obsKey(b, bn) {
			t.Fatal("states that differ only by a time shift or a saturated past do not share a key")
		}
		sameMoves(t, a, b, an, bn, 3)
	}
	shift := func(s State, now time.Time, d time.Duration) (State, time.Time) {
		s = s.clone()
		s.Lease.Epoch += 3
		s.RowGeneration += 5
		add := func(t time.Time) time.Time {
			if t.IsZero() {
				return t
			}
			return t.Add(d)
		}
		s.Lease.ExpireAt = add(s.Lease.ExpireAt)
		s.Lease.AcquiredAt = add(s.Lease.AcquiredAt)
		s.RetryUntil = add(s.RetryUntil)
		s.FailedAt = add(s.FailedAt)
		s.TailSince = add(s.TailSince)
		s.Chain.VerifiedAt = add(s.Chain.VerifiedAt)
		s.Lock.Since = add(s.Lock.Since)
		s.UnownedSince = add(s.UnownedSince)
		s.ProgressAt = add(s.ProgressAt)
		s.EvaluatedAt = add(s.EvaluatedAt)
		for i := range s.Nodes {
			s.Nodes[i].HeartbeatAt = add(s.Nodes[i].HeartbeatAt)
		}
		for id, r := range s.Refusal {
			r.Until = add(r.Until)
			s.Refusal[id] = r
		}
		return s, now.Add(d)
	}
	bases := []func() (State, time.Time){
		func() (State, time.Time) { return freshClaim(t, StoragePerNode) },
		func() (State, time.Time) { return freshClaim(t, StorageShared) },
		func() (State, time.Time) { return qaLockFixture(t) },
		func() (State, time.Time) { return stoppedOwnerFixture(t) },
		func() (State, time.Time) { return retryBackoffFixture(t) },
		func() (State, time.Time) { return j11Fixture(t) },
	}
	for _, mk := range bases {
		s, now := mk()
		t2, now2 := shift(s, now, 3*time.Hour+time.Millisecond)
		mustShare(s, t2, now, now2)
	}
	for seed := 0; seed < 20; seed++ {
		s, now := fairRandState(t, int64(seed))
		t2, now2 := shift(s, now, time.Duration(seed+1)*time.Minute)
		mustShare(s, t2, now, now2)
		perturbBoundaries(t, s, now)
	}
	// P2: NodeGoneAfter is 20h, so the old <= -limit key merged these two.
	// age == 20h rejects ChainOwnerGone; age == 25h applies it.
	now := t0
	young := chainGoneAtAge(t, now, 20*time.Hour)
	old := chainGoneAtAge(t, now, 25*time.Hour)
	gy := Step(young, Event{Kind: EventChainOwnerGone, Node: "_health"}, now)
	goo := Step(old, Event{Kind: EventChainOwnerGone, Node: "_health"}, now)
	if gy.Decision == goo.Decision {
		t.Fatalf("20h %s and 25h %s should disagree", gy.Decision, goo.Decision)
	}
	sameMoves(t, young, old, now, now, 3)
}

// perturbBoundaries moves one clock onto each boundary and onto a strictly
// older instant. Equal keys must share moves for three steps. The last
// boundary must not share a key with the saturated region: that is the
// r11 off-by-one, which used <= and swallowed the boundary.
func perturbBoundaries(t *testing.T, base State, now time.Time) {
	t.Helper()
	seen := map[string]bool{}
	for _, g := range stepTimeGuards() {
		if seen[g.field] {
			continue
		}
		seen[g.field] = true
		limit := fieldLimit(g.field, base.Params)
		ages := []time.Duration{
			limit - time.Nanosecond,
			limit,
			limit + time.Nanosecond,
			limit + time.Hour,
			limit + 5*time.Hour,
		}
		var states []State
		for _, age := range ages {
			if age <= 0 {
				continue
			}
			states = append(states, setClock(base, g.field, now.Add(-age)))
		}
		atLimit := setClock(base, g.field, now.Add(-limit))
		far := setClock(base, g.field, now.Add(-limit-time.Hour))
		if obsKey(atLimit, now) == obsKey(far, now) {
			t.Fatalf("%s at the last boundary shares the saturated key", g.field)
		}
		for i := range states {
			for j := i + 1; j < len(states); j++ {
				sameMoves(t, states[i], states[j], now, now, 3)
			}
		}
	}
}

func chainGoneAtAge(t *testing.T, now time.Time, age time.Duration) State {
	t.Helper()
	s := boot(t, "n1", now, nil)
	s = expireLease(s, now)
	s.Phase = PhaseLeaseDegraded
	s.Desired = DesiredRun
	s.Lease.Owner = ""
	s.Params.NodeGoneAfter = 20 * time.Hour
	s.TailSince = now.Add(-time.Hour)
	s.Chain.Owner = "n1"
	s.Chain.VerifiedAt = now.Add(-2 * time.Hour)
	s.Nodes[0].Stopped = true
	s.Nodes[0].CanRead = true
	s.Nodes[0].HeartbeatAt = now.Add(-age)
	return s
}

func setClock(s State, field string, at time.Time) State {
	s = s.clone()
	switch field {
	case "lockSince":
		s.Lock.Held = true
		if s.Lock.HeldBy == "" {
			s.Lock.HeldBy = "n1"
		}
		s.Lock.Since = at
	case "unowned":
		s.UnownedSince = at
	case "progress":
		s.ProgressAt = at
	case "acquired":
		s.Lease.AcquiredAt = at
	case "heartbeat":
		if len(s.Nodes) == 0 {
			s.Nodes = []Node{{ID: "n1", CanRead: true}}
		}
		s.Nodes[0].Stopped = true
		s.Nodes[0].HeartbeatAt = at
	case "expire":
		s.Lease.Present = true
		if s.Lease.Owner == "" {
			s.Lease.Owner = "n1"
		}
		s.Lease.ExpireAt = at
	case "retry":
		s.RetryUntil = at
	case "until":
		if s.Refusal == nil {
			s.Refusal = map[string]Refusal{}
		}
		id := "n1"
		if len(s.Nodes) > 0 {
			id = s.Nodes[0].ID
		}
		r := s.Refusal[id]
		r.Until = at
		r.OpenEnded = false
		if r.Reason == "" {
			r.Reason = RefusalEpochLost
		}
		s.Refusal[id] = r
	case "evaluated":
		s.EvaluatedAt = at
	case "failedAt":
		s.FailedAt = at
	default:
		panic("unknown clock " + field)
	}
	return s
}

func sameMoves(t *testing.T, a, b State, an, bn time.Time, depth int) {
	t.Helper()
	a, b = obsPrep(a, an), obsPrep(b, bn)
	if obsKey(a, an) != obsKey(b, bn) {
		return
	}
	ma, mb := fairMoves(a, an), fairMoves(b, bn)
	if len(ma) != len(mb) {
		t.Fatalf("equal keys, move count %d %d", len(ma), len(mb))
	}
	if depth == 0 {
		return
	}
	for i := range ma {
		if ma[i].kind != mb[i].kind {
			t.Fatalf("equal keys, move %d kind %d %d", i, ma[i].kind, mb[i].kind)
		}
		ca := obsPrep(ma[i].s, ma[i].at)
		cb := obsPrep(mb[i].s, mb[i].at)
		if obsKey(ca, ma[i].at) != obsKey(cb, mb[i].at) {
			t.Fatalf("equal keys, successor %d kind %d", i, ma[i].kind)
		}
		sameMoves(t, ca, cb, ma[i].at, mb[i].at, depth-1)
	}
}

func TestStepTimeGuardsCoverSource(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	dir := filepath.Dir(file)
	var blob string
	var lines []string
	for _, name := range []string{"step.go", "invariant.go", "bound.go"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		blob += string(raw)
		for _, line := range strings.Split(string(raw), "\n") {
			trim := strings.TrimSpace(line)
			if strings.HasPrefix(trim, "//") {
				continue
			}
			if strings.Contains(trim, "now.Sub(") || strings.Contains(trim, "now.Before(") || strings.Contains(trim, "now.After(") || strings.Contains(trim, ".After(now)") || strings.Contains(trim, ".Before(now)") || strings.Contains(trim, "time.Nanosecond") {
				lines = append(lines, trim)
			}
		}
	}
	var srcs []string
	for _, g := range stepTimeGuards() {
		if !strings.Contains(blob, g.src) {
			t.Errorf("guard src missing from Step: %s", g.src)
		}
		srcs = append(srcs, g.src)
	}
	allow := map[string]bool{
		"if at.After(now) {":  true,
		"if !at.After(now) {": true,
	}
	for _, line := range lines {
		if allow[line] {
			continue
		}
		covered := false
		for _, src := range srcs {
			if strings.Contains(line, src) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("clock comparison is not in the boundary table: %s", line)
		}
	}
}

// TestStepTimeGuardConstants pins each boundary-table constant to the Params
// field (or named function) written in that row's source. Halving no_progress,
// sweep, or stale_after moves the constant off the named value.
func TestStepTimeGuardConstants(t *testing.T) {
	p := Params{
		LeaseTTL:        30 * time.Second,
		ClaimInterval:   2 * time.Second,
		Sweep:           13 * time.Second,
		LockWait:        4 * time.Second,
		IdleKill:        3 * time.Second,
		StartToReady:    10 * time.Second,
		NodeGoneAfter:   7 * time.Hour,
		NoProgressAfter: 11 * time.Minute,
		StaleAfter:      9 * time.Minute,
	}
	for _, g := range stepTimeGuards() {
		got := g.cons(p)
		want := guardConstant(g.src, p)
		if got != want {
			t.Errorf("%s: constant %s, source names %s", g.src, got, want)
		}
	}
}

func guardConstant(src string, p Params) time.Duration {
	filled := p.fill()
	switch {
	case strings.Contains(src, "AcquiredAt"):
		return filled.NoProgressAfter
	case strings.Contains(src, "IdleKill"):
		return filled.IdleKill
	case strings.Contains(src, "LockWait"):
		return filled.LockWait
	case strings.Contains(src, "NoProgressAfter"):
		return filled.NoProgressAfter
	case strings.Contains(src, "Sweep"):
		return filled.Sweep
	case strings.Contains(src, "EffectiveNodeGone") || strings.Contains(src, "due.Before(now)"):
		return EffectiveNodeGone(p)
	case strings.Contains(src, "EffectiveStaleAfter"):
		return EffectiveStaleAfter(p)
	case strings.Contains(src, "time.Millisecond"):
		return time.Millisecond
	default:
		return 0
	}
}

func TestAdvanceBeforeDeadlinePreservesStep(t *testing.T) {
	for seed := 0; seed < 40; seed++ {
		s, now := fairRandState(t, int64(seed))
		next := NextDeadline(s, now)
		if !next.After(now) {
			continue
		}
		window := next.Sub(now)
		base := stepPrint(s, now)
		for _, dt := range []time.Duration{time.Nanosecond, window / 2, window - time.Nanosecond} {
			if dt <= 0 || !now.Add(dt).Before(next) {
				continue
			}
			if got := stepPrint(s, now.Add(dt)); got != base {
				t.Fatalf("seed %d advance %s before deadline %s changed Step", seed, dt, window)
			}
		}
	}
}

func TestUnkeyedStepInputsDoNotChangeOutcomes(t *testing.T) {
	s, now := fairRandState(t, 3)
	s = obsPrep(s, now)
	baseK := obsKey(s, now)
	base := stepPrint(s, now)
	check := func(name string, mut State) {
		t.Helper()
		if obsKey(mut, now) != baseK || stepPrint(mut, now) != base {
			t.Fatalf("%s changes the key or Step", name)
		}
	}
	next := s.clone()
	next.Lease.Epoch += 4
	check("epoch", next)
	next = s.clone()
	next.RowGeneration += 7
	check("row_generation", next)
	next = s.clone()
	next.RunID = "other"
	check("run_id", next)
	next = s.clone()
	next.LastError = "other"
	check("last_error", next)
	next = s.clone()
	next.Lease.RenewedAt = now.Add(time.Minute)
	check("renewed_at", next)
	next = s.clone()
	next.NoProgress = !s.NoProgress
	check("no_progress", next)
	next = s.clone()
	next.LastSweep = now.Add(-time.Minute)
	check("last_sweep", next)
	next = s.clone()
	next.HealthStatus = "OTHER"
	check("health_status", next)
	next = s.clone()
	next.Partitioned = !s.Partitioned
	check("partitioned", next)
	next = s.clone()
	next.RegistrationLive = map[string]bool{"n9": true}
	check("registration", next)
	next = s.clone()
	next.StoppedByOperator = !s.StoppedByOperator
	check("stopped_by_operator", next)
	next = s.clone()
	next.Chain.VerifiedEpoch += 3
	check("verified_epoch", next)
	next = s.clone()
	next.Params.SideKill = "auto"
	check("side_kill", next)
	next = s.clone()
	next.Params.Uploads = !s.Params.Uploads
	check("uploads", next)
	next = s.clone()
	next.Params.StartToReady = 3 * time.Second
	check("start_to_ready", next)
	next = s.clone()
	next.trace = append(next.trace, TraceEvent{Kind: EventSweep, At: now})
	check("trace", next)

	a := s.clone()
	a.MetaLockStalled = true
	a.Lock = Lock{Held: true, HeldBy: "n1", Since: now}
	a.LockRecoveries = 1
	a.LockObserver = "n1"
	b := a.clone()
	b.LockObserver = "n2"
	if obsKey(a, now) == obsKey(b, now) {
		t.Fatal("LockObserver is not in the key")
	}
	ra := Step(a, Event{Kind: EventLock, Node: "n1"}, now)
	rb := Step(b, Event{Kind: EventLock, Node: "n1"}, now)
	if ra.State.MetaLockStalled == rb.State.MetaLockStalled {
		t.Fatal("LockObserver did not change takeLock")
	}
}

func intentText(in []Intent) string {
	if len(in) == 0 {
		return "-"
	}
	parts := make([]string, len(in))
	for i, it := range in {
		parts[i] = fmt.Sprintf("%s/%s/%s/%s/%t", it.Kind, it.Node, it.Finding.Name, it.Finding.Subject, it.LocksTaskLease)
	}
	return strings.Join(parts, ",")
}

func stepPrint(s State, now time.Time) string {
	ids := []string{"_health"}
	for _, n := range s.Nodes {
		ids = append(ids, n.ID)
	}
	if s.Lease.Owner != "" {
		ids = append(ids, s.Lease.Owner)
	}
	if s.Chain.Owner != "" {
		ids = append(ids, s.Chain.Owner)
	}
	sort.Strings(ids)
	uniq := ids[:0]
	var prev string
	for _, id := range ids {
		if id == prev {
			continue
		}
		uniq = append(uniq, id)
		prev = id
	}
	kinds := []Kind{
		EventOperatorStart, EventOperatorStop, EventOperatorDelete,
		EventStopConfirmed, EventStopExpired, EventAcquire, EventLeaseExpired,
		EventDumpReady, EventRetryableError, EventTerminalError, EventTailUnreadable,
		EventEpochLost, EventChainVerified, EventChainOwnerGone, EventChainOwnerBack,
		EventMetaLockTimeout, EventRenew, EventSweep, EventLock, EventEvalDirFailed,
		EventLegacyUpdate,
	}
	var b strings.Builder
	for _, id := range uniq {
		for _, k := range kinds {
			ev := Event{Kind: k, Node: id, Cause: CauseENOENT, ReadOK: true}
			if epochFenced(k) {
				ev = Held(s, k, id)
				ev.Cause = CauseENOENT
				ev.ReadOK = true
			}
			res := Step(s, ev, now)
			fmt.Fprintf(&b, "%s %s %s %s %s %s %s|", id, k, res.Decision, res.Detail, res.State.Phase, res.State.FailedReason, intentText(res.Intents))
		}
	}
	return b.String()
}

func TestNobodyCanReadFitsBound(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", false, t0)})
	now := t0.Add(time.Hour)
	s = expireLease(s, now)
	for i := range s.Nodes {
		s.Nodes[i].CanRead = false
	}
	end := observeFair(s, now)
	b := FairStepBound(s, now)
	if end.over || !end.ok || end.stuck || end.steps > b {
		t.Fatalf("nobody can read steps %d bound %d over %v ok %v stuck %v %s phase %s",
			end.steps, b, end.over, end.ok, end.stuck, end.violation, end.state.Phase)
	}
	if end.state.Phase != PhaseFailed || end.state.FailedReason != ReasonChainOwnerGone {
		t.Fatalf("end %s %s steps %d bound %d", end.state.Phase, end.state.FailedReason, end.steps, b)
	}
}

func TestFairStepBoundObserved(t *testing.T) {
	// Dropping the window-end jump leaves this schedule over the bound.
	// The non-reader is ready 1ns after the reader, so it never claims.
	u06s, u06n := untilJustAfterHorizon(t)
	u06 := observeFair(u06s, u06n)
	if u06.over || !u06.ok || u06.steps != 3 || u06.steps > FairStepBound(u06s, u06n) || u06.steps != FairStepBound(u06s, u06n) {
		t.Fatalf("until = H+1ns observed %d bound %d over %v ok %v", u06.steps, FairStepBound(u06s, u06n), u06.over, u06.ok)
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
	// Stored attempts have to shrink the bound. The schedule that starts
	// those attempts at 0 is longer than the attempts=6 bound. Zeroing the
	// stored attempts makes the two bounds match, and that bound is at
	// least this longer schedule.
	fromZero := j6s
	fromZero.Refusal = make(map[string]Refusal, len(j6s.Refusal))
	for id, r := range j6s.Refusal {
		fromZero.Refusal[id] = r
	}
	zr := fromZero.Refusal["n2"]
	zr.Attempts = 0
	fromZero.Refusal["n2"] = zr
	zeroEnd := observeFair(fromZero, j6n)
	if zeroEnd.over || !zeroEnd.ok || FairStepBound(j6s, j6n) >= zeroEnd.steps {
		t.Fatalf("attempts=6 bound %d, from-zero schedule %d over %v ok %v", FairStepBound(j6s, j6n), zeroEnd.steps, zeroEnd.over, zeroEnd.ok)
	}
	j11s, j11n := j11Fixture(t)
	add("J11 per_node chain owner", j11s, j11n, 14, false)
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
	lockS, lockNow := qaLockFixture(t)
	add("QA lock + refused non-reader", lockS, lockNow, 6, false)
	stopS, stopNow := stoppedOwnerFixture(t)
	add("stopped owner + refused non-reader", stopS, stopNow, 5, false)
	twoR, twoRNow := twoReadersFixture(t)
	add("two readers", twoR, twoRNow, 12, false)
	retryS, retryNow := retryBackoffFixture(t)
	add("RETRY_BACKOFF", retryS, retryNow, 4, false)
	add("until = H+1ns", u06s, u06n, 3, true)
	u08s, u08n := untilAtHorizon(t)
	add("until = H", u08s, u08n, 4, false)

	fmt.Println("fixture\tobs\tbound\tratio")
	for _, tc := range cases {
		end := observeFair(tc.s, tc.now)
		b := FairStepBound(tc.s, tc.now)
		if end.over || !end.ok || end.stuck {
			t.Fatalf("%s search over=%v ok=%v stuck=%v states capped at %d", tc.name, end.over, end.ok, end.stuck, obsStateCap)
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
		ratio := 1.0
		if b > 0 {
			ratio = float64(end.steps) / float64(b)
		}
		fmt.Printf("%s\t%d\t%d\t%.3f\n", tc.name, end.steps, b, ratio)
		t.Logf("%s observed %d bound %d ratio %.3f", tc.name, end.steps, b, ratio)
	}
}

func chainGoneStopped(t *testing.T) (State, time.Time) {
	t.Helper()
	s, now := stoppedChainOwnerFixture(t)
	due := now
	if owner := obsFind(s, s.Chain.Owner); owner != nil && !owner.HeartbeatAt.IsZero() {
		due = owner.HeartbeatAt.Add(EffectiveNodeGone(s.Params) + time.Nanosecond)
	}
	res := Step(s, Event{Kind: EventChainOwnerGone, Node: "_health"}, due)
	if res.Decision != DecisionApplied || res.State.FailedReason != ReasonChainOwnerGone {
		t.Fatalf("sweep %s %s", res.Decision, res.Detail)
	}
	res.State.trace = nil
	return res.State, due
}

func resumeChainOwner(s State, read bool, now time.Time) State {
	nodes := append([]Node(nil), s.Nodes...)
	s.Nodes = nodes
	for i := range s.Nodes {
		if s.Nodes[i].ID != s.Chain.Owner {
			continue
		}
		s.Nodes[i].Stopped = false
		s.Nodes[i].CanRead = read
		s.Nodes[i].HeartbeatAt = now
	}
	return s
}

func bottomLabel(s State, now time.Time) (string, obsResult) {
	end := observeFair(s, now)
	if end.violation != "" {
		return "violation:" + end.violation, end
	}
	if end.stuck || !end.ok {
		return "stuck", end
	}
	if runningEnd(end.state, end.at) {
		return "running", end
	}
	if end.state.Phase == PhaseFailed {
		return "failed:" + end.state.FailedReason, end
	}
	return "other:" + string(end.state.Phase), end
}

// envFlipCanRead is the environment move that changes a node's directory.
// It is not a Step event.
func envFlipCanRead(s State, id string, read bool) State {
	nodes := append([]Node(nil), s.Nodes...)
	s.Nodes = nodes
	for i := range s.Nodes {
		if s.Nodes[i].ID == id {
			s.Nodes[i].CanRead = read
		}
	}
	return s
}

func TestZZFlipStuck(t *testing.T) {
	now := t0
	s := boot(t, "n1", now, []Node{peer("n2", false, now)})
	s.Params.Storage = StoragePerNode
	s = expireLease(s, now)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, now)
	ev := Held(s, EventTailUnreadable, "n2")
	ev.Cause = CauseSegmentNotOnWorker
	s = applied(t, s, ev, now)
	if !s.Refusal["n2"].OpenEnded {
		t.Fatal("per_node TailUnreadable did not write NULL")
	}
	s = envFlipCanRead(s, "n2", true)
	s = envFlipCanRead(s, "n1", false)
	if RefusalBlocks(s, "n2", now) {
		t.Fatal("recovered directory still blocks n2")
	}
	got := Step(s, Event{Kind: EventAcquire, Node: "n2"}, now)
	if got.Decision == DecisionApplied {
		t.Fatal("n2 took over a chain whose owner lost the directory")
	}
	label, end := bottomLabel(s, now)
	if label != "failed:"+ReasonChainOwnerGone {
		t.Fatalf("flip label %s steps %d phase %s reason %s", label, end.steps, end.state.Phase, end.state.FailedReason)
	}
	if end.steps > FairStepBound(s, now) {
		t.Fatalf("flip steps %d bound %d", end.steps, FairStepBound(s, now))
	}
	held := boot(t, "n1", now, nil)
	held = envFlipCanRead(held, "n1", false)
	label, end = bottomLabel(held, now)
	if label != "failed:"+ReasonChainOwnerGone {
		t.Fatalf("owner loop label %s steps %d phase %s", label, end.steps, end.state.Phase)
	}
}

func TestRecoveredDirectoryClearsTailRefusal(t *testing.T) {
	now := t0
	s := boot(t, "n1", now, []Node{peer("n2", false, now)})
	s.Params.Storage = StoragePerNode
	s = expireLease(s, now)
	s = applied(t, s, Event{Kind: EventAcquire, Node: "n2"}, now)
	ev := Held(s, EventTailUnreadable, "n2")
	ev.Cause = CauseSegmentNotOnWorker
	s = applied(t, s, ev, now)
	s = envFlipCanRead(s, "n2", true)
	if RefusalBlocks(s, "n2", now) {
		t.Fatal("NULL tail refusal outlived CanRead")
	}
	res := step(t, s, Event{Kind: EventAcquire, Node: "n2"}, now)
	if res.State.Phase != PhaseStarting || res.State.Lease.Owner != "n2" {
		t.Fatalf("recovered acquire: %s %s %s", res.Decision, res.State.Phase, res.Detail)
	}
}

func TestStoppedChainOwnerReachesTerminal(t *testing.T) {
	s, now := sweepOnlyFixture(t)
	due := now.Add(EffectiveNodeGone(s.Params) + time.Nanosecond)
	swept := Step(s, Event{Kind: EventChainOwnerGone, Node: "_health"}, due)
	if swept.Decision != DecisionApplied || swept.State.FailedReason != ReasonChainOwnerGone {
		t.Fatalf("sweep at the deadline: %s %s", swept.Decision, swept.Detail)
	}
	label, end := bottomLabel(s, now)
	b := FairStepBound(s, now)
	if label != "failed:"+ReasonChainOwnerGone || end.steps == 0 || end.steps != b {
		t.Fatalf("sweep-only label %s steps %d bound %d", label, end.steps, b)
	}
	fmt.Printf("fixture\tsweep only chain owner\t%d\t%d\n", end.steps, b)
}

func sweepOnlyFixture(t *testing.T) (State, time.Time) {
	t.Helper()
	now := t0
	s := New()
	s.Params.Storage = StoragePerNode
	s.Params.NodeGoneAfter = 3 * time.Second
	s.Phase = PhaseLeaseDegraded
	s.Desired = DesiredRun
	s.TailSince = now
	s.Chain.Owner = "n2"
	s.Chain.VerifiedAt = now.Add(-time.Second)
	s.Nodes = []Node{
		{ID: "n1", CanRead: false, HeartbeatAt: now},
		{ID: "n2", CanRead: true, Stopped: true, HeartbeatAt: now},
	}
	s.Refusal = map[string]Refusal{"n1": {Reason: RefusalTail, OpenEnded: true, Attempts: 1}}
	s.Lease.Present = true
	return s, now
}

func TestHeldLockRunningReachesRunning(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	s.Lock = Lock{Held: true, HeldBy: "n1", Since: t0}
	if FairTerminal(s, t0) {
		t.Fatal("a held lock is not a declared terminal")
	}
	label, end := bottomLabel(s, t0)
	if label != "running" || end.state.Lock.Held {
		t.Fatalf("held lock label %s held %v phase %s", label, end.state.Lock.Held, end.state.Phase)
	}
}

func TestChainOwnerUnreadableStaysTerminal(t *testing.T) {
	base, at := chainGoneStopped(t)
	s := resumeChainOwner(base, false, at)
	if InClaimSet(s, at) || !FairTerminal(s, at) || FairStepBound(s, at) != 0 {
		t.Fatal("alive chain owner without a directory is claimable or not terminal")
	}
	if !findOpen(s, FindingFailed, ReasonChainOwnerGone) {
		t.Fatal("CHAIN_OWNER_GONE is not surfaced as TASK_FAILED")
	}
	end := observeFair(s, at)
	if end.stuck || end.over || !end.ok || end.steps > FairStepBound(s, at) {
		t.Fatalf("unreadable owner steps %d bound %d stuck %v over %v ok %v", end.steps, FairStepBound(s, at), end.stuck, end.over, end.ok)
	}
	if end.state.Phase != PhaseFailed || end.state.FailedReason != ReasonChainOwnerGone {
		t.Fatalf("end phase %s reason %s", end.state.Phase, end.state.FailedReason)
	}
	if v := Check(end.state, end.at); len(v) > 0 {
		t.Fatalf("terminal state: %+v", v)
	}
	fmt.Printf("fixture\tchain owner alive unreadable\t%d\t%d\n", end.steps, FairStepBound(s, at))
	t.Logf("unreadable chain owner observed %d bound %d", end.steps, FairStepBound(s, at))
}

func TestChainOwnerReadableReturns(t *testing.T) {
	base, at := chainGoneStopped(t)
	s := resumeChainOwner(base, true, at)
	if FairTerminal(s, at) {
		t.Fatal("readable chain owner is terminal before ChainVerified")
	}
	end := observeFair(s, at)
	b := FairStepBound(s, at)
	if end.stuck || end.over || !end.ok || end.steps == 0 || end.steps > b || !obsRunning(end.state, end.at) {
		t.Fatalf("readable return steps %d bound %d stuck %v over %v ok %v phase %s violation %s", end.steps, b, end.stuck, end.over, end.ok, end.state.Phase, end.violation)
	}
	if v := Check(end.state, end.at); len(v) > 0 {
		t.Fatalf("running state: %+v", v)
	}
	fmt.Printf("fixture\tchain owner readable again\t%d\t%d\n", end.steps, b)
	t.Logf("readable chain owner observed %d bound %d", end.steps, b)
}

func TestClaimableFailedReachesRunning(t *testing.T) {
	s := boot(t, "n1", t0, nil)
	s = applied(t, s, heldCause(s, EventTerminalError, "n1", CauseSourceAuth), t0)
	if !FairTerminal(s, t0) || FairStepBound(s, t0) != 0 {
		t.Fatal("SOURCE_AUTH at the failed spec is not terminal")
	}
	bumped := s
	bumped.SpecRevision = s.FailedSpec + 1
	if FairTerminal(bumped, t0) || !InClaimSet(bumped, t0) {
		t.Fatal("spec bump left SOURCE_AUTH terminal")
	}
	if v := Check(bumped, t0); len(v) > 0 {
		t.Fatalf("claim-pull shape: %+v", v)
	}
	end := observeFair(bumped, t0)
	b := FairStepBound(bumped, t0)
	if end.stuck || !end.ok || end.steps == 0 || end.steps > b || !obsRunning(end.state, end.at) {
		t.Fatalf("spec bump steps %d bound %d stuck %v ok %v phase %s", end.steps, b, end.stuck, end.ok, end.state.Phase)
	}
	fmt.Printf("fixture\tSOURCE_AUTH spec bump\t%d\t%d\n", end.steps, b)
	t.Logf("spec bump observed %d bound %d", end.steps, b)
}

func TestStoppedChainOwnerDelaySweep(t *testing.T) {
	s, now := stoppedChainOwnerFixture(t)
	due := now
	if owner := obsFind(s, s.Chain.Owner); owner != nil && !owner.HeartbeatAt.IsZero() {
		due = owner.HeartbeatAt.Add(EffectiveNodeGone(s.Params) + time.Nanosecond)
	}
	swept := Step(s, Event{Kind: EventChainOwnerGone, Node: "_health"}, due)
	if swept.Decision != DecisionApplied || swept.State.FailedReason != ReasonChainOwnerGone {
		t.Fatalf("sweep at the deadline: %s %s", swept.Decision, swept.Detail)
	}
	// The longest fair path waits out the reader's refusal and reaches
	// RUNNING. Firing the sweep at the deadline is the shorter choice.
	end := observeFair(s, now)
	b := FairStepBound(s, now)
	if end.stuck || end.over || !end.ok || end.steps > b || !obsRunning(end.state, end.at) {
		t.Fatalf("delayed sweep steps %d bound %d stuck %v over %v ok %v phase %s", end.steps, b, end.stuck, end.over, end.ok, end.state.Phase)
	}
	if v := Check(end.state, end.at); len(v) > 0 {
		t.Fatalf("running state: %+v", v)
	}
	fmt.Printf("fixture\tstopped chain owner delayed sweep\t%d\t%d\n", end.steps, b)
	t.Logf("stopped chain owner delayed sweep observed %d bound %d", end.steps, b)
}

func TestIdleKillAndStoppedNonReader(t *testing.T) {
	lockS, lockNow := idleKillFixture(t)
	lock := observeFair(lockS, lockNow)
	if lock.stuck || !lock.ok || lock.steps != 7 || lock.steps > FairStepBound(lockS, lockNow) {
		t.Fatalf("idle_kill observed %d bound %d stuck %v ok %v", lock.steps, FairStepBound(lockS, lockNow), lock.stuck, lock.ok)
	}
	stopS, stopNow := stoppedNonReaderOwnerFixture(t)
	stop := observeFair(stopS, stopNow)
	if stop.stuck || !stop.ok || stop.steps != 5 || stop.steps > FairStepBound(stopS, stopNow) {
		t.Fatalf("stopped non-reader observed %d bound %d stuck %v ok %v", stop.steps, FairStepBound(stopS, stopNow), stop.stuck, stop.ok)
	}
	fmt.Printf("fixture\tidle_kill 8s\t%d\t%d\n", lock.steps, FairStepBound(lockS, lockNow))
	fmt.Printf("fixture\tstopped non-reader owner\t%d\t%d\n", stop.steps, FairStepBound(stopS, stopNow))
}

func TestRetryWaitAfterLeaseExpiry(t *testing.T) {
	s, now := retryAfterExpiry(t)
	if FairTerminal(s, now) {
		t.Fatal("expired lease inside RetryUntil is not terminal")
	}
	if len(fairMoves(s, now)) == 0 {
		t.Fatal("no fair move while RetryUntil is still ahead")
	}
	end := observeFair(s, now)
	b := FairStepBound(s, now)
	if end.stuck || !end.ok || end.steps > b {
		t.Fatalf("retry gap steps %d bound %d stuck %v ok %v", end.steps, b, end.stuck, end.ok)
	}
	if !obsRunning(end.state, end.at) {
		t.Fatalf("retry gap ended in %s", end.state.Phase)
	}
	fmt.Printf("fixture\tretry after lease expiry\t%d\t%d\n", end.steps, b)
	t.Logf("retry gap observed %d bound %d", end.steps, b)
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
