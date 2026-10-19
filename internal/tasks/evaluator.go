// Package tasks provides module-level functionality for tasks.
// input: dirty marks (SEAL, UPLOAD, TRANSITION, LEASE_*, CATALOG, STARTUP, SWEEP, REHASH, API, CONFIRM) for tasks this node is responsible for, the task directory and replay order, rehash results, and the task's open findings from a healthstore.Store
// output: Evaluator: a dirty set (marks merge per task), per-task serial evaluation on a worker pool with a 1s debounce, a sweep that marks every responsible task dirty (CATALOG when the directory fingerprint changed), Facts with chain.local from assurance.Chain.Check, health.Reconcile and HealthOf, and the transitions written under a health.Fence; feature flag off (nothing), shadow (compute, write nothing) and on (write; the legacy Report path stops writing the managed kinds while the store has migration 000009); P2-4: signal facts (task.state, signals, open keys, probes, stale signals) from signals.go, the pseudo task _cluster in standalone, and a LivenessTick that marks a task dirty when its source liveness clock crosses a threshold (LIVENESS); #385: standalone with no task store also evaluates, once, a task whose stored health names TASK_UNADOPTED after its directory left data_dir, with only task.state complete, so that row closes; NewEvaluator moves signal-kind failures raised before it existed (legacy book) into the signal book (adoptEarlySignals) and replays standalone task record signals (ADR 0010)
// note P2-5.1 (§20): chain.local includes the planner (I3_LEGACY_FAILBACK); cluster mode drops I6_MISSING_LOCAL; the Confirm keys a pass derives are kept for the next one, and a pending key queues a CONFIRM pass
// note P2-5.3 (§22): chainFacts passes Switches, reads checkpoint before order/disk, caches CheckBody only and appends CheckCheckpoint; Facts.Waivers from WaiverStore
// note P2-6c (§24): cluster responsibility and the pass's fence come from authorityFor (holder, or the _health holder for an unowned task); a proxy pass uses unownedFacts and a fenced proxy write is not latched
// pos: ADR 0009 phase 2 P2-3 (docs/design/0009-phase2-health-evaluator.md §5); the one place a managed finding is opened or closed
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"binlog_server/internal/assurance"
	"binlog_server/internal/binlog"
	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
)

// EvaluatorMode is the health.evaluator feature flag.
type EvaluatorMode string

const (
	// EvaluatorOff: no evaluation; the legacy Report/Resolve paths own every kind.
	EvaluatorOff EvaluatorMode = "off"
	// EvaluatorShadow: evaluate and count transitions, write nothing.
	EvaluatorShadow EvaluatorMode = "shadow"
	// EvaluatorOn: evaluate and write; managed kinds have one writer.
	EvaluatorOn EvaluatorMode = "on"
)

// ParseEvaluatorMode reads health.evaluator ("" is off).
func ParseEvaluatorMode(s string) (EvaluatorMode, error) {
	switch m := EvaluatorMode(strings.ToLower(strings.TrimSpace(s))); m {
	case "", EvaluatorOff:
		return EvaluatorOff, nil
	case EvaluatorShadow, EvaluatorOn:
		return m, nil
	}
	return EvaluatorOff, fmt.Errorf("health.evaluator must be off, shadow or on, got %q", s)
}

// EvalReason says why a task was marked dirty (design §5.2).
type EvalReason string

const (
	ReasonSeal          EvalReason = "SEAL"
	ReasonUpload        EvalReason = "UPLOAD"
	ReasonRetention     EvalReason = "RETENTION"
	ReasonTransition    EvalReason = "TRANSITION"
	ReasonLeaseAcquired EvalReason = "LEASE_ACQUIRED"
	ReasonLeaseLost     EvalReason = "LEASE_LOST"
	ReasonCatalog       EvalReason = "CATALOG"
	ReasonStartup       EvalReason = "STARTUP"
	ReasonSweep         EvalReason = "SWEEP"
	ReasonAPI           EvalReason = "API"
	ReasonRehash        EvalReason = "REHASH"
	// ReasonConfirm: a Confirm-kind instance was derived for the first
	// time; the next pass decides whether it opens (design 0009 §20.3).
	ReasonConfirm EvalReason = "CONFIRM"
)

// EvaluatorConfig configures NewEvaluator.
type EvaluatorConfig struct {
	Mode  EvaluatorMode
	Store healthstore.Store
	// Registry defaults to health.DefaultRegistry().
	Registry health.Registry
	// Sweep is the period that marks every responsible task dirty (default 60s).
	Sweep time.Duration
	// Debounce is the minimum gap between two passes of one task (default 1s).
	Debounce time.Duration
	// Workers evaluate tasks in parallel (default 4); one task is never
	// evaluated twice at once.
	Workers int
	// Cluster: the fence is the task lease (cluster mode); otherwise the
	// standalone fence with the persisted node id.
	Cluster bool
	// NodeID is the cluster worker id, or the standalone {data_dir}/.node_id;
	// a control-plane-only node uses cluster.worker_id or its persisted id
	// (design §24.13).
	NodeID string
	// ControlPlaneOnly: this node runs no tasks (cluster role control-plane).
	// It never evaluates as a task holder, only through _health (§24.13).
	ControlPlaneOnly bool
	// Instance is this process's id (standalone fence).
	Instance string
	// RehashCursor, when set, persists the rehasher's cursor (rehash_cursor).
	RehashCursor RehashCursorStore
	// UnreachableAfter is health.unreachable_after_sec (default 90s);
	// UnreachableAfterTask overrides it per task id.
	UnreachableAfter     time.Duration
	UnreachableAfterTask map[string]time.Duration
	// SignalStaleAfter is health.signal_stale_after (default 24h).
	SignalStaleAfter time.Duration
	// StalledAfter is the TASK_TRANSITION_STALLED threshold (default: the scheduler's).
	StalledAfter time.Duration
	// DumpStalledAfter is the DUMP_STALLED silence threshold (default 45s).
	DumpStalledAfter time.Duration
	// NoProgressAfter is health.no_progress_after_sec, the TASK_NO_PROGRESS
	// threshold (default 120s, design §24.9).
	NoProgressAfter time.Duration
	// MetaUnreachableAfter is how long metadata store failures last before
	// META_UNREACHABLE opens (default 10s, design §25 D2).
	MetaUnreachableAfter time.Duration
	// LivenessTick is how often the source liveness clocks are checked for
	// a threshold crossing (default 5s); a crossing marks the task dirty.
	LivenessTick time.Duration
	// UploadGrace is upload.grace_sec (0 = assurance.DefaultUploadGrace);
	// UploadGraceTask overrides it per task id (design 0009 §21.1).
	UploadGrace     time.Duration
	UploadGraceTask map[string]time.Duration
	// UploadLateCriticalAfter is health.upload_late_critical_after_sec
	// (0 = 24h); UploadLateCriticalAfterTask overrides it per task (§21.7).
	UploadLateCriticalAfter     time.Duration
	UploadLateCriticalAfterTask map[string]time.Duration
	// ObjectListInterval is health.object_list_interval_sec (default 300s);
	// ObjectListLimit is health.object_list_limit (default 10000) (§21.3).
	ObjectListInterval time.Duration
	ObjectListLimit    int
	Now                func() time.Time
}

