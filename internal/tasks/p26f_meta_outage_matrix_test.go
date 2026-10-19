// Package tasks provides module-level functionality for tasks.
// input: the failure-path matrix pieces (shared task table, MemoryLease, lease-fenced health store) with every metadata call gated: while the gate is closed a lease call, a health or finding store call and the MetaProbe ping hang until their ctx ends, as against a SIGSTOPped meta; real Evaluator.Run loops and finding sinks; meta_unreachable_after 300ms, ping every 20ms
// output: one row per meta outage shape (30s, 12s, sc5 12s-then-30s, and a 5s control, scaled to meta_unreachable_after 10s), in cluster (three nodes: the task owner, the _health holder, and a node that is neither) and standalone (one node with a running task): every node records its own META_UNREACHABLE with first_seen at the outage start and resolved at the first answer (cluster: the legacy task_findings row for the latest outage and one FINDING_RESOLVED _cluster event per outage; standalone: OPEN and CLOSE transitions whose fact_at are those times); the control records nothing
// pos: ADR 0009 P2-6d failure-path matrix, meta outage rows (docs/design/0009-phase2-health-evaluator.md §25.8, #430 QA r1 sc5); runs in the per-PR go test
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
)

// metaGate is the metadata server: closed, every call through it hangs
// until its ctx ends (a SIGSTOPped server accepts and never answers).
type metaGate struct {
	mu     sync.Mutex
	paused chan struct{}
}

func (g *metaGate) pause() {
	g.mu.Lock()
	g.paused = make(chan struct{})
	g.mu.Unlock()
}

func (g *metaGate) resume() {
	g.mu.Lock()
	if g.paused != nil {
		close(g.paused)
		g.paused = nil
	}
	g.mu.Unlock()
}

