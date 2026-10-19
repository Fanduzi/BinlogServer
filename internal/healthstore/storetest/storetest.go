// Package storetest provides module-level functionality for storetest.
// input: a healthstore.Store constructor and, for stores with a lease table, a hook that sets a task_leases row
// output: Run, the contract every Store must pass (design §14 P2-2): store-allocated generations, digest replay, complete paging, standalone and cluster fences, 1000-way concurrent writers; a cluster storm seeds chain_owner with an empty holder write first (§30)
// pos: shared contract tests for the file store and the MySQL store
// note: if this file changes, update this header and module README.md.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
)

// Harness builds the store under test.
type Harness struct {
	New func(t *testing.T) healthstore.Store
	// SetLease sets (or with epoch 0 deletes) the task's task_leases row;
	// nil when the store has no lease table (cluster cases are skipped).
	SetLease func(t *testing.T, taskID, owner string, epoch int64, expired bool)
	// Concurrency for the concurrent-writers case (default 1000).
	Concurrency int
}

var t0 = time.Date(2026, 10, 15, 2, 0, 0, 0, time.UTC)

func taskID(name string) string {
	return fmt.Sprintf("st-%s-%d", name, rand.New(rand.NewSource(time.Now().UnixNano())).Intn(1_000_000_000))
}

func open(task, subject string, sev health.Severity, evidence string, at time.Time) health.Transition {
	k := health.Key{TaskID: task, Kind: health.SegmentOverlap, Subject: subject}
	return health.Transition{Key: k, Op: health.OpOpen, Reason: health.ReasonEval, At: at, FactAt: at, KindVersion: 1,
		Instance: health.Instance{Key: k, Severity: sev, Detail: "twice", Evidence: evidence, Segments: []string{subject}}}
}

func closeT(task, subject string, at time.Time) health.Transition {
	t := open(task, subject, health.Critical, "", at)
	t.Op = health.OpClose
	return t
}

func standalone(node, inst string) health.Fence {
	return health.Fence{Mode: health.FenceStandalone, Node: node, Instance: inst}
}

func th(task string, st health.Status) health.TaskHealth {
	return health.TaskHealth{TaskID: task, Status: st, EvaluatedAt: t0}
}

