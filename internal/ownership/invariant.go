// Package ownership is the pure task-ownership transition from ADR 0011.
// input: State and the clock used for lease, refusal, and stale comparisons
// output: Violation list for invariants 1–10, claim and stop sets, /api/health code
// pos: the checkable predicates ADR 0011 §2 requires of every prefix
// note: if this file changes, update this header and module README.md.
package ownership

import "time"

// Violation is one failed predicate. Invariant is the ADR 0011 §2 number.
type Violation struct {
	Invariant string
	Detail    string
}

func add(v []Violation, id, detail string) []Violation {
	return append(v, Violation{Invariant: id, Detail: detail})
}

// LeaseLive is owner set and expire_at > now. expire_at <= now can be acquired.
func LeaseLive(s State, now time.Time) bool {
	return s.Lease.Present && s.Lease.Owner != "" && s.Lease.ExpireAt.After(now)
}

// LegalRetry is a RETRY_BACKOFF whose budget remains and whose deadline is still ahead.
func LegalRetry(s State, now time.Time) bool {
	if s.Phase != PhaseRetryBackoff || s.Deleted {
		return false
	}
	if s.RetryAttempt >= RetryBudget || s.RetryUntil.IsZero() {
		return false
	}
	return now.Before(s.RetryUntil)
}

// RefusalBlocks is an unexpired refusal or a NULL per_node tail refusal.
func RefusalBlocks(s State, node string, now time.Time) bool {
	r, ok := s.Refusal[node]
	if !ok {
		return false
	}
	if r.OpenEnded {
		return true
	}
	return r.Until.After(now)
}

// InClaimSet is the running-claim union of invariant 9.
// STARTING with no live lease is included, whether or not the owner column is
// still filled: Acquire has committed, the dump has not opened, and the lease
// has since expired. That is the shape the peer must be able to take.
func InClaimSet(s State, now time.Time) bool {
	if s.Deleted || s.PendingDelete || s.Desired != DesiredRun || LeaseLive(s, now) || s.Phase == PhaseFailed {
		return false
	}
	switch s.Phase {
	case PhaseStarting, PhaseLeaseDegraded, PhaseRunning:
		return true
	case PhaseRetryBackoff:
		return !LegalRetry(s, now)
	case PhaseStopped:
		return s.Lease.Owner == ""
	default:
		return false
	}
}

// InStopSet is the stop-finalization union of invariant 9.
func InStopSet(s State, now time.Time) bool {
	return !s.Deleted && s.Desired == DesiredStop && s.Phase == PhaseStopping && !LeaseLive(s, now)
}

// Covered is invariant 9's safety half: every non-deleted shape is in a claim
// set, a stop set, held by a live lease, in a legal retry window, FAILED, or CREATED.
func Covered(s State, now time.Time) bool {
	if s.Deleted {
		return true
	}
	if s.Phase == PhaseFailed || s.Phase == PhaseCreated {
		return true
	}
	if s.Phase == PhaseStopped && s.Desired == DesiredStop {
		return true
	}
	if LeaseLive(s, now) || LegalRetry(s, now) || InClaimSet(s, now) || InStopSet(s, now) {
		return true
	}
	return false
}

// HealthCode is 503 when a CRITICAL finding is open, the health row is older
// than the effective stale_after, this round could not take the lock, or the
// node is partitioned. The stored HEALTHY status is not consulted.
func HealthCode(s State, now time.Time) int {
	if s.HealthBlocked || s.Partitioned || criticalOpen(s) {
		return 503
	}
	if s.Desired == DesiredRun && !s.EvaluatedAt.IsZero() && now.Sub(s.EvaluatedAt) > EffectiveStaleAfter(s.Params) {
		return 503
	}
	return 200
}

