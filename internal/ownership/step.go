// Package ownership is the pure task-ownership transition from ADR 0011.
// input: State, one Event (LeaseUnreadable included), and the caller-supplied clock
// output: Result, the next state and the side-effect intents
// pos: the single transition function; it does no I/O and reads no clock
// note: if this file changes, update this header and module README.md.
package ownership

import "time"

// Decision is what Step did with one event.
type Decision string

const (
	DecisionApplied    Decision = "applied"
	DecisionIdempotent Decision = "idempotent"
	DecisionRejected   Decision = "rejected"
	DecisionDiscarded  Decision = "discarded"
	DecisionReread     Decision = "reread"
)

// Intent is a side effect the caller performs. Step itself does not.
type Intent struct {
	Kind           string
	Node           string
	Finding        Finding
	Detail         string
	LocksTaskLease bool
}

const (
	IntentAcquire        = "acquire"
	IntentRelease        = "release"
	IntentStopLocal      = "stop_local"
	IntentDeleteTask     = "delete_task"
	IntentTombstone      = "tombstone"
	IntentDeleteRefusals = "delete_refusals"
	IntentClusterFinding = "cluster_finding"
	IntentRollback       = "rollback"
	IntentReread         = "reread"
)

// Result is the next state. HTTP is set only for operator events.
type Result struct {
	State          State
	Decision       Decision
	HTTP           int
	Detail         string
	Intents        []Intent
	Opened         []Finding
	Closed         []Finding
	LocksTaskLease bool
	ZeroRow        bool
}

// Step applies one event at the caller-supplied time. The input state is not modified.
func Step(s State, e Event, now time.Time) Result {
	s = s.clone()
	s.Params = s.Params.fill()
	// The server drops an idle transaction at idle_kill. MetaLockTimeout only
	// records the stall; it must not pretend the row lock is already gone.
	if s.Lock.Held && !s.Lock.Since.IsZero() && !now.Before(s.Lock.Since.Add(s.Params.IdleKill)) {
		s.Lock = Lock{}
	}
	if s.Refusal == nil {
		s.Refusal = map[string]Refusal{}
	}
	if s.TaskID == "" {
		s.TaskID = "task"
	}
	if s.Deleted && e.Kind != EventSweep {
		return Result{State: s, Decision: DecisionDiscarded, ZeroRow: true, Detail: "deleted"}
	}
	switch e.Kind {
	case EventOperatorStart:
		return finish(s, now, operatorStart(s, e, now))
	case EventOperatorStop:
		return finish(s, now, operatorStop(s, e, now))
	case EventOperatorDelete:
		return finish(s, now, operatorDelete(s, e, now))
	case EventStopConfirmed:
		return finish(s, now, stopConfirmed(s, e, now))
	case EventStopExpired:
		return finish(s, now, stopExpired(s, e, now))
	case EventAcquire, EventLeaseExpired:
		return finish(s, now, acquire(s, e, now))
	case EventDumpReady:
		return finish(s, now, dumpReady(s, e, now))
	case EventRetryableError:
		return finish(s, now, retryable(s, e, now))
	case EventTerminalError:
		return finish(s, now, terminal(s, e, now))
	case EventTailUnreadable:
		return finish(s, now, tailUnreadable(s, e, now))
	case EventEpochLost:
		return finish(s, now, epochLost(s, e, now))
	case EventChainVerified:
		return finish(s, now, chainVerified(s, e, now))
	case EventChainOwnerGone:
		return finish(s, now, chainOwnerGone(s, e, now))
	case EventChainOwnerBack:
		return finish(s, now, chainOwnerBack(s, e, now))
	case EventMetaLockTimeout:
		return finish(s, now, metaLockTimeout(s, e, now))
	case EventRenew:
		return finish(s, now, renew(s, e, now))
	case EventSweep:
		return finish(s, now, sweep(s, e, now))
	case EventLock:
		return finish(s, now, takeLock(s, e, now))
	case EventEvalDirFailed:
		return finish(s, now, appliedPair(s, Result{Decision: DecisionRejected, Detail: "directory not read"}))
	case EventLegacyUpdate:
		return finish(s, now, legacyUpdate(s))
	default:
		return finish(s, now, appliedPair(s, Result{Decision: DecisionRejected, Detail: "unknown event"}))
	}
}

