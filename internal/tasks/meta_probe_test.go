// Package tasks provides module-level functionality for tasks.
// input: a task store whose PingMeta ignores ctx and blocks, a health store whose ApplyTransitions blocks, and a manual test clock
// output: proof that Evaluator.Run's metadata ping counts a hung ping as a failure dated when it was sent, that the ping is not sent until a full interval has elapsed, and that the next ping still runs while that store call is blocked
// pos: design 0009 §25.8 R1 and R2, the independent meta ping (runMetaProbe / probeMeta)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/healthstore"
)

// hangMetaProbe is a task store whose ping does not return and does not
// observe ctx, the way a driver blocked in the server does.
type hangMetaProbe struct {
	*fakeStore
	hang chan struct{}
}

func (h *hangMetaProbe) PingMeta(context.Context) error {
	<-h.hang
	return nil
}

func (h *hangMetaProbe) MetaUnreachable(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}

// TestMetaProbe_HungPingFailsAtSendWhileStoreHangs is §25.8 R1/R2: the ping
// goroutine is not the evaluator loop. A ping that never returns counts as a
// failure once its timeout has elapsed on the test clock, dated when the
// ping was sent, and the following tick still observes the outage while a
// health-store call sits inside ApplyTransitions.
func TestMetaProbe_HungPingFailsAtSendWhileStoreHangs(t *testing.T) {
	data := t.TempDir()
	probe := &hangMetaProbe{fakeStore: newFakeStore(), hang: make(chan struct{})}
	s := NewScheduler(WithDataDir(data), WithStore(probe))
	task, dir := evalTask(t, s, data)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000001"), 37, 38)
	block := make(chan struct{})
	store := &scriptedStore{FileStore: healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName)), block: block}
	clock := newManualClock(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	const (
		interval = 2 * time.Second
		timeout  = time.Second
	)
	e := NewEvaluator(s, EvaluatorConfig{
		Mode: EvaluatorOn, Store: store, NodeID: evalNode, Workers: 2,
		Sweep: time.Hour, LivenessTick: time.Hour, Debounce: time.Millisecond,
		MetaPingInterval: interval, MetaPingTimeout: timeout,
		Now: clock.Now, After: clock.After,
	})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		e.Run(ctx)
	}()
	defer func() {
		close(probe.hang)
		select {
		case <-block:
		default:
			close(block)
		}
		clock.Advance(time.Hour)
		cancel()
		clock.Advance(time.Hour)
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Fatal("evaluator did not stop")
		}
	}()

	waitUntil(t, 5*time.Second, "store call never blocked", func() bool {
		return storeInFlight(store, task.ID) > 0
	})
	since, pinging, _, _ := metaReachSnap(s)
	if pinging || !since.IsZero() {
		t.Fatalf("failure recorded before the ping timed out: pinging %v since %s", pinging, since)
	}

	// The probe's waiter is the only one parked for a full interval: an idle
	// worker caps its wait at 1s. Advancing interval-ε must not send the ping.
	waitUntil(t, 5*time.Second, "ping interval was not armed", func() bool {
		return clock.HasDeadline(clock.Now().Add(interval))
	})
	mark := clock.Now()
	clock.Advance(interval - time.Millisecond)
	if _, pinging, _, _ := metaReachSnap(s); pinging {
		t.Fatal("ping fired before the interval elapsed")
	}
	if !clock.HasDeadline(mark.Add(interval)) {
		t.Fatal("probe waiter was not armed for the full interval")
	}
	clock.Advance(time.Millisecond)
	waitUntil(t, 5*time.Second, "timed out waiting for the metadata ping", func() bool {
		_, pinging, _, _ := metaReachSnap(s)
		return pinging
	})
	sent := mark.Add(interval)
	_, _, start, _ := metaReachSnap(s)
	if !start.Equal(sent) {
		t.Fatalf("ping started at %s, clock %s", start, sent)
	}
	since, _, _, _ = metaReachSnap(s)
	if !since.IsZero() {
		t.Fatalf("since %s set at send, before the ping timed out", since)
	}
	if got := storeInFlight(store, task.ID); got == 0 {
		t.Fatal("store call was not hanging when the ping was sent")
	}

	budget := timeout + timeout/10
	waitUntil(t, 5*time.Second, "ping timeout was not armed", func() bool {
		return clock.HasDeadline(sent.Add(budget))
	})
	clock.Advance(budget)
	waitUntil(t, 5*time.Second, "timed-out ping was not counted", func() bool {
		since, _, _, _ := metaReachSnap(s)
		return since.Equal(sent)
	})
	if got := storeInFlight(store, task.ID); got == 0 {
		t.Fatal("probing stopped while the store call was still hanging")
	}

	advanceUntil(t, clock, interval, func() bool {
		_, _, _, last := metaReachSnap(s)
		return strings.Contains(last, "still waiting")
	})
	since, pinging, start, _ = metaReachSnap(s)
	if !since.Equal(sent) {
		t.Fatalf("failure dated %s, want the send time %s", since, sent)
	}
	if !pinging || !start.Equal(sent) {
		t.Fatalf("ping no longer the one sent at %s: pinging %v start %s", sent, pinging, start)
	}
	if got := storeInFlight(store, task.ID); got == 0 {
		t.Fatal("store call finished while the probe was required to keep running")
	}
}

// advanceUntil moves the clock by step whenever a waiter is parked, until
// done is true. It returns the clock time at the moment done first became
// true. A waiter that is armed after an advance is not missed: the next
// loop sees it and advances again.
func advanceUntil(t *testing.T, clock *manualClock, step time.Duration, done func() bool) time.Time {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if done() {
			return clock.Now()
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the metadata ping")
		}
		if clock.WaiterCount() > 0 {
			clock.Advance(step)
			continue
		}
		time.Sleep(time.Millisecond)
	}
}

func waitUntil(t *testing.T, budget time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		if ok() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(time.Millisecond)
	}
}

func storeInFlight(store *scriptedStore, id string) int {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.inFlight == nil {
		return 0
	}
	return store.inFlight[id]
}

func metaReachSnap(s *Scheduler) (since time.Time, pinging bool, pingStart time.Time, lastErr string) {
	r := &s.metaReach
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.since, r.pinging, r.pingStart, r.lastErr
}
