// Package tasks provides module-level functionality for tasks.
// input: worker B running at epoch 1 with a lease clock and a store fenced on task_leases; worker A claims epoch 2 on task_leases before it writes the task row; B's data-plane fence refuses its next append (lease handoff) or B's renew sees the lost lease first
// output: proof that B leaves the shared task row as it is in both orders, so A's claim is not read back as a remote STOPPED (#420 QA test 3: a 30s metadata pause left the task STOPPED with no owner)
// pos: regression for the P2-6a / P2-6b interaction (ADR 0009 §8.4, #360, #417)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"testing"
	"time"
)

// handoffRunner runs until cancelled, or returns the lease handoff a fenced
// data-plane write returns once refuse is closed.
type handoffRunner struct {
	started chan struct{}
	refuse  chan struct{}
}

func (r *handoffRunner) Run(ctx context.Context, task Task) error {
	select {
	case r.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return context.Canceled
	case <-r.refuse:
		return NewLeaseHandoff(ErrWriteFenced)
	}
}

func TestLeaseHandoff_ThawedOwnerLeavesRowForClaimer(t *testing.T) {
	for _, order := range []string{"fence-refuses-first", "renew-sees-loss-first"} {
		t.Run(order, func(t *testing.T) {
			leases := NewMemoryLease()
			store := &fencedMemoryStore{lockedFakeStore: &lockedFakeStore{fakeStore: newFakeStore()}, leases: leases}
			runner := &handoffRunner{started: make(chan struct{}, 4), refuse: make(chan struct{})}
			renew := time.Hour
			if order == "renew-sees-loss-first" {
				renew = 10 * time.Millisecond
			}
			s := NewScheduler(
				WithStore(store),
				WithClusterLeaseManager(leases),
				WithClusterWorkerID("worker-b"),
				WithClusterLease(time.Minute, renew, time.Minute),
				WithLeaseClock(NewLeaseClock()),
			)
			s.SetRunner(runner)
			task := ownedIdleTask(t, s, "", 0, StateStopped)
			if err := s.startTask(task.ID, taskStart{acquireLease: true}); err != nil {
				t.Fatalf("start: %v", err)
			}
			select {
			case <-runner.started:
			case <-time.After(2 * time.Second):
				t.Fatal("dump did not start")
			}
			waitTaskState(t, s, task.ID, 2*time.Second, StateRunning)
			before, err := store.GetTask(context.Background(), task.ID)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}

			// Worker A claims the expired lease at a newer epoch. Its task row
			// write has not landed yet: the row still names worker B.
			leases.mu.Lock()
			leases.rows[task.ID] = memoryLeaseRow{owner: "worker-a", epoch: before.Epoch + 1, expire: time.Now().Add(time.Hour)}
			leases.mu.Unlock()
			if order == "fence-refuses-first" {
				close(runner.refuse)
			}

			deadline := time.Now().Add(2 * time.Second)
			for {
				s.mu.Lock()
				_, open := s.runs[task.ID]
				s.mu.Unlock()
				if !open {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("worker-b kept its run open after losing the lease")
				}
				time.Sleep(5 * time.Millisecond)
			}

			got, err := store.GetTask(context.Background(), task.ID)
			if err != nil {
				t.Fatalf("GetTask after handoff: %v", err)
			}
			if got.State != StateRunning || got.DesiredRun != TaskDesiredRun || got.OwnerWorkerID != "worker-b" || got.Epoch != before.Epoch {
				t.Fatalf("worker-b wrote the row worker-a is claiming: state=%s desired=%s owner=%q epoch=%d last_error=%q",
					got.State, got.DesiredRun, got.OwnerWorkerID, got.Epoch, got.LastError)
			}
		})
	}
}