type partial struct {
	state  State
	result Result
}

func appliedPair(s State, r Result) partial { return partial{state: s, result: r} }

func finish(before State, now time.Time, p partial) Result {
	next := p.state
	res := p.result
	if res.Decision == DecisionApplied {
		noteOwnership(&next, now)
		next.NoProgress = noProgressFact(next, now)
		var opened, closed []Finding
		derived := derive(next)
		opened, closed = diffFindings(before.Findings, derived)
		next.Findings = derived
		res.Opened = opened
		res.Closed = closed
		for _, f := range opened {
			in := Intent{Kind: IntentClusterFinding, Finding: f, Node: f.Subject}
			if !f.OnCluster {
				in.Kind = "open_finding"
				in.LocksTaskLease = true
			}
			res.Intents = append(res.Intents, in)
		}
	}
	res.State = next
	return res
}

func noteOwnership(s *State, now time.Time) {
	unowned := s.Desired == DesiredRun && !LeaseLive(*s, now) && s.Phase != PhaseFailed && s.Phase != PhaseCreated && s.Phase != PhaseStopped && !s.Deleted
	if unowned {
		if s.UnownedSince.IsZero() {
			s.UnownedSince = now
		}
		return
	}
	s.UnownedSince = time.Time{}
}

func noProgressFact(s State, now time.Time) bool {
	if s.Deleted || s.Desired != DesiredRun || s.Phase == PhaseFailed || s.Phase == PhaseCreated {
		return false
	}
	if !LeaseLive(s, now) && !LegalRetry(s, now) && s.Phase != PhaseStopped {
		return !s.UnownedSince.IsZero() && !now.Before(s.UnownedSince.Add(s.Params.Sweep))
	}
	if LeaseLive(s, now) {
		anchor := s.ProgressAt
		if anchor.IsZero() {
			anchor = s.Lease.AcquiredAt
		}
		if !anchor.IsZero() && now.Sub(anchor) > s.Params.NoProgressAfter {
			return true
		}
	}
	return false
}

func (s State) clone() State {
	n := s
	n.Refusal = make(map[string]Refusal, len(s.Refusal))
	for k, v := range s.Refusal {
		n.Refusal[k] = v
	}
	n.Findings = append([]Finding(nil), s.Findings...)
	n.Nodes = append([]Node(nil), s.Nodes...)
	if s.RegistrationLive != nil {
		n.RegistrationLive = make(map[string]bool, len(s.RegistrationLive))
		for k, v := range s.RegistrationLive {
			n.RegistrationLive[k] = v
		}
	}
	return n
}

func (s *State) bump() { s.RowGeneration++ }

func (s *State) node(id string) *Node {
	for i := range s.Nodes {
		if s.Nodes[i].ID == id {
			return &s.Nodes[i]
		}
	}
	return nil
}

func release(s *State, now time.Time) {
	s.Lease.Owner = ""
	s.Lease.ExpireAt = now
	s.RunID = ""
}

func (s *State) takeLease(node string, now time.Time) {
	same := s.Lease.Owner == node && node != ""
	if !same {
		s.Lease.Epoch++
		s.Lease.AcquiredAt = now
		s.Lease.RenewedAt = time.Time{}
		s.ProgressAt = time.Time{}
	} else {
		s.Lease.RenewedAt = now
	}
	s.Lease.Present = true
	s.Lease.Owner = node
	s.Lease.ExpireAt = now.Add(s.Params.LeaseTTL)
	s.RunID = ""
}

func (s *State) clearRefusals() {
	if len(s.Refusal) == 0 {
		return
	}
	s.Refusal = map[string]Refusal{}
}