// Run runs the contract.
func Run(t *testing.T, h Harness) {
	ctx := context.Background()
	t.Run("GenerationsAllocatedByStore", func(t *testing.T) {
		s := h.New(t)
		task := taskID("gen")
		f := standalone("n1", "i1")
		a, err := s.ApplyTransitions(ctx, task, f, []health.Transition{open(task, "a", health.Critical, "x", t0), open(task, "b", health.Warning, "y", t0)}, th(task, health.StatusUnhealthy))
		if err != nil || fmt.Sprint(a.Generations) != "[1 2]" {
			t.Fatalf("first apply %v %v", a, err)
		}
		a, err = s.ApplyTransitions(ctx, task, f, []health.Transition{closeT(task, "a", t0.Add(time.Minute))}, th(task, health.StatusDegraded))
		if err != nil || fmt.Sprint(a.Generations) != "[3]" {
			t.Fatalf("close %v %v", a, err)
		}
		got, err := s.ListOpenFindingsForTask(ctx, task)
		if err != nil || len(got) != 1 || got[0].Subject != "b" || got[0].Severity != health.Warning || got[0].Evidence != "y" || got[0].Generation != 2 {
			t.Fatalf("open %+v %v", got, err)
		}
		hs, err := s.ListHealth(ctx)
		if err != nil || !hasHealth(hs, task, health.StatusDegraded) {
			t.Fatalf("health %+v %v", hs, err)
		}
	})
	t.Run("ReplayByContentNotGeneration", func(t *testing.T) {
		s := h.New(t)
		task := taskID("replay")
		f := standalone("n1", "i1")
		tr := open(task, "a", health.Critical, "x", t0)
		if _, err := s.ApplyTransitions(ctx, task, f, []health.Transition{tr}, th(task, health.StatusUnhealthy)); err != nil {
			t.Fatal(err)
		}
		a, err := s.ApplyTransitions(ctx, task, f, []health.Transition{tr}, th(task, health.StatusUnhealthy))
		if err != nil || fmt.Sprint(a.Generations) != "[0]" {
			t.Fatalf("identical replay must be a no-op: %v %v", a, err)
		}
		changed := tr
		changed.Op, changed.Instance.Evidence = health.OpUpdate, "x2"
		a, err = s.ApplyTransitions(ctx, task, f, []health.Transition{changed}, th(task, health.StatusUnhealthy))
		if err != nil || fmt.Sprint(a.Generations) != "[2]" {
			t.Fatalf("different content must get a new generation: %v %v", a, err)
		}
		if lister, ok := s.(healthstore.TransitionLister); ok {
			rows, err := lister.ListTransitions(ctx, task)
			if err != nil || len(rows) != 2 {
				t.Fatalf("transitions %+v %v", rows, err)
			}
		}
	})
	t.Run("PagingReadsEveryOpenFinding", func(t *testing.T) {
		s := h.New(t)
		task := taskID("page")
		f := standalone("n1", "i1")
		const n = 5000
		var batch []health.Transition
		for i := 0; i < n; i++ {
			batch = append(batch, open(task, fmt.Sprintf("seg-%05d", i), health.Warning, "", t0))
			if len(batch) == 500 {
				if _, err := s.ApplyTransitions(ctx, task, f, batch, th(task, health.StatusDegraded)); err != nil {
					t.Fatal(err)
				}
				batch = nil
			}
		}
		other := taskID("page-other")
		if _, err := s.ApplyTransitions(ctx, other, f, []health.Transition{open(other, "x", health.Warning, "", t0)}, th(other, health.StatusDegraded)); err != nil {
			t.Fatal(err)
		}
		got, err := s.ListOpenFindingsForTask(ctx, task)
		if err != nil || len(got) != n {
			t.Fatalf("paged %d open, want %d (%v)", len(got), n, err)
		}
		seen := map[string]bool{}
		for _, g := range got {
			if g.TaskID != task || seen[g.Subject] {
				t.Fatalf("bad row %+v", g)
			}
			seen[g.Subject] = true
		}
	})
	t.Run("StandaloneFence", func(t *testing.T) {
		s := h.New(t)
		task := taskID("standalone")
		if _, err := s.ApplyTransitions(ctx, task, standalone("n1", "i1"), []health.Transition{open(task, "a", health.Critical, "", t0)}, th(task, health.StatusUnhealthy)); err != nil {
			t.Fatalf("first write (no lease row, standalone + meta): %v", err)
		}
		if _, err := s.ApplyTransitions(ctx, task, standalone("n1", "i2"), []health.Transition{closeT(task, "a", t0.Add(time.Minute))}, th(task, health.StatusHealthy)); err != nil {
			t.Fatalf("same node, new instance (restart) must take over: %v", err)
		}
		if _, err := s.ApplyTransitions(ctx, task, standalone("n2", "i9"), []health.Transition{open(task, "b", health.Critical, "", t0)}, th(task, health.StatusUnhealthy)); !errors.Is(err, healthstore.ErrFenced) {
			t.Fatalf("another node on the same meta: %v, want ErrFenced", err)
		}
	})
	t.Run("ConcurrentWritersNeverShareAGeneration", func(t *testing.T) {
		s := h.New(t)
		task := taskID("conc")
		n := h.Concurrency
		if n == 0 {
			n = 1000
		}
		// Two writers with one fence: the _health proxy and the delegated last
		// owner (cluster), or two goroutines of one standalone node.
		proxy, deleg := standalone("n1", "i1"), standalone("n1", "i1")
		if h.SetLease != nil {
			// A delegated write needs the chain owner a holder write records
			// (design §30). Empty transitions leave generation at 0.
			h.SetLease(t, task, "n1", 3, false)
			if _, err := s.ApplyTransitions(ctx, task, health.Fence{Mode: health.FenceCluster, TaskEpoch: 3, Node: "n1"}, nil, th(task, health.StatusHealthy)); err != nil {
				t.Fatalf("seed chain owner: %v", err)
			}
			h.SetLease(t, task, "n1", 3, true)
			proxy = health.Fence{Mode: health.FenceCluster, TaskEpoch: 3, Node: "n1", Proxy: true}
			deleg = health.Fence{Mode: health.FenceCluster, TaskEpoch: 3, Node: "n1", Delegated: true}
		}
		var wg sync.WaitGroup
		gens := make([]int64, n)
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				f := proxy
				if i%2 == 1 {
					f = deleg
				}
				a, err := s.ApplyTransitions(ctx, task, f, []health.Transition{open(task, fmt.Sprintf("c-%04d", i), health.Warning, "", t0)}, th(task, health.StatusDegraded))
				errs[i] = err
				if err == nil {
					gens[i] = a.Generations[0]
				}
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("writer %d: %v", i, err)
			}
		}
		sort.Slice(gens, func(i, j int) bool { return gens[i] < gens[j] })
		for i, g := range gens {
			if g != int64(i+1) {
				t.Fatalf("generations not exactly 1..%d: position %d has %d", n, i, g)
			}
		}
		if lister, ok := s.(healthstore.TransitionLister); ok {
			rows, err := lister.ListTransitions(ctx, task)
			if err != nil || len(rows) != n {
				t.Fatalf("%d transition rows, want %d (%v)", len(rows), n, err)
			}
		}
		got, err := s.ListOpenFindingsForTask(ctx, task)
		if err != nil || len(got) != n {
			t.Fatalf("%d open, want %d (%v)", len(got), n, err)
		}
	})
	if h.SetLease == nil {
		return
	}
	t.Run("ClusterFence", func(t *testing.T) {
		s := h.New(t)
		task := taskID("cluster")
		w := func(f health.Fence, subject string) error {
			_, err := s.ApplyTransitions(ctx, task, f, []health.Transition{open(task, subject, health.Warning, subject, t0)}, th(task, health.StatusDegraded))
			return err
		}
		never := health.Fence{Mode: health.FenceNeverLeased, Node: "hh"}
		if err := w(never, "never"); err != nil {
			t.Fatalf("never-leased task, no lease row: %v", err)
		}
		h.SetLease(t, task, "n1", 1, false)
		if err := w(never, "never2"); !errors.Is(err, healthstore.ErrFenced) {
			t.Fatalf("never fence after a lease row exists: %v", err)
		}
		e1 := health.Fence{Mode: health.FenceCluster, TaskEpoch: 1, Node: "n1"}
		if err := w(e1, "e1"); err != nil {
			t.Fatalf("holder: %v", err)
		}
		h.SetLease(t, task, "n1", 1, true)
		if err := w(e1, "e1-late"); !errors.Is(err, healthstore.ErrLeaseLagging) {
			t.Fatalf("same owner and epoch, expired: %v, want ErrLeaseLagging", err)
		}
		h.SetLease(t, task, "n2", 2, false)
		if err := w(e1, "e1-stale"); !errors.Is(err, healthstore.ErrFenced) {
			t.Fatalf("frozen old owner: %v, want ErrFenced", err)
		}
		e2 := health.Fence{Mode: health.FenceCluster, TaskEpoch: 2, Node: "n2"}
		stale := health.Fence{Mode: health.FenceCluster, TaskEpoch: 1, Node: "n1", Proxy: true}
		for i := 0; i < 100; i++ { // _health proxy at the old epoch alternating with the holder
			if err := w(e2, fmt.Sprintf("holder-%d", i)); err != nil {
				t.Fatalf("new owner must never be fenced (round %d): %v", i, err)
			}
			if err := w(stale, fmt.Sprintf("proxy-%d", i)); !errors.Is(err, healthstore.ErrFenced) {
				t.Fatalf("stale proxy round %d: %v", i, err)
			}
		}
		h.SetLease(t, task, "", 2, true) // released
		if err := w(health.Fence{Mode: health.FenceCluster, TaskEpoch: 2, Node: "n2", Delegated: true}, "deleg"); err != nil {
			t.Fatalf("delegated last owner on a released lease: %v", err)
		}
		h.SetLease(t, task, "n3", 3, false)
		if err := w(health.Fence{Mode: health.FenceCluster, TaskEpoch: 2, Node: "n2", Delegated: true}, "deleg2"); !errors.Is(err, healthstore.ErrFenced) {
			t.Fatalf("delegation must lapse on a new lease: %v", err)
		}
	})
}

func hasHealth(hs []health.TaskHealth, task string, st health.Status) bool {
	for _, h := range hs {
		if h.TaskID == task && h.Status == st {
			return true
		}
	}
	return false
}
