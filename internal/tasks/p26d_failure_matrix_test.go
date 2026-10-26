// Package tasks provides module-level functionality for tasks.
// input: one row per path that leaves a task FAILED, STOPPED with desired RUN, or held without progress, in cluster (two Schedulers sharing a task table, MemoryLease and a lease-fenced health store; the _health holder proxies unowned tasks) and standalone (MemoryLease, worker "standalone"); a deliberate operator Stop, a healthy advancing run, and a relative data_dir taking over its own segment recorded at an absolute path as controls
// output: proof that every failure row opens its CRITICAL finding (TASK_FAILED or TASK_NO_PROGRESS), that Scheduler.HealthSummary (GET /api/health) is degraded and names it on every node, and that the stored state and last_error survive another round (the other node's remote-stop poll, the control loop, a second sweep); the controls open nothing CRITICAL and stay ok
// pos: ADR 0009 P2-6d failure-path matrix (docs/design/0009-phase2-health-evaluator.md §24.10); runs in the per-PR go test. RULE: a new code path that can leave a task FAILED, STOPPED or stalled gets a row here.
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/health"
)

// leaseHealthStore is also the meta finding store, as in production: the
// meta store serves both health writes and GET /api/findings, so
// HealthSummary reads the evaluator's rows from it.
func (s *leaseHealthStore) UpsertFinding(context.Context, Finding) error { return nil }

func (s *leaseHealthStore) ListFindings(ctx context.Context, taskID string, _ bool, limit int) ([]Finding, error) {
	all, err := s.ListOpenFindings(ctx, limit)
	var out []Finding
	for _, f := range all {
		if f.TaskID == taskID {
			out = append(out, f)
		}
	}
	return out, err
}

func (s *leaseHealthStore) ListOpenFindings(_ context.Context, _ int) ([]Finding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Finding
	for taskID, m := range s.open {
		for _, f := range m {
			out = append(out, Finding{TaskID: taskID, Kind: FindingKind(f.Kind), Subject: f.Subject, Severity: FindingSeverity(f.Severity), Detail: f.Detail, Count: 1})
		}
	}
	return out, nil
}

// matrixRunner is the runner of the node that drives the task.
type matrixRunner struct {
	started chan struct{}
	// fail, when set, is returned as soon as the run starts.
	fail error
	// report, when set, is called every 10ms while the run is open.
	report func(task Task, n uint32)
	// resolve, when set, runs first, as the replication runner's takeover
	// resolution does; its error ends the run.
	resolve func(task Task) error
	// exit, when set, ends the open run with the error sent on it.
	exit chan error
}

func (r *matrixRunner) Run(ctx context.Context, task Task) error {
	select {
	case r.started <- struct{}{}:
	default:
	}
	if r.resolve != nil {
		if err := r.resolve(task); err != nil {
			return err
		}
	}
	if r.fail != nil {
		return r.fail
	}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	var n uint32
	for {
		select {
		case <-ctx.Done():
			return context.Canceled
		case err := <-r.exit:
			return err
		case <-tick.C:
			if r.report != nil {
				n++
				r.report(task, n)
			}
		}
	}
}

type matrixNode struct {
	name  string
	s     *Scheduler
	e     *Evaluator
	lease *roLease
	// unaware: nothing on this node reads the task (a control plane with
	// no API call for it); reload skips it, only its evaluator may.
	unaware bool
}

type matrixEnv struct {
	cluster bool
	leases  *MemoryLease
	table   *sharedTaskTable
	hs      *leaseHealthStore
	dataDir string
	nodes   []*matrixNode // nodes[0] drives the task
	runnerB *matrixRunner
	// ro is meta read-only: task row, lease and health writes fail.
	ro atomic.Bool
}

const matrixNoProgressAfter = 300 * time.Millisecond

// matrixOpts shapes the cluster of one row (QA r3 rows, design §24.13-24.16).
type matrixOpts struct {
	// relDataDir configures data_dir as a relative path.
	relDataDir bool
	// controlPlane makes the second cluster node control-plane only: no
	// runner and no worker id, as app wires that role.
	controlPlane bool
	// runnerB is the second cluster node's runner (nil: none).
	runnerB Runner
}

