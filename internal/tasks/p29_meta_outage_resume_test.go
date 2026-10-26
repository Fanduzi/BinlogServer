// Package tasks provides module-level functionality for tasks.
// input: three in-process schedulers sharing one MemoryLease and one fenced task table; the owner's meta calls can hang until their ctx ends, or its renew can freeze as under SIGSTOP; an in-flight append that returns WRITE_FENCED when grace cancels the dump
// output: proof that a meta outage longer than lease grace resumes the task on a new epoch with one dump and no overlap, that a WRITE_FENCED append at that handoff leaves a claimable row and resumes at epoch+1 (#454), that a STOPPED row with desired RUN and an empty owner is claimed the same way, that a shorter outage and a SIGSTOP keep the same dump, that an operator Stop and a FAILED task stay stopped, and that a desired-RUN task nobody claims opens CRITICAL TASK_NO_PROGRESS within one sweep and degrades HealthSummary
// pos: regression for issue 450 (design 0009 §29); the in-process three-scheduler harness
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"fmt"
	"testing"
	"time"

	"binlog_server/internal/health"
)

// resumeTable is the shared backup_tasks table. ListTasksWithExpiredLease
// matches production: lease_expire_at <= now, including a released row
// whose owner is empty. sharedTaskTable hides those rows.
type resumeTable struct {
	*fencedMemoryStore
}