// Check is the state half of invariants 1, 2, 4, 5, 6, 7, 9, and 10.
func Check(s State, now time.Time) []Violation {
	if s.Deleted {
		return nil
	}
	var v []Violation
	if s.Desired != DesiredRun && s.Desired != DesiredStop {
		v = add(v, "1", "desired is neither RUN nor STOP")
	}
	if (s.Desired == DesiredStop) != s.StoppedByOperator {
		v = add(v, "1", "desired STOP is not exactly the operator stop flag")
	}
	if s.Phase == PhaseFailed {
		switch s.FailedReason {
		case ReasonSourceAuth, ReasonLogBinOff, ReasonChainOwnerGone, ReasonLegacyUnknown:
		default:
			v = add(v, "2", "FAILED reason "+s.FailedReason+" is outside the allowlist")
		}
	}
	for node, r := range s.Refusal {
		if r.OpenEnded {
			if r.Reason != RefusalTail || s.Params.Storage != StoragePerNode {
				v = add(v, "2", "NULL refusal on "+node+" is not per_node TailUnreadable")
			}
			if !r.Until.IsZero() {
				v = add(v, "10", "NULL refusal on "+node+" also has a deadline")
			}
		} else if r.Until.IsZero() {
			v = add(v, "10", "refusal on "+node+" has neither a deadline nor NULL")
		}
	}
	if s.Chain.Verified() && s.Chain.Owner == "" {
		v = add(v, "4", "verified chain has an empty owner")
	}
	if s.Chain.VerifiedAt.IsZero() && (s.Chain.MissingSealed || s.Chain.Unreadable) {
		v = add(v, "4", "unverified chain carries a directory finding fact")
	}
	wantTail := s.Desired == DesiredRun && !s.TailSince.IsZero()
	if wantTail != hasFinding(s, FindingTail, s.TaskID) {
		v = add(v, "5", "tail finding does not match tail_unreadable_since")
	}
	if s.Chain.Verified() && s.Chain.MissingSealed != hasFinding(s, FindingMissing, s.TaskID) {
		v = add(v, "5", "SEGMENT_MISSING does not match the verified directory")
	}
	if s.Chain.Verified() && s.Chain.Unreadable != hasFinding(s, FindingUnreadable, s.TaskID) {
		v = add(v, "5", "SEGMENT_UNREADABLE does not match the verified directory")
	}
	seen := map[string]bool{}
	for _, f := range s.Findings {
		if seen[f.Key()] {
			v = add(v, "5", "duplicate finding "+f.Key())
		}
		seen[f.Key()] = true
	}
	if !Covered(s, now) {
		v = add(v, "9", "phase "+string(s.Phase)+" desired "+string(s.Desired)+" is outside both unions")
	}
	if criticalOpen(s) && HealthCode(s, now) != 503 {
		v = add(v, "7", "open CRITICAL finding but /api/health is not 503")
	}
	if s.MetaLockStalled && HealthCode(s, now) != 503 {
		v = add(v, "6", "META_LOCK_STALLED but /api/health is not 503")
	}
	if s.NoProgress && !hasFinding(s, FindingNoProgress, s.TaskID) {
		v = add(v, "7", "no-progress fact without TASK_NO_PROGRESS")
	}
	return v
}

// CheckStep is the transition half: desired STOP, FAILED, the epoch fence,
// chain writes, close-then-open, and operator HTTP.
func CheckStep(before, after State, e Event, res Result, now time.Time) []Violation {
	v := Check(after, now)
	if before.Desired != DesiredStop && after.Desired == DesiredStop {
		if e.Kind != EventOperatorStop && e.Kind != EventOperatorDelete {
			v = add(v, "1", "non-operator event wrote desired STOP")
		}
	}
	if before.Desired == DesiredStop && after.Desired == DesiredRun && res.Decision == DecisionApplied {
		if e.Kind != EventOperatorStart {
			v = add(v, "1", "event cleared desired STOP")
		}
	}
	if after.Phase == PhaseFailed && before.Phase != PhaseFailed {
		if e.Kind != EventTerminalError && e.Kind != EventChainOwnerGone {
			v = add(v, "2", "event entered FAILED")
		}
	}
	switch e.Cause {
	case CauseENOENT, CauseENOSPC, CauseEROFS, CauseUnclassified, CauseWriteFenced, CauseLeaseUnreadable:
		if before.Phase != PhaseFailed && after.Phase == PhaseFailed {
			v = add(v, "2", "local cause "+string(e.Cause)+" became FAILED")
		}
	}
	if res.Decision == DecisionDiscarded || res.Decision == DecisionReread || res.Decision == DecisionRejected {
		if before.Phase != after.Phase || before.RowGeneration != after.RowGeneration || before.Lease.Epoch != after.Lease.Epoch || before.Desired != after.Desired {
			v = add(v, "3", "a dropped event wrote the task row")
		}
	}
	chainMoved := before.Chain.Owner != after.Chain.Owner || !before.Chain.VerifiedAt.Equal(after.Chain.VerifiedAt)
	if chainMoved && !(e.Kind == EventChainVerified && e.ReadOK && !e.Incomplete && res.Decision == DecisionApplied) && !after.Deleted {
		v = add(v, "4", "chain changed without ChainVerified")
	}
	opened := map[string]bool{}
	for _, f := range res.Opened {
		opened[f.Key()] = true
	}
	for _, f := range res.Closed {
		if opened[f.Key()] {
			v = add(v, "5", "finding "+f.Name+" closed and opened in one step")
		}
	}
	if res.HTTP >= 400 {
		if before.Phase != after.Phase || before.Desired != after.Desired || before.RowGeneration != after.RowGeneration || before.Lease.Epoch != after.Lease.Epoch || before.Lease.Owner != after.Lease.Owner || before.PendingDelete != after.PendingDelete {
			v = add(v, "8", "HTTP error still changed the row")
		}
	}
	if res.HTTP == 400 {
		v = add(v, "8", "operator HTTP is 400")
	}
	if e.Kind == EventOperatorStart && res.HTTP == 204 && res.Decision == DecisionApplied {
		if after.Phase != PhaseStarting || after.Desired != DesiredRun || after.RunID != "" || after.Lease.Owner == "" {
			v = add(v, "8", "advancing Start did not commit STARTING with an owner")
		}
		if !after.TailSince.Equal(before.TailSince) {
			v = add(v, "5", "OperatorStart cleared tail_unreadable_since")
		}
	}
	if e.Kind == EventOperatorStart && res.Decision == DecisionIdempotent {
		if before.SpecRevision != after.SpecRevision || before.Lease.Epoch != after.Lease.Epoch || before.RowGeneration != after.RowGeneration {
			v = add(v, "8", "idempotent Start wrote the row")
		}
	}
	return v
}