func (s *State) erase() []Intent {
	perNode := s.Params.Storage == StoragePerNode
	*s = State{
		TaskID:       s.TaskID,
		Deleted:      true,
		Params:       s.Params,
		Refusal:      map[string]Refusal{},
		HealthStatus: "HEALTHY",
	}
	intents := []Intent{{Kind: IntentDeleteTask}}
	if perNode {
		intents = append(intents, Intent{Kind: IntentTombstone})
	}
	return intents
}

type fenceKind int

const (
	fenceOK fenceKind = iota
	fenceDiscard
	fenceReread
	fenceZero
)

func fence(s State, e Event) fenceKind {
	if s.Deleted || !s.Lease.Present {
		return fenceZero
	}
	if s.Lease.Epoch != e.Epoch {
		return fenceDiscard
	}
	if s.Lease.Owner == "" {
		return fenceZero
	}
	if s.Lease.Owner != e.Node {
		return fenceDiscard
	}
	if e.RowGeneration != s.RowGeneration {
		return fenceReread
	}
	return fenceOK
}

func dropped(s State, k fenceKind) partial {
	switch k {
	case fenceReread:
		return appliedPair(s, Result{Decision: DecisionReread, Intents: []Intent{{Kind: IntentReread}}})
	case fenceZero:
		return appliedPair(s, Result{Decision: DecisionDiscarded, ZeroRow: true, Detail: "noop"})
	default:
		return appliedPair(s, Result{Decision: DecisionDiscarded, Intents: []Intent{{Kind: IntentStopLocal}}, Detail: "epoch"})
	}
}

func operatorStart(s State, e Event, now time.Time) partial {
	if e.MetaDown {
		return appliedPair(s, Result{Decision: DecisionRejected, HTTP: 503, Detail: "meta down"})
	}
	if e.LostRace {
		return appliedPair(s, Result{Decision: DecisionRejected, HTTP: 409, Detail: "desired STOP", Intents: []Intent{{Kind: IntentRollback}}})
	}
	if s.PendingDelete {
		return appliedPair(s, Result{Decision: DecisionRejected, HTTP: 409, Detail: string(s.Phase) + " pending_delete"})
	}
	if (s.Phase == PhaseStarting || s.Phase == PhaseRunning) && !e.ChangesSpec {
		return appliedPair(s, Result{Decision: DecisionIdempotent, HTTP: 204, Detail: string(s.Phase)})
	}
	if s.Phase == PhaseStopping || ((s.Phase == PhaseStarting || s.Phase == PhaseRunning) && e.ChangesSpec) {
		return appliedPair(s, Result{Decision: DecisionRejected, HTTP: 409, Detail: string(s.Phase)})
	}
	switch s.Phase {
	case PhaseCreated, PhaseStopped, PhaseFailed, PhaseRetryBackoff, PhaseLeaseDegraded:
	default:
		return appliedPair(s, Result{Decision: DecisionRejected, HTTP: 409, Detail: string(s.Phase)})
	}
	if e.Node == "" {
		return appliedPair(s, Result{Decision: DecisionRejected, HTTP: 409, Detail: "no node"})
	}
	if LeaseLive(s, now) && s.Lease.Owner != e.Node {
		return appliedPair(s, Result{Decision: DecisionRejected, HTTP: 409, Detail: string(s.Phase) + " lease_held"})
	}
	next := s
	next.takeLease(e.Node, now)
	next.bump()
	next.Phase = PhaseStarting
	next.Desired = DesiredRun
	next.StoppedByOperator = false
	next.SpecRevision++
	next.RetryAttempt = 0
	next.RetryUntil = time.Time{}
	next.LastError = ""
	next.RunID = ""
	next.FailedReason = ""
	next.FailedAt = time.Time{}
	next.FailedSpec = 0
	hadRefusal := len(next.Refusal) > 0
	next.clearRefusals()
	intents := []Intent{{Kind: IntentAcquire, Node: e.Node, LocksTaskLease: true}}
	if hadRefusal {
		intents = append(intents, Intent{Kind: IntentDeleteRefusals})
	}
	return partial{state: next, result: Result{Decision: DecisionApplied, HTTP: 204, Detail: "STARTING", LocksTaskLease: true, Intents: intents}}
}

