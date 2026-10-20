// Package ownership is the pure task-ownership transition from ADR 0011.
// input: State, the caller clock, and RefusalDelay
// output: FairStepBound, an upper bound on fair steps until RUNNING
// pos: the RTO step ceiling in ADR 0011 §4.1(a). It is not a schedule search
// note: if this file changes, update this header and module README.md.
package ownership

import "time"

// FairStepBound is an upper bound on fair-schedule steps from s at now until
// a reader is RUNNING. It is not the length of the longest schedule. A larger
// value is allowed. A smaller value is a bug.
//
// Proof sketch (ADR 0011 §4.1(a)):
//
// The reader is the live owner when that owner can read, is not stopped, and
// has no open-ended refusal; otherwise the first node in order with those
// properties. H is the longest dated refusal still ahead on any such node, or
// 0. Using the longest one only adds room.
//
// Forced steps are each counted once: a held row lock, chain verify, chain
// owner back, waiting out a stopped owner's TTL, yielding a live owner who
// cannot read, StopExpired, and a legal retry wait.
//
// Each non-reader (cannot read, not stopped, not the reader) contributes:
//   - 0 if it already holds an open-ended refusal, or its next ready time is
//     after H.
//   - at most 1 if storage is per_node and it is not the verified chain
//     owner. That TailUnreadable is NULL and does not expire. The verified
//     chain owner is not in this case: its release is EpochLost, which is dated.
//   - otherwise n+q. Stored Attempts is a. n counts successive calls
//     RefusalDelay(a+1), RefusalDelay(a+2), … until the delay is at least
//     min(cap, H), and counts that last delay. q is 0 when H or cap is 0,
//     otherwise floor(H/cap)+1, the most claims that fit in a window of
//     length H when each gap is at least cap. Those cap-spaced claims were
//     already eligible to be inside n, so n+q over-counts. A deadline already
//     pending inside (now, now+H] is one extra jump target.
//
// When H > 0 every waste creates at most one new deadline, every non-reader
// has at most one pending deadline, and the horizon is one more jump, so
// jumps ≤ wastes + pending + 1. When H is 0 there is no future deadline.
// A reader then takes Acquire and DumpReady. The sum of those pieces is K.
func FairStepBound(s State, now time.Time) int {
	if boundRunning(s, now) {
		return 0
	}
	p := s.Params.fill()
	H, reader := boundHorizon(s, now)
	forced := boundForced(s, now)
	wastes, pending := 0, 0
	for _, node := range s.Nodes {
		if node.ID == reader || node.CanRead || node.Stopped {
			continue
		}
		w, pend := boundNodeWastes(s, node.ID, now, H, p)
		wastes += w
		pending += pend
	}
	jumps := 0
	if H > 0 {
		jumps = wastes + pending + 1
	}
	finish := 0
	if reader != "" {
		finish = 2 // acquire and DumpReady
	}
	return forced + wastes + jumps + finish
}

func boundRunning(s State, now time.Time) bool {
	if s.Lock.Held || s.Phase != PhaseRunning || s.Desired != DesiredRun {
		return false
	}
	if !s.Lease.Present || s.Lease.Owner == "" || !s.Lease.ExpireAt.After(now) {
		return false
	}
	return boundCanRead(s, s.Lease.Owner)
}

func boundHorizon(s State, now time.Time) (time.Duration, string) {
	reader := ""
	if s.Lease.Present && s.Lease.ExpireAt.After(now) && boundCanRead(s, s.Lease.Owner) {
		reader = s.Lease.Owner
	}
	if reader == "" {
		for _, node := range s.Nodes {
			if boundCanRead(s, node.ID) {
				reader = node.ID
				break
			}
		}
	}
	H := time.Duration(0)
	for _, node := range s.Nodes {
		if !boundCanRead(s, node.ID) {
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

func boundForced(s State, now time.Time) int {
	n := 0
	live := s.Lease.Present && s.Lease.Owner != "" && s.Lease.ExpireAt.After(now)
	if s.Lock.Held {
		n++
	}
	if s.Phase == PhaseFailed && s.FailedReason == ReasonChainOwnerGone {
		n += 2
	}
	if live {
		owner := boundFind(s, s.Lease.Owner)
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

func boundNodeWastes(s State, id string, now time.Time, H time.Duration, p Params) (int, int) {
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

func boundCanRead(s State, id string) bool {
	n := boundFind(s, id)
	if n == nil || !n.CanRead || n.Stopped {
		return false
	}
	if r, ok := s.Refusal[id]; ok && r.OpenEnded {
		return false
	}
	return true
}

func boundFind(s State, id string) *Node {
	for i := range s.Nodes {
		if s.Nodes[i].ID == id {
			return &s.Nodes[i]
		}
	}
	return nil
}
