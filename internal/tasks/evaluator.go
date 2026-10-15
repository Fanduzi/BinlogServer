// Package tasks provides module-level functionality for tasks.
// input: dirty marks (SEAL, UPLOAD, TRANSITION, LEASE_*, CATALOG, STARTUP, SWEEP, REHASH, API) for tasks this node is responsible for, the task directory and replay order, rehash results, and the task's open findings from a healthstore.Store
// output: Evaluator: a dirty set (marks merge per task), per-task serial evaluation on a worker pool with a 1s debounce, a sweep that marks every responsible task dirty (CATALOG when the directory fingerprint changed), Facts with chain.local from assurance.Chain.Check, health.Reconcile and HealthOf, and the transitions written under a health.Fence; feature flag off (nothing), shadow (compute, write nothing) and on (write; the legacy Report path stops writing the managed kinds while the store has migration 000009); P2-4: signal facts (task.state, signals, open keys, probes, stale signals) from signals.go, and the pseudo task _cluster in standalone
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
)

// EvaluatorConfig configures NewEvaluator.
type EvaluatorConfig struct {
	// LivenessTick and DumpStalledAfter: stub fields (#383 red commit).
	LivenessTick, DumpStalledAfter time.Duration
	Mode                           EvaluatorMode
	Store                          healthstore.Store
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
	// NodeID is the cluster worker id, or the standalone {data_dir}/.node_id.
	NodeID string
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
	Now          func() time.Time
}

// EvaluatorStats counts what the evaluator did.
type EvaluatorStats struct {
	Evaluations, Applied, Errors, Shadowed, Pending int64
}

