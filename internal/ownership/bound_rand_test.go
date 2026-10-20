// Package ownership is the pure task-ownership transition from ADR 0011.
// input: Step, Check, and FairStepBound
// output: seeded configs, including CanRead recovery, whose fair-step count stays within FairStepBound. Overflow fails the test. Bottom SCCs are counted by kind
// pos: the randomized check that forced clocks and terminal paths stay inside the bound
// note: if this file changes, update this header and module README.md.
package ownership

import (
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

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
	var states []int
	exhaustive, tight, v04Hits, v17Hits := 0, 0, 0, 0
	bottoms := map[string]int{}
	for seed := 0; seed < n; seed++ {
		s, now := fairRandState(t, int64(seed))
		if !FairTerminal(s, now) && len(fairMoves(s, now)) == 0 {
			t.Fatalf("seed %d non-terminal state has no fair move", seed)
		}
		bound := FairStepBound(s, now)
		end := observeFair(s, now)
		states = append(states, end.states)
		if end.violation != "" || end.stuck {
			st := end.state
			var nodes string
			for _, node := range st.Nodes {
				r := st.Refusal[node.ID]
				nodes += fmt.Sprintf(" %s read=%v stop=%v open=%v until=%s att=%d",
					node.ID, node.CanRead, node.Stopped, r.OpenEnded, r.Until.Format(time.RFC3339Nano), r.Attempts)
			}
			t.Fatalf("seed %d stuck at %s phase %s reason %s desired %s owner %q present %v lock %v storage %s chain %s verified %v tail %v violation %s%s",
				seed, end.at.Format(time.RFC3339Nano), st.Phase, st.FailedReason, st.Desired, st.Lease.Owner, st.Lease.Present, st.Lock.Held,
				st.Params.Storage, st.Chain.Owner, st.Chain.Verified(), !st.TailSince.IsZero(), end.violation, nodes)
		}
		if end.over {
			t.Fatalf("seed %d overflow states %d", seed, end.states)
		}
		if !end.ok {
			t.Fatalf("seed %d has no fair schedule", seed)
		}
		exhaustive++
		if end.steps > bound {
			t.Fatalf("seed %d observed %d exceeds bound %d", seed, end.steps, bound)
		}
		for k, c := range end.bottoms {
			bottoms[k] += c
		}
		if bound > 0 && end.steps == bound {
			tight++
		}
		if s.Lock.Held && s.Params.IdleKill > 5*time.Second {
			shrunk := s
			shrunk.Params.IdleKill = 5 * time.Second
			if end.steps > FairStepBound(shrunk, now) {
				v04Hits++
			}
		}
		if v17Undercounts(s, now, end.steps) {
			v17Hits++
		}
	}
	sort.Ints(states)
	share := float64(exhaustive) / float64(n)
	med := states[len(states)/2]
	p90 := states[len(states)*9/10]
	fmt.Printf("rand\tconfigs\t%d\texhaustive\t%d\tshare\t%.3f\toverflow\t%d\ttight\t%d\tstates_min\t%d\tstates_med\t%d\tstates_p90\t%d\tstates_max\t%d\tv04\t%d\tv17\t%d\tbottoms\t%s\n",
		n, exhaustive, share, n-exhaustive, tight, states[0], med, p90, states[len(states)-1], v04Hits, v17Hits, formatBottoms(bottoms))
	t.Logf("configs %d exhaustive %d share %.3f overflow %d tight %d states min %d med %d p90 %d max %d v04 %d v17 %d bottoms %s",
		n, exhaustive, share, n-exhaustive, tight, states[0], med, p90, states[len(states)-1], v04Hits, v17Hits, formatBottoms(bottoms))
	if exhaustive != n {
		t.Fatalf("exhaustive share %.3f (%d overflow)", share, n-exhaustive)
	}
	if tight < 20 {
		t.Fatalf("tight schedules %d, want at least 20", tight)
	}
	if med < 10 {
		t.Fatalf("median states %d, searches are still the 3-9 state shape", med)
	}
	if v04Hits < 1 || v17Hits < 1 {
		t.Fatalf("generator did not undercut the idle_kill=5s bound (%d) or the can-read stopped-owner bound (%d)", v04Hits, v17Hits)
	}
}

