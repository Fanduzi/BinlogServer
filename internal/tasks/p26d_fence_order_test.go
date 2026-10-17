// Package tasks provides module-level functionality for tasks.
// input: two real Schedulers (worker-a, worker-b) sharing one task table, one MemoryLease and one lease-fenced health store; worker-b runs at epoch 1 and is frozen (its lease renew blocks, as under SIGSTOP) until the lease expires; worker-a claims epoch 2 through ClaimExpiredTasks and its run has not failed yet; worker-b thaws and its renew, its data-plane fence, or its remote-stop poll acts before worker-a writes FAILED
// output: proof that in each reverse order worker-b's run exits without writing a task row (no STOPPING / STOPPED over worker-a's claim), that worker-a's SEGMENT_NOT_ON_WORKER then leaves the row FAILED with its last_error even after worker-b polls remote stops and converges again, and that a CRITICAL TASK_FAILED is open with /api/health degraded on both nodes
// pos: integration regression for ADR 0009 P2-6c/P2-6d (docs/design/0009-phase2-health-evaluator.md §24.11, #421 QA on #424: the reverse fence order was only unit-tested)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/health"
)

// freezableLease is worker-b's view of the shared MemoryLease. While frozen
// a Renew blocks and ignores its context: a SIGSTOPped process does not time
// out, it just stops.
type freezableLease struct {
	*MemoryLease
	mu     sync.Mutex
	frozen chan struct{}
}

func (l *freezableLease) freeze() {
	l.mu.Lock()
	l.frozen = make(chan struct{})
	l.mu.Unlock()
}

func (l *freezableLease) thaw() {
	l.mu.Lock()
	if l.frozen != nil {
		close(l.frozen)
		l.frozen = nil
	}
	l.mu.Unlock()
}

func (l *freezableLease) Renew(ctx context.Context, taskID, workerID string, epoch int64, now time.Time, ttl time.Duration) (bool, error) {
	l.mu.Lock()
	ch := l.frozen
	l.mu.Unlock()
	if ch != nil {
		<-ch
	}
	return l.MemoryLease.Renew(ctx, taskID, workerID, epoch, now, ttl)
}

// taskWrite is one task row write and the node that made it.
type taskWrite struct {
	node         string
	state        State
	owner        string
	epoch        int64
	lastError    string
	fencedRefuse bool
}

// sharedTaskTable is the shared backup_tasks table; nodeStore is one node's
// connection to it, which records every row write that node makes.
type sharedTaskTable struct {
	*fencedMemoryStore
	mu     sync.Mutex
	writes []taskWrite
}

func (t *sharedTaskTable) record(w taskWrite) {
	t.mu.Lock()
	t.writes = append(t.writes, w)
	t.mu.Unlock()
}

func (t *sharedTaskTable) writesBy(node string) []taskWrite {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []taskWrite
	for _, w := range t.writes {
		if w.node == node {
			out = append(out, w)
		}
	}
	return out
}

