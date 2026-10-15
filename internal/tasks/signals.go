// Package tasks provides module-level functionality for tasks.
// input: hot-path failure and success reports (SignalFailure/SignalSuccess, and Report/Resolve for a kind the evaluator manages), the task rows, the task directory, the catalog, and the event store
// output: the evaluator's signal book (per task, per kind+subject, one process-wide monotonic seq, the lease epoch stamped at the first pass after a failure) and the P2-4 facts built from it: task.state (state, desired, FailedCurrent, OwnedElsewhere, RunHere, Unadopted for a standalone leftover directory (#385), and the source liveness clock from liveness.go; DUMP_STALLED is derived, so its runner signal is dropped while the evaluator owns it), signals, the open keys, probes (UPLOAD_RECORD_FAILED, OPEN_TAIL_CUT_FAILED, DISK_LIST_FAILED, EVENT_READ_FAILED) and stale signals; while the evaluator does not own the managed kinds (off, shadow, schema without 000009, or a _cluster signal in cluster mode) the legacy Report/Resolve path writes them unchanged
// pos: ADR 0009 phase 2 P2-4 (docs/design/0009-phase2-health-evaluator.md §5.5, §5.6, §6); the only way a signal kind reaches task_findings while the evaluator is on
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"binlog_server/internal/health"
)

// ReasonSignal marks a task dirty because a hot-path signal arrived.
const ReasonSignal EvalReason = "SIGNAL"

// ReasonLiveness marks a task dirty because its source liveness clock crossed a threshold.
const ReasonLiveness EvalReason = "LIVENESS"

// Kinds added by P2-4; both are derived from the task row, nothing reports them.
const (
	// FindingSourceUnreachable: the task has stayed in STARTING/RETRY_BACKOFF with desired RUN past health.unreachable_after_sec.
	FindingSourceUnreachable FindingKind = "SOURCE_UNREACHABLE"
	// FindingTaskFailed: the task is FAILED for the current spec revision; the subject is the failure class.
	FindingTaskFailed FindingKind = "TASK_FAILED"
)

// DefaultSignalStaleAfter is health.signal_stale_after: an open signal row
// with no failure seen by this process for this long is shown as INFO.
const DefaultSignalStaleAfter = 24 * time.Hour

var signalSeq atomic.Uint64

// signalEntry is one signal of the book.
type signalEntry struct {
	st          health.SignalState
	lastFailure time.Time
	stamped     uint64 // FailureSeq whose epoch was stamped
}

// isSignalKind: a managed kind fed by hot-path signals (not derived from
// the task row or the chain).
func isSignalKind(k FindingKind) bool {
	switch health.Kind(k) {
	case health.TaskPersistFailed, health.MetadataWriteBlocked, health.EventPersistFailed, health.EventReadFailed,
		health.DumpCleanupPersistFailed, health.DiskListFailed, health.UploadUnverified, health.UploadRecordFailed,
		health.RetentionRecordFailed, health.RetryLoopFailed, health.OpenTailCutFailed, health.MetricSourceFailed:
		return true
	}
	return false
}

// manages: the evaluator owns kind, so the legacy sink must not write it.
func (e *Evaluator) manages(kind FindingKind) bool {
	return e.ownsManaged() && e.reg[health.Kind(kind)].Managed
}

// ownsSignal: the evaluator, not the legacy sink, writes this kind for this task.
func (e *Evaluator) ownsSignal(taskID string, kind FindingKind) bool {
	if !e.ownsManaged() || !e.reg[health.Kind(kind)].Managed {
		return false
	}
	if normTask(taskID) == ClusterFindingTask && e.cfg.Cluster {
		return false // the _health holder evaluates _cluster from P2-6
	}
	return true
}

func normTask(id string) string {
	if id = strings.TrimSpace(id); id == "" {
		return ClusterFindingTask
	}
	return id
}