func (g *metaGate) wait(ctx context.Context) error {
	g.mu.Lock()
	ch := g.paused
	g.mu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// outageTaskStore is one node's task store with the MetaProbe.
type outageTaskStore struct {
	*nodeStore
	g *metaGate
}

func (s *outageTaskStore) PingMeta(ctx context.Context) error { return s.g.wait(ctx) }
func (s *outageTaskStore) MetaUnreachable(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

type outageLease struct {
	*MemoryLease
	g *metaGate
}

func (l *outageLease) Acquire(ctx context.Context, taskID, workerID string, ttl time.Duration) (int64, bool, error) {
	if err := l.g.wait(ctx); err != nil {
		return 0, false, err
	}
	return l.MemoryLease.Acquire(ctx, taskID, workerID, ttl)
}

func (l *outageLease) Renew(ctx context.Context, taskID, workerID string, epoch int64, now time.Time, ttl time.Duration) (bool, error) {
	if err := l.g.wait(ctx); err != nil {
		return false, err
	}
	return l.MemoryLease.Renew(ctx, taskID, workerID, epoch, now, ttl)
}

func (l *outageLease) Release(ctx context.Context, taskID, workerID string, epoch int64) (bool, error) {
	if err := l.g.wait(ctx); err != nil {
		return false, err
	}
	return l.MemoryLease.Release(ctx, taskID, workerID, epoch)
}

// outageRow is a META_UNREACHABLE task_findings row as meta keeps it.
type outageRow struct {
	firstSeen time.Time
	resolved  *time.Time
}

// outageTransition is one stored META_UNREACHABLE transition.
type outageTransition struct {
	subject string
	op      health.Op
	factAt  time.Time
}

// outageStore is meta's health and findings tables behind the gate; it
// keeps the META_UNREACHABLE rows (legacy upserts) and transitions.
type outageStore struct {
	*leaseHealthStore
	g     *metaGate
	mu    sync.Mutex
	rows  map[string]outageRow // subject -> _cluster META_UNREACHABLE row
	trans []outageTransition
}

func (s *outageStore) ApplyTransitions(ctx context.Context, taskID string, f health.Fence, ts []health.Transition, h health.TaskHealth) (healthstore.Applied, error) {
	if err := s.g.wait(ctx); err != nil {
		return healthstore.Applied{}, err
	}
	out, err := s.leaseHealthStore.ApplyTransitions(ctx, taskID, f, ts, h)
	if err == nil && taskID == ClusterFindingTask {
		s.mu.Lock()
		for _, t := range ts {
			if t.Kind == health.MetaUnreachable {
				s.trans = append(s.trans, outageTransition{subject: t.Subject, op: t.Op, factAt: t.FactAt})
			}
		}
		s.mu.Unlock()
	}
	return out, err
}

func (s *outageStore) ListOpenFindingsForTask(ctx context.Context, taskID string) ([]health.Finding, error) {
	if err := s.g.wait(ctx); err != nil {
		return nil, err
	}
	return s.leaseHealthStore.ListOpenFindingsForTask(ctx, taskID)
}

// UpsertFinding keeps the earlier first_seen while the row is open, takes
// the new one when a resolved row reopens, and resolved_at as written, as
// meta's upsertFindingSQL does (§25.8 R4).
func (s *outageStore) UpsertFinding(ctx context.Context, f Finding) error {
	if err := s.g.wait(ctx); err != nil {
		return err
	}
	if f.TaskID != ClusterFindingTask || f.Kind != FindingMetaUnreachable {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[f.Subject]
	if !ok || row.resolved != nil || f.FirstSeen.Before(row.firstSeen) {
		row.firstSeen = f.FirstSeen
	}
	row.resolved = f.ResolvedAt
	s.rows[f.Subject] = row
	return nil
}

func (s *outageStore) ListOpenFindings(ctx context.Context, limit int) ([]Finding, error) {
	if err := s.g.wait(ctx); err != nil {
		return nil, err
	}
	return s.leaseHealthStore.ListOpenFindings(ctx, limit)
}

func (s *outageStore) ListFindings(ctx context.Context, taskID string, includeResolved bool, limit int) ([]Finding, error) {
	if err := s.g.wait(ctx); err != nil {
		return nil, err
	}
	return s.leaseHealthStore.ListFindings(ctx, taskID, includeResolved, limit)
}

func (s *outageStore) row(subject string) (outageRow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[subject]
	return r, ok
}

func (s *outageStore) transitions(subject string) []outageTransition {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []outageTransition
	for _, t := range s.trans {
		if t.subject == subject {
			out = append(out, t)
		}
	}
	return out
}

// outageEvents is meta's task_events behind the gate; it keeps the
// _cluster events (the per-outage record in cluster mode, §25.8 R4).
type outageEvents struct {
	g    *metaGate
	mu   sync.Mutex
	list []TaskEvent
}

func (s *outageEvents) AppendEvent(ctx context.Context, ev TaskEvent) error {
	if err := s.g.wait(ctx); err != nil {
		return err
	}
	if ev.TaskID == ClusterFindingTask {
		s.mu.Lock()
		s.list = append(s.list, ev)
		s.mu.Unlock()
	}
	return nil
}

func (s *outageEvents) ListEvents(context.Context, string, int) ([]TaskEvent, error) { return nil, nil }

// reported is the FINDING_REPORTED META_UNREACHABLE events of subject: a
// write that lands only after the outage, often twice (a retried and a
// newer copy); the RESOLVED event carries the whole window instead.
func (s *outageEvents) reported(subject string) []TaskEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []TaskEvent
	for _, ev := range s.list {
		if ev.Type == "FINDING_REPORTED" && ev.Message == string(FindingMetaUnreachable)+" "+subject {
			out = append(out, ev)
		}
	}
	return out
}

// resolved is the FINDING_RESOLVED META_UNREACHABLE events of subject.
func (s *outageEvents) resolved(subject string) []TaskEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []TaskEvent
	for _, ev := range s.list {
		if ev.Type == "FINDING_RESOLVED" && ev.Message == string(FindingMetaUnreachable)+" "+subject {
			out = append(out, ev)
		}
	}
	return out
}

const (
	outageAfter    = 300 * time.Millisecond // meta_unreachable_after, scaled from 10s
	outagePing     = 20 * time.Millisecond  // ping interval and timeout, scaled from 1s
	outageSlackIn  = outagePing + 80*time.Millisecond
	outageSlackOut = outagePing + 150*time.Millisecond
)

// scaled maps an outage of real seconds onto meta_unreachable_after 10s.
func scaled(seconds float64) time.Duration {
	return time.Duration(seconds / 10 * float64(outageAfter))
}

type outageNode struct {
	name string
	role string
	s    *Scheduler
	e    *Evaluator
}

type outageEnv struct {
	g      *metaGate
	leases *MemoryLease
	table  *sharedTaskTable
	hs     *outageStore
	events *outageEvents
	nodes  []*outageNode
}

func newOutageEnv() *outageEnv {
	leases := NewMemoryLease()
	g := &metaGate{}
	return &outageEnv{g: g, leases: leases,
		table:  &sharedTaskTable{fencedMemoryStore: &fencedMemoryStore{lockedFakeStore: &lockedFakeStore{fakeStore: newFakeStore()}, leases: leases}},
		hs:     &outageStore{leaseHealthStore: newLeaseHealthStore(leases), g: g, rows: map[string]outageRow{}},
		events: &outageEvents{g: g}}
}

// start builds one process and runs its evaluator loop and finding sink.
func (env *outageEnv) start(t *testing.T, name, role string, cluster bool, runner Runner, dataDir string) *outageNode {
	t.Helper()
	opts := []Option{
		WithStore(&outageTaskStore{nodeStore: &nodeStore{sharedTaskTable: env.table, node: name}, g: env.g}),
		WithFindingStore(env.hs),
		WithEventStore(env.events),
		WithClusterLeaseManager(&outageLease{MemoryLease: env.leases, g: env.g}),
		WithClusterWorkerID(name),
		WithDataDir(dataDir),
		WithRetryBackoff(time.Millisecond, time.Millisecond),
	}
	if cluster {
		opts = append(opts, WithClusterLease(time.Minute, time.Hour, time.Minute), WithLeaseClock(NewLeaseClock()))
	}
	s := NewScheduler(opts...)
	if runner != nil {
		s.SetRunner(runner)
	}
	e := NewEvaluator(s, EvaluatorConfig{Mode: EvaluatorOn, Store: env.hs, Cluster: cluster, NodeID: name, Instance: name + "-i",
		Sweep: 50 * time.Millisecond, Debounce: 5 * time.Millisecond, LivenessTick: outagePing, NoProgressAfter: time.Hour,
		MetaUnreachableAfter: outageAfter, MetaPingInterval: outagePing, MetaPingTimeout: outagePing})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	s.StartFindingSink(ctx, 20*time.Millisecond)
	t.Cleanup(func() {
		env.g.resume()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("%s: evaluator loop did not stop", name)
		}
		s.WaitFindingSink(5 * time.Second)
	})
	n := &outageNode{name: name, role: role, s: s, e: e}
	env.nodes = append(env.nodes, n)
	return n
}

