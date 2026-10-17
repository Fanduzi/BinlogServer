// Package tasks provides module-level functionality for tasks.
// input: one row per path that leaves a task FAILED, STOPPED with desired RUN, or held without progress, in cluster (two Schedulers sharing a task table, MemoryLease and a lease-fenced health store; the _health holder proxies unowned tasks) and standalone (MemoryLease, worker "standalone"); a deliberate operator Stop and a healthy advancing run as controls
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
}

func (r *matrixRunner) Run(ctx context.Context, task Task) error {
	select {
	case r.started <- struct{}{}:
	default:
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
		case <-tick.C:
			if r.report != nil {
				n++
				r.report(task, n)
			}
		}
	}
}

type matrixNode struct {
	name string
	s    *Scheduler
	e    *Evaluator
}

type matrixEnv struct {
	cluster bool
	leases  *MemoryLease
	table   *sharedTaskTable
	hs      *leaseHealthStore
	dataDir string
	nodes   []*matrixNode // nodes[0] drives the task
}

const matrixNoProgressAfter = 300 * time.Millisecond

func newMatrixEnv(t *testing.T, cluster bool, runner Runner) *matrixEnv {
	t.Helper()
	leases := NewMemoryLease()
	env := &matrixEnv{cluster: cluster, leases: leases, hs: newLeaseHealthStore(leases), dataDir: t.TempDir(),
		table: &sharedTaskTable{fencedMemoryStore: &fencedMemoryStore{lockedFakeStore: &lockedFakeStore{fakeStore: newFakeStore()}, leases: leases}}}
	names := []string{"standalone"}
	if cluster {
		names = []string{"worker-a", "worker-b"}
	}
	for i, name := range names {
		opts := []Option{
			WithStore(&nodeStore{sharedTaskTable: env.table, node: name}),
			WithFindingStore(env.hs),
			WithClusterLeaseManager(leases),
			WithClusterWorkerID(name),
			WithDataDir(env.dataDir),
			WithRetryBackoff(time.Millisecond, time.Millisecond),
		}
		if cluster {
			opts = append(opts, WithClusterLease(time.Minute, time.Hour, time.Minute), WithLeaseClock(NewLeaseClock()))
		}
		s := NewScheduler(opts...)
		if i == 0 && runner != nil {
			s.SetRunner(runner)
		}
		e := NewEvaluator(s, EvaluatorConfig{Mode: EvaluatorOn, Store: env.hs, Cluster: cluster, NodeID: name, Instance: name + "-i",
			Sweep: time.Minute, NoProgressAfter: matrixNoProgressAfter})
		env.nodes = append(env.nodes, &matrixNode{name: name, s: s, e: e})
	}
	return env
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
		_, _ = n.s.GetTask(id)
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
		waitStoredState(t, env.table, task.ID, StateFailed)
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
		{name: "permanent-error", cluster: true, single: true, kind: health.TaskFailed, state: StateFailed,
			runner: failWith(NewPermanentError(CodeStreamRegression, "event end pos 120 is behind stored pos 400 in mysql-bin.000003")), setup: startFailed},
		// §24.8: a takeover whose catalog path is outside this worker's data_dir.
		{name: "segment-not-on-worker", cluster: true, single: true, kind: health.TaskFailed, state: StateFailed,
			runner: failWith(NewPermanentError(CodeSegmentNotOnWorker, foreignSegmentError)), setup: startFailed},
		// A runner error outside the retry allowlist fails once.
		{name: "unclassified-error", cluster: true, single: true, kind: health.TaskFailed, state: StateFailed,
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
				env := newMatrixEnv(t, cluster, runner)
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
