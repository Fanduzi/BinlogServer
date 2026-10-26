// Package tasks provides module-level functionality for tasks.
// input: failure reports from the scheduler, the runner, and background loops
// output: the finding sink (ADR 0009), served from memory when persisting fails (FINDINGS_PERSIST_FAILED, HealthSummary, whose Detail does not change status and is kept when status is already degraded): an in-process book of open findings and counters, written to a FindingStore by one background writer that keeps failed writes and retries them, with a task event per write (none for _cluster except the FINDING_RESOLVED of META_UNREACHABLE (dated at the first answer, §25.8 R4) and of HEALTH_STORE_UNAVAILABLE (dated when 000009 can be written again, §27)); on shutdown WaitFindingSink waits for its final flush; Report never blocks on I/O; forget drops a key the health evaluator closed (with its queued writes), and ListFindings/OpenFindings merge the evaluator's open rows when its store is separate (standalone .health.json; OpenFindings includes _cluster in standalone and in cluster, and a cluster ErrSchemaAbsent on that id is ignored, §28); Report/Resolve of a kind the evaluator manages becomes a signal (SignalFailure/SignalSuccess) or is dropped (P2-4); ResolveWithReason writes the close reason into the resolved row's detail so one FINDING_RESOLVED carries it
// pos: the single exit for failures that are not returned to a caller that already makes them visible
// note ADR 0011: META_LOCK_STALLED is not a managed kind. HealthSummary adds side_kill, a session-timeout mismatch, and lock_detail=reduced when the store reports them. lock_detail is not a degraded reason. A reason names chain_owner or node when the finding detail does. An unreadable task directory is copied into Detail and does not change Status. A TLS transport failure in the object probe or upload retry is reason UPLOAD_TLS and degrades status; other probe text does not
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"binlog_server/internal/healthstore"
	"binlog_server/internal/upload"
)

// FindingKind is a closed set. Every kind has a fault-injection test in
// finding_kinds_test.go; adding a kind without one fails that test.
type FindingKind string

const (
	// FindingTaskPersistFailed: a task state transition was not written to the store (S2/S3).
	FindingTaskPersistFailed FindingKind = "TASK_PERSIST_FAILED"
	// FindingMetadataWriteBlocked: the dump stopped because the metadata store refused a write; it retries with backoff.
	FindingMetadataWriteBlocked FindingKind = "METADATA_WRITE_BLOCKED"
	// FindingEventPersistFailed: a task event was not written to the event store (S1).
	FindingEventPersistFailed FindingKind = "EVENT_PERSIST_FAILED"
	// FindingFindingsPersistFailed (#395 QA P2) is synthesized, never stored:
	// the health or findings file could not be written (typically a full
	// data_dir). The findings are then served from memory; it closes on its
	// own once writes land again.
	FindingFindingsPersistFailed FindingKind = "FINDINGS_PERSIST_FAILED"
	// FindingEventReadFailed: the retry budget could not be read back from events (S13).
	FindingEventReadFailed FindingKind = "EVENT_READ_FAILED"
	// FindingDumpCleanupPersistFailed: the pending Binlog Dump KILL marker was not written (S3).
	FindingDumpCleanupPersistFailed FindingKind = "DUMP_CLEANUP_PERSIST_FAILED"
	// FindingDiskListFailed: on-disk backup directories could not be listed (S15).
	FindingDiskListFailed FindingKind = "DISK_LIST_FAILED"
	// FindingUploadUnverified: an object was stored but could not be read back to compare (S9).
	FindingUploadUnverified FindingKind = "UPLOAD_UNVERIFIED"
	// FindingUploadRecordFailed: an upload result could not be written to the catalog (S8).
	FindingUploadRecordFailed FindingKind = "UPLOAD_RECORD_FAILED"
	// FindingRetentionRecordFailed: retention could not record what it did or refused (S5/S6).
	FindingRetentionRecordFailed FindingKind = "RETENTION_RECORD_FAILED"
	// FindingRetryLoopFailed: a background upload retry pass did not run to the end (S7).
	FindingRetryLoopFailed FindingKind = "RETRY_LOOP_FAILED"
	// FindingDumpStalled: the dump received nothing, not even a source heartbeat, for the read timeout (S12).
	FindingDumpStalled FindingKind = "DUMP_STALLED"
	// FindingOpenTailCutFailed: an unfinished transaction at the end of the open segment could not be cut (S4).
	FindingOpenTailCutFailed FindingKind = "OPEN_TAIL_CUT_FAILED"
	// FindingMetricSourceFailed: a per-task metric could not be computed (S10/S11).
	FindingMetricSourceFailed FindingKind = "METRIC_SOURCE_FAILED"
	// FindingTransitionStalled: a task sat in STARTING or STOPPING with no dump driving it; the watchdog handed it to the control loop.
	FindingTransitionStalled FindingKind = "TASK_TRANSITION_STALLED"
	// FindingSegmentUnreadable: a sealed segment has a corrupt event size; /replay and /window stop before it (#358).
	FindingSegmentUnreadable FindingKind = "SEGMENT_UNREADABLE"
	// FindingSegmentOverlap: a sealed source file is in the task directory twice with the same events, or one copy cannot be read; /replay and /window stop before both (#370).
	FindingSegmentOverlap FindingKind = "SEGMENT_OVERLAP"
	// FindingTaskRecordsFailed: the standalone task record file cannot be read or written (ADR 0010); subject key_missing, key_invalid, wrong_key, corrupt or write.
	FindingTaskRecordsFailed FindingKind = "TASK_RECORDS_FAILED"
	// FindingMetaUnreachable: this node could not reach the metadata store (a
	// ping or read timed out, or the connection failed) for
	// meta_unreachable_after (S17, design 0009 §25); task _cluster, subject
	// the node id.
	FindingMetaUnreachable FindingKind = "META_UNREACHABLE"
	// FindingHealthStoreUnavailable: the evaluator is on and migration 000009
	// cannot be read or written (S18, design §27, #436).
	FindingHealthStoreUnavailable FindingKind = "HEALTH_STORE_UNAVAILABLE"
	// FindingMetaLockStalled: a task_leases row lock outlived one session-kill
	// bound twice (ADR 0011). Task _cluster, subject the task id. Not a
	// managed health kind, so Reconcile does not close it.
	FindingMetaLockStalled FindingKind = "META_LOCK_STALLED"
)