// EvaluatorStats counts what the evaluator did.
type EvaluatorStats struct {
	Evaluations, Applied, Errors, Shadowed, Pending int64
}

// chainCacheEntry is the last CheckBody of a task directory: reused while the
// directory fingerprint (names, sizes, mtimes), replay order, rows and
// Switches key stay the same. I5 is never cached (design 0009 §22.4).
type chainCacheEntry struct {
	fp, orderKey string
	order        []string
	violations   []assurance.Violation // CheckBody only (no I5)
}

type rehashResult struct {
	stamp    binlog.FileStamp
	ok       bool
	evidence string
}

// Evaluator owns the managed findings of the tasks this node is responsible for.
type Evaluator struct {
	s    *Scheduler
	cfg  EvaluatorConfig
	reg  health.Registry
	wake chan struct{}

	mu      sync.Mutex
	dirty   map[string]map[EvalReason]time.Time
	running map[string]bool
	lastRun map[string]time.Time
	fenced  map[string]int64
	pending map[string][]health.Transition
	// unsaved (#395 QA P2): the open set a failed store write left behind,
	// per task. It is what GET /api/findings and /api/health serve until a
	// write lands, so a full data_dir cannot hide a finding.
	unsaved      map[string]unsavedHealth
	fingerprints map[string]string
	rehash       map[string]map[string]rehashResult
	chainCache   map[string]chainCacheEntry
	// confirmed: per task, the Confirm-kind keys the previous pass derived.
	confirmed map[string]map[health.Key]bool
	signals   map[string]map[health.SignalKey]*signalEntry
	// listings: the object listing cache per prefix (design 0009 §21.3);
	// objectConfig: the last object storage configuration seen (§21.5).
	listings     map[string]*objectListing
	listingGen   int64
	objectConfig string
	phase        map[string]string

	// schemaAbsent: the store has no 000009 (ErrSchemaAbsent on the last
	// write); the legacy Report path keeps writing the managed kinds.
	schemaAbsent atomic.Bool

	// healthHeld / healthEpoch: this node holds the _health lease and
	// evaluates unowned tasks (P2-6c, design §24.2).
	healthHeld  atomic.Bool
	healthEpoch atomic.Int64
	// leaseExpired (§24.13): tasks whose row names an owner whose lease
	// expired, with the time this node first saw it; refreshed by the
	// _health holder each sweep. leaseWriteErr: the last _health lease write
	// error and since when (§24.14: every node shows a meta write failure).
	leaseExpired    map[string]time.Time
	leaseWriteErr   error
	leaseWriteErrAt time.Time

	evaluations, applied, errs, shadowed atomic.Int64
}

