// Package ownership is the pure task-ownership transition from ADR 0011.
// input: Step and CheckStep
// output: proof that an ownerless Start leaves an empty owner and the same epoch
// pos: N8, the API-only Start that does not Acquire
// note: if this file changes, update this header and module README.md.
package ownership

import "testing"

func TestOwnerlessStartDoesNotTakeLease(t *testing.T) {
	s := New()
	s.Phase = PhaseStopped
	s.Desired = DesiredStop
	s.StoppedByOperator = true
	s.Lease.Epoch = 4
	now := t0
	ev := Event{Kind: EventOperatorStart, Node: "api", Ownerless: true}
	res := step(t, s, ev, now)
	if res.HTTP != 204 || res.State.Phase != PhaseStarting || res.State.Desired != DesiredRun {
		t.Fatalf("ownerless start: http %d phase %s desired %s", res.HTTP, res.State.Phase, res.State.Desired)
	}
	if res.State.Lease.Owner != "" || res.State.Lease.Epoch != 4 || res.State.RunID != "" {
		t.Fatalf("ownerless start took a lease: owner %q epoch %d run %q", res.State.Lease.Owner, res.State.Lease.Epoch, res.State.RunID)
	}
	if !res.State.TailSince.Equal(s.TailSince) {
		t.Fatal("ownerless start cleared tail_unreadable_since")
	}
}

func TestOwnerlessStartRejectsForeignLease(t *testing.T) {
	s := New()
	s.Phase = PhaseStopped
	s.Desired = DesiredStop
	s.StoppedByOperator = true
	s.Lease = Lease{Present: true, Owner: "holder", Epoch: 2, ExpireAt: t0.Add(s.Params.LeaseTTL)}
	res := step(t, s, Event{Kind: EventOperatorStart, Node: "api", Ownerless: true}, t0)
	if res.Decision != DecisionRejected || res.HTTP != 409 {
		t.Fatalf("foreign lease: decision %s http %d", res.Decision, res.HTTP)
	}
	if res.State.Lease.Owner != "holder" || res.State.Lease.Epoch != 2 {
		t.Fatal("rejected ownerless start changed the live lease")
	}
}

func TestOwnerlessStartReleasesOwnLease(t *testing.T) {
	s := New()
	s.Phase = PhaseStopped
	s.Desired = DesiredStop
	s.StoppedByOperator = true
	s.Lease = Lease{Present: true, Owner: "api", Epoch: 3, ExpireAt: t0.Add(s.Params.LeaseTTL)}
	res := step(t, s, Event{Kind: EventOperatorStart, Node: "api", Ownerless: true}, t0)
	if res.State.Phase != PhaseStarting || res.State.Lease.Owner != "" || res.State.Lease.Epoch != 3 {
		t.Fatalf("release own lease: phase %s owner %q epoch %d", res.State.Phase, res.State.Lease.Owner, res.State.Lease.Epoch)
	}
	released := false
	for _, in := range res.Intents {
		if in.Kind == IntentRelease {
			released = true
		}
	}
	if !released {
		t.Fatal("ownerless start of the holder did not ask for release")
	}
}
