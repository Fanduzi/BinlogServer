// Package ownership is the pure task-ownership transition from ADR 0011.
// input: Check and CheckStep
// output: direct predicate tests, a seeded random safety walk, and a bounded exhaustive walk
// pos: safety half of ADR 0011 §4.1; random prefixes are not a liveness proof
// note: if this file changes, update this header and module README.md.
package ownership

import (
	"math/rand"
	"testing"
	"time"
)

func TestStepDoesNotMutateInput(t *testing.T) {
	s := New()
	s.Nodes = []Node{peer("n1", true, t0)}
	s.Refusal = map[string]Refusal{"n2": {Reason: RefusalTail, OpenEnded: true}}
	s.TailSince = t0
	s.Findings = derive(s)
	snapshot := s.RowGeneration
	res := Step(s, Event{Kind: EventOperatorStart, Node: "n1"}, t0)
	if s.RowGeneration != snapshot || len(s.Refusal) != 1 || s.Phase != PhaseCreated {
		t.Fatalf("input changed: phase %s gen %d refusals %d", s.Phase, s.RowGeneration, len(s.Refusal))
	}
	if res.State.Phase != PhaseStarting || len(res.State.Refusal) != 0 {
		t.Fatalf("result: phase %s refusals %d", res.State.Phase, len(res.State.Refusal))
	}
	if !res.State.TailSince.Equal(t0) {
		t.Fatal("start cleared tail_unreadable_since")
	}
}

func TestStepIsDeterministic(t *testing.T) {
	s := boot(t, "n1", t0, []Node{peer("n2", false, t0)})
	e := Held(s, EventEpochLost, "n1")
	e.Cause = CauseENOSPC
	a := Step(s, e, t0)
	b := Step(s, e, t0)
	if a.Decision != b.Decision || a.State.Phase != b.State.Phase || a.State.RowGeneration != b.State.RowGeneration || a.State.Lease.Epoch != b.State.Lease.Epoch {
		t.Fatalf("not deterministic: %+v vs %+v", a.Decision, b.Decision)
	}
	if len(a.Opened) != len(b.Opened) || len(a.Closed) != len(b.Closed) {
		t.Fatal("finding diff differs")
	}
}

func TestPredicatesCatchBrokenStates(t *testing.T) {
	now := t0
	badStop := New()
	badStop.Desired = DesiredStop
	if v := Check(badStop, now); !hasInv(v, "1") {
		t.Fatalf("desired STOP without the operator flag: %+v", v)
	}
	badFail := New()
	badFail.Phase = PhaseFailed
	badFail.FailedReason = "ENOENT"
	if v := Check(badFail, now); !hasInv(v, "2") {
		t.Fatalf("FAILED/ENOENT: %+v", v)
	}
	shared := New()
	shared.Params.Storage = StorageShared
	shared.Refusal = map[string]Refusal{"n2": {Reason: RefusalTail, OpenEnded: true}}
	if v := Check(shared, now); !hasInv(v, "2") {
		t.Fatalf("shared NULL refusal: %+v", v)
	}
	dangling := New()
	dangling.Refusal = map[string]Refusal{"n2": {Reason: RefusalEpochLost}}
	if v := Check(dangling, now); !hasInv(v, "10") {
		t.Fatalf("refusal without a deadline: %+v", v)
	}
	weird := New()
	weird.Phase = "WEIRD"
	if v := Check(weird, now); !hasInv(v, "9") {
		t.Fatalf("weird phase: %+v", v)
	}
	// STOPPED + desired RUN + empty owner is claimable, including the §29.10 shape.
	stopped := New()
	stopped.Phase = PhaseStopped
	stopped.Desired = DesiredRun
	stopped.Lease.Owner = ""
	if !InClaimSet(stopped, now) || !Covered(stopped, now) {
		t.Fatal("STOPPED + RUN + empty owner must be claimable")
	}
	// Expired STARTING is claimable even though the owner column is still filled.
	starting := boot(t, "n1", now, nil)
	starting.Phase = PhaseStarting
	starting = expireLease(starting, now.Add(time.Hour))
	if LeaseLive(starting, now.Add(time.Hour)) || !InClaimSet(starting, now.Add(time.Hour)) {
		t.Fatal("expired STARTING must be in the claim set")
	}
}

func hasInv(v []Violation, id string) bool {
	for _, item := range v {
		if item.Invariant == id {
			return true
		}
	}
	return false
}

// Safety walk. Fixed seed 20261020, so a failure replays.
// This checks every prefix. It does not prove liveness; see liveness_test.go.
func TestSafetyRandomPrefixes(t *testing.T) {
	const seed = 20261020
	for _, n := range []int{1, 2, 3} {
		for seq := 0; seq < 30; seq++ {
			rng := rand.New(rand.NewSource(seed + int64(seq*10+n)))
			s := New()
			s.Params.Storage = StoragePerNode
			if rng.Intn(2) == 0 {
				s.Params.Storage = StorageShared
			}
			s.Params.Uploads = rng.Intn(2) == 0
			now := t0
			s.Nodes = make([]Node, n)
			for i := 0; i < n; i++ {
				s.Nodes[i] = Node{ID: nodeID(i), CanRead: i == 0 || rng.Intn(2) == 0, HeartbeatAt: now}
			}
			for stepN := 0; stepN < 25; stepN++ {
				e := randomEvent(rng, s, n)
				now = now.Add(time.Duration(1+rng.Intn(20)) * time.Second)
				res := Step(s, e, now)
				if v := CheckStep(s, res.State, e, res, now); len(v) > 0 {
					t.Fatalf("N=%d seq=%d step=%d event=%s: %+v", n, seq, stepN, e.Kind, v)
				}
				again := Step(s, e, now)
				if again.Decision != res.Decision || again.State.Phase != res.State.Phase || again.State.RowGeneration != res.State.RowGeneration || again.HTTP != res.HTTP {
					t.Fatalf("nondeterministic %s", e.Kind)
				}
				s = res.State
			}
		}
	}
}

