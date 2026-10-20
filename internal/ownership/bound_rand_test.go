// Package ownership is the pure task-ownership transition from ADR 0011.
// input: Step, Check, FairStepBound, and a local copy of the reader-horizon bound
// output: seeded configs whose fair-step count stays within FairStepBound
// pos: the randomized check that a reader-horizon-only window undercounts
// note: if this file changes, update this header and module README.md.
package ownership

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"
	"time"
)

// horizonOnlyBound is the d90eb7b8 bound. The window is the longest dated
// refusal on an eligible reader. A lock, a stopped owner, and a retry wait
// do not extend it, so a non-reader who becomes ready in that gap counts as 0.
// It is inlined here so the check does not call boundWindow or the other
// bound helpers.
func horizonOnlyBound(s State, now time.Time) int {
	if r6Running(s, now) {
		return 0
	}
	p := s.Params.fill()
	H, reader := r6Horizon(s, now)
	forced := r6Forced(s, now)
	wastes, pending := 0, 0
	for _, node := range s.Nodes {
		if node.ID == reader || node.CanRead || node.Stopped {
			continue
		}
		w, pend := r6Wastes(s, node.ID, now, H, p)
		wastes += w
		pending += pend
	}
	jumps := 0
	if H > 0 {
		jumps = wastes + pending + 1
	}
	finish := 0
	if reader != "" {
		finish = 2
	}
	return forced + wastes + jumps + finish
}

func r6Running(s State, now time.Time) bool {
	if s.Lock.Held || s.Phase != PhaseRunning || s.Desired != DesiredRun {
		return false
	}
	if !s.Lease.Present || s.Lease.Owner == "" || !s.Lease.ExpireAt.After(now) {
		return false
	}
	return r6CanRead(s, s.Lease.Owner)
}

func r6Horizon(s State, now time.Time) (time.Duration, string) {
	reader := ""
	if s.Lease.Present && s.Lease.ExpireAt.After(now) && r6CanRead(s, s.Lease.Owner) {
		reader = s.Lease.Owner
	}
	if reader == "" {
		for _, node := range s.Nodes {
			if r6CanRead(s, node.ID) {
				reader = node.ID
				break
			}
		}
	}
	H := time.Duration(0)
	for _, node := range s.Nodes {
		if !r6CanRead(s, node.ID) {
			continue
		}
		r, ok := s.Refusal[node.ID]
		if !ok || r.OpenEnded || !r.Until.After(now) {
			continue
		}
		if d := r.Until.Sub(now); d > H {
			H = d
		}
	}
	return H, reader
}

func r6Forced(s State, now time.Time) int {
	n := 0
	live := s.Lease.Present && s.Lease.Owner != "" && s.Lease.ExpireAt.After(now)
	if s.Lock.Held {
		n++
	}
	if s.Phase == PhaseFailed && s.FailedReason == ReasonChainOwnerGone {
		n += 2
	}
	if live {
		owner := r6Find(s, s.Lease.Owner)
		if owner != nil && owner.Stopped && now.Before(s.Lease.ExpireAt) {
			n++
		}
		if owner != nil && !owner.CanRead {
			n++
		}
	}
	if s.Desired == DesiredStop && s.Phase == PhaseStopping && !live {
		n++
	}
	if s.Phase == PhaseRetryBackoff && s.RetryUntil.After(now) {
		n++
	}
	return n
}

func r6Wastes(s State, id string, now time.Time, H time.Duration, p Params) (int, int) {
	r := s.Refusal[id]
	if r.OpenEnded {
		return 0, 0
	}
	ready := time.Duration(0)
	pending := 0
	if r.Until.After(now) {
		ready = r.Until.Sub(now)
		if ready <= H {
			pending = 1
		}
	}
	if ready > H {
		return 0, 0
	}
	if p.Storage == StoragePerNode && !(s.Chain.Verified() && s.Chain.Owner == id) {
		return 1, pending
	}
	cap := p.RefusalRetryCap
	if cap <= 0 {
		cap = 600 * time.Second
	}
	limit := cap
	if H < limit {
		limit = H
	}
	att := r.Attempts
	n := 0
	for n < maxRefusalAttempts {
		att++
		if att > maxRefusalAttempts {
			att = maxRefusalAttempts
		}
		if att < 1 {
			att = 1
		}
		n++
		if RefusalDelay(att, p.ClaimInterval, cap) >= limit {
			break
		}
	}
	extra := 0
	if H > 0 && cap > 0 {
		extra = int(H/cap) + 1
	}
	return n + extra, pending
}

func r6CanRead(s State, id string) bool {
	n := r6Find(s, id)
	if n == nil || !n.CanRead || n.Stopped {
		return false
	}
	if r, ok := s.Refusal[id]; ok && r.OpenEnded {
		return false
	}
	return true
}

func r6Find(s State, id string) *Node {
	for i := range s.Nodes {
		if s.Nodes[i].ID == id {
			return &s.Nodes[i]
		}
	}
	return nil
}