// formatBottoms is the bottom-SCC tally by kind. Zero is not hard-coded.
func formatBottoms(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%d", k, m[k])
	}
	return b.String()
}

// v17Shape is a stopped owner who cannot read. A bound that extends
// W only when that owner can read counts the refusal as zero.
func v17Shape(s State, now time.Time) bool {
	if s.Lock.Held || s.Phase == PhaseRetryBackoff || !s.Lease.ExpireAt.After(now) {
		return false
	}
	owner := obsFind(s, s.Lease.Owner)
	return owner != nil && owner.Stopped && !owner.CanRead
}

func v17Undercounts(s State, now time.Time, steps int) bool {
	if !v17Shape(s, now) {
		return false
	}
	owner := obsFind(s, s.Lease.Owner)
	if owner == nil || !owner.Stopped || owner.CanRead {
		return false
	}
	// The mutant keeps W at the reader horizon only.
	horizon := time.Duration(0)
	for _, node := range s.Nodes {
		if !node.CanRead || node.Stopped {
			continue
		}
		r := s.Refusal[node.ID]
		if r.OpenEnded || !r.Until.After(now) {
			continue
		}
		if d := r.Until.Sub(now); d > horizon {
			horizon = d
		}
	}
	late := 0
	for _, node := range s.Nodes {
		if node.CanRead || node.Stopped || node.ID == s.Lease.Owner {
			continue
		}
		r := s.Refusal[node.ID]
		if r.OpenEnded || !r.Until.After(now) {
			continue
		}
		ready := r.Until.Sub(now)
		if ready > horizon && ready <= s.Lease.ExpireAt.Sub(now) {
			late++
		}
	}
	if late < 2 || horizon > 0 {
		return false
	}
	// Mutant: W=0, those refusals contribute 0, jumps 0.
	// Forced steps are the stopped wait and the unreadable owner. Finish is 2.
	mutant := 2 + 2
	return steps > mutant
}

// fairRandState mixes tight claims, a long idle_kill, a stopped non-reader
// owner, chain owners, and several forced clocks at once.
func fairRandState(t *testing.T, seed int64) (State, time.Time) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	now := t0.Add(time.Hour)
	switch rng.Intn(12) {
	case 0:
		if rng.Intn(3) == 0 {
			return oneNodeClaim(t)
		}
		if rng.Intn(2) == 0 {
			return freshClaim(t, StorageShared)
		}
		return freshClaim(t, StoragePerNode)
	case 1:
		return randIdleKill(t, now, rng)
	case 2:
		return randStoppedNonReader(t, now, rng)
	case 3, 4:
		return randStacked(t, now, rng)
	case 5, 6:
		return randCanReadFlip(t, now, rng)
	default:
		return randBranch(t, now, rng)
	}
}

// randCanReadFlip applies the environment move that restores a directory
// after a NULL tail refusal. Some seeds then drop the chain owner's directory.
func randCanReadFlip(t *testing.T, now time.Time, rng *rand.Rand) (State, time.Time) {
	t.Helper()
	s := bootPeers(t, 3)
	s = expireLease(s, now)
	s.Params.Storage = StoragePerNode
	s.Phase = PhaseLeaseDegraded
	s.TailSince = now.Add(-time.Minute)
	s.Chain.Owner = "n1"
	s.Chain.VerifiedAt = now.Add(-time.Hour)
	s.Nodes[0].CanRead = false
	s.Nodes[0].HeartbeatAt = now
	s.Nodes[1].CanRead = false
	s.Nodes[2].CanRead = false
	s.Refusal = map[string]Refusal{
		"n2": {Reason: RefusalTail, OpenEnded: true, Attempts: 1},
	}
	s = envFlipCanRead(s, "n2", true)
	if rng.Intn(2) == 0 {
		s = envFlipCanRead(s, "n1", true)
		delete(s.Refusal, "n2")
	}
	return s, now
}