// FindingSeverity is CRITICAL (backup data or its record is at risk) or WARNING.
type FindingSeverity string

const (
	FindingCritical FindingSeverity = "CRITICAL"
	FindingWarning  FindingSeverity = "WARNING"
)

// ErrFindingStoreAbsent means the store has no task_findings table (schema 6).
// The sink then keeps findings in memory and /metrics only.
var ErrFindingStoreAbsent = errors.New("task_findings table is absent; run ./migrate up to persist findings")

// ClusterFindingTask is the task id used for findings that belong to no task
// (for example a whole background pass that failed).
const ClusterFindingTask = "_cluster"

// Finding is one persisted failure. (TaskID, Kind, Subject) is the key: the
// same failure reported again only moves LastSeen and Count.
type Finding struct {
	TaskID     string          `json:"task_id"`
	Kind       FindingKind     `json:"kind"`
	Subject    string          `json:"subject"`
	Severity   FindingSeverity `json:"severity"`
	Detail     string          `json:"detail"`
	FirstSeen  time.Time       `json:"first_seen"`
	LastSeen   time.Time       `json:"last_seen"`
	Count      int64           `json:"count"`
	ResolvedAt *time.Time      `json:"resolved_at,omitempty"`
}

// Open reports whether the finding has not been resolved.
func (f Finding) Open() bool { return f.ResolvedAt == nil }

// FindingStore persists findings. UpsertFinding writes the row for the key as
// given: severity, detail, last_seen, count, and resolved_at (NULL while open).
// first_seen keeps the earlier of the stored and the given value. A resolve is
// the same write with ResolvedAt set, so a report and its resolve that were
// coalesced while the store was down still leave the history row.
type FindingStore interface {
	UpsertFinding(ctx context.Context, f Finding) error
	ListFindings(ctx context.Context, taskID string, includeResolved bool, limit int) ([]Finding, error)
	ListOpenFindings(ctx context.Context, limit int) ([]Finding, error)
}

type findingKey struct {
	task    string
	kind    FindingKind
	subject string
}

type findingOp struct {
	finding Finding
	resolve bool
}

// FindingStats is the sink's own state for /metrics.
type FindingStats struct {
	Reported    map[FindingKind]int64
	SinkErrors  int64
	PendingOps  int
	Open        []Finding
	StoreActive bool
}

const maxPendingFindingOps = 1000

// findingBook is the in-process half of the sink.
type findingBook struct {
	mu         sync.Mutex
	open       map[findingKey]Finding
	reported   map[FindingKind]int64
	sinkErrors int64
	pending    []findingOp
	wake       chan struct{}
	store      FindingStore
	events     EventStore
	timeout    time.Duration
	started    bool
	sinkDone   chan struct{} // closed when the writer has made its last flush
	lastErr    error         // the last failed write, cleared when nothing is left pending
	lastErrAt  time.Time
	now        func() time.Time
}

