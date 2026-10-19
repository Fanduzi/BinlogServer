package tasks

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/binlog"
)

// design 0009 §27.8: the evaluator does not wait on a stuck rotate, its
// checkpoint read under HoldChain is bounded, and every sweep probes the
// health store.

type hangingCheckpoints struct{ err error }

func (c hangingCheckpoints) LoadCheckpoint(ctx context.Context, _ string) (binlog.Checkpoint, bool, error) {
	if c.err != nil {
		return binlog.Checkpoint{}, false, c.err
	}
	<-ctx.Done()
	return binlog.Checkpoint{}, false, ctx.Err()
}

func chainFactsWithin(t *testing.T, e *Evaluator, task Task, limit time.Duration) (returned bool, nilFacts bool) {
	t.Helper()
	done := make(chan bool, 1)
	go func() { done <- e.chainFacts(task, time.Now()) == nil }()
	select {
	case n := <-done:
		return true, n
	case <-time.After(limit):
		return false, false
	}
}

// R1: a rotate stuck inside HoldChain (checkpoint write or openAt hung)
// must not stop the evaluator; the chain scope is incomplete instead.
func TestChainFactsDoNotWaitForAStuckRotate(t *testing.T) {
	f := sizedDir(t)
	e := newEval(t, f.s, nil, EvaluatorOn)
	release := f.s.HoldChain(f.task.ID)
	defer release()
	ok, isNil := chainFactsWithin(t, e, f.task, 5*time.Second)
	if !ok {
		t.Fatal("chainFacts waited on a rotate that holds HoldChain; TASK_NO_PROGRESS for that task could never be judged")
	}
	if !isNil {
		t.Fatal("chainFacts without the chain lock must leave chain.local incomplete")
	}
}

// R2: a hung checkpoint read under HoldChain is bounded and the lock is
// released, so a rotate is never held behind the evaluator's meta read.
func TestChainSnapshotCheckpointReadIsBounded(t *testing.T) {
	f := sizedDirWith(t, WithCheckpointReader(hangingCheckpoints{}), WithInternalCallTimeouts(InternalCallTimeouts{Read: 100 * time.Millisecond}))
	e := newEval(t, f.s, nil, EvaluatorOn)
	ok, isNil := chainFactsWithin(t, e, f.task, 3*time.Second)
	if !ok {
		t.Fatal("checkpoint read under HoldChain is unbounded")
	}
	if !isNil {
		t.Fatal("a failed checkpoint read must leave chain.local incomplete, not judge I5 without a checkpoint")
	}
	got := make(chan struct{})
	go func() { f.s.HoldChain(f.task.ID)(); close(got) }()
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("HoldChain still held after chainFacts returned")
	}
}

// R2: a checkpoint read error is not "no checkpoint": I5 is not judged,
// so an open CHECKPOINT_AHEAD is not closed by a meta hiccup.
func TestChainSnapshotCheckpointReadErrorIsIncomplete(t *testing.T) {
	f := sizedDirWith(t, WithCheckpointReader(hangingCheckpoints{err: errors.New("meta: connection refused")}))
	e := newEval(t, f.s, nil, EvaluatorOn)
	ok, isNil := chainFactsWithin(t, e, f.task, 3*time.Second)
	if !ok || !isNil {
		t.Fatalf("checkpoint read error: returned=%v incomplete=%v, want an incomplete chain scope", ok, isNil)
	}
}

// R3: a cluster node that evaluates no task notices an online downgrade
// below 000009, and its recovery, within one sweep.
func TestHealthStoreProbedEverySweepOnATasklessNode(t *testing.T) {
	findings := newMemFindingStore()
	s := NewScheduler(WithDataDir(t.TempDir()), WithStore(schemaStore(8, false)), WithFindingStore(findings), WithEventStore(newFakeEventStore()))
	hs := newAbsentHealth()
	e := NewEvaluator(s, EvaluatorConfig{Mode: EvaluatorOn, Cluster: true, Store: hs, NodeID: evalNode, Instance: "i1", Sweep: 15 * time.Millisecond, Debounce: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	wait := func(want string, sub string) HealthSummary {
		deadline := time.Now().Add(2 * time.Second)
		var got HealthSummary
		for time.Now().Before(deadline) {
			got = s.HealthSummary(context.Background())
			if got.Status == want && (sub == "" || strings.Contains(joinedReasons(got), sub)) {
				return got
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("health %+v, want %s %q", got, want, sub)
		return got
	}
	wait(HealthOK, "")
	hs.down.Store(true)
	wait(HealthDegraded, "HEALTH_STORE_UNAVAILABLE")
	hs.down.Store(false)
	wait(HealthOK, "")
}