func oneNodeClaim(t *testing.T) (State, time.Time) {
	t.Helper()
	s := boot(t, "n1", t0, nil)
	now := t0.Add(time.Hour)
	return expireLease(s, now), now
}

func randIdleKill(t *testing.T, now time.Time, rng *rand.Rand) (State, time.Time) {
	t.Helper()
	n := 2 + rng.Intn(2)
	s := bootPeers(t, n)
	s = expireLease(s, now)
	if rng.Intn(2) == 0 {
		s.Params.Storage = StorageShared
	}
	s.Params.IdleKill = []time.Duration{8, 10, 12}[rng.Intn(3)] * time.Second
	s.Params.RefusalRetryCap = []time.Duration{8, 15, 30}[rng.Intn(3)] * time.Second
	s.Lock = Lock{Held: true, HeldBy: "n1", Since: now}
	until := 6 * time.Second
	if s.Params.IdleKill > 10*time.Second {
		until = 8 * time.Second
	}
	for i := 1; i < len(s.Nodes); i++ {
		s.Refusal[s.Nodes[i].ID] = Refusal{Reason: RefusalEpochLost, Until: now.Add(until), Attempts: rng.Intn(3)}
	}
	return s, now
}

func randStoppedNonReader(t *testing.T, now time.Time, rng *rand.Rand) (State, time.Time) {
	t.Helper()
	// Four nodes: the stopped owner, the reader, and two non-readers
	// whose refusals sit inside the lease. Two late refusals are what
	// make a can-read-only window undercount.
	s := bootPeers(t, 4)
	s.Nodes[0].Stopped = true
	s.Nodes[0].CanRead = false
	// Fresh heartbeat. A stale one would make the first waste trip
	// ChainOwnerGone and the non-readers inside the lease would never claim.
	s.Nodes[0].HeartbeatAt = now
	s.Nodes[1].CanRead = true
	s.Chain.Owner = "n2"
	s.Chain.VerifiedAt = now
	s.Lease.Present = true
	s.Lease.Owner = "n1"
	ttl := []time.Duration{8, 12, 15}[rng.Intn(3)] * time.Second
	s.Lease.ExpireAt = now.Add(ttl)
	s.Phase = PhaseRunning
	if rng.Intn(2) == 0 {
		s.Params.Storage = StorageShared
	}
	s.Params.LeaseTTL = ttl
	s.Params.RefusalRetryCap = []time.Duration{10, 20, 30}[rng.Intn(3)] * time.Second
	until := ttl / 2
	if until < time.Second {
		until = time.Second
	}
	for i := 2; i < len(s.Nodes); i++ {
		s.Refusal[s.Nodes[i].ID] = Refusal{Reason: RefusalEpochLost, Until: now.Add(until), Attempts: rng.Intn(3)}
	}
	return s, now
}

func randBranch(t *testing.T, now time.Time, rng *rand.Rand) (State, time.Time) {
	t.Helper()
	// Shared storage with two or three non-readers and a reader horizon
	// of several seconds. per_node is the smaller tree; it is still more
	// than a single acquire.
	n := 3 + rng.Intn(2)
	s := bootPeers(t, n)
	s = expireLease(s, now)
	if rng.Intn(3) == 0 {
		s.Params.Storage = StoragePerNode
	} else {
		s.Params.Storage = StorageShared
	}
	s.Params.IdleKill = []time.Duration{8, 12}[rng.Intn(2)] * time.Second
	s.Params.LeaseTTL = []time.Duration{8, 15}[rng.Intn(2)] * time.Second
	s.Params.RefusalRetryCap = []time.Duration{4, 6, 8}[rng.Intn(3)] * time.Second
	s.Params.ClaimInterval = []time.Duration{time.Second, 2 * time.Second}[rng.Intn(2)]
	hold := []time.Duration{8, 10, 12}[rng.Intn(3)] * time.Second
	s.Refusal["n1"] = Refusal{Reason: RefusalEpochLost, Until: now.Add(hold), Attempts: 1}
	for i := 1; i < len(s.Nodes); i++ {
		s.Nodes[i].CanRead = false
		wait := time.Duration(1+rng.Intn(3)) * time.Second
		s.Refusal[s.Nodes[i].ID] = Refusal{
			Reason:   RefusalEpochLost,
			Until:    now.Add(wait),
			Attempts: rng.Intn(3),
		}
	}
	if s.Params.Storage == StoragePerNode && rng.Intn(2) == 0 {
		s.Chain.Owner = "n2"
		s.Chain.VerifiedAt = now
		s.Nodes[1].CanRead = false
	}
	return s, now
}