func newFindingBook() *findingBook {
	return &findingBook{
		open:     make(map[findingKey]Finding),
		reported: make(map[FindingKind]int64),
		wake:     make(chan struct{}, 1),
		timeout:  5 * time.Second,
		now:      time.Now,
	}
}

// WithFindingStore sets where findings are persisted. Without one the sink
// still keeps open findings in memory and in /metrics.
func WithFindingStore(store FindingStore) Option {
	return func(s *Scheduler) {
		s.findings.store = store
	}
}

// Report records a failure. It never blocks on I/O and is safe with or
// without s.mu held: the store write happens on the sink's writer.
//
// A kind the health evaluator manages has one writer (design §5.6): while
// the evaluator owns it, a signal kind becomes a SignalFailure and a
// derived kind is dropped. New code calls SignalFailure (lintguard).
func (s *Scheduler) Report(f Finding) {
	if s.healthEval.Load().manages(f.Kind) {
		if isSignalKind(f.Kind) {
			s.SignalFailure(f)
		}
		return
	}
	s.findings.report(f)
}

// Resolve marks a finding resolved when this process has it open. Resolving a
// finding this process never saw is a no-op, so success paths stay cheap.
func (s *Scheduler) Resolve(taskID string, kind FindingKind, subject string) {
	s.ResolveWithReason(taskID, kind, subject, "")
}

// ResolveWithReason marks a finding resolved. A non-empty reason replaces
// the row's detail, so the FINDING_RESOLVED event records why it closed
// (design 0009 phase 3 §10: "row removed" and the other Closure reasons).
func (s *Scheduler) ResolveWithReason(taskID string, kind FindingKind, subject, reason string) {
	if s.healthEval.Load().manages(kind) {
		if isSignalKind(kind) {
			s.SignalSuccess(taskID, kind, subject)
		}
		return
	}
	s.findings.resolve(taskID, kind, subject, reason)
}

func (b *findingBook) report(f Finding) {
	f.TaskID = strings.TrimSpace(f.TaskID)
	if f.TaskID == "" {
		f.TaskID = ClusterFindingTask
	}
	if f.Severity == "" {
		f.Severity = FindingCritical
	}
	now := b.now()
	key := findingKey{f.TaskID, f.Kind, f.Subject}
	b.mu.Lock()
	prev, ok := b.open[key]
	if ok {
		f.FirstSeen = prev.FirstSeen
		f.Count = prev.Count + 1
	} else {
		if f.FirstSeen.IsZero() || f.FirstSeen.After(now) {
			f.FirstSeen = now // a caller may name an earlier first failure (META_UNREACHABLE, §25)
		}
		f.Count = 1
	}
	f.LastSeen = now
	f.ResolvedAt = nil
	b.open[key] = f
	b.reported[f.Kind]++
	b.enqueueLocked(findingOp{finding: f})
	b.mu.Unlock()
	b.kick()
}

func (b *findingBook) resolve(taskID string, kind FindingKind, subject, reason string) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		taskID = ClusterFindingTask
	}
	key := findingKey{taskID, kind, subject}
	b.mu.Lock()
	f, ok := b.open[key]
	if !ok {
		b.mu.Unlock()
		return
	}
	delete(b.open, key)
	at := b.now()
	f.ResolvedAt = &at
	if reason != "" {
		f.Detail = reason
	}
	b.enqueueLocked(findingOp{finding: f, resolve: true})
	b.mu.Unlock()
	b.kick()
}

// recordClosed enqueues one already-resolved row. It does not pass through
// the open book, so a gap that could not be stored still leaves a single
// FINDING_RESOLVED and no REPORTED (design §27 D3, same shape as §25.7).
func (b *findingBook) recordClosed(f Finding, at time.Time) {
	f.TaskID = strings.TrimSpace(f.TaskID)
	if f.TaskID == "" {
		f.TaskID = ClusterFindingTask
	}
	if f.Severity == "" {
		f.Severity = FindingCritical
	}
	if at.IsZero() {
		at = b.now()
	}
	if f.FirstSeen.IsZero() || f.FirstSeen.After(at) {
		f.FirstSeen = at
	}
	f.LastSeen = at
	f.ResolvedAt = &at
	if f.Count == 0 {
		f.Count = 1
	}
	b.mu.Lock()
	delete(b.open, findingKey{f.TaskID, f.Kind, f.Subject})
	b.enqueueLocked(findingOp{finding: f, resolve: true})
	b.mu.Unlock()
	b.kick()
}

