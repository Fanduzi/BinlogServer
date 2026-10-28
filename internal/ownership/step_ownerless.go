// Package ownership provides module-level functionality for ownership.
// input: an ownerless operator Start event (control plane has no runner) on a state
// output: the N8 applied partial for a start the accepting process does not lease, and the applied partial for a start recorded while STOPPING
// pos: ownerless start transitions for ADR 0011 N8, split from step.go
// note: if this file changes, update this header and module README.md.
package ownership

import "time"

// ownerlessStart is N8. The accepting process does not hold the lease.
// A live lease this node already holds is released in the same decision.
func ownerlessStart(s State, e Event, now time.Time) partial {
	next := s
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
	var intents []Intent
	if LeaseLive(s, now) && s.Lease.Owner == e.Node {
		release(&next, now)
		intents = append(intents, Intent{Kind: IntentRelease, Node: e.Node})
	} else {
		next.Lease.Owner = ""
	}
	had := len(next.Refusal) > 0
	next.clearRefusals()
	if had {
		intents = append(intents, Intent{Kind: IntentDeleteRefusals})
	}
	return partial{state: next, result: Result{Decision: DecisionApplied, HTTP: 204, Detail: "STARTING", LocksTaskLease: true, Intents: intents}}
}

// ownerlessRestartWhileStopping is the control plane's Start while the
// worker is still closing the dump: record desired RUN and leave the lease
// and epoch alone. A worker Start, or a delete, still waits until STOPPING
// ends.
func ownerlessRestartWhileStopping(s State) partial {
	next := s
	next.bump()
	next.Desired = DesiredRun
	next.StoppedByOperator = false
	next.SpecRevision++
	next.RetryAttempt = 0
	next.RetryUntil = time.Time{}
	next.LastError = ""
	next.FailedReason = ""
	next.FailedAt = time.Time{}
	next.FailedSpec = 0
	return partial{state: next, result: Result{Decision: DecisionApplied, HTTP: 204, Detail: "STOPPING", LocksTaskLease: true}}
}
