// Package ownership is the pure task-ownership transition from ADR 0011.
// input: State, the caller clock, and RefusalDelay
// output: FairStepBound, an upper bound on fair steps. NextDeadline and the window share fairDeadlines
// pos: the RTO step ceiling in ADR 0011 §4.1(a). It is not a schedule search
// note: if this file changes, update this header and module README.md.
package ownership

import "time"

// FairStepBound is an upper bound on fair-schedule steps from s at now until
// a reader is RUNNING or the task is in a declared terminal FAILED state.
// It is not the length of the longest schedule. A larger value is allowed.
// A smaller value is a bug.
//
// Proof sketch (ADR 0011 §4.1(a)):
//
// The reader is the live owner when that owner can read, is not stopped, and
// has no open-ended refusal; otherwise the first node in order with those
// properties. The non-reader window W is the furthest the fair scheduler can
// move the clock before the claim set is worked: the longest dated refusal
// still ahead on any eligible reader, and every forced deadline below.
// Using the longest one only adds room.
//
// Clock jumps the fair scheduler can take, from fairMoves. W is the max of
// the absolute deadlines, not the sum. Each is also counted once as a step
// when it applies:
//   - row lock: Step drops it at Lock.Since+idle_kill before the event
//     (step.go preamble). A zero Since is already in the past.
//   - stopped owner: wait until Lease.ExpireAt. Not yielded.
//   - live owner who is not stopped and is not the reader: yield now, or
//     wait until Lease.ExpireAt if TailUnreadable and EpochLost are refused.
//   - RETRY_BACKOFF: jump to RetryUntil even when the lease has already
//     expired. Acquire stays refused until that instant, and the instant
//     can land after ExpireAt.
//   - CHAIN_OWNER_GONE before VerifiedAt is after FailedAt: jump to
//     FailedAt+1ms. ChainOwnerBack then runs at that same instant.
//   - dated refusals on eligible readers (the old horizon H).
//
// Not jumps, so they do not extend W:
//   - StopExpired, ChainOwnerBack, DumpReady, Acquire: applied at now.
//   - node_gone of a node that is not stopped: the search refreshes that
//     heartbeat, so the sweep never waits it out.
//   - a stopped chain owner is the exception. The heartbeat stays put.
//     Once the phase is LEASE_DEGRADED and tail_unreadable_since is set,
//     ChainOwnerGone may fire at any time from HeartbeatAt+node_gone+1ns
//     onward. The search may take another enabled move first. The deadline
//     is part of W, and the ChainOwnerGone step is one of the forced steps.
//     FAILED/CHAIN_OWNER_GONE is terminal while that owner is stopped,
//     absent, or alive with CanRead false. If the directory is already
//     gone but the row is not yet LEASE_DEGRADED with a tail timestamp,
//     the owner's EpochLost writes that timestamp and ChainOwnerGone is
//     the following step. That step is in K.
//   - UnownedSince+Sweep and no_progress_after: sweep facts, not a fair path
//     toward RUNNING or a terminal FAILED.
//   - lock_wait: only the second take that clears META_LOCK_STALLED. The
//     fair path is MetaLockTimeout at idle_kill.
//   - StartToReady and Renew: not fair failover steps.
//   - a waste's new refuse_until: a jump target inside W, not a new arm of
//     the max. takeLease's ExpireAt is the same. The scheduler does not
//     wait out a refusal created after the reader can already acquire.
//
// Each non-reader (cannot read, not stopped, not the reader) contributes:
//   - 0 if it already holds an open-ended refusal, or its next ready time is
//     after W. A ready time after the reader's own H still counts when a
//     forced jump lands past H.
//   - at most 1 if storage is per_node and it is not the verified chain
//     owner. That TailUnreadable is NULL and does not expire. The verified
//     chain owner is not in this case: its release is EpochLost, which is dated.
//   - otherwise n+q. Stored Attempts is a. n counts successive calls
//     RefusalDelay(a+1), RefusalDelay(a+2), … until the delay is at least
//     min(cap, W), and counts that last delay. q is 0 when W or cap is 0,
//     otherwise floor(W/cap)+1. Those cap-spaced claims were already eligible
//     to be inside n, so n+q over-counts. A deadline already pending inside
//     (now, now+W] is one extra jump target. The endpoint W is included.
//
// When W > 0 every waste creates at most one new deadline, every non-reader
// has at most one pending deadline, and the window end is one more jump, so
// jumps ≤ wastes + pending + 1. When W is 0 there is no future deadline.
// A reader then takes Acquire and DumpReady. The sum of those pieces is K.
func FairStepBound(s State, now time.Time) int {
	if boundTerminal(s, now) {
		return 0
	}
	p := s.Params.fill()
	W, reader := boundWindow(s, now)
	forced := boundForced(s, now)
	wastes, pending := 0, 0
	for _, node := range s.Nodes {
		if node.ID == reader || node.CanRead || node.Stopped {
			continue
		}
		w, pend := boundNodeWastes(s, node.ID, now, W, p)
		wastes += w
		pending += pend
	}
	jumps := 0
	if W > 0 {
		jumps = wastes + pending + 1
	}
	finish := 0
	if reader != "" {
		finish = 2 // acquire and DumpReady
	}
	return forced + wastes + jumps + finish
}

