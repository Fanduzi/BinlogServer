// Package ownership is the pure task-ownership transition from ADR 0011.
// input: State and the clock used for lease, refusal, and stale comparisons
// output: Violation list for invariants 1–10, ProgressOverdue from the applied-event trace, claim and stop sets, FairTerminal, /api/health code
// pos: the checkable predicates ADR 0011 §2 requires of every prefix
// note: if this file changes, update this header and module README.md.
package ownership

import "time"

// Violation is one failed predicate. Invariant is the ADR 0011 §2 number.
type Violation struct {
	Invariant string
	Detail    string
}

func epochFenced(k Kind) bool {
	switch k {
	case EventDumpReady, EventRetryableError, EventTerminalError, EventTailUnreadable, EventEpochLost, EventStopConfirmed, EventRenew:
		return true
	default:
		return false
	}
}

func dropInv(v []Violation, id string) []Violation {
	out := v[:0:0]
	for _, item := range v {
		if item.Invariant != id {
			out = append(out, item)
		}
	}
	return out
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
// A terminal FAILED row is claimable only after spec_revision moves past
// failed_spec_revision. CHAIN_OWNER_GONE is not claimable.
func InClaimSet(s State, now time.Time) bool {
	if s.Deleted || s.PendingDelete || s.Desired != DesiredRun || LeaseLive(s, now) {
		return false
	}
	if s.Phase == PhaseFailed {
		return failedSpecClaimable(s)
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
		switch s.FailedReason {
		case ReasonSourceAuth, ReasonLogBinOff, ReasonLegacyUnknown:
			// spec_revision > failed_spec_revision is the claim-pull shape and
			// still FAILED. Zero, or a revision above the row, means the
			// terminal write was dropped.
			if s.SpecRevision > 0 && (s.FailedSpec == 0 || s.FailedSpec > s.SpecRevision) {
				v = add(v, "2", "terminal FAILED did not record failed_spec_revision")
			}
		}
	} else if s.FailedSpec != 0 {
		v = add(v, "2", "failed_spec_revision set outside FAILED")
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
	// Invariant 7 is recomputed from the applied-event trace. ProgressAt,
	// UnownedSince, and NoProgress are the model's copies and are not read.
	if ProgressOverdue(s, now) != hasFinding(s, FindingNoProgress, s.TaskID) {
		v = add(v, "7", "TASK_NO_PROGRESS does not match the sweep and no_progress_after clocks")
	}
	return v
}

// failedSpecClaimable is the claim gate for a whitelisted FAILED row.
// SOURCE_AUTH, LOG_BIN_OFF, and LEGACY_UNKNOWN resume only when the spec
// revision has moved past the revision recorded at failure.
// CHAIN_OWNER_GONE never does: another node does not take over a chain
// whose owner lost the directory.
func failedSpecClaimable(s State) bool {
	switch s.FailedReason {
	case ReasonSourceAuth, ReasonLogBinOff, ReasonLegacyUnknown:
		return s.SpecRevision > s.FailedSpec
	default:
		return false
	}
}

// FairTerminal is the liveness end. RUNNING is one. A FAILED row is the
// other only while the fair scheduler cannot leave it.
// A whitelisted reason is terminal only when InClaimSet is false: the spec
// revision has not moved past failed_spec_revision. CHAIN_OWNER_GONE is
// never claimable. It stays terminal while the chain owner is stopped,
// absent, or alive with CanRead false. An owner who can still read the
// directory can ChainVerified and ChainOwnerBack.
func FairTerminal(s State, now time.Time) bool {
	if fairRunning(s, now) {
		return true
	}
	if s.Phase != PhaseFailed {
		return false
	}
	switch s.FailedReason {
	case ReasonSourceAuth, ReasonLogBinOff, ReasonLegacyUnknown:
		return !InClaimSet(s, now)
	case ReasonChainOwnerGone:
		n := s.node(s.Chain.Owner)
		return n == nil || n.Stopped || !n.CanRead
	default:
		return false
	}
}

func fairRunning(s State, now time.Time) bool {
	if s.Lock.Held || s.Phase != PhaseRunning || s.Desired != DesiredRun {
		return false
	}
	if !LeaseLive(s, now) {
		return false
	}
	return nodeReady(s, s.Lease.Owner)
}

func nodeReady(s State, id string) bool {
	n := s.node(id)
	if n == nil || !n.CanRead || n.Stopped {
		return false
	}
	if r, ok := s.Refusal[id]; ok && r.OpenEnded {
		return false
	}
	return true
}

// ProgressOverdue is invariant 7's clock, replayed from the applied-event trace.
// Step appends that log and never reads it back. This function does not read
// ProgressAt, UnownedSince, or NoProgress, and it does not call LeaseLive or
// LegalRetry. An empty trace is not overdue: a silent lease expiry does not
// start the unowned clock, and a later Check only extends a clock an applied
// event already started.
func ProgressOverdue(s State, now time.Time) bool {
	if len(s.trace) == 0 {
		return false
	}
	anchor, unownedSince, snap := replayProgress(s.trace)
	p := s.Params.fill()
	live := snap.Present && snap.Owner != "" && snap.Expire.After(now)
	legal := snap.Phase == PhaseRetryBackoff && !snap.Deleted && snap.RetryAttempt < RetryBudget && !snap.RetryUntil.IsZero() && snap.RetryUntil.After(now)
	if snap.Deleted || snap.Desired != DesiredRun || snap.Phase == PhaseFailed || snap.Phase == PhaseCreated {
		return false
	}
	if !live && !legal && snap.Phase != PhaseStopped {
		if unownedSince.IsZero() {
			return false
		}
		return !now.Before(unownedSince.Add(p.Sweep))
	}
	if live {
		if anchor.IsZero() {
			anchor = snap.Acquired
		}
		if !anchor.IsZero() && now.Sub(anchor) > p.NoProgressAfter {
			return true
		}
	}
	return false
}

// replayProgress walks the applied-event log. The progress anchor is the last
// DumpReady that landed in RUNNING, and it is cleared when Acquire,
// LeaseExpired, or OperatorStart changes AcquiredAt. The unowned clock starts
// at the first event whose snapshot is unowned, and clears when a later event
// is not. Lease and retry comparisons are strict After, so equality is outside
// both windows.
func replayProgress(trace []TraceEvent) (anchor, unownedSince time.Time, last TraceEvent) {
	var prevAcquired time.Time
	havePrev := false
	for _, ev := range trace {
		last = ev
		live := ev.Present && ev.Owner != "" && ev.Expire.After(ev.At)
		legal := ev.Phase == PhaseRetryBackoff && !ev.Deleted && ev.RetryAttempt < RetryBudget && !ev.RetryUntil.IsZero() && ev.RetryUntil.After(ev.At)
		unowned := ev.Desired == DesiredRun && !live && !legal && ev.Phase != PhaseCreated && ev.Phase != PhaseStopped && ev.Phase != PhaseFailed && !ev.Deleted
		if unowned {
			if unownedSince.IsZero() {
				unownedSince = ev.At
			}
		} else {
			unownedSince = time.Time{}
		}
		if ev.Kind == EventDumpReady && ev.Phase == PhaseRunning {
			anchor = ev.At
		}
		acquiredChanged := !havePrev || !ev.Acquired.Equal(prevAcquired)
		if acquiredChanged && (ev.Kind == EventAcquire || ev.Kind == EventLeaseExpired || ev.Kind == EventOperatorStart) {
			anchor = time.Time{}
		}
		prevAcquired = ev.Acquired
		havePrev = true
	}
	return anchor, unownedSince, last
}

// CheckStep is the transition half: desired STOP, FAILED, the epoch fence,
// chain writes, close-then-open, and operator HTTP.
func CheckStep(before, after State, e Event, res Result, now time.Time) []Violation {
	v := Check(after, now)
	// A dropped event does not re-derive findings, so the clock moving past
	// one sweep must not be blamed on the row that was just refused.
	if res.Decision != DecisionApplied {
		v = dropInv(v, "7")
	}
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
	// Invariant 3 also constrains events that were accepted. A dropped-event
	// check cannot see a stolen live lease or a stale epoch that was applied.
	if LeaseLive(before, now) && after.Lease.Owner != "" && after.Lease.Owner != before.Lease.Owner {
		v = add(v, "3", "another node took a live lease")
	}
	if e.Kind == EventStopExpired && res.Decision == DecisionApplied && LeaseLive(before, now) {
		v = add(v, "3", "StopExpired applied while the lease was live")
	}
	if res.Decision == DecisionApplied && epochFenced(e.Kind) {
		if e.Node != before.Lease.Owner || e.Epoch != before.Lease.Epoch {
			v = add(v, "3", "runner event applied without the current owner and epoch")
		}
	}
	if e.Kind == EventChainOwnerGone && before.Phase != PhaseFailed && after.Phase == PhaseFailed {
		if before.Phase != PhaseLeaseDegraded || before.TailSince.IsZero() {
			v = add(v, "2", "ChainOwnerGone entered FAILED outside LEASE_DEGRADED with a tail")
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