// forget drops a key the health evaluator closed: from the open book and
// from the pending writes, so a queued legacy upsert cannot reopen it.
func (b *findingBook) forget(taskID string, kind FindingKind, subject string) {
	key := findingKey{strings.TrimSpace(taskID), kind, subject}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.open, key)
	kept := b.pending[:0]
	for _, op := range b.pending {
		if (findingKey{op.finding.TaskID, op.finding.Kind, op.finding.Subject}) == key && !op.resolve {
			continue
		}
		kept = append(kept, op)
	}
	b.pending = kept
}

// enqueueLocked keeps the newest op per key and drops the oldest op beyond the cap.
func (b *findingBook) enqueueLocked(op findingOp) {
	key := findingKey{op.finding.TaskID, op.finding.Kind, op.finding.Subject}
	for i, existing := range b.pending {
		if (findingKey{existing.finding.TaskID, existing.finding.Kind, existing.finding.Subject}) == key {
			b.pending = append(b.pending[:i], b.pending[i+1:]...)
			break
		}
	}
	b.pending = append(b.pending, op)
	if len(b.pending) > maxPendingFindingOps {
		b.pending = b.pending[len(b.pending)-maxPendingFindingOps:]
		b.sinkErrors++
	}
}

func (b *findingBook) kick() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// findingEvented: a finding write that also gets a task event. _cluster
// rows have no task timeline, except the RESOLVED of META_UNREACHABLE: in
// cluster mode its row is reused by the node's next outage, so that event,
// which names the whole window, is the only record left of the earlier
// one. Its REPORTED writes only land after the outage, often twice (a
// retried and a newer copy), so they get no event (design 0009 §25.8 R4).
func findingEvented(op findingOp) bool {
	f := op.finding
	if f.Kind == FindingEventPersistFailed {
		return false
	}
	if f.TaskID == ClusterFindingTask {
		return op.resolve && (f.Kind == FindingMetaUnreachable || f.Kind == FindingHealthStoreUnavailable)
	}
	return true
}

// flush writes every pending op once. Failed ops stay pending (unless a newer
// op for the same key arrived meanwhile) and count as sink errors.
func (b *findingBook) flush(ctx context.Context) {
	b.mu.Lock()
	store, events := b.store, b.events
	batch := b.pending
	b.pending = nil
	b.mu.Unlock()
	if store == nil {
		return
	}
	var failed []findingOp
	var failedErr error
	for _, op := range batch {
		wctx, cancel := context.WithTimeout(ctx, b.timeout)
		err := store.UpsertFinding(wctx, op.finding)
		if errors.Is(err, ErrFindingStoreAbsent) {
			cancel()
			b.mu.Lock()
			if b.store != nil {
				log.Printf("%v", err)
			}
			b.store = nil
			b.mu.Unlock()
			return
		}
		if err == nil && events != nil && findingEvented(op) {
			// The finding row is the record; the event is the timeline entry.
			// An event failure here is counted, not reported, so it cannot loop.
			evType, msg := "FINDING_REPORTED", string(op.finding.Kind)+" "+op.finding.Subject
			detail, at := op.finding.Detail, b.now()
			if op.resolve {
				evType = "FINDING_RESOLVED"
				if op.finding.ResolvedAt != nil && op.finding.Kind == FindingMetaUnreachable {
					// The outage window, dated at the first answer (§25.8 R4).
					at = *op.finding.ResolvedAt
					detail = fmt.Sprintf("%s; open from %s to %s (%s)", detail, op.finding.FirstSeen.UTC().Format(time.RFC3339Nano),
						at.UTC().Format(time.RFC3339Nano), at.Sub(op.finding.FirstSeen).Round(time.Millisecond))
				}
			}
			if evErr := events.AppendEvent(wctx, TaskEvent{TaskID: op.finding.TaskID, Type: evType, Message: strings.TrimSpace(msg), Detail: detail, Time: at}); evErr != nil {
				b.mu.Lock()
				b.sinkErrors++
				b.mu.Unlock()
			}
		}
		cancel()
		if err != nil {
			failed = append(failed, op)
			failedErr = err
		}
	}
	if len(failed) == 0 {
		b.mu.Lock()
		if len(b.pending) == 0 {
			b.lastErr = nil
		}
		b.mu.Unlock()
		return
	}
	b.mu.Lock()
	b.sinkErrors += int64(len(failed))
	if b.lastErr == nil {
		b.lastErrAt = b.now()
	}
	b.lastErr = failedErr
	newer := make(map[findingKey]bool, len(b.pending))
	for _, op := range b.pending {
		newer[findingKey{op.finding.TaskID, op.finding.Kind, op.finding.Subject}] = true
	}
	var keep []findingOp
	for _, op := range failed {
		if !newer[findingKey{op.finding.TaskID, op.finding.Kind, op.finding.Subject}] {
			keep = append(keep, op)
		}
	}
	b.pending = append(keep, b.pending...)
	if len(b.pending) > maxPendingFindingOps {
		b.pending = b.pending[len(b.pending)-maxPendingFindingOps:]
	}
	b.mu.Unlock()
}