// Signal records one observation of a signal kind. Never blocks on I/O;
// safe with s.mu held.
func (e *Evaluator) Signal(taskID string, kind FindingKind, subject string, ok bool, detail string) {
	if e == nil || e.cfg.Mode == EvaluatorOff {
		return
	}
	taskID = normTask(taskID)
	seq := signalSeq.Add(1)
	now := e.cfg.Now()
	key := health.SignalKey{Kind: health.Kind(kind), Subject: subject}
	e.mu.Lock()
	if e.signals == nil {
		e.signals = map[string]map[health.SignalKey]*signalEntry{}
	}
	m := e.signals[taskID]
	if m == nil {
		m = map[health.SignalKey]*signalEntry{}
		e.signals[taskID] = m
	}
	en := m[key]
	if en == nil {
		en = &signalEntry{}
		m[key] = en
	}
	if ok {
		en.st.SuccessSeq = seq
	} else {
		en.st.FailureSeq = seq
		en.st.Failures++
		en.st.Detail = detail
		en.st.FactAt = now
		en.st.Epoch = 0
		en.lastFailure = now
	}
	e.mu.Unlock()
	e.MarkDirty(taskID, ReasonSignal)
}

// SignalFailure reports a hot-path failure of a signal kind: to the
// evaluator when it owns the kind, otherwise to the legacy sink.
func (s *Scheduler) SignalFailure(f Finding) {
	e := s.healthEval.Load()
	f.TaskID = normTask(f.TaskID)
	if e.ownsSignal(f.TaskID, f.Kind) {
		if isSignalKind(f.Kind) { // a derived kind (DUMP_STALLED) has no signal
			e.Signal(f.TaskID, f.Kind, f.Subject, false, f.Detail)
		}
		return
	}
	e.Signal(f.TaskID, f.Kind, f.Subject, false, f.Detail) // shadow mode counts it
	s.findings.report(f)
}

// SignalSuccess reports the matching success.
func (s *Scheduler) SignalSuccess(taskID string, kind FindingKind, subject string) {
	e := s.healthEval.Load()
	taskID = normTask(taskID)
	if e.ownsSignal(taskID, kind) {
		if isSignalKind(kind) {
			e.Signal(taskID, kind, subject, true, "")
		}
		return
	}
	e.Signal(taskID, kind, subject, true, "")
	s.findings.resolve(taskID, kind, subject)
}

// signalFacts fills the P2-4 scopes of facts for task (cluster: the pseudo
// task _cluster with no row).
func (e *Evaluator) signalFacts(ctx context.Context, task Task, isCluster bool, facts *health.Facts) {
	now := facts.Now
	facts.UnreachableAfter = e.cfg.UnreachableAfter
	if d, ok := e.cfg.UnreachableAfterTask[task.ID]; ok && d > 0 {
		facts.UnreachableAfter = d
	}
	facts.DumpStalledAfter = e.cfg.DumpStalledAfter
	facts.StalledAfter = e.cfg.StalledAfter
	if facts.StalledAfter <= 0 && e.s != nil {
		e.s.mu.Lock()
		facts.StalledAfter = e.s.stalledAfter
		e.s.mu.Unlock()
	}
	if !isCluster {
		facts.Task = e.taskFacts(task, now)
	}
	facts.Signals = map[health.SignalKey]health.SignalState{}
	e.mu.Lock()
	for k, en := range e.signals[task.ID] {
		if en.st.FailureSeq > en.stamped && facts.Task != nil {
			en.st.Epoch = facts.Task.Epoch
			en.stamped = en.st.FailureSeq
		}
		facts.Signals[k] = en.st
	}
	e.mu.Unlock()
	facts.Complete[health.ScopeTaskState] = true
	facts.Complete[health.ScopeSignals] = true
}