func randStacked(t *testing.T, now time.Time, rng *rand.Rand) (State, time.Time) {
	t.Helper()
	// Lock, stopped owner, retry, and refusals are all set. Each deadline
	// is inside the reader horizon, so after the forced jumps the
	// non-readers still branch. n1 stays a reader that is not stopped.
	n := 3 + rng.Intn(2)
	if rng.Intn(8) == 0 {
		n = 2
	}
	s, _ := randBranch(t, now, rng)
	if len(s.Nodes) != n {
		s = bootPeers(t, n)
		s = expireLease(s, now)
		s.Params.Storage = StorageShared
		s.Params.RefusalRetryCap = 6 * time.Second
		s.Params.ClaimInterval = time.Second
		s.Refusal["n1"] = Refusal{Reason: RefusalEpochLost, Until: now.Add(10 * time.Second), Attempts: 1}
		for i := 1; i < len(s.Nodes); i++ {
			s.Nodes[i].CanRead = false
			s.Refusal[s.Nodes[i].ID] = Refusal{Reason: RefusalEpochLost, Until: now.Add(time.Second), Attempts: 1}
		}
	}
	s.Params.IdleKill = []time.Duration{2, 4}[rng.Intn(2)] * time.Second
	s.Params.LeaseTTL = []time.Duration{4, 8, 15}[rng.Intn(3)] * time.Second
	s.Lock = Lock{Held: true, HeldBy: "n1", Since: now}
	if len(s.Nodes) >= 3 {
		s.Nodes[1].Stopped = true
		s.Nodes[1].CanRead = false
		s.Nodes[1].HeartbeatAt = now
		delete(s.Refusal, "n2")
		s.Lease.Present = true
		s.Lease.Owner = "n2"
		s.Lease.ExpireAt = now.Add(3 * time.Second)
	}
	s.Phase = PhaseRetryBackoff
	s.RetryAttempt = 1
	s.RetryUntil = now.Add(4 * time.Second)
	s.Lease.Present = true
	if s.Lease.Owner == "" {
		s.Lease.Owner = "n1"
	}
	if !s.RetryUntil.After(s.Lease.ExpireAt) {
		s.Lease.ExpireAt = now.Add(time.Second)
	}
	if rng.Intn(3) == 0 && len(s.Nodes) >= 2 {
		s.Params.Storage = StoragePerNode
		s.Chain.Owner = "n2"
		s.Chain.VerifiedAt = now
		s.Nodes[1].CanRead = false
	}
	if rng.Intn(4) == 0 && len(s.Nodes) >= 2 {
		s.Phase = PhaseLeaseDegraded
		s.RetryUntil = time.Time{}
		s.RetryAttempt = 0
		s.TailSince = now
		s.Chain.Owner = "n2"
		s.Chain.VerifiedAt = now.Add(-time.Second)
		s.Params.NodeGoneAfter = []time.Duration{6, 8}[rng.Intn(2)] * time.Second
		s.Nodes[1].Stopped = true
		s.Nodes[1].CanRead = false
		s.Nodes[1].HeartbeatAt = now
		delete(s.Refusal, "n2")
	}
	s.Nodes[0].CanRead = true
	s.Nodes[0].Stopped = false
	return s, now
}

func bootPeers(t *testing.T, n int) State {
	t.Helper()
	peers := make([]Node, 0, n-1)
	for i := 1; i < n; i++ {
		peers = append(peers, peer(nodeID(i), false, t0))
	}
	return boot(t, "n1", t0, peers)
}