// StartFindingSink runs the sink writer until ctx ends. It writes on every
// Report/Resolve and retries failed writes every interval. Each retry tick
// also republishes task rows whose last write failed (S2).
// WaitFindingSink blocks until the writer started by StartFindingSink has
// made its final flush after its context ended, or until timeout. Shutdown
// calls it so the last findings land before the process (or a test's
// data_dir cleanup) goes away. It returns at once when no writer started.
func (s *Scheduler) WaitFindingSink(timeout time.Duration) bool {
	b := s.findings
	b.mu.Lock()
	done := b.sinkDone
	b.mu.Unlock()
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (s *Scheduler) StartFindingSink(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	b := s.findings
	b.mu.Lock()
	if b.started {
		b.mu.Unlock()
		return
	}
	b.started = true
	b.sinkDone = make(chan struct{})
	done := b.sinkDone
	if b.events == nil {
		b.events = s.eventStore
	}
	b.mu.Unlock()
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				b.flush(context.Background())
				return
			case <-b.wake:
				b.flush(ctx)
			case <-ticker.C:
				s.republishUnpublished()
				s.reconcileStalledTransitions()
				s.reconcileUnadoptedLegacy()
				b.flush(ctx)
			}
		}
	}()
}

// FlushFindings writes pending findings now (tests and shutdown).
func (s *Scheduler) FlushFindings(ctx context.Context) {
	b := s.findings
	b.mu.Lock()
	if b.events == nil {
		b.events = s.eventStore
	}
	b.mu.Unlock()
	b.flush(ctx)
}

// FindingStats is the sink snapshot for /metrics.
func (s *Scheduler) FindingStats() FindingStats {
	b := s.findings
	b.mu.Lock()
	defer b.mu.Unlock()
	out := FindingStats{
		Reported:    make(map[FindingKind]int64, len(b.reported)),
		SinkErrors:  b.sinkErrors,
		PendingOps:  len(b.pending),
		StoreActive: b.store != nil,
	}
	for k, v := range b.reported {
		out.Reported[k] = v
	}
	for _, f := range b.open {
		out.Open = append(out.Open, f)
	}
	sortFindings(out.Open)
	return out
}

// ListFindings returns a task's findings. With a store the store answers
// (it includes other processes' findings); this process's open findings that
// are not written yet are merged in so a failed sink write is still visible.
func (s *Scheduler) ListFindings(ctx context.Context, taskID string, includeResolved bool, limit int) ([]Finding, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	b := s.findings
	b.mu.Lock()
	store := b.store
	var local []Finding
	for k, f := range b.open {
		if k.task == taskID {
			local = append(local, f)
		}
	}
	b.mu.Unlock()
	var stored []Finding
	if store != nil {
		var err error
		stored, err = store.ListFindings(ctx, taskID, includeResolved, limit)
		if errors.Is(err, ErrFindingStoreAbsent) {
			stored, err = nil, nil
		}
		if err != nil {
			return nil, err
		}
	}
	managed, err := s.healthEval.Load().openManagedFindings(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("managed findings of task %s unreadable: %w", taskID, err)
	}
	local = append(local, managed...)
	if taskID == ClusterFindingTask {
		if f, ok := s.persistFailureFinding(); ok {
			local = append(local, f)
		}
		if f, ok := s.metaReachFinding(); ok {
			local = append(local, f)
		}
		if f, ok := s.healthStoreFinding(); ok {
			local = append(local, f)
		}
	}
	return mergeFindings(stored, local, limit), nil
}