func operatorStop(s State, e Event, now time.Time) partial {
	if e.MetaDown {
		return appliedPair(s, Result{Decision: DecisionRejected, HTTP: 503, Detail: "meta down"})
	}
	if s.Phase == PhaseStopping && !e.ChangesSpec {
		return appliedPair(s, Result{Decision: DecisionIdempotent, HTTP: 204, Detail: "STOPPING"})
	}
	if s.Phase == PhaseStopped && s.Desired == DesiredStop && !e.ChangesSpec {
		return appliedPair(s, Result{Decision: DecisionIdempotent, HTTP: 204, Detail: "STOPPED"})
	}
	active := s.Phase == PhaseLeaseDegraded || s.Phase == PhaseStarting || s.Phase == PhaseRunning || s.Phase == PhaseRetryBackoff || s.Phase == PhaseStopping || (s.Phase == PhaseStopped && s.Desired == DesiredRun)
	if !active {
		return appliedPair(s, Result{Decision: DecisionRejected, HTTP: 409, Detail: string(s.Phase)})
	}
	next := s
	next.bump()
	next.Desired = DesiredStop
	next.StoppedByOperator = true
	next.Phase = PhaseStopping
	next.SpecRevision++
	_ = now
	return partial{state: next, result: Result{Decision: DecisionApplied, HTTP: 204, Detail: "STOPPING", LocksTaskLease: true}}
}

func operatorDelete(s State, e Event, now time.Time) partial {
	if e.MetaDown {
		return appliedPair(s, Result{Decision: DecisionRejected, HTTP: 503, Detail: "meta down"})
	}
	next := s
	next.bump()
	next.Desired = DesiredStop
	next.StoppedByOperator = true
	next.Phase = PhaseStopping
	next.PendingDelete = true
	if LeaseLive(s, now) {
		return partial{state: next, result: Result{Decision: DecisionApplied, HTTP: 204, Detail: "STOPPING pending_delete", LocksTaskLease: true}}
	}
	intents := next.erase()
	return partial{state: next, result: Result{Decision: DecisionApplied, HTTP: 204, Detail: "deleted", LocksTaskLease: true, Intents: intents}}
}

func stopConfirmed(s State, e Event, now time.Time) partial {
	switch fence(s, e) {
	case fenceOK:
	default:
		return dropped(s, fence(s, e))
	}
	if s.Desired != DesiredStop && s.Phase != PhaseStopping {
		return appliedPair(s, Result{Decision: DecisionDiscarded, Detail: "not stopping", Intents: []Intent{{Kind: IntentStopLocal}}})
	}
	return finishStop(s, now)
}

func stopExpired(s State, e Event, now time.Time) partial {
	if !InStopSet(s, now) {
		return appliedPair(s, Result{Decision: DecisionRejected, Detail: "not in stop set"})
	}
	_ = e
	return finishStop(s, now)
}

func finishStop(s State, now time.Time) partial {
	next := s
	next.bump()
	if s.PendingDelete {
		intents := next.erase()
		return partial{state: next, result: Result{Decision: DecisionApplied, Intents: intents, LocksTaskLease: true}}
	}
	next.Phase = PhaseStopped
	next.Desired = DesiredStop
	next.StoppedByOperator = true
	release(&next, now)
	return partial{state: next, result: Result{Decision: DecisionApplied, Intents: []Intent{{Kind: IntentRelease}}, LocksTaskLease: true}}
}