func newMatrixEnv(t *testing.T, cluster bool, runner Runner, o matrixOpts) *matrixEnv {
	relDataDir := o.relDataDir
	t.Helper()
	leases := NewMemoryLease()
	dataDir := t.TempDir()
	if relDataDir {
		// data_dir configured as a relative path ("data"), resolved
		// against the process working directory.
		base := t.TempDir()
		if err := os.MkdirAll(filepath.Join(base, "data"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(base)
		dataDir = "data"
	}
	env := &matrixEnv{cluster: cluster, leases: leases, hs: newLeaseHealthStore(leases), dataDir: dataDir,
		table: &sharedTaskTable{fencedMemoryStore: &fencedMemoryStore{lockedFakeStore: &lockedFakeStore{fakeStore: newFakeStore()}, leases: leases}}}
	names := []string{"standalone"}
	if cluster {
		names = []string{"worker-a", "worker-b"}
		if o.controlPlane {
			names[1] = "cp-1"
		}
	}
	for i, name := range names {
		workerID := name
		if o.controlPlane && i == 1 {
			workerID = "" // app: a control-plane node resolves no worker id
		}
		var r Runner
		if i == 0 && runner != nil {
			r = runner
		}
		if i == 1 && o.runnerB != nil {
			r = o.runnerB
		}
		env.nodes = append(env.nodes, env.newNode(name, workerID, r))
	}
	return env
}

// newNode builds one process on the shared tables. A fresh node knows no
// task until it reads one: a control-plane node started (or restarted)
// after the task was created on a worker has none in memory (QA r3 (b)).
func (env *matrixEnv) newNode(name, workerID string, runner Runner) *matrixNode {
	hs := &roHealthStore{leaseHealthStore: env.hs, ro: &env.ro}
	lease := &roLease{freezableLease: &freezableLease{MemoryLease: env.leases}, ro: &env.ro}
	opts := []Option{
		WithStore(&roNodeStore{nodeStore: &nodeStore{sharedTaskTable: env.table, node: name}, ro: &env.ro}),
		WithFindingStore(hs),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID(workerID),
		WithDataDir(env.dataDir),
		WithRetryBackoff(time.Millisecond, time.Millisecond),
	}
	if env.cluster {
		opts = append(opts, WithClusterLease(time.Minute, time.Hour, time.Minute), WithLeaseClock(NewLeaseClock()))
	}
	s := NewScheduler(opts...)
	if runner != nil {
		s.SetRunner(runner)
	}
	e := NewEvaluator(s, EvaluatorConfig{Mode: EvaluatorOn, Store: hs, Cluster: env.cluster, NodeID: name, Instance: name + "-i",
		Sweep: time.Minute, NoProgressAfter: matrixNoProgressAfter, ControlPlaneOnly: workerID == "" && env.cluster})
	return &matrixNode{name: name, s: s, e: e, lease: lease}
}

// newTask creates a sourced task on the driving node and loads it everywhere.
func (env *matrixEnv) newTask(t *testing.T) Task {
	t.Helper()
	task := mustStartSourcedTaskNoStart(t, env.nodes[0].s)
	env.reload(task.ID)
	return task
}

func mustStartSourcedTaskNoStart(t *testing.T, s *Scheduler) Task {
	t.Helper()
	task, err := s.CreateTask("matrix", "matrix-key")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource: %v", err)
	}
	return task
}

func (env *matrixEnv) reload(id string) {
	for _, n := range env.nodes {
		if !n.unaware {
			_, _ = n.s.GetTask(id)
		}
	}
}

// writeRow stores row as another process left it and loads it everywhere.
func (env *matrixEnv) writeRow(t *testing.T, row Task) {
	t.Helper()
	if err := env.table.fakeStore.UpsertTask(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	env.reload(row.ID)
}

// again is the round that must not rewrite the state: every other node
// polls remote stops and converges, then every node sweeps (twice).
func (env *matrixEnv) again(t *testing.T, id string) {
	t.Helper()
	row, _ := env.table.GetTask(context.Background(), id)
	for _, n := range env.nodes[1:] {
		n.s.applyRemoteStops()
	}
	if row.State == StateFailed {
		// The control loop of every node must leave FAILED alone. (On a
		// STOPPED desired-RUN row a worker's control loop restarting the
		// task is the recovery, not a rewrite, so it is not run there.)
		for _, n := range env.nodes {
			_, _ = n.s.convergeTask(id)
		}
	}
	env.reload(id)
	env.sweep(t)
	env.sweep(t)
}

func (env *matrixEnv) sweep(t *testing.T) {
	t.Helper()
	for _, n := range env.nodes {
		if err := n.e.SweepNow(context.Background()); err != nil {
			t.Fatalf("%s sweep: %v", n.name, err)
		}
	}
}

func (env *matrixEnv) cleanup(id string) {
	for _, n := range env.nodes {
		n.s.mu.Lock()
		cancel := n.s.cancels[id]
		n.s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
	for _, n := range env.nodes {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			n.s.mu.Lock()
			done, open := n.s.runs[id]
			n.s.mu.Unlock()
			if !open || isClosed(done) {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func startAndWait(t *testing.T, s *Scheduler, r *matrixRunner, id string) {
	t.Helper()
	if err := s.StartTask(id); err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	select {
	case <-r.started:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not start")
	}
}

type matrixRow struct {
	name     string
	cluster  bool
	single   bool
	kind     health.Kind // "" = control: nothing CRITICAL, health ok
	detail   string
	state    State
	setup    func(t *testing.T, env *matrixEnv, r *matrixRunner) string // returns the task id
	runner   func(env *matrixEnv) *matrixRunner
	noRunner bool
	// relDataDir configures data_dir as a relative path.
	relDataDir bool
	// controlPlane / runnerB: see matrixOpts.
	controlPlane bool
	runnerB      func() *matrixRunner
}

func TestFailurePathMatrix(t *testing.T) {
	var progressTarget atomic.Pointer[Scheduler]
	reportProgress := func(task Task, n uint32) {
		if s := progressTarget.Load(); s != nil {
			s.ReportReplicationProgress(task.ID, time.Now(), "mysql-bin.000001", 4+n, false)
		}
	}
	failWith := func(err error) func(*matrixEnv) *matrixRunner {
		return func(*matrixEnv) *matrixRunner { return &matrixRunner{started: make(chan struct{}, 8), fail: err} }
	}
	startFailed := func(t *testing.T, env *matrixEnv, r *matrixRunner) string {
		task := env.newTask(t)
		startAndWait(t, env.nodes[0].s, r, task.ID)
		waitStoredState(t, env.table, task.ID, StateLeaseDegraded)
		env.cleanup(task.ID)
		env.reload(task.ID)
		return task.ID
	}
	startRunningTask := func(t *testing.T, env *matrixEnv, r *matrixRunner, task Task) string {
		progressTarget.Store(env.nodes[0].s)
		startAndWait(t, env.nodes[0].s, r, task.ID)
		waitStoredState(t, env.table, task.ID, StateRunning)
		env.reload(task.ID)
		time.Sleep(matrixNoProgressAfter + 200*time.Millisecond)
		return task.ID
	}
	startRunning := func(t *testing.T, env *matrixEnv, r *matrixRunner) string {
		return startRunningTask(t, env, r, env.newTask(t))
	}
	blocking := func(report bool) func(*matrixEnv) *matrixRunner {
		return func(env *matrixEnv) *matrixRunner {
			r := &matrixRunner{started: make(chan struct{}, 8)}
			if report {
				r.report = reportProgress
			}
			return r
		}
	}
	rows := []matrixRow{
		// markFailedLocked from a permanent runner error.
		// STREAM_REGRESSION is not on the terminal allowlist. It yields.
		{name: "permanent-error", cluster: true, single: true, kind: "", state: StateLeaseDegraded,
			runner: failWith(NewPermanentError(CodeStreamRegression, "event end pos 120 is behind stored pos 400 in mysql-bin.000003")), setup: startFailed},
		// §24.8: a takeover whose catalog path is outside this worker's data_dir.
		{name: "segment-not-on-worker", cluster: true, single: true, kind: "", state: StateLeaseDegraded,
			runner: failWith(NewPermanentError(CodeSegmentNotOnWorker, foreignSegmentError)), setup: startFailed},
		// A runner error outside the retry allowlist yields once. Desired stays RUN.
		{name: "unclassified-error", cluster: true, single: true, kind: "", state: StateLeaseDegraded,
			runner: failWith(errors.New("unexpected runner failure: boom")), setup: startFailed},
		// RUNNING with the lease held, and the run never reports progress.
		{name: "running-no-progress", cluster: true, single: true, kind: health.TaskNoProgress, state: StateRunning, detail: "has written nothing",
			runner: blocking(false), setup: startRunning},
		// #421 sc1 shape: the run advances, but not under this worker's data_dir.
		{name: "progress-outside-data-dir", cluster: true, single: true, kind: health.TaskNoProgress, state: StateRunning, detail: "not under this worker's data_dir",
			runner: blocking(true), setup: startRunning},
		// Desired RUN, dispatched STARTING with no owner; nobody claims it.
		{name: "unclaimed-starting", cluster: true, kind: health.TaskNoProgress, state: StateStarting, detail: "no worker is running", noRunner: true,
			setup: func(t *testing.T, env *matrixEnv, _ *matrixRunner) string {
				task := env.newTask(t)
				row, _ := env.table.GetTask(context.Background(), task.ID)
				row.State, row.DesiredRun, row.OwnerWorkerID, row.Epoch, row.SpecRevision = StateStarting, TaskDesiredRun, "", 0, row.SpecRevision+1
				row.UpdatedAt = time.Now().Add(-time.Hour)
				env.writeRow(t, row)
				return task.ID
			}},
		// Lease released, fail-safe wrote STOPPED with desired RUN; nobody restarts it.
		{name: "lease-expired-stopped", cluster: true, single: true, kind: health.TaskNoProgress, state: StateStopped, detail: "no worker is running", noRunner: true,
			setup: func(t *testing.T, env *matrixEnv, _ *matrixRunner) string {
				task := env.newTask(t)
				row, _ := env.table.GetTask(context.Background(), task.ID)
				row.State, row.DesiredRun, row.OwnerWorkerID, row.Epoch, row.SpecRevision = StateStopped, TaskDesiredRun, "", 0, row.SpecRevision+1
				row.LastError = "lease lost"
				row.UpdatedAt = time.Now().Add(-time.Hour)
				env.leases.mu.Lock()
				env.leases.rows[task.ID] = memoryLeaseRow{owner: "", epoch: 1, expire: time.Now().Add(-time.Minute)}
				env.leases.mu.Unlock()
				env.writeRow(t, row)
				return task.ID
			}},
		// Controls.
		{name: "operator-stop", cluster: true, single: true, state: StateStopped, runner: blocking(false),
			setup: func(t *testing.T, env *matrixEnv, r *matrixRunner) string {
				task := env.newTask(t)
				startAndWait(t, env.nodes[0].s, r, task.ID)
				waitStoredState(t, env.table, task.ID, StateRunning)
				if err := env.nodes[0].s.StopTask(task.ID); err != nil {
					t.Fatalf("StopTask: %v", err)
				}
				waitStoredState(t, env.table, task.ID, StateStopped)
				env.reload(task.ID)
				time.Sleep(matrixNoProgressAfter + 200*time.Millisecond)
				return task.ID
			}},
		{name: "healthy-running", cluster: true, single: true, state: StateRunning, runner: blocking(true),
			setup: func(t *testing.T, env *matrixEnv, r *matrixRunner) string {
				task := env.newTask(t)
				dir := filepath.Join(env.dataDir, task.ID)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "mysql-bin.000001.open.e1"), []byte{0xfe, 'b', 'i', 'n'}, 0o644); err != nil {
					t.Fatal(err)
				}
				return startRunningTask(t, env, r, task)
			}},
		// Relative data_dir owning its own segments: a takeover whose catalog
		// records the segment's absolute path is this worker's own segment,
		// not SEGMENT_NOT_ON_WORKER, and the task stays healthy.
		{name: "relative-data-dir-own-segments", cluster: true, single: true, state: StateRunning, runner: blocking(true), relDataDir: true,
			setup: func(t *testing.T, env *matrixEnv, r *matrixRunner) string {
				task := env.newTask(t)
				dir := filepath.Join(env.dataDir, task.ID)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				local := filepath.Join(dir, "mysql-bin.000001.open.e1")
				if err := os.WriteFile(local, []byte{0xfe, 'b', 'i', 'n'}, 0o644); err != nil {
					t.Fatal(err)
				}
				abs, err := filepath.Abs(local)
				if err != nil {
					t.Fatal(err)
				}
				r.resolve = func(task Task) error {
					took := task
					took.Epoch = 2
					cp := binlog.Checkpoint{File: "mysql-bin.000001", Pos: 154}
					d := ResolveTakeover(env.dataDir, took, cp, true, []BinlogFile{{
						TaskID: task.ID, FileName: "mysql-bin.000001", FilePath: abs,
						Epoch: 1, State: "OPEN", StartPos: 154, EndPos: 154, UploadState: "LOCAL_ONLY",
					}})
					if d.Missing != "" {
						return NewPermanentError(CodeSegmentNotOnWorker, "segment "+d.Missing+" is not on this worker")
					}
					return nil
				}
				return startRunningTask(t, env, r, task)
			}},
		// QA r3 (b), §24.13: the only worker dies (kill -9: no release, no
		// write, renew stops), only a control-plane node is left. Its lease
		// expires; nobody claims it. The row still names the dead owner.
		{name: "dead-worker-control-plane-only", cluster: true, kind: health.TaskNoProgress, state: StateRunning, detail: "no worker is running",
			runner: blocking(true), controlPlane: true,
			setup: func(t *testing.T, env *matrixEnv, r *matrixRunner) string {
				id := startRunning(t, env, r)
				a := env.nodes[0]
				a.lease.freeze() // the dead process renews nothing
				t.Cleanup(a.lease.thaw)
				progressTarget.Store(nil)
				// The dead process evaluates nothing; the control plane
				// restarted since (as in QA r3 (b), it never ran the task),
				// so the task is in no node's memory.
				cp := env.newNode("cp-2", "", nil)
				cp.unaware = true
				env.nodes = []*matrixNode{cp}
				env.expireLease(id)
				env.sweep(t) // the control plane sees the expired lease
				time.Sleep(matrixNoProgressAfter + 200*time.Millisecond)
				t.Cleanup(func() {
					a.lease.thaw()
					a.s.mu.Lock()
					cancel := a.s.cancels[id]
					a.s.mu.Unlock()
					if cancel != nil {
						cancel()
					}
				})
				return id
			}},
		// QA r3 (d), #425, §24.14: meta read-only. Every node's health is
		// degraded while it lasts; the owner's row write fails and its run
		// yields the lease; after recovery the other worker claims epoch 2,
		// the old owner republishes and the new owner polls remote stops:
		// the task keeps running, never a silent STOPPED.
		{name: "meta-read-only", cluster: true, state: StateRunning, runner: blocking(true),
			runnerB: func() *matrixRunner { return &matrixRunner{started: make(chan struct{}, 8)} },
			setup: func(t *testing.T, env *matrixEnv, r *matrixRunner) string {
				r.exit = make(chan error, 2)
				id := startRunning(t, env, r)
				a, b := env.nodes[0], env.nodes[1]
				env.ro.Store(true)
				env.sweepAll()
				assertHealth(t, env.nodes, HealthDegraded, "during meta read-only")
				// The owner's write fails (RETRY_BACKOFF not stored), then
				// its run yields: the local lease clock refused the epoch.
				r.exit <- errors.New("save checkpoint: " + errMetaReadOnly.Error())
				select {
				case <-r.started:
				case <-time.After(2 * time.Second):
					t.Fatal("retry did not start")
				}
				r.exit <- NewLeaseHandoff(errors.New("lease/epoch mismatch: WRITE_FENCED: lease epoch no longer owns the task: run start"))
				waitRunClosed(t, a.s, id, "owner run after the handoff")
				env.expireLease(id)
				a.s.republishUnpublished() // still read-only: fails
				env.ro.Store(false)
				// The other worker claims the expired lease and runs epoch 2.
				if _, err := b.s.ClaimExpiredTasks(); err != nil {
					t.Fatalf("ClaimExpiredTasks: %v", err)
				}
				select {
				case <-env.runnerB.started:
				case <-time.After(2 * time.Second):
					t.Fatal("worker-b run did not start")
				}
				row := waitStoredState(t, env.table, id, StateRunning)
				if row.OwnerWorkerID != b.name || row.Epoch != 2 {
					t.Fatalf("claim: row %s/%s/%d, want RUNNING %s/2", row.State, row.OwnerWorkerID, row.Epoch, b.name)
				}
				// The old owner's sink tick, then the new owner's remote-stop poll.
				a.s.republishUnpublished()
				b.s.applyRemoteStops()
				time.Sleep(100 * time.Millisecond)
				row, _ = env.table.GetTask(context.Background(), id)
				if row.State != StateRunning || row.OwnerWorkerID != b.name || row.DesiredRun != TaskDesiredRun {
					t.Fatalf("after recovery: row %s/%s/%d desired %s, want RUNNING %s desired RUN (silent stop, #425)",
						row.State, row.OwnerWorkerID, row.Epoch, row.DesiredRun, b.name)
				}
				b.s.mu.Lock()
				done, open := b.s.runs[id]
				b.s.mu.Unlock()
				if !open || isClosed(done) {
					t.Fatal("worker-b run stopped after recovery (silent stop, #425)")
				}
				env.reload(id)
				env.sweepAll()
				return id
			}},
		// QA r3 (c), §24.15: the owner cannot open the dump (the old dump
		// connection cannot be KILLed) and retries with no run open; each
		// retry rewrites the row, so updated_at never ages.
		{name: "retrying-owner-no-run", cluster: true, single: true, kind: health.TaskNoProgress, state: StateRetryBackoff, runner: blocking(false),
			setup: func(t *testing.T, env *matrixEnv, r *matrixRunner) string {
				task := env.newTask(t)
				row, _ := env.table.GetTask(context.Background(), task.ID)
				row.PendingDumpCleanup = &DumpCleanup{ConnectionID: 19, Host: "127.0.0.1", Port: 3306, Held: true, Epoch: 0}
				env.writeRow(t, row)
				a := env.nodes[0]
				a.s.mu.Lock()
				a.s.dumpKiller = func(SourceConfig, uint32) error {
					return errors.New("dial tcp 127.0.0.1:3306: connect: connection refused")
				}
				a.s.mu.Unlock()
				if err := a.s.StartTask(task.ID); err != nil {
					t.Fatalf("StartTask: %v", err)
				}
				deadline := time.Now().Add(matrixNoProgressAfter + 250*time.Millisecond)
				for time.Now().Before(deadline) {
					_, _ = a.s.convergeTask(task.ID) // the claim loop retrying
					time.Sleep(40 * time.Millisecond)
				}
				waitStoredState(t, env.table, task.ID, StateRetryBackoff)
				env.reload(task.ID)
				return task.ID
			}},
	}
	for _, row := range rows {
		for _, cluster := range []bool{true, false} {
			if (cluster && !row.cluster) || (!cluster && !row.single) {
				continue
			}
			mode := "standalone"
			if cluster {
				mode = "cluster"
			}
			row := row
			t.Run(row.name+"/"+mode, func(t *testing.T) {
				var r *matrixRunner
				var runner Runner
				if row.runner != nil {
					r = row.runner(nil)
					runner = r
				} else if !row.noRunner {
					r = &matrixRunner{started: make(chan struct{}, 8)}
					runner = r
				}
				if row.noRunner && !cluster {
					// standalone always has a runner; it just has no run open.
					r = &matrixRunner{started: make(chan struct{}, 8)}
					runner = r
				}
				o := matrixOpts{relDataDir: row.relDataDir, controlPlane: row.controlPlane}
				var rb *matrixRunner
				if row.runnerB != nil {
					rb = row.runnerB()
					o.runnerB = rb
				}
				env := newMatrixEnv(t, cluster, runner, o)
				env.runnerB = rb
				id := row.setup(t, env, r)
				defer env.cleanup(id)
				before, err := env.table.GetTask(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
				env.sweep(t)
				env.again(t, id)

				// 3. the state is not silently rewritten.
				after, _ := env.table.GetTask(context.Background(), id)
				if after.State != row.state || after.State != before.State || after.LastError != before.LastError || after.DesiredRun != before.DesiredRun {
					t.Fatalf("state rewritten: before %s/%s %q, after %s/%s %q (want %s)",
						before.State, before.DesiredRun, before.LastError, after.State, after.DesiredRun, after.LastError, row.state)
				}
				// 1. the finding is open and CRITICAL.
				_, open, _ := env.hs.snapshot(id)
				var critical []health.Finding
				var found *health.Finding
				for _, f := range open {
					if f.Severity == health.Critical {
						critical = append(critical, f)
					}
					if f.Kind == row.kind {
						f := f
						found = &f
					}
				}
				if row.kind == "" {
					if len(critical) > 0 {
						t.Fatalf("control opened CRITICAL findings: %+v", critical)
					}
				} else {
					if found == nil || found.Severity != health.Critical {
						t.Fatalf("open findings %+v, want CRITICAL %s", open, row.kind)
					}
					if row.detail != "" && !strings.Contains(found.Detail, row.detail) {
						t.Fatalf("%s detail %q, want it to contain %q", row.kind, found.Detail, row.detail)
					}
				}
				// 2. GET /api/health on every node.
				for _, n := range env.nodes {
					sum := n.s.HealthSummary(context.Background())
					if row.kind == "" {
						if sum.Status != HealthOK {
							t.Fatalf("%s /api/health %+v, want ok", n.name, sum)
						}
						continue
					}
					named := false
					for _, reason := range sum.Reasons {
						if strings.HasPrefix(reason, string(row.kind)) {
							named = true
						}
					}
					if sum.Status != HealthDegraded || !named {
						t.Fatalf("%s /api/health %+v, want degraded naming %s", n.name, sum, row.kind)
					}
				}
			})
		}
	}
}