// chainCacheEntry is the last Check of a task directory: reused while the
// directory fingerprint (names, sizes, mtimes) and the replay order are the
// same, the size+mtime rule the ledger's own caches use.
type chainCacheEntry struct {
	fp, orderKey string
	order        []string
	violations   []assurance.Violation
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

	mu           sync.Mutex
	dirty        map[string]map[EvalReason]time.Time
	running      map[string]bool
	lastRun      map[string]time.Time
	fenced       map[string]int64
	pending      map[string][]health.Transition
	fingerprints map[string]string
	rehash       map[string]map[string]rehashResult
	chainCache   map[string]chainCacheEntry
	signals      map[string]map[health.SignalKey]*signalEntry
	unreach      map[string]unreachTrack

	// schemaAbsent: the store has no 000009 (ErrSchemaAbsent on the last
	// write); the legacy Report path keeps writing the managed kinds.
	schemaAbsent atomic.Bool

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
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Mode == "" {
		cfg.Mode = EvaluatorOff
	}
	e := &Evaluator{s: s, cfg: cfg, reg: cfg.Registry, wake: make(chan struct{}, cfg.Workers),
		dirty: map[string]map[EvalReason]time.Time{}, running: map[string]bool{}, lastRun: map[string]time.Time{},
		fenced: map[string]int64{}, pending: map[string][]health.Transition{}, fingerprints: map[string]string{},
		rehash: map[string]map[string]rehashResult{}, chainCache: map[string]chainCacheEntry{}}
	if s != nil {
		s.healthEval.Store(e)
	}
	return e
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
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-t.C:
			e.markResponsible(ReasonSweep)
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
	for _, t := range e.s.ListTasks() {
		if _, ok := e.fenceFor(t); ok {
			out = append(out, t)
		}
	}
	if !e.cfg.Cluster && e.clusterHasWork() {
		out = append(out, Task{ID: ClusterFindingTask})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
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
		if open, err := e.cfg.Store.ListOpenFindingsForTask(context.Background(), ClusterFindingTask); err == nil && len(open) > 0 {
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
// this node evaluates it. Cluster: only the task lease holder in P2-3
// (the _health holder and delegation land in P2-6).
func (e *Evaluator) fenceFor(t Task) (health.Fence, bool) {
	if !e.cfg.Cluster {
		return health.Fence{Mode: health.FenceStandalone, Node: e.cfg.NodeID, Instance: e.cfg.Instance}, true
	}
	if t.OwnerWorkerID == "" || t.OwnerWorkerID != e.cfg.NodeID || t.Epoch <= 0 {
		return health.Fence{}, false
	}
	return health.Fence{Mode: health.FenceCluster, TaskEpoch: t.Epoch, Node: e.cfg.NodeID, Instance: e.cfg.Instance}, true
}

func (e *Evaluator) evaluate(ctx context.Context, id string) (health.TaskHealth, error) {
	isCluster := id == ClusterFindingTask
	var task Task
	if isCluster {
		if e.cfg.Cluster || !e.clusterHasWork() {
			return health.TaskHealth{}, errNotResponsible
		}
		task = Task{ID: id}
	} else {
		var err error
		if task, err = e.s.GetTask(id); err != nil {
			return health.TaskHealth{}, err
		}
	}
	fence, ok := e.fenceFor(task)
	if !ok {
		return health.TaskHealth{}, errNotResponsible
	}
	e.mu.Lock()
	if epoch, stopped := e.fenced[id]; stopped && epoch == fence.TaskEpoch {
		e.mu.Unlock()
		return health.TaskHealth{}, errNotResponsible
	}
	delete(e.fenced, id)
	e.mu.Unlock()
	e.evaluations.Add(1)
	now := e.cfg.Now()
	facts := &health.Facts{TaskID: id, Now: now, Complete: map[health.Scope]bool{}}
	if isCluster {
		facts.Complete[health.ScopeChainLocal] = true // _cluster has no chain
	} else if chain := e.chainFacts(task, now); chain != nil {
		facts.Chain = chain
		facts.Complete[health.ScopeChainLocal] = true
	}
	e.signalFacts(ctx, task, isCluster, facts)
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
				return unknown(h, err)
			}
		}
	}
	open, err := e.openFindings(ctx, id)
	if err != nil {
		return unknown(health.TaskHealth{}, err)
	}
	e.openAndProbes(ctx, id, open, facts)
	ts, _ := e.reg.Reconcile(open, facts)
	h := e.reg.HealthOf(afterTransitions(open, ts), facts)
	if e.cfg.Mode == EvaluatorShadow {
		e.shadowed.Add(1)
		if len(ts) > 0 {
			log.Printf("health shadow task=%s transitions=%s status=%s", id, describeTransitions(ts), h.Status)
		}
		return h, nil
	}
	if err := e.write(ctx, id, fence, ts, h); err != nil {
		return unknown(h, err)
	}
	return h, nil
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
		e.mu.Unlock()
		e.afterApply(ts)
		return nil
	case errors.Is(err, healthstore.ErrFenced):
		e.mu.Lock()
		delete(e.pending, id)
		e.fenced[id] = fence.TaskEpoch
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

// chainFacts is chain.local for the task: Check over the replay order
// (open copies included, empty leftovers flagged), plus rehash mismatches
// of segments still on the order with the size and mtime that were hashed.
// nil when the task directory cannot be read (the scope is incomplete).
func (e *Evaluator) chainFacts(task Task, now time.Time) *health.ChainFacts {
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
	orderKey := strings.Join(order, "\x00") + "\x01" + rowsKey(rows)
	var violations []assurance.Violation
	if hit && cached.fp == fp && cached.orderKey == orderKey {
		violations = cached.violations
	} else {
		chain := assurance.ChainOf(assurance.Inputs{
			Flavor:    task.Source.Flavor,
			StartGTID: StartGTIDText(task),
			Local:     binlog.LedgerAt(dir),
			Order:     order,
			Rows:      rows,
			NoCatalog: !catalog,
		})
		violations = chain.Check(now)
		e.mu.Lock()
		e.chainCache[task.ID] = chainCacheEntry{fp: fp, orderKey: orderKey, order: order, violations: violations}
		e.mu.Unlock()
	}
	out := &health.ChainFacts{Violations: append([]assurance.Violation(nil), violations...)}
	e.mu.Lock()
	results := e.rehash[task.ID]
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
func (e *Evaluator) openManagedFindings(ctx context.Context, taskID string) []Finding {
	if e == nil || e.cfg.Mode != EvaluatorOn || !e.separateLegacyStore() {
		return nil
	}
	open, err := e.cfg.Store.ListOpenFindingsForTask(ctx, taskID)
	if err != nil {
		return nil
	}
	out := make([]Finding, 0, len(open))
	for _, f := range open {
		out = append(out, Finding{TaskID: f.TaskID, Kind: FindingKind(f.Kind), Subject: f.Subject, Severity: FindingSeverity(f.Severity),
			Detail: f.Detail, FirstSeen: f.FirstSeen, LastSeen: f.LastSeen, Count: 1})
	}
	return out
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