func TestFairStepBoundRandomConfigs(t *testing.T) {
	n := 300
	if v := os.Getenv("FAIR_BOUND_SEEDS"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 1 {
			t.Fatalf("FAIR_BOUND_SEEDS %q", v)
		}
		if parsed > n {
			n = parsed
		}
	}
	want := []int{94, 195, 231, 270}
	violations := 0
	got := map[int][3]int{}
	for seed := 0; seed < n; seed++ {
		s, now := fairRandState(t, int64(seed))
		bound := FairStepBound(s, now)
		old := horizonOnlyBound(s, now)
		end := observeFair(s, now)
		steps := end.steps
		if end.over || !end.ok {
			steps, _ = walkMax(s, now, 4)
			if !end.ok && !end.over {
				t.Fatalf("seed %d has no fair schedule", seed)
			}
		}
		if steps > bound {
			t.Fatalf("seed %d observed %d exceeds bound %d (r6 %d)", seed, steps, bound, old)
		}
		if steps > old {
			violations++
			got[seed] = [3]int{steps, old, bound}
		}
	}
	fmt.Printf("rand\tseeds\t%d\tr6_violations\t%d\tfair_violations\t0\n", n, violations)
	t.Logf("seeds %d r6 violations %d fair violations 0", n, violations)
	for _, seed := range want {
		row, ok := got[seed]
		if !ok {
			t.Fatalf("seed %d did not exceed the reader-horizon bound", seed)
		}
		fmt.Printf("rand\tseed\t%d\tobs\t%d\tr6\t%d\tbound\t%d\n", seed, row[0], row[1], row[2])
		t.Logf("seed %d observed %d r6 %d bound %d", seed, row[0], row[1], row[2])
	}
	if violations < 4 {
		t.Fatalf("reader-horizon bound undercounted %d seeds, want the QA seeds", violations)
	}
}

// fairRandState builds a small claim set plus one forced clock: a row lock,
// a stopped owner, or RETRY_BACKOFF. The non-reader refusal sits strictly
// after the reader horizon and on or before that forced deadline.
func fairRandState(t *testing.T, seed int64) (State, time.Time) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	now := t0.Add(time.Hour)
	nNodes := 1 + rng.Intn(4)
	kind := rng.Intn(3)
	if kind == 1 && nNodes < 3 {
		nNodes = 3
	}
	peers := make([]Node, 0, nNodes-1)
	for i := 1; i < nNodes; i++ {
		peers = append(peers, peer(nodeID(i), false, t0))
	}
	s := boot(t, "n1", t0, peers)
	s = expireLease(s, now)
	if rng.Intn(2) == 0 {
		s.Params.Storage = StorageShared
	}
	caps := []time.Duration{5, 7, 10, 15, 30}
	s.Params.RefusalRetryCap = caps[rng.Intn(len(caps))] * time.Second
	if rng.Intn(2) == 0 {
		s.Params.ClaimInterval = time.Second
	}
	holds := []time.Duration{0, time.Second, 2 * time.Second}
	hold := holds[rng.Intn(len(holds))]
	// One excluded claim is absorbed by the horizon jump. Two are not.
	nonReaders := nNodes - 1
	if kind == 1 {
		nonReaders = nNodes - 2
	}
	if nonReaders < 2 {
		hold = 0
	}
	gap := []time.Duration{time.Second, 2 * time.Second}[rng.Intn(2)]
	until := hold + gap
	slack := []time.Duration{time.Second, 2 * time.Second, 5 * time.Second}[rng.Intn(3)]
	deadline := until + slack
	attempts := rng.Intn(7)
	if hold > 0 {
		s.Refusal["n1"] = Refusal{Reason: RefusalEpochLost, Until: now.Add(hold), Attempts: 1}
	}
	if nNodes >= 4 && kind != 1 && rng.Intn(2) == 0 {
		s.Nodes[1].CanRead = true
		if hold > 0 {
			s.Refusal["n2"] = Refusal{Reason: RefusalEpochLost, Until: now.Add(hold), Attempts: 1}
		}
	}
	startNon := 1
	if kind == 1 {
		s.Nodes[0].Stopped = true
		s.Nodes[1].CanRead = true
		delete(s.Refusal, "n1")
		if hold > 0 {
			s.Refusal["n2"] = Refusal{Reason: RefusalEpochLost, Until: now.Add(hold), Attempts: 1}
		}
		s.Lease.Present = true
		s.Lease.Owner = "n1"
		s.Lease.ExpireAt = now.Add(deadline)
		s.Phase = PhaseRunning
		startNon = 2
	}
	for i := startNon; i < len(s.Nodes); i++ {
		if s.Nodes[i].CanRead {
			continue
		}
		s.Refusal[s.Nodes[i].ID] = Refusal{
			Reason:   RefusalEpochLost,
			Until:    now.Add(until),
			Attempts: attempts,
		}
	}
	switch kind {
	case 0:
		s.Lock = Lock{Held: true, HeldBy: "n1", Since: now}
		s.Params.IdleKill = deadline
	case 2:
		s.Phase = PhaseRetryBackoff
		s.RetryAttempt = 1 + rng.Intn(3)
		s.Lease.Present = true
		s.Lease.Owner = "n1"
		s.Lease.ExpireAt = now.Add(until)
		s.RetryUntil = now.Add(deadline)
	}
	return s, now
}
