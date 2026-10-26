// Package tasks tests scheduler lease handoff.
// input: three in-process schedulers, one shared lease, a runner that returns ENOENT for this epoch's .open.eN
// output: after a peer takes the next epoch, or after this epoch is lost, the task stays desired RUN and is not FAILED
// pos: regression for #457 on the scheduler side of the stat-after-takeover path
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// statOpenRunner blocks until release, then reports that this run's open
// segment is gone. A cancelled context still returns that stat error, so a
// renew that loses the race cannot hide it as context.Canceled.
type statOpenRunner struct {
	node    string
	log     *dumpLog
	release chan struct{}
}

func (r *statOpenRunner) Run(ctx context.Context, task Task) error {
	end := r.log.begin(r.node, task.Epoch)
	defer end()
	select {
	case <-r.release:
	case <-ctx.Done():
	}
	return &os.PathError{
		Op:   "stat",
		Path: filepath.Join("data", task.ID, openEpochBase(task.Epoch)),
		Err:  os.ErrNotExist,
	}
}

func openEpochBase(epoch int64) string {
	return "mysql-bin.000001.open.e" + itoa(epoch)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestOpenEpochENOENTAfterTakeoverDoesNotFail(t *testing.T) {
	// The new owner's lease must not expire while this test joins the old
	// run. A 150ms TTL does: under -race the join outlasts it, the other
	// peer claims, and two runs stay open because renew is not due for an
	// hour. Advance a frozen lease clock once so only the original lease
	// expires.
	const ttl = time.Hour
	clock := &leaseTestClock{now: time.Now()}
	w := newP29World(t, ttl, time.Hour, time.Hour, false, false)
	w.leases.now = clock.Now
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	w.owner().SetRunner(&statOpenRunner{node: "n1", log: w.log, release: release})
	id, epoch := w.start()
	clock.Advance(ttl + time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for !w.leaseFree(id) {
		if time.Now().After(deadline) {
			t.Fatal("lease did not expire")
		}
		time.Sleep(5 * time.Millisecond)
	}
	var owner string
	var next int64
	deadline = time.Now().Add(2 * time.Second)
	for {
		for _, node := range []string{"n2", "n3"} {
			if _, err := w.nodes[node].ClaimRunnableTasks(); err != nil {
				t.Fatalf("%s claim: %v", node, err)
			}
		}
		row, err := w.table.GetTask(context.Background(), id)
		if err == nil && row.DesiredRun == TaskDesiredRun && row.State == StateRunning && row.Epoch > epoch && row.OwnerWorkerID != "" && row.OwnerWorkerID != "n1" {
			owner, next = row.OwnerWorkerID, row.Epoch
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("peer did not take over: state %s desired %s owner %q epoch %d", row.State, row.DesiredRun, row.OwnerWorkerID, row.Epoch)
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	waitRunClosed(t, w.owner(), id, "old owner stat after takeover")
	stored, err := w.table.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State == StateFailed || stored.DesiredRun != TaskDesiredRun {
		t.Fatalf("row %s desired %s owner %q epoch %d last %q; want desired RUN, not FAILED", stored.State, stored.DesiredRun, stored.OwnerWorkerID, stored.Epoch, stored.LastError)
	}
	if stored.State != StateRunning || stored.OwnerWorkerID != owner || stored.Epoch != next {
		t.Fatalf("row %s owner %q epoch %d; want RUNNING %s epoch %d", stored.State, stored.OwnerWorkerID, stored.Epoch, owner, next)
	}
	if w.openRuns(id) != 1 {
		t.Fatalf("open runs %d, want the new epoch only", w.openRuns(id))
	}
}

func TestOpenEpochENOENTAfterLostLeaseDoesNotFail(t *testing.T) {
	w := newP29World(t, time.Hour, time.Hour, time.Hour, false, false)
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	w.owner().SetRunner(&statOpenRunner{node: "n1", log: w.log, release: release})
	id, epoch := w.start()
	w.owner().leaseClock.Lose(id, epoch)
	close(release)
	waitRunClosed(t, w.owner(), id, "old owner stat after lost lease")
	stored, err := w.table.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State == StateFailed || stored.DesiredRun != TaskDesiredRun {
		t.Fatalf("row %s desired %s last %q; want desired RUN, not FAILED", stored.State, stored.DesiredRun, stored.LastError)
	}
	w.owner().mu.Lock()
	mem := w.owner().tasks[id]
	w.owner().mu.Unlock()
	if mem.State == StateFailed || mem.DesiredRun != TaskDesiredRun {
		t.Fatalf("memory %s desired %s; want claimable, desired RUN", mem.State, mem.DesiredRun)
	}
}

func TestOpenEpochENOENTWhileOwnerStillFails(t *testing.T) {
	w := newP29World(t, time.Hour, time.Hour, time.Hour, false, false)
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	w.owner().SetRunner(&statOpenRunner{node: "n1", log: w.log, release: release})
	id, _ := w.start()
	close(release)
	waitRunClosed(t, w.owner(), id, "stat while still owner")
	stored := waitStoredState(t, w.table, id, StateFailed)
	if stored.DesiredRun != TaskDesiredStop {
		t.Fatalf("desired %s, want STOP after a missing open file while this epoch still holds the lease", stored.DesiredRun)
	}
}