// OpenFindings returns open findings for every task (store plus this process).
func (s *Scheduler) OpenFindings(ctx context.Context, limit int) ([]Finding, error) {
	if limit <= 0 {
		limit = 1000
	}
	b := s.findings
	b.mu.Lock()
	store := b.store
	local := make([]Finding, 0, len(b.open))
	for _, f := range b.open {
		local = append(local, f)
	}
	b.mu.Unlock()
	var stored []Finding
	var err error
	if store != nil {
		stored, err = store.ListOpenFindings(ctx, limit)
		if errors.Is(err, ErrFindingStoreAbsent) {
			stored, err = nil, nil
		}
	}
	if e := s.healthEval.Load(); e != nil {
		seen := map[string]bool{}
		ids := make([]string, 0)
		for _, t := range s.ListTasks() {
			if seen[t.ID] {
				continue
			}
			seen[t.ID] = true
			ids = append(ids, t.ID)
		}
		// _cluster has no task row. Standalone evaluates it directly;
		// in cluster the _health holder writes HEALTH_STALE there (§28).
		if !seen[ClusterFindingTask] {
			ids = append(ids, ClusterFindingTask)
		}
		var unreadable []string
		var firstErr error
		for _, id := range ids {
			managed, merr := e.openManagedFindings(ctx, id)
			if merr != nil {
				// A missing 000009 on _cluster is already HEALTH_STORE_UNAVAILABLE (§28).
				if id == ClusterFindingTask && e.cfg.Cluster && errors.Is(merr, healthstore.ErrSchemaAbsent) {
					continue
				}
				// Not an empty set: the task's managed findings are unknown (§25 D1).
				unreadable = append(unreadable, id)
				if firstErr == nil {
					firstErr = merr
				}
				continue
			}
			local = append(local, managed...)
		}
		if len(unreadable) > 0 {
			merr := fmt.Errorf("managed findings of task %s unreadable: %w", strings.Join(unreadable, ", "), firstErr)
			if err == nil {
				err = merr
			} else {
				err = fmt.Errorf("%w; %v", err, merr)
			}
		}
	}
	if f, ok := s.persistFailureFinding(); ok {
		local = append(local, f)
	}
	if f, ok := s.metaReachFinding(); ok {
		local = append(local, f)
	}
	if f, ok := s.healthStoreFinding(); ok {
		local = append(local, f)
	}
	return mergeFindings(stored, local, limit), err
}

func mergeFindings(stored, local []Finding, limit int) []Finding {
	seen := make(map[findingKey]int, len(stored))
	out := append([]Finding(nil), stored...)
	for i, f := range out {
		seen[findingKey{f.TaskID, f.Kind, f.Subject}] = i
	}
	for _, f := range local {
		key := findingKey{f.TaskID, f.Kind, f.Subject}
		if i, ok := seen[key]; ok {
			if f.LastSeen.After(out[i].LastSeen) {
				out[i] = f
			}
			continue
		}
		out = append(out, f)
	}
	sortFindings(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func sortFindings(items []Finding) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Open() != items[j].Open() {
			return items[i].Open()
		}
		if !items[i].LastSeen.Equal(items[j].LastSeen) {
			return items[i].LastSeen.After(items[j].LastSeen)
		}
		if items[i].TaskID != items[j].TaskID {
			return items[i].TaskID < items[j].TaskID
		}
		return string(items[i].Kind)+items[i].Subject < string(items[j].Kind)+items[j].Subject
	})
}

// FindingBinder is the runner hook into the finding sink. The runner reports
// failures it handles itself (upload records, retention records, stalls).
type FindingBinder interface {
	BindFindings(report func(Finding), resolve func(taskID string, kind FindingKind, subject string))
}

// UploadOutcomeFinding is S9: an object that was stored but could not be read
// back to compare is UPLOADED with an empty checksum. It looks done and is
// not verified. ok is false when the outcome needs no finding.
func UploadOutcomeFinding(file BinlogFile) (Finding, bool) {
	if !strings.EqualFold(strings.TrimSpace(file.UploadState), "UPLOADED") || strings.TrimSpace(file.Checksum) != "" {
		return Finding{}, false
	}
	return Finding{
		TaskID:   file.TaskID,
		Kind:     FindingUploadUnverified,
		Subject:  file.FileName,
		Severity: FindingWarning,
		Detail:   "object " + file.ObjectKey + " was stored but the uploader cannot read it back; it is not verified against the local file",
	}, true
}

// NoteUploadOutcome reports or resolves the per-file upload findings after a
// sealed upload or verify: S8 when the catalog write failed, S9 when the
// object is not verified.
func NoteUploadOutcome(signalFail func(Finding), signalOK func(string, FindingKind, string), file BinlogFile, recordErr error) {
	if signalFail == nil || signalOK == nil {
		return
	}
	if recordErr != nil {
		// The row keeps its previous state (seal writes UPLOAD_FAILED before
		// the put), so the retry still sees it; the outcome itself is lost.
		signalFail(Finding{TaskID: file.TaskID, Kind: FindingUploadRecordFailed, Subject: file.FileName, Severity: FindingWarning,
			Detail: "upload result (" + strings.TrimSpace(file.UploadState) + ") not written to the binlog catalog; the row keeps its previous state: " + recordErr.Error()})
	} else {
		signalOK(file.TaskID, FindingUploadRecordFailed, file.FileName)
	}
	if f, ok := UploadOutcomeFinding(file); ok {
		signalFail(f)
	} else if strings.EqualFold(strings.TrimSpace(file.UploadState), "UPLOADED") {
		signalOK(file.TaskID, FindingUploadUnverified, file.FileName)
	}
}

