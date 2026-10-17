// Package tasks provides module-level functionality for tasks.
// input: worker B running at epoch 1; meanwhile another worker claimed epoch 2, failed the task (SEGMENT_NOT_ON_WORKER) and released the lease (” / 2); B wakes up and reads the shared row; the task-row fence rule table
// output: proof that B stops its dump and leaves the row FAILED with its last_error (no STOPPED / desired STOP / owner cleared rewrite), and that CheckTaskRowFence refuses a release at a newer epoch and judges a missing lease row by the task row
// pos: regression for ADR 0009 P2-6c (design 0009 §24.4, #421)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"testing"
	"time"

	"binlog_server/internal/healthstore"
)

func TestCheckTaskRowFence(t *testing.T) {
	cases := []struct {
		name string
		in   TaskRowFenceInput
		want error
	}{
		{"held by writer", TaskRowFenceInput{LeaseFound: true, LeaseOwner: "b", LeaseEpoch: 1}, nil},
		{"held by another owner", TaskRowFenceInput{LeaseFound: true, LeaseOwner: "a", LeaseEpoch: 2}, healthstore.ErrFenced},
		{"held by writer at a newer epoch", TaskRowFenceInput{LeaseFound: true, LeaseOwner: "b", LeaseEpoch: 3}, healthstore.ErrFenced},
		{"released at writer epoch", TaskRowFenceInput{LeaseFound: true, LeaseEpoch: 1}, nil},
		{"released at a newer epoch (#421)", TaskRowFenceInput{LeaseFound: true, LeaseEpoch: 2}, healthstore.ErrFenced},
		{"no lease row, no task row", TaskRowFenceInput{}, nil},
		{"no lease row, task row unowned", TaskRowFenceInput{RowFound: true}, nil},
		{"no lease row, task row names writer", TaskRowFenceInput{RowFound: true, RowOwner: "b", RowEpoch: 1}, nil},
		{"no lease row, task row names another owner", TaskRowFenceInput{RowFound: true, RowOwner: "a", RowEpoch: 2}, healthstore.ErrFenced},
		{"no lease row, task row at a newer epoch", TaskRowFenceInput{RowFound: true, RowOwner: "b", RowEpoch: 2}, healthstore.ErrFenced},
	}
	for _, c := range cases {
		if err := CheckTaskRowFence(c.in, "b", 1); !errors.Is(err, c.want) || (c.want == nil && err != nil) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
}

// TestThawedOwnerKeepsForeignFailed is the #421 repro: the old owner's
// remote-stop branch used to read the FAILED row as a Stop and its run exit
// wrote STOPPED / desired STOP / owner "" / epoch 0 over the failure.
func TestThawedOwnerKeepsForeignFailed(t *testing.T) {
	leases := NewMemoryLease()
	store := &fencedMemoryStore{lockedFakeStore: &lockedFakeStore{fakeStore: newFakeStore()}, leases: leases}
	runner := &handoffRunner{started: make(chan struct{}, 4), refuse: make(chan struct{})}
	s := NewScheduler(
		WithStore(store),
		WithClusterLeaseManager(leases),
		WithClusterWorkerID("worker-b"),
		WithClusterLease(time.Minute, time.Hour, time.Minute),
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
	ctx := context.Background()
	mine, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}

	// Worker A claimed epoch 2, could not read B's segments, failed the task
	// and released the lease. markFailedLocked clears owner and epoch.
	const lastErr = "SEGMENT_NOT_ON_WORKER: /data/n1/1/mbin.000001.open.e1 is not on this worker"
	failed := mine
	failed.State, failed.DesiredRun, failed.LastError = StateFailed, TaskDesiredStop, lastErr
	failed.OwnerWorkerID, failed.Epoch, failed.RunID = "", 0, ""
	failed.FailedSpecRevision = failed.SpecRevision
	if err := store.fakeStore.UpsertTask(ctx, failed); err != nil {
		t.Fatalf("write FAILED: %v", err)
	}
	leases.mu.Lock()
	leases.rows[task.ID] = memoryLeaseRow{owner: "", epoch: mine.Epoch + 1, expire: time.Now()}
	leases.mu.Unlock()

	s.applyRemoteStops()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		_, open := s.runs[task.ID]
		s.mu.Unlock()
		if !open {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker-b kept dumping after the row was FAILED by another epoch")
		}
		time.Sleep(5 * time.Millisecond)
	}

	got, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State != StateFailed || got.LastError != lastErr || got.DesiredRun != TaskDesiredStop {
		t.Fatalf("the failure was rewritten: state=%s desired=%s owner=%q epoch=%d last_error=%q",
			got.State, got.DesiredRun, got.OwnerWorkerID, got.Epoch, got.LastError)
	}
	s.mu.Lock()
	mem := s.tasks[task.ID]
	s.mu.Unlock()
	if mem.State != StateFailed {
		t.Fatalf("memory state %s, want FAILED (the stored row)", mem.State)
	}
	if s.leaseClock.Fresh(task.ID, mine.Epoch) {
		t.Fatal("the old epoch is still fresh on the lease clock; the data-plane fence would let it write")
	}
}

// The renew loop of the old run failed with "context canceled" once the run
// was cancelled; markLeaseDegradedLocked wrote LEASE_DEGRADED over the
// FAILED row (seen on the real #421 repro). And finishStoredStopLocked
// wrote STOPPED over a stored FAILED row without a fence.
func TestStopPathsKeepStoredFailed(t *testing.T) {
	for _, path := range []string{"lease-degraded", "finish-stored-stop"} {
		t.Run(path, func(t *testing.T) {
			store := newFakeStore()
			s := NewScheduler(WithStore(store))
			task := s.mustTask(t)
			ctx := context.Background()
			failed, err := store.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			failed.State, failed.DesiredRun, failed.LastError = StateFailed, TaskDesiredStop, failedLastError
			failed.OwnerWorkerID, failed.Epoch = "", 0
			if err := store.UpsertTask(ctx, failed); err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			s.tasks[task.ID] = failed
			switch path {
			case "lease-degraded":
				err = s.markLeaseDegradedLocked(task.ID, "context canceled", time.Now())
			default:
				err = s.finishStoredStopLocked(task.ID)
			}
			mem := s.tasks[task.ID]
			s.mu.Unlock()
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			got, _ := store.GetTask(ctx, task.ID)
			if got.State != StateFailed || got.LastError != failedLastError || mem.State != StateFailed {
				t.Fatalf("FAILED rewritten: row=%s %q memory=%s", got.State, got.LastError, mem.State)
			}
		})
	}
}
