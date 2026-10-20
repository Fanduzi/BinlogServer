// Package ownership is the pure task-ownership transition from ADR 0011.
// input: Step, Check, and CheckStep
// output: test fixtures shared by the model tests
// pos: deterministic clock and the running-task setup the trajectories start from
// note: if this file changes, update this header and module README.md.
package ownership

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)

func mustOK(t *testing.T, s State, now time.Time) {
	t.Helper()
	if v := Check(s, now); len(v) > 0 {
		t.Fatalf("invariant %+v phase=%s desired=%s owner=%s epoch=%d gen=%d reason=%s", v, s.Phase, s.Desired, s.Lease.Owner, s.Lease.Epoch, s.RowGeneration, s.FailedReason)
	}
}

func step(t *testing.T, s State, e Event, now time.Time) Result {
	t.Helper()
	res := Step(s, e, now)
	if v := CheckStep(s, res.State, e, res, now); len(v) > 0 {
		t.Fatalf("%s node=%s: %+v (decision %s detail %s phase %s -> %s)", e.Kind, e.Node, v, res.Decision, res.Detail, s.Phase, res.State.Phase)
	}
	return res
}

func applied(t *testing.T, s State, e Event, now time.Time) State {
	t.Helper()
	res := step(t, s, e, now)
	if res.Decision != DecisionApplied && res.Decision != DecisionIdempotent {
		t.Fatalf("%s: decision %s detail %s", e.Kind, res.Decision, res.Detail)
	}
	return res.State
}

// boot runs OperatorStart, DumpReady, and ChainVerified for owner.
func boot(t *testing.T, owner string, now time.Time, rest []Node) State {
	t.Helper()
	s := New()
	s.Nodes = append([]Node{{ID: owner, CanRead: true, HeartbeatAt: now}}, rest...)
	s = applied(t, s, Event{Kind: EventOperatorStart, Node: owner}, now)
	if s.Phase != PhaseStarting || s.Lease.Owner != owner || s.RunID != "" {
		t.Fatalf("start: phase %s owner %s run %q", s.Phase, s.Lease.Owner, s.RunID)
	}
	s = applied(t, s, Held(s, EventDumpReady, owner), now)
	if s.Phase != PhaseRunning {
		t.Fatalf("dump: %s", s.Phase)
	}
	s = applied(t, s, Event{Kind: EventChainVerified, Node: owner, ReadOK: true}, now)
	if !s.Chain.Verified() || s.Chain.Owner != owner {
		t.Fatalf("chain: %+v", s.Chain)
	}
	return s
}

func peer(id string, read bool, now time.Time) Node {
	return Node{ID: id, CanRead: read, HeartbeatAt: now}
}

func renewLease(t *testing.T, s State, now time.Time) State {
	t.Helper()
	return applied(t, s, Event{Kind: EventRenew, Node: s.Lease.Owner, Epoch: s.Lease.Epoch}, now)
}

func expireLease(s State, now time.Time) State {
	s.Lease.ExpireAt = now
	return s
}

func findOpen(s State, name, subject string) bool {
	for _, f := range s.Findings {
		if f.Name == name && f.Subject == subject {
			return true
		}
	}
	return false
}