func (t *resumeTable) ListTasksWithExpiredLease(ctx context.Context) ([]Task, error) {
	all, err := t.ListTasks(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if t.leases != nil && t.leases.now != nil {
		now = t.leases.now()
	}
	var out []Task
	for _, task := range all {
		switch task.State {
		case StateRunning, StateLeaseDegraded, StateRetryBackoff, StateStopping:
		default:
			if !claimableStoppedRun(task) {
				continue
			}
		}
		t.leases.mu.Lock()
		row, ok := t.leases.rows[task.ID]
		t.leases.mu.Unlock()
		if !ok || row.expire.After(now) {
			continue
		}
		out = append(out, task)
	}
	return out, nil
}

// ownerGatedLease is one node's view of the shared lease table. While the
// gate is closed, Acquire, Renew and Release hang until their ctx ends.
type ownerGatedLease struct {
	*MemoryLease
	g *metaGate
}

func (l *ownerGatedLease) Acquire(ctx context.Context, taskID, workerID string, ttl time.Duration) (int64, bool, error) {
	if err := l.g.wait(ctx); err != nil {
		return 0, false, err
	}
	return l.MemoryLease.Acquire(ctx, taskID, workerID, ttl)
}

func (l *ownerGatedLease) Renew(ctx context.Context, taskID, workerID string, epoch int64, now time.Time, ttl time.Duration) (bool, error) {
	if err := l.g.wait(ctx); err != nil {
		return false, err
	}
	return l.MemoryLease.Renew(ctx, taskID, workerID, epoch, now, ttl)
}

func (l *ownerGatedLease) Release(ctx context.Context, taskID, workerID string, epoch int64) (bool, error) {
	if err := l.g.wait(ctx); err != nil {
		return false, err
	}
	return l.MemoryLease.Release(ctx, taskID, workerID, epoch)
}

// ownerGatedStore is the owner's task store. Writes hang with the gate, so a
// STOPPED row that cannot land during the outage is republished after.
type ownerGatedStore struct {
	*resumeTable
	g *metaGate
}

func (s *ownerGatedStore) UpsertTask(ctx context.Context, task Task) error {
	if err := s.g.wait(ctx); err != nil {
		return err
	}
	return s.resumeTable.UpsertTask(ctx, task)
}

func (s *ownerGatedStore) UpsertTaskFenced(ctx context.Context, task Task, owner string, epoch int64) error {
	if err := s.g.wait(ctx); err != nil {
		return err
	}
	return s.resumeTable.UpsertTaskFenced(ctx, task, owner, epoch)
}

// dumpSpan is one dump this process opened.
type dumpSpan struct {
	node       string
	epoch      int64
	start, end time.Time
}

// dumpLog records how many dumps are open at once and whether their
// lifetimes overlap.
type dumpLog struct {
	mu      chan struct{}
	live    int
	maxLive int
	spans   []dumpSpan
	started chan struct{}
}

func newDumpLog() *dumpLog {
	return &dumpLog{mu: make(chan struct{}, 1), started: make(chan struct{}, 8)}
}

func (l *dumpLog) lock()   { l.mu <- struct{}{} }
func (l *dumpLog) unlock() { <-l.mu }

func (l *dumpLog) begin(node string, epoch int64) func() {
	l.lock()
	l.live++
	if l.live > l.maxLive {
		l.maxLive = l.live
	}
	i := len(l.spans)
	l.spans = append(l.spans, dumpSpan{node: node, epoch: epoch, start: time.Now()})
	l.unlock()
	select {
	case l.started <- struct{}{}:
	default:
	}
	return func() {
		l.lock()
		l.live--
		l.spans[i].end = time.Now()
		l.unlock()
	}
}

func (l *dumpLog) peak() int {
	l.lock()
	defer l.unlock()
	return l.maxLive
}

func (l *dumpLog) count() int {
	l.lock()
	defer l.unlock()
	return len(l.spans)
}

func (l *dumpLog) overlaps() bool {
	l.lock()
	defer l.unlock()
	now := time.Now()
	for i := range l.spans {
		a0, a1 := l.spans[i].start, l.spans[i].end
		if a1.IsZero() {
			a1 = now
		}
		for j := i + 1; j < len(l.spans); j++ {
			b0, b1 := l.spans[j].start, l.spans[j].end
			if b1.IsZero() {
				b1 = now
			}
			if a0.Before(b1) && b0.Before(a1) {
				return true
			}
		}
	}
	return false
}

// nodeRunner is one scheduler's dump. fail, when set, ends the run at once.
type nodeRunner struct {
	node string
	log  *dumpLog
	fail error
}

func (r *nodeRunner) Run(ctx context.Context, task Task) error {
	end := r.log.begin(r.node, task.Epoch)
	defer end()
	if r.fail != nil {
		return r.fail
	}
	<-ctx.Done()
	return context.Canceled
}

// fencedAppendRunner is an append still in flight when grace cancels the
// dump. The local fence refuses that append (Lose already ran), and that
// error is what the scheduler sees, not the cancel (#454).
type fencedAppendRunner struct {
	node string
	log  *dumpLog
}

func (r *fencedAppendRunner) Run(ctx context.Context, task Task) error {
	end := r.log.begin(r.node, task.Epoch)
	defer end()
	<-ctx.Done()
	return NewLeaseHandoff(fmt.Errorf("%w: append", ErrWriteFenced))
}

// p29World is three schedulers on one lease table and one task table.
type p29World struct {
	t      *testing.T
	leases *MemoryLease
	table  *resumeTable
	hs     *leaseHealthStore
	log    *dumpLog
	gate   *metaGate
	freeze *freezableLease
	nodes  map[string]*Scheduler
}

func newP29World(t *testing.T, ttl, renew, grace time.Duration, withGate, withFreeze bool) *p29World {
	t.Helper()
	leases := NewMemoryLease()
	table := &resumeTable{fencedMemoryStore: &fencedMemoryStore{lockedFakeStore: &lockedFakeStore{fakeStore: newFakeStore()}, leases: leases}}
	w := &p29World{
		t: t, leases: leases, table: table, hs: newLeaseHealthStore(leases), log: newDumpLog(),
		nodes: map[string]*Scheduler{},
	}
	var ownerLease LeaseManager = leases
	ownerStore := TaskStore(table)
	if withGate {
		w.gate = &metaGate{}
		ownerLease = &ownerGatedLease{MemoryLease: leases, g: w.gate}
		ownerStore = &ownerGatedStore{resumeTable: table, g: w.gate}
	}
	if withFreeze {
		w.freeze = &freezableLease{MemoryLease: leases}
		ownerLease = w.freeze
	}
	timeouts := InternalCallTimeouts{Read: 40 * time.Millisecond, Write: 40 * time.Millisecond, Lease: 25 * time.Millisecond, Upload: 40 * time.Millisecond}
	for _, id := range []string{"n1", "n2", "n3"} {
		store := TaskStore(table)
		lease := LeaseManager(leases)
		if id == "n1" {
			store, lease = ownerStore, ownerLease
		}
		runner := &nodeRunner{node: id, log: w.log}
		s := NewScheduler(
			WithStore(store),
			WithFindingStore(w.hs),
			WithClusterLeaseManager(lease),
			WithClusterWorkerID(id),
			WithClusterLease(ttl, renew, grace),
			WithLeaseClock(NewLeaseClock()),
			WithInternalCallTimeouts(timeouts),
		)
		s.SetRunner(runner)
		w.nodes[id] = s
	}
	return w
}

func (w *p29World) owner() *Scheduler { return w.nodes["n1"] }

func (w *p29World) runner(id string) *nodeRunner {
	return w.nodes[id].runner.(*nodeRunner)
}

func (w *p29World) start() (string, int64) {
	w.t.Helper()
	s := w.owner()
	task := ownedIdleTask(w.t, s, "", 0, StateStopped)
	if err := s.startTask(task.ID, taskStart{acquireLease: true}); err != nil {
		w.t.Fatalf("start: %v", err)
	}
	select {
	case <-w.log.started:
	case <-time.After(2 * time.Second):
		w.t.Fatal("dump did not start")
	}
	waitTaskState(w.t, s, task.ID, 2*time.Second, StateRunning)
	got, err := w.table.GetTask(context.Background(), task.ID)
	if err != nil {
		w.t.Fatal(err)
	}
	if got.Epoch <= 0 || got.DesiredRun != TaskDesiredRun {
		w.t.Fatalf("started row epoch %d desired %s", got.Epoch, got.DesiredRun)
	}
	return task.ID, got.Epoch
}

func (w *p29World) claim() int {
	w.t.Helper()
	n := 0
	for _, id := range []string{"n1", "n2", "n3"} {
		c, err := w.nodes[id].ClaimRunnableTasks()
		if err != nil {
			w.t.Fatalf("%s claim: %v", id, err)
		}
		n += c
	}
	return n
}

func (w *p29World) openRuns(id string) int {
	n := 0
	for _, s := range w.nodes {
		s.mu.Lock()
		done, ok := s.runs[id]
		s.mu.Unlock()
		if ok && !isClosed(done) {
			n++
		}
	}
	return n
}

func (w *p29World) leaseFree(id string) bool {
	w.leases.mu.Lock()
	row, ok := w.leases.rows[id]
	w.leases.mu.Unlock()
	if !ok || row.owner == "" {
		return true
	}
	now := time.Now()
	if w.leases.now != nil {
		now = w.leases.now()
	}
	return !row.expire.After(now)
}

// waitResumed fails immediately once the row has been stored as an operator
// stop, and otherwise waits until one node is RUNNING at a newer epoch.
func (w *p29World) waitResumed(id string, epochBefore int64) Task {
	w.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last Task
	for {
		got, err := w.table.GetTask(context.Background(), id)
		if err == nil {
			last = got
		}
		if err == nil && got.DesiredRun == TaskDesiredRun && got.State == StateRunning && got.Epoch > epochBefore && w.openRuns(id) == 1 && !w.log.overlaps() && w.log.peak() == 1 {
			return got
		}
		if err == nil && got.State == StateStopped && got.DesiredRun == TaskDesiredStop {
			w.t.Fatalf("row %s desired %s owner %q epoch %d; want desired RUN, one node RUNNING, epoch > %d", got.State, got.DesiredRun, got.OwnerWorkerID, got.Epoch, epochBefore)
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("not resumed: state %s desired %s owner %q epoch %d runs %d peak %d overlap %v; want epoch > %d", last.State, last.DesiredRun, last.OwnerWorkerID, last.Epoch, w.openRuns(id), w.log.peak(), w.log.overlaps(), epochBefore)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (w *p29World) outagePastGrace(id string) {
	w.t.Helper()
	w.gate.pause()
	waitRunClosed(w.t, w.owner(), id, "grace exceeded")
	time.Sleep(60 * time.Millisecond) // the release attempt must time out while meta is down
	w.gate.resume()
	w.owner().republishUnpublished()
}

func TestMetaOutagePastGraceResumesOnNewEpoch(t *testing.T) {
	w := newP29World(t, 2*time.Second, 15*time.Millisecond, 80*time.Millisecond, true, false)
	id, epoch := w.start()
	w.outagePastGrace(id)
	w.claim()
	got := w.waitResumed(id, epoch)
	if w.openRuns(id) != 1 {
		t.Fatalf("open runs %d, want 1", w.openRuns(id))
	}
	if got.OwnerWorkerID == "" {
		t.Fatalf("resumed row has no owner")
	}
}

func TestFencedAppendAtGraceHandoffResumesOnNewEpoch(t *testing.T) {
	w := newP29World(t, 2*time.Second, 15*time.Millisecond, 80*time.Millisecond, true, false)
	w.owner().SetRunner(&fencedAppendRunner{node: "n1", log: w.log})
	id, epoch := w.start()
	w.gate.pause()
	waitRunClosed(t, w.owner(), id, "fenced append at grace")
	time.Sleep(60 * time.Millisecond) // the release attempt must time out while meta is down
	w.gate.resume()
	// A persist that was in flight fails after the handoff dropped the
	// unpublished mark, and the sink republishes whatever memory holds.
	w.owner().mu.Lock()
	w.owner().unpublished[id] = struct{}{}
	w.owner().mu.Unlock()
	w.owner().republishUnpublished()

	w.owner().mu.Lock()
	mem := w.owner().tasks[id]
	w.owner().mu.Unlock()
	if mem.DesiredRun != TaskDesiredRun || (mem.State != StateLeaseDegraded && mem.State != StateRetryBackoff) {
		t.Fatalf("memory %s desired %s owner %q, want LEASE_DEGRADED or RETRY_BACKOFF desired RUN", mem.State, mem.DesiredRun, mem.OwnerWorkerID)
	}
	got, err := w.table.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == StateStopped || got.DesiredRun != TaskDesiredRun {
		t.Fatalf("published %s desired %s owner %q; a fenced append with no new owner must not store STOPPED", got.State, got.DesiredRun, got.OwnerWorkerID)
	}
	w.claim()
	resumed := w.waitResumed(id, epoch)
	if resumed.Epoch <= epoch || w.openRuns(id) != 1 || w.log.overlaps() || w.log.peak() != 1 {
		t.Fatalf("resumed epoch %d runs %d peak %d overlap %v, want epoch > %d and one dump", resumed.Epoch, w.openRuns(id), w.log.peak(), w.log.overlaps(), epoch)
	}
}

func TestStoppedDesiredRunWithNoOwnerIsClaimed(t *testing.T) {
	w := newP29World(t, time.Minute, time.Hour, time.Minute, false, false)
	s := w.owner()
	task := ownedIdleTask(t, s, "", 0, StateStopped)
	s.mu.Lock()
	item := s.tasks[task.ID]
	item.SpecRevision = 1
	item.AppliedSpecRevision = 1
	item.State = StateStopped
	item.DesiredRun = TaskDesiredRun
	item.OwnerWorkerID = ""
	item.Epoch = 0
	s.tasks[task.ID] = item
	if err := s.persistTaskLocked(item); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.mu.Unlock()
	w.leases.mu.Lock()
	w.leases.rows[task.ID] = memoryLeaseRow{owner: "", epoch: 1, expire: time.Now().Add(-time.Second)}
	w.leases.mu.Unlock()

	w.claim()
	got := w.waitResumed(task.ID, 1)
	if got.Epoch != 2 || got.DesiredRun != TaskDesiredRun || w.openRuns(task.ID) != 1 || w.log.overlaps() {
		t.Fatalf("resumed %s desired %s epoch %d runs %d overlap %v, want RUNNING desired RUN epoch 2 and one dump", got.State, got.DesiredRun, got.Epoch, w.openRuns(task.ID), w.log.overlaps())
	}
}

func TestMetaOutageShorterThanGraceKeepsDump(t *testing.T) {
	w := newP29World(t, 2*time.Second, 15*time.Millisecond, 400*time.Millisecond, true, false)
	id, epoch := w.start()
	w.gate.pause()
	time.Sleep(50 * time.Millisecond)
	w.gate.resume()
	waitTaskState(t, w.owner(), id, 2*time.Second, StateRunning)
	time.Sleep(120 * time.Millisecond)
	got, err := w.table.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == StateStopped || got.State == StateStopping || got.DesiredRun != TaskDesiredRun || got.Epoch != epoch {
		t.Fatalf("row %s desired %s epoch %d, want RUNNING desired RUN epoch %d", got.State, got.DesiredRun, got.Epoch, epoch)
	}
	if w.openRuns(id) != 1 || w.log.count() != 1 || w.log.peak() != 1 {
		t.Fatalf("runs %d spans %d peak %d, want the same dump", w.openRuns(id), w.log.count(), w.log.peak())
	}
}

func TestMetaSplitBrainDoesNotDoubleDump(t *testing.T) {
	w := newP29World(t, 2*time.Second, 15*time.Millisecond, 80*time.Millisecond, true, false)
	id, epoch := w.start()
	w.gate.pause()
	waitRunClosed(t, w.owner(), id, "owner grace")
	time.Sleep(60 * time.Millisecond)
	if n, err := w.nodes["n2"].ClaimExpiredTasks(); err != nil || n != 0 {
		t.Fatalf("peer ClaimExpiredTasks = %d, %v while the owner lease is still held", n, err)
	}
	if w.openRuns(id) != 0 || w.log.peak() != 1 || w.log.overlaps() {
		t.Fatalf("runs %d peak %d overlap %v during the split", w.openRuns(id), w.log.peak(), w.log.overlaps())
	}
	w.gate.resume()
	w.owner().republishUnpublished()
	w.claim()
	w.waitResumed(id, epoch)
}

func TestNodeFreezeShorterThanTTLKeepsDump(t *testing.T) {
	w := newP29World(t, 2*time.Second, 15*time.Millisecond, 80*time.Millisecond, false, true)
	id, epoch := w.start()
	w.freeze.freeze()
	time.Sleep(120 * time.Millisecond)
	if w.openRuns(id) != 1 {
		t.Fatal("SIGSTOP closed the dump")
	}
	got, err := w.table.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == StateStopped || got.State == StateStopping || got.Epoch != epoch {
		t.Fatalf("while frozen: %s epoch %d, want the same dump at epoch %d", got.State, got.Epoch, epoch)
	}
	w.freeze.thaw()
	time.Sleep(80 * time.Millisecond)
	got, err = w.table.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == StateStopped || got.DesiredRun != TaskDesiredRun || got.Epoch != epoch || w.openRuns(id) != 1 || w.log.count() != 1 {
		t.Fatalf("after thaw: %s desired %s epoch %d runs %d spans %d", got.State, got.DesiredRun, got.Epoch, w.openRuns(id), w.log.count())
	}
}

func TestOperatorStopIsNotClaimed(t *testing.T) {
	w := newP29World(t, time.Minute, time.Hour, time.Minute, false, false)
	id, _ := w.start()
	if err := w.owner().StopTask(id); err != nil {
		t.Fatal(err)
	}
	waitStoredState(t, w.table, id, StateStopped)
	before := w.log.count()
	w.claim()
	time.Sleep(40 * time.Millisecond)
	got, err := w.table.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateStopped || got.DesiredRun != TaskDesiredStop || w.openRuns(id) != 0 || w.log.count() != before {
		t.Fatalf("after claim: %s desired %s runs %d spans %d (was %d)", got.State, got.DesiredRun, w.openRuns(id), w.log.count(), before)
	}
}

func TestFailedTaskIsNotClaimed(t *testing.T) {
	w := newP29World(t, time.Minute, time.Hour, time.Minute, false, false)
	w.runner("n1").fail = NewPermanentError(CodeSegmentNotOnWorker, foreignSegmentError)
	s := w.owner()
	task := ownedIdleTask(t, s, "", 0, StateStopped)
	if err := s.startTask(task.ID, taskStart{acquireLease: true}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitTaskState(t, s, task.ID, 2*time.Second, StateFailed)
	before := w.log.count()
	w.claim()
	time.Sleep(40 * time.Millisecond)
	got, err := w.table.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateFailed || got.DesiredRun != TaskDesiredStop || got.LastError == "" || w.log.count() != before || w.openRuns(task.ID) != 0 {
		t.Fatalf("after claim: %s desired %s err %q spans %d runs %d", got.State, got.DesiredRun, got.LastError, w.log.count(), w.openRuns(task.ID))
	}
}

func TestUnclaimedAfterGraceOpensNoProgress(t *testing.T) {
	w := newP29World(t, 400*time.Millisecond, 15*time.Millisecond, 80*time.Millisecond, true, false)
	id, _ := w.start()
	w.outagePastGrace(id)
	deadline := time.Now().Add(2 * time.Second)
	for !w.leaseFree(id) {
		if time.Now().After(deadline) {
			t.Fatal("lease still held")
		}
		time.Sleep(5 * time.Millisecond)
	}
	peer := w.nodes["n2"]
	ev := NewEvaluator(peer, EvaluatorConfig{
		Mode: EvaluatorOn, Store: w.hs, Cluster: true, NodeID: "n2", Instance: "n2-i",
		Sweep: 30 * time.Millisecond, NoProgressAfter: time.Hour,
	})
	ctx := context.Background()
	if err := ev.SweepNow(ctx); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := ev.SweepNow(ctx); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	open, err := w.hs.ListOpenFindings(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found Finding
	for _, f := range open {
		if f.TaskID == id && f.Kind == FindingKind(health.TaskNoProgress) {
			found = f
		}
	}
	if found.Kind == "" || found.Severity != FindingCritical {
		t.Fatalf("open %+v, want CRITICAL TASK_NO_PROGRESS", open)
	}
	sum := peer.HealthSummary(ctx)
	if sum.Status != HealthDegraded {
		t.Fatalf("health %+v, want degraded", sum)
	}
	named := false
	for _, r := range sum.Reasons {
		if len(r) >= len("TASK_NO_PROGRESS") && r[:len("TASK_NO_PROGRESS")] == "TASK_NO_PROGRESS" {
			named = true
		}
	}
	if !named {
		t.Fatalf("health reasons %v, want TASK_NO_PROGRESS", sum.Reasons)
	}

	w.claim()
	w.waitResumed(id, 0)
	ownerEv := NewEvaluator(w.owner(), EvaluatorConfig{
		Mode: EvaluatorOn, Store: w.hs, Cluster: true, NodeID: "n1", Instance: "n1-i",
		Sweep: 30 * time.Millisecond, NoProgressAfter: time.Hour,
	})
	if err := ownerEv.SweepNow(ctx); err != nil {
		t.Fatalf("owner sweep: %v", err)
	}
	if err := ev.SweepNow(ctx); err != nil {
		t.Fatalf("peer sweep after resume: %v", err)
	}
	open, err = w.hs.ListOpenFindings(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range open {
		if f.TaskID == id && f.Kind == FindingKind(health.TaskNoProgress) {
			t.Fatalf("TASK_NO_PROGRESS still open after resume: %+v", f)
		}
	}
	if sum = peer.HealthSummary(ctx); sum.Status != HealthOK {
		t.Fatalf("health after resume %+v, want ok", sum)
	}
}