func acquire(s State, e Event, now time.Time) partial {
	if e.Node == "" {
		return appliedPair(s, Result{Decision: DecisionRejected, Detail: "no node"})
	}
	if s.Desired != DesiredRun || s.PendingDelete || e.LostRace {
		return appliedPair(s, Result{Decision: DecisionRejected, Detail: "rollback", Intents: []Intent{{Kind: IntentRollback}}})
	}
	if RefusalBlocks(s, e.Node, now) || !InClaimSet(s, now) {
		return appliedPair(s, Result{Decision: DecisionRejected, Detail: "not claimable"})
	}
	next := s
	next.takeLease(e.Node, now)
	next.bump()
	next.Phase = PhaseStarting
	next.RunID = ""
	next.LastError = ""
	return partial{state: next, result: Result{Decision: DecisionApplied, LocksTaskLease: true, Intents: []Intent{{Kind: IntentAcquire, Node: e.Node}}}}
}

func dumpReady(s State, e Event, now time.Time) partial {
	switch k := fence(s, e); k {
	case fenceOK:
	default:
		return dropped(s, k)
	}
	if e.LeaseUnreadable {
		return epochLost(s, e, now)
	}
	if s.Desired != DesiredRun {
		return appliedPair(s, Result{Decision: DecisionDiscarded, Detail: "desired STOP", Intents: []Intent{{Kind: IntentStopLocal}}})
	}
	next := s
	next.bump()
	next.Phase = PhaseRunning
	next.RetryAttempt = 0
	next.RetryUntil = time.Time{}
	next.LastError = ""
	next.FailedReason = ""
	next.FailedAt = time.Time{}
	next.TailSince = time.Time{}
	next.ProgressAt = now
	had := len(next.Refusal) > 0
	next.clearRefusals()
	intents := []Intent{}
	if had {
		intents = append(intents, Intent{Kind: IntentDeleteRefusals})
	}
	return partial{state: next, result: Result{Decision: DecisionApplied, LocksTaskLease: true, Intents: intents}}
}

func retryable(s State, e Event, now time.Time) partial {
	switch k := fence(s, e); k {
	case fenceOK:
	default:
		return dropped(s, k)
	}
	if s.Desired == DesiredStop || s.Phase == PhaseStopping {
		return finishStop(s, now)
	}
	if e.LeaseUnreadable || !LeaseLive(s, now) {
		return epochLost(s, e, now)
	}
	if s.RetryAttempt >= RetryBudget {
		return epochLost(s, e, now)
	}
	next := s
	next.bump()
	next.Phase = PhaseRetryBackoff
	next.RetryAttempt++
	next.RetryUntil = now.Add(RefusalDelay(next.RetryAttempt, next.Params.ClaimInterval, 16*time.Hour))
	next.LastError = string(e.Cause)
	return partial{state: next, result: Result{Decision: DecisionApplied, LocksTaskLease: true}}
}

func terminal(s State, e Event, now time.Time) partial {
	switch k := fence(s, e); k {
	case fenceOK:
	default:
		return dropped(s, k)
	}
	if e.LeaseUnreadable {
		return epochLost(s, e, now)
	}
	var reason string
	switch e.Cause {
	case CauseSourceAuth:
		reason = ReasonSourceAuth
	case CauseLogBinOff:
		reason = ReasonLogBinOff
	default:
		return appliedPair(s, Result{Decision: DecisionRejected, Detail: "not on the terminal allowlist"})
	}
	if s.Desired == DesiredStop || s.Phase == PhaseStopping {
		return finishStop(s, now)
	}
	next := s
	next.bump()
	next.Phase = PhaseFailed
	next.FailedReason = reason
	next.FailedAt = now
	next.FailedSpec = s.SpecRevision
	next.LastError = reason
	next.Desired = DesiredRun
	release(&next, now)
	return partial{state: next, result: Result{Decision: DecisionApplied, LocksTaskLease: true, Intents: []Intent{{Kind: IntentRelease}}}}
}