// taskFacts is task.state, plus the evaluator-side SOURCE_UNREACHABLE streak.
func (e *Evaluator) taskFacts(task Task, now time.Time) *health.TaskFacts {
	tf := &health.TaskFacts{State: string(task.State), Desired: task.DesiredRun, Owner: task.OwnerWorkerID, Epoch: task.Epoch,
		StateSince: task.UpdatedAt, LastError: task.LastError,
		FailedCurrent: task.State == StateFailed && task.FailedSpecRevision > 0 && task.FailedSpecRevision == task.SpecRevision}
	if tf.Desired == "" {
		tf.Desired = TaskDesiredRun
		if task.State == StateStopped || task.State == StateCreated || task.State == StateFailed {
			tf.Desired = TaskDesiredStop
		}
	}
	if s := e.s; s != nil {
		s.mu.Lock()
		if s.runner == nil {
			tf.RunHere = true // nothing here can drive a transition
		} else if done, ok := s.runs[task.ID]; ok && !isClosed(done) {
			tf.RunHere = true
		}
		tf.OwnedElsewhere = s.leaseManager != nil && task.OwnerWorkerID != "" && task.OwnerWorkerID != s.clusterWorkerID
		_, known := s.tasks[task.ID]
		tf.Unadopted = s.store == nil && !known // only data_dir knows the task (#385)
		s.mu.Unlock()
	}
	if e.s != nil {
		if tf.Desired != TaskDesiredRun {
			e.s.forgetLiveness(task.ID) // the next Start counts silence from then
		} else if snap, ok := e.s.LivenessSnapshot(task.ID); ok || e.s.hasRunner() {
			if !ok {
				e.s.SourceLiveness(task.ID) // the clock starts now
				snap, _ = e.s.LivenessSnapshot(task.ID)
			}
			tf.Liveness = &health.Liveness{LastRead: snap.LastRead, LastFailure: snap.LastFailure, FailureDetail: snap.FailureDetail}
		}
	}
	return tf
}

// openAndProbes sets the open keys, the probes of open signal rows that a
// cheap check proves wrong, and the stale open signals.
func (e *Evaluator) openAndProbes(ctx context.Context, taskID string, open []health.Finding, facts *health.Facts) {
	facts.Open = map[health.Key]bool{}
	facts.Probes = map[health.Key]bool{}
	facts.StaleSignals = map[health.Key]bool{}
	staleAfter := e.cfg.SignalStaleAfter
	if staleAfter <= 0 {
		staleAfter = DefaultSignalStaleAfter
	}
	var dataDir string
	var files FileStore
	var events EventStore
	var uploader FileUploader
	if e.s != nil {
		e.s.mu.Lock()
		dataDir, files, events, uploader = e.s.dataDir, e.s.fileStore, e.s.eventStore, e.s.fileUploader
		e.s.mu.Unlock()
	}
	var catalog map[string]string
	for _, f := range open {
		k := f.Key()
		facts.Open[k] = true
		if !isSignalKind(FindingKind(k.Kind)) {
			continue
		}
		e.mu.Lock()
		en := e.signals[taskID][health.SignalKey{Kind: k.Kind, Subject: k.Subject}]
		seen := en != nil && en.st.FailureSeq > 0
		e.mu.Unlock()
		if !seen && !f.LastSeen.IsZero() && facts.Now.Sub(f.LastSeen) >= staleAfter {
			facts.StaleSignals[k] = true
		}
		switch k.Kind {
		case health.UploadRecordFailed:
			if uploader == nil {
				facts.Probes[k] = true
				continue
			}
			if files != nil && catalog == nil {
				catalog = map[string]string{}
				if rows, err := files.ListBinlogFiles(ctx, taskID, segmentInventoryLimit); err == nil {
					for _, r := range rows {
						catalog[r.FileName] = strings.ToUpper(strings.TrimSpace(r.UploadState))
					}
				}
			}
			if catalog[k.Subject] == "UPLOADED" {
				facts.Probes[k] = true
			}
		case health.OpenTailCutFailed:
			if dir, ok := taskBinlogDir(dataDir, taskID); ok && k.Subject != "" {
				name := filepath.Base(k.Subject)
				if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) || !strings.Contains(name, ".open") {
					facts.Probes[k] = true
				}
			}
		case health.DiskListFailed:
			if k.Subject != "" {
				if _, err := os.ReadDir(k.Subject); err == nil {
					facts.Probes[k] = true
				}
			}
		case health.EventReadFailed:
			if events != nil {
				c, cancel := context.WithTimeout(ctx, 5*time.Second)
				if _, err := events.ListEvents(c, taskID, 1); err == nil {
					facts.Probes[k] = true
				}
				cancel()
			}
		}
	}
}

// signalTasks is every task id with a signal in the book.
func (e *Evaluator) signalTasks() map[string]bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]bool{}
	for id := range e.signals {
		out[id] = true
	}
	return out
}