// NewEvaluator builds the evaluator and attaches it to s, so the triggers
// in s mark tasks dirty and the legacy Report path knows who owns which kind.
func NewEvaluator(s *Scheduler, cfg EvaluatorConfig) *Evaluator {
	if cfg.Registry == nil {
		cfg.Registry = health.DefaultRegistry()
	}
	if cfg.Sweep <= 0 {
		cfg.Sweep = 60 * time.Second
	}
	if cfg.Debounce <= 0 {
		cfg.Debounce = time.Second
	}
	if cfg.LivenessTick <= 0 {
		cfg.LivenessTick = 5 * time.Second
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Mode == "" {
		cfg.Mode = EvaluatorOff
	}
	if cfg.ObjectListInterval <= 0 {
		cfg.ObjectListInterval = defaultObjectListInterval
	}
	if cfg.ObjectListLimit <= 0 {
		cfg.ObjectListLimit = defaultObjectListLimit
	}
	e := &Evaluator{s: s, cfg: cfg, reg: cfg.Registry, wake: make(chan struct{}, cfg.Workers),
		dirty: map[string]map[EvalReason]time.Time{}, running: map[string]bool{}, lastRun: map[string]time.Time{},
		fenced: map[string]int64{}, pending: map[string][]health.Transition{}, unsaved: map[string]unsavedHealth{}, fingerprints: map[string]string{},
		rehash: map[string]map[string]rehashResult{}, chainCache: map[string]chainCacheEntry{}, confirmed: map[string]map[health.Key]bool{}, listings: map[string]*objectListing{}, leaseExpired: map[string]time.Time{}}
	if s != nil {
		s.healthEval.Store(e)
		s.setMetaReachNode(cfg.NodeID, cfg.MetaUnreachableAfter)
		e.adoptEarlySignals()
		s.replayRecordSignals()
	}
	return e
}

// adoptEarlySignals moves signal-kind failures raised before the evaluator
// was attached (they went to the legacy book because nothing owned the
// kind yet, e.g. a standalone Restore) into the signal book. Left in the
// legacy book they would read as already open and never be written.
func (e *Evaluator) adoptEarlySignals() {
	if !e.ownsManaged() {
		return
	}
	b := e.s.findings
	b.mu.Lock()
	var early []Finding
	for k, f := range b.open {
		if isSignalKind(k.kind) && e.ownsSignal(k.task, k.kind) {
			early = append(early, f)
		}
	}
	b.mu.Unlock()
	for _, f := range early {
		b.forget(f.TaskID, f.Kind, f.Subject)
		e.Signal(f.TaskID, f.Kind, f.Subject, false, f.Detail)
	}
	// TASK_UNADOPTED the legacy sink raised before the evaluator took over
	// (reconcileUnadoptedLegacy) is handed over: resolved in the legacy
	// store, then derived and stored by the evaluator on its first pass.
	if e.s != nil {
		e.s.mu.Lock()
		handed := e.s.unadoptedOpen
		e.s.unadoptedOpen = nil
		e.s.mu.Unlock()
		for id := range handed {
			b.resolve(id, findingTaskUnadopted, "")
			e.MarkDirty(id, ReasonStartup)
		}
	}
}

// Mode is the feature flag.
func (e *Evaluator) Mode() EvaluatorMode { return e.cfg.Mode }

// ownsManaged: the legacy Report path must not write managed kinds.
func (e *Evaluator) ownsManaged() bool {
	return e != nil && e.cfg.Mode == EvaluatorOn && !e.schemaAbsent.Load()
}

// MarkDirty never blocks and may be called with the scheduler lock held.
// LEASE_LOST drops the task's queued marks instead of queueing one.
func (e *Evaluator) MarkDirty(taskID string, r EvalReason) {
	if e == nil || e.cfg.Mode == EvaluatorOff || strings.TrimSpace(taskID) == "" {
		return
	}
	e.mu.Lock()
	switch r {
	case ReasonLeaseLost:
		delete(e.dirty, taskID)
		delete(e.pending, taskID)
		e.mu.Unlock()
		return
	case ReasonLeaseAcquired:
		delete(e.fenced, taskID)
	}
	m := e.dirty[taskID]
	if m == nil {
		m = map[EvalReason]time.Time{}
		e.dirty[taskID] = m
	}
	if _, ok := m[r]; !ok {
		m[r] = e.cfg.Now()
	}
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Stats is a snapshot of the counters.
func (e *Evaluator) Stats() EvaluatorStats {
	e.mu.Lock()
	var pending int64
	for _, ts := range e.pending {
		pending += int64(len(ts))
	}
	e.mu.Unlock()
	return EvaluatorStats{Evaluations: e.evaluations.Load(), Applied: e.applied.Load(), Errors: e.errs.Load(), Shadowed: e.shadowed.Load(), Pending: pending}
}

// Run evaluates dirty tasks on the worker pool and sweeps every Sweep
// until ctx ends. The first sweep (STARTUP) runs at once.
func (e *Evaluator) Run(ctx context.Context) {
	if e.cfg.Mode == EvaluatorOff {
		return
	}
	var wg sync.WaitGroup
	for i := 0; i < e.cfg.Workers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); e.work(ctx) }()
	}
	e.markResponsible(ReasonStartup)
	t := time.NewTicker(e.cfg.Sweep)
	defer t.Stop()
	lt := time.NewTicker(e.cfg.LivenessTick)
	defer lt.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-t.C:
			e.markResponsible(ReasonSweep)
		case <-lt.C:
			e.markLivenessCrossings()
			go e.s.probeMeta() // S17 (§25); bounded, never blocks this loop
		}
	}
}

func (e *Evaluator) work(ctx context.Context) {
	for ctx.Err() == nil {
		id, wait := e.take()
		if id == "" {
			if wait <= 0 || wait > time.Second {
				wait = time.Second
			}
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-e.wake:
			case <-t.C:
			}
			t.Stop()
			continue
		}
		if _, err := e.evaluate(ctx, id); err != nil && !errors.Is(err, errNotResponsible) {
			log.Printf("health evaluate task=%s: %v", id, err)
			e.s.noteMetaReach(err, false) // a timeout or connection error counts (§25 D2)
		}
		e.finish(id)
	}
}

// take picks a dirty task that is not running and is past its debounce.
func (e *Evaluator) take() (string, time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.cfg.Now()
	var soonest time.Duration
	ids := make([]string, 0, len(e.dirty))
	for id := range e.dirty {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if e.running[id] {
			continue
		}
		if next := e.lastRun[id].Add(e.cfg.Debounce); now.Before(next) {
			if d := next.Sub(now); soonest == 0 || d < soonest {
				soonest = d
			}
			continue
		}
		delete(e.dirty, id)
		e.running[id] = true
		return id, 0
	}
	return "", soonest
}

func (e *Evaluator) finish(id string) {
	e.mu.Lock()
	delete(e.running, id)
	e.lastRun[id] = e.cfg.Now()
	again := len(e.dirty[id]) > 0
	e.mu.Unlock()
	if again {
		select {
		case e.wake <- struct{}{}:
		default:
		}
	}
}