// FindingCounter is an optional FindingStore method: open findings per task,
// counted in the store (GROUP BY), not from a capped list.
type FindingCounter interface {
	CountOpenFindings(ctx context.Context) (map[string]int, error)
}

// OpenFindingCounts returns the number of open findings per task. The store
// count covers every process; this process's open findings cover writes the
// store has not taken yet. Per task the larger of the two is used, so a
// finding is not counted twice. A store read error still returns this
// process's counts.
func (s *Scheduler) OpenFindingCounts(ctx context.Context) (map[string]int, error) {
	b := s.findings
	b.mu.Lock()
	store := b.store
	out := make(map[string]int)
	for key := range b.open {
		out[key.task]++
	}
	b.mu.Unlock()
	if store == nil {
		return out, nil
	}
	var stored map[string]int
	var err error
	if counter, ok := store.(FindingCounter); ok {
		stored, err = counter.CountOpenFindings(ctx)
	} else {
		var open []Finding
		open, err = store.ListOpenFindings(ctx, 0)
		stored = make(map[string]int, len(open))
		for _, f := range open {
			stored[f.TaskID]++
		}
	}
	if errors.Is(err, ErrFindingStoreAbsent) {
		return out, nil
	}
	for task, n := range stored {
		if n > out[task] {
			out[task] = n
		}
	}
	return out, err
}

// persistFailureFinding is FINDINGS_PERSIST_FAILED while the evaluator's
// health store or the findings file is failing to write (#395 QA P2).
func (s *Scheduler) persistFailureFinding() (Finding, bool) {
	return s.persistFailureFindingFrom(true)
}

// persistFailureFindingFrom: prune false reads no task row (design §25).
func (s *Scheduler) persistFailureFindingFrom(prune bool) (Finding, bool) {
	var parts []string
	var first time.Time
	ids, herr, hsince := s.healthEval.Load().persistFailureFrom(prune)
	if herr != nil {
		parts = append(parts, fmt.Sprintf("health state for %s could not be saved: %v", strings.Join(ids, ", "), herr))
		first = hsince
	}
	b := s.findings
	b.mu.Lock()
	ferr, fsince, pending := b.lastErr, b.lastErrAt, len(b.pending)
	b.mu.Unlock()
	if ferr != nil {
		parts = append(parts, fmt.Sprintf("%d finding write(s) could not be saved: %v", pending, ferr))
		if first.IsZero() || fsince.Before(first) {
			first = fsince
		}
	}
	if len(parts) == 0 {
		return Finding{}, false
	}
	now := time.Now()
	return Finding{TaskID: ClusterFindingTask, Kind: FindingFindingsPersistFailed, Severity: FindingCritical, FirstSeen: first, LastSeen: now, Count: 1,
		Detail: strings.Join(parts, "; ") + ". Findings and health are served from memory and are lost on a restart until a write lands; free space in data_dir"}, true
}

// Health summary states for GET /api/health.
const (
	HealthOK       = "ok"
	HealthDegraded = "degraded"
)

// HealthSummary is the process health GET /api/health serves.
// Detail is operator context that does not change Status: a startup
// object-storage probe failure is listed here while status stays ok.
type HealthSummary struct {
	Status   string   `json:"status"`
	Reasons  []string `json:"reasons,omitempty"`
	Detail   []string `json:"detail,omitempty"`
	SideKill string   `json:"side_kill,omitempty"`
	// LockDetail is "reduced" when the meta account cannot read lock tables.
	// Stall detection still uses server error 1205, so this is not a reason.
	LockDetail string `json:"lock_detail,omitempty"`
}

// SetVerifyStorageDetail records a startup object-storage probe failure.
// HealthSummary copies it into Detail. A TLS transport failure degrades
// status. Any other probe text does not.
func (s *Scheduler) SetVerifyStorageDetail(detail string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.verifyStorageDetail = detail
	s.mu.Unlock()
}