// Bounded exhaustive search over a small alphabet for N=2, depth 3.
// Every prefix must satisfy CheckStep. 20^3 sequences.
func TestSafetyBoundedExhaustive(t *testing.T) {
	now := t0
	s0 := New()
	s0.Nodes = []Node{peer("n1", true, now), peer("n2", false, now)}
	type item struct {
		s    State
		now  time.Time
		left int
	}
	queue := []item{{s: s0, now: now, left: 3}}
	seen := 0
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.left == 0 {
			continue
		}
		for _, e := range alphabet(cur.s) {
			res := Step(cur.s, e, cur.now)
			if v := CheckStep(cur.s, res.State, e, res, cur.now); len(v) > 0 {
				t.Fatalf("depth %d %s: %+v", 4-cur.left, e.Kind, v)
			}
			seen++
			queue = append(queue, item{s: res.State, now: cur.now.Add(2 * time.Second), left: cur.left - 1})
		}
	}
	if seen < 1000 {
		t.Fatalf("exhaustive search too small: %d", seen)
	}
}

func alphabet(s State) []Event {
	var out []Event
	for _, n := range []string{"n1", "n2"} {
		out = append(out,
			Event{Kind: EventOperatorStart, Node: n},
			Event{Kind: EventOperatorStop, Node: n},
			Event{Kind: EventOperatorDelete, Node: n},
			Event{Kind: EventAcquire, Node: n},
			Event{Kind: EventLeaseExpired, Node: n},
			Held(s, EventDumpReady, n),
			heldCause(s, EventRetryableError, n, CauseLockWait),
			heldCause(s, EventTerminalError, n, CauseSourceAuth),
			heldCause(s, EventTerminalError, n, CauseUnclassified),
			heldCause(s, EventTailUnreadable, n, CauseSegmentNotOnWorker),
			heldCause(s, EventEpochLost, n, CauseENOENT),
			Event{Kind: EventChainVerified, Node: n, ReadOK: true},
			Event{Kind: EventChainVerified, Node: n, ReadOK: false},
			Event{Kind: EventChainOwnerGone, Node: n},
			Event{Kind: EventChainOwnerBack, Node: n},
			Event{Kind: EventSweep, Node: n},
			Event{Kind: EventRenew, Node: n, Epoch: s.Lease.Epoch},
		)
	}
	out = append(out, Event{Kind: EventMetaLockTimeout, Node: "n2"}, Event{Kind: EventLegacyUpdate})
	return out
}

func heldCause(s State, kind Kind, node string, c Cause) Event {
	e := Held(s, kind, node)
	e.Cause = c
	return e
}

func randomEvent(rng *rand.Rand, s State, n int) Event {
	node := nodeID(rng.Intn(n))
	kinds := []Kind{
		EventOperatorStart, EventOperatorStop, EventOperatorDelete,
		EventAcquire, EventLeaseExpired, EventDumpReady, EventRetryableError,
		EventTerminalError, EventTailUnreadable, EventEpochLost, EventChainVerified,
		EventChainOwnerGone, EventChainOwnerBack, EventMetaLockTimeout, EventRenew,
		EventSweep, EventLock, EventEvalDirFailed, EventLegacyUpdate, EventStopConfirmed,
		EventStopExpired,
	}
	e := Event{Kind: kinds[rng.Intn(len(kinds))], Node: node}
	e.Epoch = s.Lease.Epoch
	e.RowGeneration = s.RowGeneration
	if rng.Intn(5) == 0 && s.Lease.Epoch > 0 {
		e.Epoch = s.Lease.Epoch - 1
	}
	if rng.Intn(5) == 0 {
		e.RowGeneration = s.RowGeneration - 1
	}
	switch rng.Intn(6) {
	case 0:
		e.Cause = CauseSourceAuth
	case 1:
		e.Cause = CauseLogBinOff
	case 2:
		e.Cause = CauseENOENT
	case 3:
		e.Cause = CauseENOSPC
	case 4:
		e.Cause = CauseEROFS
	default:
		e.Cause = CauseUnclassified
	}
	e.ReadOK = rng.Intn(2) == 0
	e.MissingSealed = rng.Intn(4) == 0
	e.Incomplete = rng.Intn(4) == 0
	e.ChangesSpec = rng.Intn(4) == 0
	e.MetaDown = rng.Intn(8) == 0
	e.LostRace = rng.Intn(8) == 0
	return e
}

func nodeID(i int) string { return "n" + string(rune('1'+i)) }
