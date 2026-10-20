// Package ownership is the pure task-ownership transition from ADR 0011.
// input: Step, via observeFair, from a state that has not reached RUNNING
// output: a failing test when a bottom SCC is not RUNNING or one operator FAILED
// pos: the first test in this package, so a DumpReady livelock is killed here
// note: if this file changes, update this header and module README.md.
package ownership

import "testing"

func TestBottomSCC(t *testing.T) {
	// Start before DumpReady. The search stops at RUNNING, so a boot that
	// is already running never takes the progress step.
	s := New()
	s.Nodes = []Node{{ID: "n1", CanRead: true, HeartbeatAt: t0}}
	s = applied(t, s, Event{Kind: EventOperatorStart, Node: "n1"}, t0)
	if s.Phase == PhaseRunning {
		t.Fatal("start already running")
	}
	end := observeFair(s, t0)
	if end.over || end.violation != "" || !runningEnd(end.state, end.at) {
		t.Fatalf("bottom %q over %v phase %s steps %d", end.violation, end.over, end.state.Phase, end.steps)
	}
	if end.bottoms["running"] != 1 {
		t.Fatalf("bottoms %v", end.bottoms)
	}
}
