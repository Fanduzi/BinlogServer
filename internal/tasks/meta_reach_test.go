package tasks

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
)

// errMetaDown stands in for a refused or dropped metadata connection.
var errMetaDown = errors.New("dial tcp 127.0.0.1:3306: connect: connection refused")

// reachStore is a task store with a MetaProbe (design 0009 §25).
type reachStore struct {
	*fakeStore
	pingErr atomic.Pointer[error]
}

func (r *reachStore) PingMeta(context.Context) error {
	if p := r.pingErr.Load(); p != nil {
		return *p
	}
	return nil
}

func (r *reachStore) MetaUnreachable(err error) bool {
	return errors.Is(err, errMetaDown) || errors.Is(err, context.DeadlineExceeded)
}

// hangingFindings blocks ListOpenFindings: until ctx ends, or (ignoreCtx)
// until release is closed, as a driver that ignores ctx would.
type hangingFindings struct {
	ignoreCtx bool
	release   chan struct{}
	calls     atomic.Int32
}

func (h *hangingFindings) UpsertFinding(context.Context, Finding) error { return nil }
func (h *hangingFindings) ListFindings(context.Context, string, bool, int) ([]Finding, error) {
	return nil, nil
}
func (h *hangingFindings) ListOpenFindings(ctx context.Context, _ int) ([]Finding, error) {
	h.calls.Add(1)
	if h.ignoreCtx {
		<-h.release
		return nil, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestHealthSummaryBoundedWhenMetaHangs (#427 r4 (h)): a findings read that
// hangs no longer hangs /api/health; it answers degraded with
// META_UNREACHABLE within the 1s budget, and concurrent probes share one read.
func TestHealthSummaryBoundedWhenMetaHangs(t *testing.T) {
	for _, ignoreCtx := range []bool{false, true} {
		hf := &hangingFindings{ignoreCtx: ignoreCtx, release: make(chan struct{})}
		t.Cleanup(func() { close(hf.release) })
		s := NewScheduler(WithStore(&reachStore{fakeStore: newFakeStore()}), WithFindingStore(hf))
		var wg sync.WaitGroup
		results := make([]HealthSummary, 10)
		took := make([]time.Duration, 10)
		for i := range results {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				start := time.Now()
				results[i] = s.HealthSummary(context.Background())
				took[i] = time.Since(start)
			}(i)
		}
		wg.Wait()
		for i, got := range results {
			if took[i] > healthReadBudget+200*time.Millisecond {
				t.Errorf("ignoreCtx=%v: HealthSummary took %s, want <= %s", ignoreCtx, took[i], healthReadBudget+200*time.Millisecond)
			}
			if got.Status != HealthDegraded || !strings.Contains(strings.Join(got.Reasons, "; "), "META_UNREACHABLE") {
				t.Errorf("ignoreCtx=%v: summary %+v, want degraded naming META_UNREACHABLE", ignoreCtx, got)
			}
		}
		if n := hf.calls.Load(); n != 1 {
			t.Errorf("ignoreCtx=%v: %d store reads for 10 concurrent probes, want 1 (single-flight)", ignoreCtx, n)
		}
	}
}

// flakyHealth is a file health store that can stop answering; Unwrap makes
// it the separate (standalone) store whose managed findings are listed.
type flakyHealth struct {
	*healthstore.FileStore
	down atomic.Bool
}

func (f *flakyHealth) Unwrap() healthstore.Store { return f.FileStore }
func (f *flakyHealth) ListOpenFindingsForTask(ctx context.Context, id string) ([]health.Finding, error) {
	if f.down.Load() {
		return nil, errMetaDown
	}
	return f.FileStore.ListOpenFindingsForTask(ctx, id)
}
func (f *flakyHealth) ApplyTransitions(ctx context.Context, id string, fence health.Fence, ts []health.Transition, h health.TaskHealth) (healthstore.Applied, error) {
	if f.down.Load() {
		return healthstore.Applied{}, errMetaDown
	}
	return f.FileStore.ApplyTransitions(ctx, id, fence, ts, h)
}

// TestHealthSummaryManagedReadErrorIsNotOK (§25 D1): managed findings that
// cannot be read are reported as unreadable, never as an empty set.
func TestHealthSummaryManagedReadErrorIsNotOK(t *testing.T) {
	s := NewScheduler(WithStore(&reachStore{fakeStore: newFakeStore()}))
	hs := &flakyHealth{FileStore: healthstore.NewFileStore(t.TempDir() + "/h")}
	newEval(t, s, hs, EvaluatorOn)
	if got := s.HealthSummary(context.Background()); got.Status != HealthOK {
		t.Fatalf("healthy store: %+v", got)
	}
	hs.down.Store(true)
	got := s.HealthSummary(context.Background())
	if got.Status != HealthDegraded || !strings.Contains(strings.Join(got.Reasons, "; "), "managed findings of task _cluster unreadable") {
		t.Fatalf("unreadable managed findings: %+v, want degraded naming them", got)
	}
}

// TestMetaUnreachableOpensAfterTenSecondsAndLeavesOpenClose (§25 D2): 9s of
// failures open nothing; 10s opens META_UNREACHABLE/<node>; after recovery
// the store gets an OPEN at the first failure and then a CLOSE, although the
// OPEN could not be written while meta was away.
func TestMetaUnreachableOpensAfterTenSecondsAndLeavesOpenClose(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	s := NewScheduler(WithStore(&reachStore{fakeStore: newFakeStore()}))
	hs := &flakyHealth{FileStore: healthstore.NewFileStore(t.TempDir() + "/h")}
	e := NewEvaluator(s, EvaluatorConfig{Mode: EvaluatorOn, Store: hs, NodeID: "n1", Instance: "i1", Sweep: time.Minute, Debounce: time.Millisecond, Now: clock})
	key := health.SignalKey{Kind: health.MetaUnreachable, Subject: "n1"}
	failing := func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		en := e.signals[ClusterFindingTask][key]
		return en != nil && en.st.FailureSeq > en.st.SuccessSeq
	}

	hs.down.Store(true)
	start := clock()
	s.noteMetaReach(errMetaDown, false)
	advance(9 * time.Second)
	s.noteMetaReach(errMetaDown, false)
	s.noteMetaReach(errors.New("Error 1290: read-only"), false) // answered: not unreachable
	if failing() {
		t.Fatal("META_UNREACHABLE signalled after 9s, want 10s")
	}
	advance(time.Second)
	s.noteMetaReach(errMetaDown, false)
	if !failing() {
		t.Fatal("META_UNREACHABLE not signalled after 10s of failures")
	}
	_ = e.SweepNow(context.Background()) // the store is down: nothing lands

	advance(20 * time.Second)
	hs.down.Store(false)
	s.noteMetaReach(nil, true)
	for i := 0; i < 4; i++ {
		advance(time.Second)
		_ = e.SweepNow(context.Background())
	}
	ts, err := hs.ListTransitions(context.Background(), ClusterFindingTask)
	if err != nil {
		t.Fatal(err)
	}
	var ops []string
	var openAt time.Time
	for _, tr := range ts {
		if tr.Kind == string(health.MetaUnreachable) && tr.Subject == "n1" {
			ops = append(ops, tr.Op)
			if tr.Op == string(health.OpOpen) {
				openAt = tr.FactAt
			}
		}
	}
	if strings.Join(ops, ",") != "OPEN,CLOSE" {
		t.Fatalf("META_UNREACHABLE/n1 transitions %v, want OPEN,CLOSE (all: %+v)", ops, ts)
	}
	if !openAt.Equal(start) {
		t.Fatalf("OPEN fact_at %s, want the first failure %s", openAt, start)
	}
	if failing() {
		t.Fatal("signal still failing after recovery")
	}
}
