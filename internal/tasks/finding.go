// Package tasks provides module-level functionality for tasks.
// input: failure reports from the scheduler, the runner, and background loops
// output: the finding sink (ADR 0009): an in-process book of open findings and counters, written to a FindingStore by one background writer that keeps failed writes and retries them; Report never blocks on I/O
// pos: the single exit for failures that are not returned to a caller that already makes them visible
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// FindingKind is a closed set. Every kind has a fault-injection test in
// finding_kinds_test.go; adding a kind without one fails that test.
type FindingKind string

const (
	// FindingTaskPersistFailed: a task state transition was not written to the store (S2/S3).
	FindingTaskPersistFailed FindingKind = "TASK_PERSIST_FAILED"
	// FindingEventPersistFailed: a task event was not written to the event store (S1).
	FindingEventPersistFailed FindingKind = "EVENT_PERSIST_FAILED"
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
func (s *Scheduler) Report(f Finding) {
	s.findings.report(f)
}

// Resolve marks a finding resolved when this process has it open. Resolving a
// finding this process never saw is a no-op, so success paths stay cheap.
func (s *Scheduler) Resolve(taskID string, kind FindingKind, subject string) {
	s.findings.resolve(taskID, kind, subject)
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
		f.FirstSeen = now
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

func (b *findingBook) resolve(taskID string, kind FindingKind, subject string) {
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
	b.enqueueLocked(findingOp{finding: f, resolve: true})
	b.mu.Unlock()
	b.kick()
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
		if err == nil && events != nil && op.finding.TaskID != ClusterFindingTask && op.finding.Kind != FindingEventPersistFailed {
			// The finding row is the record; the event is the timeline entry.
			// An event failure here is counted, not reported, so it cannot loop.
			evType, msg := "FINDING_REPORTED", string(op.finding.Kind)+" "+op.finding.Subject
			if op.resolve {
				evType = "FINDING_RESOLVED"
			}
			if evErr := events.AppendEvent(wctx, TaskEvent{TaskID: op.finding.TaskID, Type: evType, Message: strings.TrimSpace(msg), Detail: op.finding.Detail, Time: b.now()}); evErr != nil {
				b.mu.Lock()
				b.sinkErrors++
				b.mu.Unlock()
			}
		}
		cancel()
		if err != nil {
			failed = append(failed, op)
		}
	}
	if len(failed) == 0 {
		return
	}
	b.mu.Lock()
	b.sinkErrors += int64(len(failed))
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
	if b.events == nil {
		b.events = s.eventStore
	}
	b.mu.Unlock()
	go func() {
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
func NoteUploadOutcome(report func(Finding), resolve func(string, FindingKind, string), file BinlogFile, recordErr error) {
	if report == nil || resolve == nil {
		return
	}
	if recordErr != nil {
		// The row keeps its previous state (seal writes UPLOAD_FAILED before
		// the put), so the retry still sees it; the outcome itself is lost.
		report(Finding{TaskID: file.TaskID, Kind: FindingUploadRecordFailed, Subject: file.FileName, Severity: FindingWarning,
			Detail: "upload result (" + strings.TrimSpace(file.UploadState) + ") not written to the binlog catalog; the row keeps its previous state: " + recordErr.Error()})
	} else {
		resolve(file.TaskID, FindingUploadRecordFailed, file.FileName)
	}
	if f, ok := UploadOutcomeFinding(file); ok {
		report(f)
	} else if strings.EqualFold(strings.TrimSpace(file.UploadState), "UPLOADED") {
		resolve(file.TaskID, FindingUploadUnverified, file.FileName)
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