// boundTerminal is FairTerminal. The search uses the same predicate.
func boundTerminal(s State, now time.Time) bool {
	return FairTerminal(s, now)
}

// boundWindow is the furthest fairDeadlines instant. NextDeadline is the
// nearest one. Both read that list; neither lists the timers again.
func boundWindow(s State, now time.Time) (time.Duration, string) {
	reader := fairReader(s, now)
	W := time.Duration(0)
	for _, at := range fairDeadlines(s, now, reader) {
		W = boundExtend(W, now, at)
	}
	return W, reader
}

// fairDeadlines is every future instant Step reacts to on a fair path.
// NextDeadline takes the minimum. The bound takes the maximum.
func fairDeadlines(s State, now time.Time, reader string) []time.Time {
	s.Params = s.Params.fill()
	var out []time.Time
	add := func(at time.Time) {
		if at.After(now) {
			out = append(out, at)
		}
	}
	if due, ok := stoppedSweepDue(s, now); ok {
		add(due) // stopped chain owner
	}
	horizon := now
	var ready, other []time.Time
	for _, node := range s.Nodes {
		if node.Stopped {
			continue
		}
		r, ok := s.Refusal[node.ID]
		if !ok || r.OpenEnded || !r.Until.After(now) {
			continue
		}
		if node.CanRead {
			if r.Until.After(horizon) {
				horizon = r.Until
			}
			ready = append(ready, r.Until)
			continue
		}
		other = append(other, r.Until)
	}
	for _, at := range ready {
		add(at)
	}
	// A non-reader refusal past the reader horizon is not a fair wait:
	// the reader is already due, and that claim does not extend W.
	for _, at := range other {
		if !at.After(horizon) {
			add(at)
		}
	}
	if s.Lock.Held && !s.Lock.Since.IsZero() {
		idle := s.Params.IdleKill
		if idle <= 0 {
			idle = 5 * time.Second
		}
		add(s.Lock.Since.Add(idle))
	}
	live := s.Lease.Present && s.Lease.Owner != "" && s.Lease.ExpireAt.After(now)
	owner := s.node(s.Lease.Owner)
	if live && owner != nil && owner.Stopped {
		add(s.Lease.ExpireAt) // stopped owner
	}
	if live && owner != nil && !owner.Stopped && s.Lease.Owner != reader {
		add(s.Lease.ExpireAt) // yielding owner
	}
	if s.Phase == PhaseRetryBackoff {
		add(s.RetryUntil) // retry window
	}
	if s.Phase == PhaseFailed && s.FailedReason == ReasonChainOwnerGone && !s.Chain.VerifiedAt.After(s.FailedAt) {
		add(s.FailedAt.Add(time.Millisecond))
	}
	return out
}

// NextDeadline is the earliest future instant in fairDeadlines.
// The zero time means nothing is pending.
func NextDeadline(s State, now time.Time) time.Time {
	var best time.Time
	for _, at := range fairDeadlines(s, now, fairReader(s, now)) {
		if best.IsZero() || at.Before(best) {
			best = at
		}
	}
	return best
}

func boundExtend(W time.Duration, now, at time.Time) time.Duration {
	if !at.After(now) {
		return W
	}
	d := at.Sub(now)
	if d > W {
		W = d
	}
	return W
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
		owner := s.node(s.Lease.Owner)
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
	if chainGoneStep(s, now) {
		n++ // ChainOwnerGone
	}
	// The owner's EpochLost fills an empty tail timestamp and the next
	// step is ChainOwnerGone. Count that step before the row shows it.
	if ownerDirGone(s) && !chainGoneStep(s, now) {
		n++ // directory gone
	}
	return n
}

func boundNodeWastes(s State, id string, now time.Time, W time.Duration, p Params) (int, int) {
	r := s.Refusal[id]
	if r.OpenEnded {
		return 0, 0
	}
	ready := time.Duration(0)
	pending := 0
	if r.Until.After(now) {
		ready = r.Until.Sub(now)
		if ready <= W {
			pending = 1
		}
	}
	if ready > W {
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
	if W < limit {
		limit = W
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
	if W > 0 && cap > 0 {
		extra = int(W/cap) + 1
	}
	return n + extra, pending
}

func boundCanRead(s State, id string) bool {
	return nodeReady(s, id)
}
