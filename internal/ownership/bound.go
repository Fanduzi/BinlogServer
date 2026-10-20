// Package ownership is the pure task-ownership transition from ADR 0011.
// input: State, the caller clock, and RefusalDelay
// output: FairStepBound, an upper bound on fair steps until RUNNING or a declared terminal FAILED
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
//     the sweep's ChainOwnerGone fires at HeartbeatAt+node_gone+1ns.
//     The owner is still stopped, so ChainOwnerBack stays refused and
//     FAILED/CHAIN_OWNER_GONE is terminal. That deadline is part of W,
//     and the ChainOwnerGone step is one of the forced steps.
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

// boundTerminal is RUNNING, or FAILED with a reason the fair scheduler
// does not leave. SOURCE_AUTH, LOG_BIN_OFF, and LEGACY_UNKNOWN have no
// fair exit. CHAIN_OWNER_GONE is terminal only while the chain owner is
// stopped or missing: the search does not refresh that heartbeat, so
// ChainOwnerBack stays refused. A live chain owner still verifies and
// comes back, and that path is not terminal.
func boundTerminal(s State, now time.Time) bool {
	if boundRunning(s, now) {
		return true
	}
	if s.Phase != PhaseFailed {
		return false
	}
	switch s.FailedReason {
	case ReasonSourceAuth, ReasonLogBinOff, ReasonLegacyUnknown:
		return true
	case ReasonChainOwnerGone:
		owner := boundFind(s, s.Chain.Owner)
		return owner == nil || owner.Stopped
	default:
		return false
	}
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

// boundWindow is how far the fair scheduler can push the clock before it
// finishes the claim set. Every deadline is folded with max, never min.
func boundWindow(s State, now time.Time) (time.Duration, string) {
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
	p := s.Params.fill()
	W := time.Duration(0)
	// Stopped chain owner. HeartbeatAt is not refreshed. ChainOwnerGone
	// applies one nanosecond after the effective node_gone deadline.
	if due, ok := boundStoppedChainGone(s, now, p); ok {
		W = boundExtend(W, now, due) // stopped chain owner
	}
	// Reader refusals. The scheduler waits for the reader it picked; taking
	// the longest eligible refusal only adds claims.
	for _, node := range s.Nodes {
		if !boundCanRead(s, node.ID) {
			continue
		}
		r, ok := s.Refusal[node.ID]
		if !ok || r.OpenEnded {
			continue
		}
		W = boundExtend(W, now, r.Until)
	}
	// Step clears a held row lock at Since+idle_kill, before the event runs.
	if s.Lock.Held && !s.Lock.Since.IsZero() {
		idle := p.IdleKill
		if idle <= 0 {
			idle = 5 * time.Second
		}
		W = boundExtend(W, now, s.Lock.Since.Add(idle))
	}
	live := s.Lease.Present && s.Lease.Owner != "" && s.Lease.ExpireAt.After(now)
	owner := boundFind(s, s.Lease.Owner)
	// A stopped holder is not yielded. fairMoves waits until ExpireAt and
	// does not also take the yielding arm.
	if live && owner != nil && owner.Stopped {
		W = boundExtend(W, now, s.Lease.ExpireAt) // stopped owner
	}
	// A live holder who is not stopped and is not the reader is yielded at
	// the current clock, or waited out until ExpireAt when that yield is
	// refused (step.go TailUnreadable / EpochLost).
	if live && owner != nil && !owner.Stopped && s.Lease.Owner != reader {
		W = boundExtend(W, now, s.Lease.ExpireAt) // yielding owner
	}
	// RETRY_BACKOFF jumps to RetryUntil even after the lease has expired.
	// Acquire stays refused until that instant.
	if s.Phase == PhaseRetryBackoff {
		W = boundExtend(W, now, s.RetryUntil) // retry window
	}
	// Chain verify is not applied at FailedAt itself. The scheduler steps one
	// millisecond later. ChainOwnerBack then runs at that same instant.
	if s.Phase == PhaseFailed && s.FailedReason == ReasonChainOwnerGone && !s.Chain.VerifiedAt.After(s.FailedAt) {
		W = boundExtend(W, now, s.FailedAt.Add(time.Millisecond))
	}
	return W, reader
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
	if _, ok := boundStoppedChainGone(s, now, s.Params.fill()); ok {
		n++ // ChainOwnerGone
	}
	return n
}

// boundStoppedChainGone is the sweep deadline for a stopped chain owner.
// The phase, the tail timestamp, and the heartbeat check are the guards in
// chainOwnerGone. A node that is not stopped is refreshed by the search and
// is not on this path.
func boundStoppedChainGone(s State, now time.Time, p Params) (time.Time, bool) {
	if s.Chain.Owner == "" || s.Phase != PhaseLeaseDegraded || s.TailSince.IsZero() {
		return time.Time{}, false
	}
	owner := boundFind(s, s.Chain.Owner)
	if owner == nil || !owner.Stopped {
		return time.Time{}, false
	}
	due := now
	if !owner.HeartbeatAt.IsZero() {
		due = owner.HeartbeatAt.Add(EffectiveNodeGone(p) + time.Nanosecond)
	}
	if due.Before(now) {
		due = now
	}
	return due, true
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