// ListTasksWithExpiredLease is what meta answers for ClaimExpiredTasks.
func (t *sharedTaskTable) ListTasksWithExpiredLease(ctx context.Context) ([]Task, error) {
	all, err := t.fencedMemoryStore.ListTasks(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var out []Task
	for _, task := range all {
		t.leases.mu.Lock()
		row, ok := t.leases.rows[task.ID]
		t.leases.mu.Unlock()
		if ok && row.owner != "" && !row.expire.After(now) {
			out = append(out, task)
		}
	}
	return out, nil
}

type nodeStore struct {
	*sharedTaskTable
	node string
}

func (n *nodeStore) UpsertTask(ctx context.Context, task Task) error {
	n.record(taskWrite{node: n.node, state: task.State, owner: task.OwnerWorkerID, epoch: task.Epoch, lastError: task.LastError})
	return n.sharedTaskTable.UpsertTask(ctx, task)
}

func (n *nodeStore) UpsertTaskFenced(ctx context.Context, task Task, owner string, epoch int64) error {
	err := n.sharedTaskTable.UpsertTaskFenced(ctx, task, owner, epoch)
	n.record(taskWrite{node: n.node, state: task.State, owner: task.OwnerWorkerID, epoch: task.Epoch, lastError: task.LastError, fencedRefuse: err != nil})
	return err
}

// failingRunner runs until fail is closed, then fails the way a takeover
// into a foreign data_dir does (§24.8).
type failingRunner struct {
	started chan struct{}
	fail    chan struct{}
}

const foreignSegmentError = "/n1/data/1/mbin.000001.open.e1 is outside this worker's data_dir /n2/data"

func (r *failingRunner) Run(ctx context.Context, task Task) error {
	select {
	case r.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return context.Canceled
	case <-r.fail:
		return NewPermanentError(CodeSegmentNotOnWorker, foreignSegmentError)
	}
}

func waitRunClosed(t *testing.T, s *Scheduler, id string, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.mu.Lock()
		done, open := s.runs[id]
		s.mu.Unlock()
		if !open || isClosed(done) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: run still open", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitStoredState(t *testing.T, store TaskStore, id string, want State) Task {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got, err := store.GetTask(context.Background(), id)
		if err == nil && got.State == want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("stored state %s (err %v), want %s", got.State, err, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestReverseFenceOrder_OldOwnerActsBeforeFailed(t *testing.T) {
	for _, order := range []string{"renew-sees-loss-first", "fence-refuses-first", "remote-stop-poll-first"} {
		t.Run(order, func(t *testing.T) {
			leases := NewMemoryLease()
			table := &sharedTaskTable{fencedMemoryStore: &fencedMemoryStore{lockedFakeStore: &lockedFakeStore{fakeStore: newFakeStore()}, leases: leases}}
			hs := newLeaseHealthStore(leases)
			ctx := context.Background()

			// worker-b: the old owner. Short TTL so a freeze expires its lease.
			bLease := &freezableLease{MemoryLease: leases}
			bRunner := &handoffRunner{started: make(chan struct{}, 4), refuse: make(chan struct{})}
			b := NewScheduler(
				WithStore(&nodeStore{sharedTaskTable: table, node: "worker-b"}),
				WithFindingStore(hs),
				WithClusterLeaseManager(bLease),
				WithClusterWorkerID("worker-b"),
				WithClusterLease(300*time.Millisecond, 40*time.Millisecond, time.Minute),
				WithLeaseClock(NewLeaseClock()),
			)
			b.SetRunner(bRunner)
			eB := NewEvaluator(b, EvaluatorConfig{Mode: EvaluatorOn, Store: hs, Cluster: true, NodeID: "worker-b", Instance: "b-i", Sweep: time.Minute})

			task := ownedIdleTask(t, b, "", 0, StateStopped)
			if err := b.startTask(task.ID, taskStart{acquireLease: true}); err != nil {
				t.Fatalf("worker-b start: %v", err)
			}
			select {
			case <-bRunner.started:
			case <-time.After(2 * time.Second):
				t.Fatal("worker-b dump did not start")
			}
			waitTaskState(t, b, task.ID, 2*time.Second, StateRunning)
			mine := waitStoredState(t, table, task.ID, StateRunning)
			if mine.OwnerWorkerID != "worker-b" || mine.Epoch != 1 {
				t.Fatalf("worker-b row %s/%d, want worker-b/1", mine.OwnerWorkerID, mine.Epoch)
			}

			// Freeze worker-b until its lease expires.
			bLease.freeze()
			time.Sleep(450 * time.Millisecond)

			// worker-a: claims the expired lease at epoch 2; its run has not failed yet.
			aRunner := &failingRunner{started: make(chan struct{}, 4), fail: make(chan struct{})}
			a := NewScheduler(
				WithStore(&nodeStore{sharedTaskTable: table, node: "worker-a"}),
				WithFindingStore(hs),
				WithClusterLeaseManager(leases),
				WithClusterWorkerID("worker-a"),
				WithClusterLease(time.Minute, time.Hour, time.Minute),
				WithLeaseClock(NewLeaseClock()),
			)
			a.SetRunner(aRunner)
			eA := NewEvaluator(a, EvaluatorConfig{Mode: EvaluatorOn, Store: hs, Cluster: true, NodeID: "worker-a", Instance: "a-i", Sweep: time.Minute})
			if n, err := a.ClaimExpiredTasks(); err != nil || n != 1 {
				t.Fatalf("worker-a ClaimExpiredTasks = %d, %v; want 1 claim", n, err)
			}
			select {
			case <-aRunner.started:
			case <-time.After(2 * time.Second):
				t.Fatal("worker-a dump did not start")
			}
			claimed := waitStoredState(t, table, task.ID, StateRunning)
			if claimed.OwnerWorkerID != "worker-a" || claimed.Epoch != 2 {
				t.Fatalf("claimed row %s/%d, want worker-a/2", claimed.OwnerWorkerID, claimed.Epoch)
			}
			bWritesBefore := len(table.writesBy("worker-b"))

			// worker-b thaws and acts first.
			switch order {
			case "renew-sees-loss-first":
				bLease.thaw()
			case "fence-refuses-first":
				close(bRunner.refuse)
				bLease.thaw()
			case "remote-stop-poll-first":
				b.applyRemoteStops()
				if got, _ := table.GetTask(ctx, task.ID); got.OwnerWorkerID != "worker-a" || got.State != StateRunning {
					t.Fatalf("worker-b's remote-stop poll changed worker-a's row: %s %s/%d", got.State, got.OwnerWorkerID, got.Epoch)
				}
				bLease.thaw()
			}
			waitRunClosed(t, b, task.ID, "worker-b after thaw")
			b.applyRemoteStops()
			_, _ = b.convergeTask(task.ID)

			got, _ := table.GetTask(ctx, task.ID)
			if got.State != StateRunning || got.OwnerWorkerID != "worker-a" || got.Epoch != 2 {
				t.Fatalf("after worker-b thawed the row is %s %s/%d (last_error %q), want RUNNING worker-a/2",
					got.State, got.OwnerWorkerID, got.Epoch, got.LastError)
			}
			for _, w := range table.writesBy("worker-b")[bWritesBefore:] {
				if !w.fencedRefuse {
					t.Fatalf("worker-b wrote the task row after losing the lease: %+v", w)
				}
			}

			// Only now does worker-a fail.
			close(aRunner.fail)
			failed := waitStoredState(t, table, task.ID, StateFailed)
			if failed.LastError == "" || failed.DesiredRun != TaskDesiredStop {
				t.Fatalf("FAILED row %+v", failed)
			}
			waitRunClosed(t, a, task.ID, "worker-a after FAILED")

			// worker-b polls and converges again; both nodes sweep.
			b.applyRemoteStops()
			_, _ = b.convergeTask(task.ID)
			_, _ = b.GetTask(task.ID)
			_, _ = a.GetTask(task.ID)
			if err := eA.SweepNow(ctx); err != nil {
				t.Fatalf("worker-a sweep: %v", err)
			}
			if err := eB.SweepNow(ctx); err != nil {
				t.Fatalf("worker-b sweep: %v", err)
			}
			final, _ := table.GetTask(ctx, task.ID)
			if final.State != StateFailed || final.LastError != failed.LastError || final.DesiredRun != TaskDesiredStop {
				t.Fatalf("FAILED rewritten: %s desired=%s last_error=%q", final.State, final.DesiredRun, final.LastError)
			}
			for _, w := range table.writesBy("worker-b")[bWritesBefore:] {
				if !w.fencedRefuse {
					t.Fatalf("worker-b wrote the task row: %+v", w)
				}
			}
			h, open, _ := hs.snapshot(task.ID)
			var tf *health.Finding
			for _, f := range open {
				if f.Kind == health.TaskFailed {
					f := f
					tf = &f
				}
			}
			if tf == nil || tf.Severity != health.Critical || h.Status != health.StatusUnhealthy {
				t.Fatalf("health %s, open %+v; want CRITICAL TASK_FAILED and UNHEALTHY", h.Status, open)
			}
			for name, s := range map[string]*Scheduler{"worker-a": a, "worker-b": b} {
				if sum := s.HealthSummary(ctx); sum.Status != HealthDegraded {
					t.Fatalf("%s /api/health = %+v, want degraded", name, sum)
				}
			}
		})
	}
}