func tailUnreadable(s State, e Event, now time.Time) partial {
	switch k := fence(s, e); k {
	case fenceOK:
	default:
		return dropped(s, k)
	}
	if s.Desired == DesiredStop || s.Phase == PhaseStopping {
		return finishStop(s, now)
	}
	if e.LeaseUnreadable {
		return epochLost(s, e, now)
	}
	if s.Chain.Verified() && s.Chain.Owner == e.Node {
		return appliedPair(s, Result{Decision: DecisionRejected, Detail: "verified chain owner"})
	}
	openEnded := s.Params.Storage == StoragePerNode
	next := yield(s, e.Node, now, openEnded, RefusalTail)
	return partial{state: next, result: Result{Decision: DecisionApplied, LocksTaskLease: true, Intents: []Intent{{Kind: IntentRelease, Node: e.Node}}}}
}

func epochLost(s State, e Event, now time.Time) partial {
	switch k := fence(s, e); k {
	case fenceOK:
	case fenceDiscard:
		return dropped(s, fenceDiscard)
	default:
		// A missing lease row, or a lease already released, is a no-op.
		// The caller must not treat that 0-row update as an error.
		return dropped(s, k)
	}
	if s.Desired == DesiredStop || s.Phase == PhaseStopping {
		return finishStop(s, now)
	}
	next := yield(s, e.Node, now, false, RefusalEpochLost)
	next.LastError = string(e.Cause)
	return partial{state: next, result: Result{Decision: DecisionApplied, LocksTaskLease: true, Intents: []Intent{{Kind: IntentRelease, Node: e.Node}}}}
}

func yield(s State, node string, now time.Time, openEnded bool, reason string) State {
	next := s
	next.bump()
	next.Phase = PhaseLeaseDegraded
	next.Desired = DesiredRun
	next.RunID = ""
	release(&next, now)
	if reason == RefusalTail && next.TailSince.IsZero() {
		next.TailSince = now
	}
	r := next.Refusal[node]
	r.Reason = reason
	r.Attempts++
	if openEnded {
		r.OpenEnded = true
		r.Until = time.Time{}
	} else {
		r.OpenEnded = false
		r.Until = now.Add(RefusalDelay(r.Attempts, next.Params.ClaimInterval, next.Params.RefusalRetryCap))
	}
	next.Refusal[node] = r
	next.LastError = reason
	return next
}

func chainVerified(s State, e Event, now time.Time) partial {
	if e.Node == "" || e.Incomplete || !e.ReadOK {
		return appliedPair(s, Result{Decision: DecisionRejected, Detail: "directory not read"})
	}
	holder := LeaseLive(s, now) && s.Lease.Owner == e.Node
	delegated := s.Chain.Owner == e.Node && !LeaseLive(s, now)
	if !holder && !delegated {
		return appliedPair(s, Result{Decision: DecisionRejected, Detail: "not the reader"})
	}
	next := s
	next.Chain.Owner = e.Node
	next.Chain.VerifiedAt = now
	next.Chain.VerifiedEpoch = s.Lease.Epoch
	next.Chain.MissingSealed = e.MissingSealed
	next.Chain.Unreadable = e.Unreadable
	return partial{state: next, result: Result{Decision: DecisionApplied, Detail: "chain"}}
}

func chainOwnerGone(s State, e Event, now time.Time) partial {
	if s.Chain.Owner == "" || s.Phase != PhaseLeaseDegraded || s.TailSince.IsZero() {
		return appliedPair(s, Result{Decision: DecisionRejected, Detail: "guard"})
	}
	hb := time.Time{}
	if n := s.node(s.Chain.Owner); n != nil {
		hb = n.HeartbeatAt
	}
	gone := hb.IsZero() || now.Sub(hb) > EffectiveNodeGone(s.Params)
	if !gone {
		return appliedPair(s, Result{Decision: DecisionRejected, Detail: "heartbeat fresh"})
	}
	next := s
	next.bump()
	next.Phase = PhaseFailed
	next.FailedReason = ReasonChainOwnerGone
	next.FailedAt = now
	next.Desired = DesiredRun
	_ = e
	return partial{state: next, result: Result{Decision: DecisionApplied, Detail: ReasonChainOwnerGone}}
}