func outageWaitFor(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// setupCluster: worker-b holds _health, worker-a owns a running task,
// worker-c is neither (the n1 of sc5's 30s pause).
func (env *outageEnv) setupCluster(t *testing.T) {
	dataDir := t.TempDir()
	b := env.start(t, "worker-b", "_health holder", true, nil, dataDir)
	outageWaitFor(t, 2*time.Second, "worker-b to hold _health", b.e.healthHeld.Load)
	r := &matrixRunner{started: make(chan struct{}, 8)}
	a := env.start(t, "worker-a", "task owner", true, r, dataDir)
	env.start(t, "worker-c", "neither owner nor holder", true, nil, dataDir)
	task := mustStartSourcedTaskNoStart(t, a.s)
	startAndWait(t, a.s, r, task.ID)
	waitStoredState(t, env.table, task.ID, StateRunning)
	t.Cleanup(func() {
		a.s.mu.Lock()
		cancel := a.s.cancels[task.ID]
		a.s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	})
}

func (env *outageEnv) setupStandalone(t *testing.T) {
	r := &matrixRunner{started: make(chan struct{}, 8)}
	n := env.start(t, "standalone", "standalone owner", false, r, t.TempDir())
	task := mustStartSourcedTaskNoStart(t, n.s)
	startAndWait(t, n.s, r, task.ID)
	waitStoredState(t, env.table, task.ID, StateRunning)
	t.Cleanup(func() {
		n.s.mu.Lock()
		cancel := n.s.cancels[task.ID]
		n.s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	})
}

// within: at in [from, from+slack].
func outageWithin(at, from time.Time, slack time.Duration) bool {
	return !at.Before(from.Add(-5*time.Millisecond)) && !at.After(from.Add(slack))
}

func TestFailurePathMatrixMetaOutage(t *testing.T) {
	rows := []struct {
		name   string
		pauses []time.Duration
		record bool
	}{
		// sc5 second pause: every node, the task owner included, records it.
		{name: "meta-pause-30s", pauses: []time.Duration{scaled(30)}, record: true},
		// sc5 first pause: longer than meta_unreachable_after, so recorded.
		{name: "meta-pause-12s", pauses: []time.Duration{scaled(12)}, record: true},
		// sc5 as run: 12s, 25s apart, then 30s. Each outage keeps its own
		// record; the second does not swallow the first (§25.8 R4).
		{name: "meta-pause-12s-then-30s", pauses: []time.Duration{scaled(12), scaled(30)}, record: true},
		// Control: shorter than meta_unreachable_after, never recorded.
		{name: "meta-pause-5s", pauses: []time.Duration{scaled(5)}, record: false},
	}
	for _, row := range rows {
		for _, cluster := range []bool{true, false} {
			mode := "standalone"
			if cluster {
				mode = "cluster"
			}
			row := row
			t.Run(row.name+"/"+mode, func(t *testing.T) {
				env := newOutageEnv()
				if cluster {
					env.setupCluster(t)
				} else {
					env.setupStandalone(t)
				}
				time.Sleep(10 * outagePing) // healthy pings first

				var pauseAt, resumeAt []time.Time
				for i, d := range row.pauses {
					if i > 0 {
						time.Sleep(scaled(25))
					}
					pauseAt = append(pauseAt, time.Now())
					env.g.pause()
					time.Sleep(d)
					resumeAt = append(resumeAt, time.Now())
					env.g.resume()
				}
				last := len(row.pauses) - 1

				if !row.record {
					time.Sleep(outageAfter + 200*time.Millisecond)
					for _, n := range env.nodes {
						if r, ok := env.hs.row(n.name); ok {
							t.Errorf("%s (%s): a %v outage was recorded %+v, want nothing below meta_unreachable_after", n.name, n.role, row.pauses, r)
						}
						if ts := env.hs.transitions(n.name); len(ts) > 0 {
							t.Errorf("%s (%s): a %v outage left transitions %+v, want none", n.name, n.role, row.pauses, ts)
						}
						if evs := env.events.resolved(n.name); len(evs) > 0 {
							t.Errorf("%s (%s): a %v outage left events %+v, want none", n.name, n.role, row.pauses, evs)
						}
					}
					return
				}
				for _, n := range env.nodes {
					if cluster {
						// The row is the latest outage; each outage has its event.
						var r outageRow
						ok := false
						var evs []TaskEvent
						deadline := time.Now().Add(3 * time.Second)
						for time.Now().Before(deadline) {
							r, ok = env.hs.row(n.name)
							evs = env.events.resolved(n.name)
							if ok && r.resolved != nil && len(evs) >= len(row.pauses) {
								break
							}
							time.Sleep(5 * time.Millisecond)
						}
						if !ok || r.resolved == nil {
							t.Errorf("%s (%s): no resolved META_UNREACHABLE/%s row for a %v outage (row %+v, present %v)", n.name, n.role, n.name, row.pauses, r, ok)
						} else {
							if !outageWithin(r.firstSeen, pauseAt[last], outageSlackIn) {
								t.Errorf("%s (%s): first_seen %s, want the last outage start %s (+%s)", n.name, n.role, r.firstSeen.Format(time.StampMicro), pauseAt[last].Format(time.StampMicro), outageSlackIn)
							}
							if !outageWithin(*r.resolved, resumeAt[last], outageSlackOut) {
								t.Errorf("%s (%s): resolved_at %s, want the first answer after %s (+%s)", n.name, n.role, r.resolved.Format(time.StampMicro), resumeAt[last].Format(time.StampMicro), outageSlackOut)
							}
						}
						if rep := env.events.reported(n.name); len(rep) > 0 {
							t.Errorf("%s (%s): FINDING_REPORTED META_UNREACHABLE events %+v, want only one RESOLVED per outage", n.name, n.role, rep)
						}
						if len(evs) != len(row.pauses) {
							t.Errorf("%s (%s): %d FINDING_RESOLVED META_UNREACHABLE events %+v for %d outages, want one each", n.name, n.role, len(evs), evs, len(row.pauses))
							continue
						}
						for i, ev := range evs {
							if !outageWithin(ev.Time, resumeAt[i], outageSlackOut) {
								t.Errorf("%s (%s): outage %d resolved event at %s, want the first answer after %s (+%s)", n.name, n.role, i+1, ev.Time.Format(time.StampMicro), resumeAt[i].Format(time.StampMicro), outageSlackOut)
							}
						}
						continue
					}
					want := 2 * len(row.pauses)
					var ts []outageTransition
					deadline := time.Now().Add(3 * time.Second)
					for time.Now().Before(deadline) {
						if ts = env.hs.transitions(n.name); len(ts) >= want {
							break
						}
						time.Sleep(5 * time.Millisecond)
					}
					if len(ts) != want {
						t.Errorf("%s (%s): transitions %+v for a %v outage, want OPEN and CLOSE per outage", n.name, n.role, ts, row.pauses)
						continue
					}
					for i := range row.pauses {
						o, c := ts[2*i], ts[2*i+1]
						if o.op != health.OpOpen || c.op != health.OpClose {
							t.Errorf("%s (%s): outage %d transitions %s,%s, want OPEN,CLOSE", n.name, n.role, i+1, o.op, c.op)
							continue
						}
						if !outageWithin(o.factAt, pauseAt[i], outageSlackIn) {
							t.Errorf("%s (%s): outage %d OPEN fact_at %s, want its start %s (+%s)", n.name, n.role, i+1, o.factAt.Format(time.StampMicro), pauseAt[i].Format(time.StampMicro), outageSlackIn)
						}
						if !outageWithin(c.factAt, resumeAt[i], outageSlackOut) {
							t.Errorf("%s (%s): outage %d CLOSE fact_at %s, want the first answer after %s (+%s)", n.name, n.role, i+1, c.factAt.Format(time.StampMicro), resumeAt[i].Format(time.StampMicro), outageSlackOut)
						}
					}
				}
			})
		}
	}
}