// responsible is every task this node evaluates, sorted.
func (e *Evaluator) responsible() []Task {
	var out []Task
	listed := e.s.ListTasks()
	listed = append(listed, e.proxyCandidates(listed)...)
	for _, t := range listed {
		if _, ok := e.authorityFor(t); ok {
			out = append(out, t)
		}
	}
	if !e.cfg.Cluster && e.clusterHasWork() {
		out = append(out, Task{ID: ClusterFindingTask})
	}
	out = append(out, e.goneUnadopted(out)...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// goneUnadopted (#385): standalone with no task store, the tasks whose
// stored health still names TASK_UNADOPTED but that are no longer listed
// (the directory was moved out of data_dir, in this process or before a
// restart). Evaluating them once closes the row.
func (e *Evaluator) goneUnadopted(listed []Task) []Task {
	if e.cfg.Cluster || e.cfg.Store == nil || e.s == nil || e.s.hasTaskStore() {
		return nil
	}
	hs, err := e.cfg.Store.ListHealth(context.Background())
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, t := range listed {
		seen[t.ID] = true
	}
	var out []Task
	for _, h := range hs {
		if seen[h.TaskID] || h.TaskID == ClusterFindingTask {
			continue
		}
		for _, k := range h.Reasons {
			if k.Kind == health.TaskUnadopted {
				out = append(out, Task{ID: h.TaskID})
				seen[h.TaskID] = true
				break
			}
		}
	}
	return out
}

func (s *Scheduler) hasTaskStore() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store != nil
}

// clusterHasWork: standalone evaluates _cluster while it has a failing
// signal, a success and an open row in the store, or a legacy open row of
// a managed kind.
func (e *Evaluator) clusterHasWork() bool {
	e.mu.Lock()
	for _, en := range e.signals[ClusterFindingTask] {
		if en.st.FailureSeq > 0 {
			e.mu.Unlock()
			return true
		}
	}
	successes := len(e.signals[ClusterFindingTask]) > 0
	e.mu.Unlock()
	if successes && e.cfg.Store != nil {
		// A success may close a row an earlier process left open.
		// Bounded: this runs on the sweep loop, which also pings meta (§25).
		ctx, cancel := context.WithTimeout(context.Background(), 2*healthReadBudget)
		open, err := e.cfg.Store.ListOpenFindingsForTask(ctx, ClusterFindingTask)
		cancel()
		if err == nil && len(open) > 0 {
			return true
		}
	}
	b := e.s.findings
	b.mu.Lock()
	defer b.mu.Unlock()
	for k := range b.open {
		if k.task == ClusterFindingTask && e.reg[health.Kind(k.kind)].Managed {
			return true
		}
	}
	return false
}

// markResponsible marks every responsible task dirty: CATALOG when its
// directory fingerprint (names, sizes, mtimes) changed since the last
// sweep, otherwise the given reason (design §5.3).
func (e *Evaluator) markResponsible(r EvalReason) []string {
	var ids []string
	e.refreshHealthLease(context.Background())
	e.refreshExpiredLeases(context.Background())
	for _, t := range e.responsible() {
		reason := r
		fp := e.fingerprint(t.ID)
		e.mu.Lock()
		if prev, ok := e.fingerprints[t.ID]; ok && prev != fp {
			reason = ReasonCatalog
		}
		e.fingerprints[t.ID] = fp
		e.mu.Unlock()
		e.MarkDirty(t.ID, reason)
		ids = append(ids, t.ID)
	}
	return ids
}

// SweepNow marks every responsible task dirty and evaluates each before
// returning (a task another worker is evaluating stays dirty for it).
func (e *Evaluator) SweepNow(ctx context.Context) error {
	if e.cfg.Mode == EvaluatorOff {
		return nil
	}
	var errs []error
	for _, id := range e.markResponsible(ReasonSweep) {
		e.mu.Lock()
		if e.running[id] {
			e.mu.Unlock()
			continue
		}
		delete(e.dirty, id)
		e.running[id] = true
		e.mu.Unlock()
		if _, err := e.evaluate(ctx, id); err != nil && !errors.Is(err, errNotResponsible) {
			errs = append(errs, fmt.Errorf("task %s: %w", id, err))
		}
		e.finish(id)
	}
	return errors.Join(errs...)
}

// EvaluateNow evaluates one task at once (API, tests), waiting for a pass
// already running on it.
func (e *Evaluator) EvaluateNow(ctx context.Context, taskID string) (health.TaskHealth, error) {
	if e.cfg.Mode == EvaluatorOff {
		return health.TaskHealth{}, nil
	}
	for {
		e.mu.Lock()
		if !e.running[taskID] {
			delete(e.dirty, taskID)
			e.running[taskID] = true
			e.mu.Unlock()
			break
		}
		e.mu.Unlock()
		select {
		case <-ctx.Done():
			return health.TaskHealth{}, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	defer e.finish(taskID)
	return e.evaluate(ctx, taskID)
}

var errNotResponsible = errors.New("this node does not evaluate the task")

// fenceFor is the write authority for the task (design §8), and whether
// this node evaluates it as holder. Cluster: only the task lease holder;
// the _health holder evaluates unowned tasks through authorityFor (P2-6c).
func (e *Evaluator) fenceFor(t Task) (health.Fence, bool) {
	if !e.cfg.Cluster {
		return health.Fence{Mode: health.FenceStandalone, Node: e.cfg.NodeID, Instance: e.cfg.Instance}, true
	}
	if e.cfg.ControlPlaneOnly || t.OwnerWorkerID == "" || t.OwnerWorkerID != e.cfg.NodeID || t.Epoch <= 0 {
		return health.Fence{}, false
	}
	return health.Fence{Mode: health.FenceCluster, TaskEpoch: t.Epoch, Node: e.cfg.NodeID, Instance: e.cfg.Instance}, true
}

func (e *Evaluator) evaluate(ctx context.Context, id string) (health.TaskHealth, error) {
	isCluster := id == ClusterFindingTask
	var task Task
	gone := false
	if isCluster {
		if e.cfg.Cluster || !e.clusterHasWork() {
			return health.TaskHealth{}, errNotResponsible
		}
		task = Task{ID: id}
	} else {
		var err error
		if task, err = e.s.GetTask(id); err != nil {
			if !errors.Is(err, ErrTaskNotFound) || e.cfg.Cluster || e.s.hasTaskStore() {
				return health.TaskHealth{}, err
			}
			// #385: a standalone leftover directory that is gone. Its
			// task-state kinds no longer hold; nothing else is judged.
			task, gone = Task{ID: id}, true
		}
	}
	fence, ok := e.authorityFor(task)
	if !ok {
		return health.TaskHealth{}, errNotResponsible
	}
	proxy := fence.Proxy || fence.Mode == health.FenceNeverLeased
	e.mu.Lock()
	if epoch, stopped := e.fenced[id]; stopped && epoch == fence.TaskEpoch && !proxy {
		e.mu.Unlock()
		return health.TaskHealth{}, errNotResponsible
	}
	delete(e.fenced, id)
	e.mu.Unlock()
	e.evaluations.Add(1)
	now := e.cfg.Now()
	facts := &health.Facts{TaskID: id, Now: now, Complete: map[health.Scope]bool{}, NoProgressAfter: e.cfg.NoProgressAfter}
	switch {
	case gone:
		facts.Task = &health.TaskFacts{State: string(StateStopped), Desired: TaskDesiredStop, RunHere: true}
		facts.Complete[health.ScopeTaskState] = true
	case proxy:
		unownedFacts(task, facts, e.expiredSince(task.ID)) // P2-6c: task.state only (§24.2)
	case isCluster:
		facts.Complete[health.ScopeChainLocal] = true // _cluster has no chain
	default:
		if chain := e.chainFactsInto(task, now, facts); chain != nil {
			facts.Chain = chain
			facts.Complete[health.ScopeChainLocal] = true
		}
	}
	if !gone && !proxy {
		e.signalFacts(ctx, task, isCluster, facts)
	}
	store := e.cfg.Store
	unknown := func(h health.TaskHealth, err error) (health.TaskHealth, error) {
		e.errs.Add(1)
		h.TaskID, h.Status, h.EvaluatedAt = id, health.StatusUnknown, now
		h.Unknown = appendScope(h.Unknown, health.ScopeStore)
		return h, err
	}
	if store == nil {
		return unknown(health.TaskHealth{}, errors.New("no health store"))
	}
	// Transitions a failed write left behind go first, as they were.
	if e.cfg.Mode == EvaluatorOn {
		e.mu.Lock()
		pending := append([]health.Transition(nil), e.pending[id]...)
		e.mu.Unlock()
		if len(pending) > 0 {
			open, err := e.openFindings(ctx, id)
			if err != nil {
				return unknown(health.TaskHealth{}, err)
			}
			h := e.reg.HealthOf(afterTransitions(open, pending), facts)
			if err := e.write(ctx, id, fence, pending, h); err != nil {
				// Keep evaluating in memory on top of the unsaved transitions so
				// a finding raised while the store is failing is still served.
				open2 := afterTransitions(open, pending)
				ts2, _ := e.reg.Reconcile(open2, facts)
				e.noteUnsaved(id, afterTransitions(open2, ts2), err)
				return unknown(h, err)
			}
		}
	}
	open, err := e.openFindings(ctx, id)
	if err != nil {
		return unknown(health.TaskHealth{}, err)
	}
	if isCluster {
		e.settleMetaReach(open)
	}
	e.openAndProbes(ctx, id, open, facts)
	e.loadWaivers(ctx, id, facts)
	e.mu.Lock()
	facts.Confirmed = e.confirmed[id]
	e.mu.Unlock()
	ts, _ := e.reg.Reconcile(open, facts)
	e.noteCandidates(id, facts)
	h := e.reg.HealthOf(afterTransitions(open, ts), facts)
	if e.cfg.Mode == EvaluatorShadow {
		e.shadowed.Add(1)
		if len(ts) > 0 {
			log.Printf("health shadow task=%s transitions=%s status=%s", id, describeTransitions(ts), h.Status)
		}
		return h, nil
	}
	if err := e.write(ctx, id, fence, ts, h); err != nil {
		e.noteUnsaved(id, afterTransitions(open, ts), err)
		return unknown(h, err)
	}
	return h, nil
}

// noteCandidates keeps this pass's Confirm-kind keys for the next pass and
// queues a CONFIRM pass (after Debounce) when one is still pending.
func (e *Evaluator) noteCandidates(id string, facts *health.Facts) {
	cands := e.reg.Candidates(facts)
	pending := len(e.reg.Pending(facts)) > 0
	e.mu.Lock()
	if len(cands) == 0 {
		delete(e.confirmed, id)
	} else {
		e.confirmed[id] = cands
	}
	e.mu.Unlock()
	if pending {
		e.MarkDirty(id, ReasonConfirm)
	}
}

// unsavedHealth is one task's in-memory open set after a failed write.
type unsavedHealth struct {
	open  []health.Finding
	err   error
	since time.Time
}

// noteUnsaved keeps the open set a failed write could not store (a fenced
// write is not kept: another owner has the task).
func (e *Evaluator) noteUnsaved(id string, open []health.Finding, err error) {
	if errors.Is(err, healthstore.ErrFenced) {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	since := e.cfg.Now()
	if prev, ok := e.unsaved[id]; ok {
		since = prev.since
	}
	e.unsaved[id] = unsavedHealth{open: open, err: err, since: since}
}

// unsavedOpen returns the in-memory open set for id when its last write failed.
func (e *Evaluator) unsavedOpen(id string) ([]health.Finding, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	u, ok := e.unsaved[id]
	return u.open, ok
}

// persistFailure summarises the tasks whose health could not be stored.
func (e *Evaluator) persistFailure() (ids []string, err error, since time.Time) {
	return e.persistFailureFrom(true)
}

// persistFailureFrom: prune false reads no task row (the metadata store may
// not answer, design §25), so tasks no longer ours are not dropped.
func (e *Evaluator) persistFailureFrom(prune bool) (ids []string, err error, since time.Time) {
	if e == nil {
		return nil, nil, time.Time{}
	}
	e.mu.Lock()
	all := make(map[string]unsavedHealth, len(e.unsaved))
	for id, u := range e.unsaved {
		all[id] = u
	}
	e.mu.Unlock()
	for id, u := range all {
		if prune && id != ClusterFindingTask && e.s != nil {
			t, gerr := e.s.GetTask(id)
			if gerr != nil || !e.mayEvaluate(t) {
				// The task is gone (or was never created: a refused create
				// still marks its id dirty), or this node no longer
				// evaluates it (another owner, design §24.14): nothing of
				// ours will write its row again.
				e.mu.Lock()
				delete(e.unsaved, id)
				e.mu.Unlock()
				continue
			}
		}
		ids = append(ids, id)
		if err == nil || u.since.Before(since) {
			err, since = u.err, u.since
		}
	}
	e.mu.Lock()
	lerr, lsince := e.leaseWriteErr, e.leaseWriteErrAt
	e.mu.Unlock()
	if lerr != nil {
		// Every cluster node writes the _health lease each sweep, so a meta
		// store refusing writes shows on every node, not only on the
		// owner of a task (design §24.14, #425).
		ids = append(ids, HealthLeaseID)
		if err == nil || lsince.Before(since) {
			err, since = fmt.Errorf("metadata store refused the %s lease write: %w", HealthLeaseID, lerr), lsince
		}
	}
	sort.Strings(ids)
	return ids, err, since
}

// write applies ts under fence. Success clears the pending list; a fenced
// write is dropped and the task is not evaluated again until the lease is
// re-acquired; any other failure keeps ts pending for the next pass.
func (e *Evaluator) write(ctx context.Context, id string, fence health.Fence, ts []health.Transition, h health.TaskHealth) error {
	_, err := e.cfg.Store.ApplyTransitions(ctx, id, fence, ts, h)
	switch {
	case err == nil:
		e.schemaAbsent.Store(false)
		e.applied.Add(int64(len(ts)))
		e.mu.Lock()
		delete(e.pending, id)
		delete(e.unsaved, id)
		e.mu.Unlock()
		e.afterApply(ts)
		return nil
	case errors.Is(err, healthstore.ErrFenced):
		e.mu.Lock()
		delete(e.pending, id)
		if !fence.Proxy && fence.Mode != health.FenceNeverLeased {
			// A proxy authority is derived again on every pass (the
			// lease may be released a moment later), so it is not latched.
			e.fenced[id] = fence.TaskEpoch
		}
		e.mu.Unlock()
		return err
	}
	if errors.Is(err, healthstore.ErrSchemaAbsent) {
		e.schemaAbsent.Store(true)
	}
	if len(ts) > 0 {
		e.mu.Lock()
		if len(e.pending[id]) == 0 {
			e.pending[id] = append([]health.Transition(nil), ts...)
		}
		e.mu.Unlock()
	}
	return err
}

// afterApply keeps the in-process finding book in step: a closed key
// leaves the book; with a separate legacy store (standalone file mode) the
// legacy row is resolved there as well.
func (e *Evaluator) afterApply(ts []health.Transition) {
	b := e.s.findings
	for _, t := range ts {
		if t.Op != health.OpClose {
			continue
		}
		if e.separateLegacyStore() {
			b.resolve(t.TaskID, FindingKind(t.Kind), t.Subject)
		}
		b.forget(t.TaskID, FindingKind(t.Kind), t.Subject)
	}
}

// separateLegacyStore: the evaluator's store is not the task_findings table
// the legacy sink writes (standalone, .health.json next to .findings.json).
func (e *Evaluator) separateLegacyStore() bool {
	_, file := e.cfg.Store.(*healthstore.FileStore)
	if !file {
		if w, ok := e.cfg.Store.(interface{ Unwrap() healthstore.Store }); ok {
			_, file = w.Unwrap().(*healthstore.FileStore)
		}
	}
	return file
}

// openFindings is the task's complete open set: the store's, plus, with a
// separate legacy store, the legacy open rows of managed kinds (so rows an
// older release wrote are closed too).
func (e *Evaluator) openFindings(ctx context.Context, id string) ([]health.Finding, error) {
	open, err := e.cfg.Store.ListOpenFindingsForTask(ctx, id)
	if err != nil {
		return nil, err
	}
	if !e.separateLegacyStore() {
		return open, nil
	}
	seen := map[health.Key]bool{}
	for _, f := range open {
		seen[f.Key()] = true
	}
	b := e.s.findings
	b.mu.Lock()
	var legacy []Finding
	for k, f := range b.open {
		if k.task == id {
			legacy = append(legacy, f)
		}
	}
	b.mu.Unlock()
	for _, f := range legacy {
		k := health.Key{TaskID: f.TaskID, Kind: health.Kind(f.Kind), Subject: f.Subject}
		if seen[k] || !e.reg[k.Kind].Managed {
			continue
		}
		seen[k] = true
		open = append(open, health.Finding{TaskID: f.TaskID, Kind: k.Kind, Subject: f.Subject, Severity: health.Severity(f.Severity),
			Detail: f.Detail, FirstSeen: f.FirstSeen, LastSeen: f.LastSeen})
	}
	return open, nil
}

// chainFacts is chain.local for the task: CheckBody over the replay order
// (open copies included, empty leftovers flagged), plus a fresh
// CheckCheckpoint (I5, never cached), plus rehash mismatches of segments
// still on the order. Checkpoint is read before order/disk (§22.4).
// nil when the task directory cannot be read (the scope is incomplete).
func (e *Evaluator) chainFacts(task Task, now time.Time) *health.ChainFacts {
	return e.chainFactsInto(task, now, nil)
}

// chainFactsInto is chainFacts; with facts set it also fills the object
// facts of design 0009 §21 (chain.objects, Produced, NoObjectStorage).
func (e *Evaluator) chainFactsInto(task Task, now time.Time, facts *health.Facts) *health.ChainFacts {
	e.s.mu.Lock()
	dataDir := e.s.dataDir
	e.s.mu.Unlock()
	dir, ok := taskBinlogDir(dataDir, task.ID)
	if !ok {
		return nil
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil
	}
	if _, err := os.ReadDir(dir); err != nil {
		return nil
	}
	// §22.4: checkpoint before order/disk so C cannot point past this snapshot.
	cp := e.assuranceCheckpoint(task)
	switches := e.s.assuranceSwitches(task.ID)
	fp := e.fingerprint(task.ID)
	e.s.mu.Lock()
	catalog := e.s.fileStore != nil
	e.s.mu.Unlock()
	e.mu.Lock()
	cached, hit := e.chainCache[task.ID]
	e.mu.Unlock()
	var (
		order []string
		rows  []assurance.Row
	)
	if hit && cached.fp == fp && !catalog {
		order = cached.order // a disk-only order is a function of the directory
	} else {
		order, rows = e.s.chainOrder(task.ID, dataDir, catalog)
	}
	obj := e.objectView(task.ID, now, rows, facts)
	swKey := switchesKey(switches)
	orderKey := strings.Join(order, "\x00") + "\x01" + rowsKey(rows) + "\x01" + obj.key + "\x02" + swKey
	var violations []assurance.Violation
	in := assurance.Inputs{
		Flavor:     task.Source.Flavor,
		StartGTID:  StartGTIDText(task),
		Checkpoint: cp, // used only for CheckCheckpoint below; body cache ignores it
		Local:      binlog.LedgerAt(dir),
		Order:      order,
		Rows:       rows,
		NoCatalog:  !catalog,
		Switches:   switches,
		// §21: object storage, the listed prefixes and the upload grace.
		ObjectStorage:  obj.storage,
		Objects:        obj.objects,
		ObjectPrefixes: obj.prefixes,
		UploadGrace:    obj.grace,
		// §20.1: the planner's legacy failback (I3_LEGACY_FAILBACK),
		// which also suppresses the legacy row size rule (§20.2).
		Plan: func() []assurance.Violation {
			return e.s.plannedProblems(task, dir, binlog.LedgerAt(dir))
		},
	}
	if hit && cached.fp == fp && cached.orderKey == orderKey {
		violations = append([]assurance.Violation(nil), cached.violations...)
	} else {
		bodyIn := in
		bodyIn.Checkpoint = assurance.Checkpoint{} // body cache must not depend on C
		violations = assurance.ChainOf(bodyIn).CheckBody(now)
		e.mu.Lock()
		e.chainCache[task.ID] = chainCacheEntry{fp: fp, orderKey: orderKey, order: order, violations: violations}
		e.mu.Unlock()
	}
	violations = append(violations, assurance.ChainOf(in).CheckCheckpoint()...)
	out := &health.ChainFacts{}
	for _, v := range violations {
		if e.cfg.Cluster && v.Kind == assurance.I6MissingLocal {
			continue // §20.1: the owner judges its own disk (P2-6 delegation)
		}
		if objectKinds[v.Kind] && anyIn(v.Segments, obj.unjudged) {
			continue // §21.3: uploaded after the listing it would be judged by
		}
		out.Violations = append(out.Violations, v)
	}
	// #413: copy the mismatches under e.mu; setRehash writes this inner map
	// under the lock, so it must never be read after Unlock.
	e.mu.Lock()
	var results map[string]rehashResult
	for name, r := range e.rehash[task.ID] {
		if !r.ok {
			if results == nil {
				results = map[string]rehashResult{}
			}
			results[name] = r
		}
	}
	e.mu.Unlock()
	for _, name := range order {
		r, ok := results[name]
		if !ok || r.ok {
			continue
		}
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || !binlog.StampOf(info).Equal(r.stamp) {
			continue // changed since it was hashed: the next cycle judges it
		}
		detail := "segment " + name + " no longer matches the sha256 recorded at seal: " + r.evidence
		out.Violations = append(out.Violations, assurance.Violation{Kind: assurance.I1Unreadable, Segments: []string{name}, Evidence: r.evidence, Detail: detail})
	}
	return out
}

// assuranceCheckpoint is the task checkpoint for I5 (empty when unknown).
func (e *Evaluator) assuranceCheckpoint(task Task) assurance.Checkpoint {
	if e == nil || e.s == nil {
		return assurance.Checkpoint{}
	}
	cp, ok, err := e.s.GetCheckpoint(context.Background(), task.ID)
	if err != nil || !ok {
		return assurance.Checkpoint{}
	}
	return assurance.Checkpoint{GTIDSet: cp.GTIDSet, File: cp.File, Pos: cp.Pos}
}

// assuranceSwitches builds Inputs.Switches from SourceChain (§22.2).
func (s *Scheduler) assuranceSwitches(taskID string) []assurance.Switch {
	if s == nil {
		return nil
	}
	chain, err := s.SourceChain(taskID)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []assurance.Switch
	add := func(id, stored string) {
		id = strings.ToLower(strings.TrimSpace(id))
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, assurance.Switch{Identity: id, Stored: stored})
	}
	// .source-chain stints after the first are recorded switches even with no events.
	for i, srv := range chain.Servers {
		if i == 0 {
			continue
		}
		add(srv.Identity, "")
	}
	for _, sw := range chain.Switches {
		add(sw.New, sw.GTIDSet)
	}
	return out
}

func switchesKey(sw []assurance.Switch) string {
	if len(sw) == 0 {
		return ""
	}
	parts := make([]string, 0, len(sw))
	for _, s := range sw {
		parts = append(parts, strings.ToLower(strings.TrimSpace(s.Identity))+"|"+s.Stored)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// loadWaivers fills Facts.Waivers when the store implements WaiverStore (§22.3).
func (e *Evaluator) loadWaivers(ctx context.Context, taskID string, facts *health.Facts) {
	if e == nil || e.cfg.Store == nil || facts == nil {
		return
	}
	ws, ok := e.cfg.Store.(healthstore.WaiverStore)
	if !ok {
		return
	}
	list, err := ws.ListWaivers(ctx, taskID)
	if err != nil {
		return
	}
	facts.Waivers = list
}

// WaiveGTIDHole stores a waiver for an exact GTID_HOLE evidence and marks the
// task dirty so the next pass closes with reason=WAIVED (§22.3).
func (e *Evaluator) WaiveGTIDHole(ctx context.Context, taskID, subject, evidence, reason, actor string) error {
	if e == nil || e.cfg.Store == nil {
		return errors.New("no health store")
	}
	ws, ok := e.cfg.Store.(healthstore.WaiverStore)
	if !ok {
		return errors.New("health store does not support waivers")
	}
	canon, err := health.CanonicalGTIDRanges(evidence)
	if err != nil {
		return err
	}
	spec := e.reg[health.GTIDHole]
	if !spec.Waivable || !spec.Managed {
		return errors.New("GTID_HOLE is not waivable")
	}
	fence, ok := e.fenceFor(Task{ID: taskID})
	if !ok {
		return errNotResponsible
	}
	if actor == "" {
		actor = "operator"
	}
	w := health.Waiver{Key: health.Key{TaskID: taskID, Kind: health.GTIDHole, Subject: subject}, Evidence: canon, Reason: reason, Actor: actor, At: e.cfg.Now()}
	if err := ws.PutWaiver(ctx, taskID, fence, w); err != nil {
		return err
	}
	e.MarkDirty(taskID, ReasonAPI)
	return nil
}

// UnwaiveGTIDHole clears a stored waiver (§22.3).
func (e *Evaluator) UnwaiveGTIDHole(ctx context.Context, taskID, subject string) error {
	if e == nil || e.cfg.Store == nil {
		return errors.New("no health store")
	}
	ws, ok := e.cfg.Store.(healthstore.WaiverStore)
	if !ok {
		return errors.New("health store does not support waivers")
	}
	fence, ok := e.fenceFor(Task{ID: taskID})
	if !ok {
		return errNotResponsible
	}
	key := health.Key{TaskID: taskID, Kind: health.GTIDHole, Subject: subject}
	if err := ws.DeleteWaiver(ctx, taskID, fence, key); err != nil {
		return err
	}
	e.MarkDirty(taskID, ReasonAPI)
	return nil
}

func (e *Evaluator) setRehash(taskID, segment string, r rehashResult) (changed bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.rehash[taskID]
	if m == nil {
		m = map[string]rehashResult{}
		e.rehash[taskID] = m
	}
	prev, had := m[segment]
	m[segment] = r
	return !had && !r.ok || had && prev.ok != r.ok
}

// fingerprint hashes the task directory's names and file stamps (size,
// mtime, ctime): an in-place edit that keeps the mtime still changes it.
func (e *Evaluator) fingerprint(taskID string) string {
	e.s.mu.Lock()
	dataDir := e.s.dataDir
	e.s.mu.Unlock()
	dir, ok := taskBinlogDir(dataDir, taskID)
	if !ok {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "error:" + err.Error()
	}
	h := sha256.New()
	for _, en := range entries {
		info, err := en.Info()
		if err != nil {
			continue
		}
		st := binlog.StampOf(info)
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00%d\n", en.Name(), st.Size, st.MTime.UnixNano(), st.CTime)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// afterTransitions is the open set once ts are applied.
func afterTransitions(open []health.Finding, ts []health.Transition) []health.Finding {
	by := map[health.Key]health.Finding{}
	var keys []health.Key
	for _, f := range open {
		if _, ok := by[f.Key()]; !ok {
			keys = append(keys, f.Key())
		}
		by[f.Key()] = f
	}
	for _, t := range ts {
		if t.Op == health.OpClose {
			delete(by, t.Key)
			continue
		}
		f, ok := by[t.Key]
		if !ok {
			keys = append(keys, t.Key)
		}
		f.TaskID, f.Kind, f.Subject = t.TaskID, t.Kind, t.Subject
		f.Severity, f.Detail, f.Evidence, f.Segments = t.Instance.Severity, t.Instance.Detail, t.Instance.Evidence, t.Instance.Segments
		by[t.Key] = f
	}
	out := make([]health.Finding, 0, len(by))
	for _, k := range keys {
		if f, ok := by[k]; ok {
			out = append(out, f)
			delete(by, k)
		}
	}
	return out
}

func appendScope(list []health.Scope, s health.Scope) []health.Scope {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

func describeTransitions(ts []health.Transition) string {
	parts := make([]string, 0, len(ts))
	for _, t := range ts {
		parts = append(parts, string(t.Op)+":"+string(t.Kind)+"/"+t.Subject)
	}
	return strings.Join(parts, ",")
}

// openManagedFindings is the evaluator's open managed findings of a task
// as API findings, when its store is separate from the legacy sink.
// A read error is returned, not an empty set: a CRITICAL finding must not
// vanish from the summary because the store did not answer (§25 D1).
func (e *Evaluator) openManagedFindings(ctx context.Context, taskID string) ([]Finding, error) {
	if e == nil || e.cfg.Mode != EvaluatorOn || !e.separateLegacyStore() {
		return nil, nil
	}
	open, ok := e.unsavedOpen(taskID)
	if !ok {
		var err error
		if open, err = e.cfg.Store.ListOpenFindingsForTask(ctx, taskID); err != nil {
			return nil, err
		}
	}
	out := make([]Finding, 0, len(open))
	for _, f := range open {
		out = append(out, Finding{TaskID: f.TaskID, Kind: FindingKind(f.Kind), Subject: f.Subject, Severity: FindingSeverity(f.Severity),
			Detail: f.Detail, FirstSeen: f.FirstSeen, LastSeen: f.LastSeen, Count: 1})
	}
	return out, nil
}

// NodeIDFileName is the persisted standalone node id under data_dir.
const NodeIDFileName = ".node_id"

// LoadOrCreateNodeID returns the node id stored in {dataDir}/.node_id,
// creating a random one atomically on first start (design §8: the id goes
// with the data directory, not the host name).
func LoadOrCreateNodeID(dataDir string) (string, error) {
	path := filepath.Join(dataDir, NodeIDFileName)
	if raw, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(raw)); id != "" {
			return id, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	id, err := RandomID()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dataDir, ".node_id-*.tmp")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(id + "\n"); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return id, nil
}

// RandomID is 16 random bytes in hex (node and instance ids).
func RandomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// WaiveFinding is the scheduler-facing waive entry (API).
func (s *Scheduler) WaiveFinding(ctx context.Context, taskID, kind, subject, evidence, reason, actor string) error {
	if s == nil {
		return errors.New("no scheduler")
	}
	e := s.healthEval.Load()
	if e == nil {
		return errors.New("health evaluator is not attached")
	}
	if health.Kind(kind) != health.GTIDHole {
		return fmt.Errorf("kind %q is not waivable", kind)
	}
	return e.WaiveGTIDHole(ctx, taskID, subject, evidence, reason, actor)
}

// UnwaiveFinding clears a stored waiver (API).
func (s *Scheduler) UnwaiveFinding(ctx context.Context, taskID, kind, subject string) error {
	if s == nil {
		return errors.New("no scheduler")
	}
	e := s.healthEval.Load()
	if e == nil {
		return errors.New("health evaluator is not attached")
	}
	if health.Kind(kind) != health.GTIDHole {
		return fmt.Errorf("kind %q is not waivable", kind)
	}
	return e.UnwaiveGTIDHole(ctx, taskID, subject)
}