// SetVerifyObjectDetail records consecutive object-pass failures.
// An empty detail clears it. HealthSummary copies it into Detail and does
// not change Status. The startup probe detail is left in place.
func (s *Scheduler) SetVerifyObjectDetail(detail string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.verifyObjectDetail = detail
	s.mu.Unlock()
}

func (s *Scheduler) noteUploadTLS(detail string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.uploadTLSDetail = detail
	s.mu.Unlock()
}

func (s *Scheduler) clearUploadTLS() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.uploadTLSDetail = ""
	s.mu.Unlock()
}

// tlsTransportLine is the first probe or upload error that is a TLS mismatch.
// NoSuchBucket and other storage errors are not this line.
func tlsTransportLine(lines ...string) string {
	for _, line := range lines {
		if upload.TLSTransportFailure(line) {
			return line
		}
	}
	return ""
}

func (s *Scheduler) verifyStorageDetails() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	startup := s.verifyStorageDetail
	runtime := s.verifyObjectDetail
	s.mu.Unlock()
	var out []string
	if startup != "" {
		out = append(out, startup)
	}
	if runtime != "" {
		out = append(out, runtime)
	}
	if e := s.healthEval.Load(); e != nil {
		out = append(out, e.directoryUnreadableLines()...)
	}
	return out
}

// directoryNodePhrase is the chain_owner or node token from a finding
// detail, prefixed for a health reason. chain_owner wins when both appear.
// Empty when the detail does not name a directory node.
func directoryNodePhrase(detail string) string {
	token := directoryNodeToken(detail)
	if token == "" {
		return ""
	}
	return ", " + token
}

func directoryNodeToken(detail string) string {
	for _, key := range []string{"chain_owner=", "node="} {
		i := strings.Index(detail, key)
		if i < 0 {
			continue
		}
		rest := detail[i:]
		end := strings.IndexAny(rest, " \t,)")
		if end < 0 {
			end = len(rest)
		}
		if end > 0 {
			return rest[:end]
		}
	}
	return ""
}

// HealthSummary is degraded while any CRITICAL finding is open, read from
// memory as GET /api/findings is, so a finding that could not be persisted
// still counts (#395 QA P2).
//
// Every metadata read shares healthReadBudget (1s) and at most one read is in
// flight; past the budget the summary is built from memory and names
// META_UNREACHABLE, so a blackholed meta store cannot hang the probe (§25).
func (s *Scheduler) HealthSummary(ctx context.Context) HealthSummary {
	open, err, timedOut := s.boundedOpenFindings(ctx)
	if timedOut {
		open = s.memoryOpenFindings()
	}
	var reasons []string
	for _, f := range open {
		if f.Severity != FindingCritical || !f.Open() {
			continue
		}
		r := string(f.Kind)
		if f.Subject != "" {
			r += "/" + f.Subject
		}
		reasons = append(reasons, r+" (task "+f.TaskID+directoryNodePhrase(f.Detail)+")")
	}
	switch {
	case isHealthReadTimeout(err):
		reasons = append(reasons, "META_UNREACHABLE: "+errHealthReadTimeout.Error())
	case err != nil:
		reasons = append(reasons, "findings store unreadable: "+err.Error())
	}
	if reason, ok := s.healthStoreDegradedReason(); ok {
		reasons = append(reasons, reason)
	}
	detail := s.verifyStorageDetails()
	s.mu.Lock()
	uploadTLS := s.uploadTLSDetail
	s.mu.Unlock()
	if line := tlsTransportLine(append(append([]string{}, detail...), uploadTLS)...); line != "" {
		reasons = append(reasons, "UPLOAD_TLS: "+line)
		if uploadTLS != "" && !strings.Contains(strings.Join(detail, "\n"), uploadTLS) {
			detail = append(detail, uploadTLS)
		}
	}
	sum := HealthSummary{Status: HealthOK, Detail: detail}
	if view, ok := s.store.(interface {
		SessionDegraded() (bool, string)
		SideKill() string
	}); ok {
		if degraded, why := view.SessionDegraded(); degraded {
			if why == "" {
				why = "@@session.wait_timeout does not match the session-kill bound"
			}
			reasons = append(reasons, "session wait_timeout mismatch: "+why)
		}
		sum.SideKill = view.SideKill()
	}
	if view, ok := s.store.(interface{ LockDetail() string }); ok {
		sum.LockDetail = view.LockDetail()
	}
	if len(reasons) == 0 {
		return sum
	}
	sort.Strings(reasons)
	return HealthSummary{Status: HealthDegraded, Reasons: reasons, Detail: detail, SideKill: sum.SideKill, LockDetail: sum.LockDetail}
}