func chainOwnerBack(s State, e Event, now time.Time) partial {
	if s.Phase != PhaseFailed || s.FailedReason != ReasonChainOwnerGone || s.Desired != DesiredRun {
		return appliedPair(s, Result{Decision: DecisionRejected, Detail: "guard"})
	}
	if e.Node != s.Chain.Owner || !s.Chain.Verified() || !s.Chain.VerifiedAt.After(s.FailedAt) {
		return appliedPair(s, Result{Decision: DecisionRejected, Detail: "directory not verified after failed_at"})
	}
	hb := time.Time{}
	if n := s.node(e.Node); n != nil {
		hb = n.HeartbeatAt
	}
	if hb.IsZero() || now.Sub(hb) > EffectiveNodeGone(s.Params) {
		return appliedPair(s, Result{Decision: DecisionRejected, Detail: "heartbeat"})
	}
	next := s
	next.bump()
	next.Phase = PhaseLeaseDegraded
	next.FailedReason = ""
	next.FailedAt = time.Time{}
	return partial{state: next, result: Result{Decision: DecisionApplied, Detail: "LEASE_DEGRADED"}}
}

func metaLockTimeout(s State, e Event, now time.Time) partial {
	next := s
	next.MetaLockStalled = true
	next.HealthBlocked = true
	next.LockObserver = e.Node
	next.LockRecoveries = 0
	_ = now
	return partial{state: next, result: Result{
		Decision:       DecisionApplied,
		LocksTaskLease: false,
		Intents:        []Intent{{Kind: IntentClusterFinding, Finding: Finding{Name: FindingLockStalled, Subject: s.TaskID, OnCluster: true}}},
		Detail:         "META_LOCK_STALLED",
	}}
}

func renew(s State, e Event, now time.Time) partial {
	if !s.Lease.Present || s.Lease.Owner != e.Node || s.Lease.Epoch != e.Epoch {
		return appliedPair(s, Result{Decision: DecisionDiscarded, Detail: "renew"})
	}
	next := s
	next.Lease.ExpireAt = now.Add(s.Params.LeaseTTL)
	next.Lease.RenewedAt = now
	return partial{state: next, result: Result{Decision: DecisionApplied, Detail: "renew"}}
}

func sweep(s State, e Event, now time.Time) partial {
	next := s
	next.EvaluatedAt = now
	next.LastSweep = now
	_ = e
	return partial{state: next, result: Result{Decision: DecisionApplied, Detail: "sweep"}}
}

func takeLock(s State, e Event, now time.Time) partial {
	// The observer does not take a lock the stalled holder still has.
	// idle_kill drops it; until then another node is refused.
	if s.Lock.Held && s.Lock.HeldBy != "" && s.Lock.HeldBy != e.Node {
		return appliedPair(s, Result{Decision: DecisionRejected, Detail: "lock held"})
	}
	next := s
	// Closing META_LOCK_STALLED takes the previously stuck lock twice, inside
	// lock_wait, and both takes are the observer. A sweep does not count.
	if next.MetaLockStalled && e.Node == next.LockObserver {
		within := next.LockRecoveries > 0 && next.Lock.HeldBy == e.Node && !now.After(next.Lock.Since.Add(next.Params.LockWait))
		if within {
			next.LockRecoveries++
		} else {
			next.LockRecoveries = 1
		}
		if next.LockRecoveries >= 2 {
			next.MetaLockStalled = false
			next.HealthBlocked = false
			next.LockRecoveries = 0
		}
	}
	next.Lock = Lock{Held: true, HeldBy: e.Node, Since: now}
	return partial{state: next, result: Result{Decision: DecisionApplied, Detail: "lock", LocksTaskLease: true}}
}

func legacyUpdate(s State) partial {
	next := s
	next.MixedVersion = true
	return partial{state: next, result: Result{Decision: DecisionApplied, Detail: "MIXED_VERSION", LocksTaskLease: false}}
}
