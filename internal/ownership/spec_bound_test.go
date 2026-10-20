// Package ownership is the pure task-ownership transition from ADR 0011.
// input: DefaultParams and explicit lease and registration durations
// output: proof of SessionKillBound and SameNodeRestartBound
// pos: the PR-2 session-kill cap and the F10 same-node restart sum
// note: if this file changes, update this header and module README.md.
package ownership

import "testing"
import "time"

func TestSessionKillBound(t *testing.T) {
	if got := SessionKillBound(15 * time.Second); got != 5*time.Second {
		t.Fatalf("default ttl bound %s, want 5s", got)
	}
	if got := SessionKillBound(0); got != 5*time.Second {
		t.Fatalf("zero ttl bound %s, want the default 5s", got)
	}
	if got := SessionKillBound(60 * time.Second); got != 5*time.Second {
		t.Fatalf("60s ttl bound %s, want 5s", got)
	}
	if got := SessionKillBound(4 * time.Second); got != 2*time.Second {
		t.Fatalf("4s ttl bound %s, want 2s", got)
	}
	if got := SessionKillBound(time.Second); got != time.Second {
		t.Fatalf("1s ttl bound %s, want 1s", got)
	}
}

func TestSameNodeRestartBoundCountsRegistrationTTL(t *testing.T) {
	p := DefaultParams()
	if RTOBound(p) != 32*time.Second {
		t.Fatalf("rto %s, want 32s", RTOBound(p))
	}
	if got := SameNodeRestartBound(p, 15*time.Second); got != 47*time.Second {
		t.Fatalf("same-node %s, want 47s", got)
	}
	if got := SameNodeRestartBound(p, 0); got != 32*time.Second {
		t.Fatalf("zero registration %s, want 32s", got)
	}
	field := DefaultParams()
	field.LeaseTTL = 60 * time.Second
	if RTOBound(field) != 77*time.Second {
		t.Fatalf("field rto %s, want 77s", RTOBound(field))
	}
}
