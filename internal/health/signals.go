// Package health provides module-level functionality for health.
// input: a task's signal book, task row facts with the source liveness clock, the stored open keys and this pass's probes
// output: the P2-4 Derive and ClearRule of every signal and task-state kind: a signal opens on a failure and closes on the first later success or its §5.5 probe, an open signal with no news for signal_stale_after shows as INFO; DUMP_STALLED (45s with no byte read, any state) and SOURCE_UNREACHABLE (a failed attempt 90s after the last read) from the one source liveness clock, with a same-pass HANDED_OFF; TASK_FAILED by failure class; TASK_TRANSITION_STALLED; TASK_NO_PROGRESS (§24.9: a desired-RUN task whose local run has not advanced under data_dir, or that no worker runs, for no_progress_after); TASK_UNADOPTED for a standalone leftover directory (#385); ClassifyFailure
// pos: ADR 0009 phase 2 P2-4 (docs/design/0009-phase2-health-evaluator.md §5.5, §6); pure
// note: if this file changes, update this header and module README.md.
package health

import (
	"strings"
	"time"
)

// Task states and desired values as the tasks package writes them.
const (
	stateRunning      = "RUNNING"
	stateStarting     = "STARTING"
	stateStopping     = "STOPPING"
	stateStopped      = "STOPPED"
	stateFailed       = "FAILED"
	stateRetryBackoff = "RETRY_BACKOFF"
	desiredRun        = "RUN"
	desiredStop       = "STOP"
)

// DefaultUnreachableAfter and DefaultStalledAfter are the §6 thresholds.
const (
	DefaultUnreachableAfter = 90 * time.Second
	DefaultDumpStalledAfter = 45 * time.Second
	DefaultStalledAfter     = 60 * time.Second
	// DefaultNoProgressAfter is health.no_progress_after_sec (§24.9).
	DefaultNoProgressAfter = 120 * time.Second
)

// TASK_FAILED subjects: a closed set; anything unrecognised is OTHER.
const (
	FailStreamRegression  = "STREAM_REGRESSION"
	FailSourceError1236   = "SOURCE_ERROR_1236"
	FailPermissionDenied  = "PERMISSION_DENIED"
	FailStorage           = "STORAGE_INCONSISTENT"
	FailSourceSwitchover  = "SOURCE_SWITCHOVER"
	FailSourceUnreachable = "SOURCE_UNREACHABLE"
	FailConfig            = "CONFIG"
	FailOther             = "OTHER"
)

// FailureClasses lists every TASK_FAILED subject.
func FailureClasses() []string {
	return []string{FailStreamRegression, FailSourceError1236, FailPermissionDenied, FailStorage, FailSourceSwitchover, FailSourceUnreachable, FailConfig, FailOther}
}

// ClassifyFailure maps a task's last_error ("CODE: message") to its
// TASK_FAILED subject: #362 STREAM_REGRESSION, #354 MySQL 1236, #338
// permission denied, and the other permanent codes; unknown is OTHER.
func ClassifyFailure(lastError string) string {
	msg := strings.TrimSpace(lastError)
	code, _, _ := strings.Cut(msg, ":")
	code = strings.ToUpper(strings.TrimSpace(code))
	low := strings.ToLower(msg)
	switch {
	case code == "STREAM_REGRESSION":
		return FailStreamRegression
	case strings.Contains(low, "1236"):
		return FailSourceError1236
	case code == "SOURCE_ACCESS_DENIED" || strings.Contains(low, "permission denied") || strings.Contains(low, "access denied"):
		return FailPermissionDenied
	case code == "STORAGE_INCONSISTENT" || code == "SEALED_FILE_EXISTS" || code == "CHECKPOINT_WRITE_FAILED":
		return FailStorage
	case code == "SOURCE_SWITCHOVER":
		return FailSourceSwitchover
	case code == "SOURCE_UNREACHABLE":
		return FailSourceUnreachable
	case code == "INVALID_REQUEST" || code == "SOURCE_LOG_BIN_OFF" || code == "SOURCE_IDENTITY_UNAVAILABLE":
		return FailConfig
	}
	return FailOther
}

func failing(st SignalState) bool { return st.FailureSeq > st.SuccessSeq }

// succeeded: a success observed since start, later than any failure this
// process saw. No success since start = the internal unknown of §5.5.
func succeeded(st SignalState, ok bool) bool {
	return ok && st.SuccessSeq > 0 && st.SuccessSeq > st.FailureSeq
}

// signalDerive opens one instance per failing signal of kind, and keeps an
// open row with no news past signal_stale_after as INFO (§5.5).
func signalDerive(kind Kind) func(f *Facts) []Instance {
	return func(f *Facts) []Instance {
		var out []Instance
		seen := map[string]bool{}
		for k, st := range f.Signals {
			if k.Kind == kind && failing(st) {
				out = append(out, Instance{Key: Key{Subject: k.Subject}, Detail: st.Detail, FactAt: st.FactAt})
				seen[k.Subject] = true
			}
		}
		for k := range f.StaleSignals {
			if k.Kind == kind && !seen[k.Subject] && !f.Probes[k] {
				out = append(out, Instance{Key: Key{Subject: k.Subject}, Severity: Info,
					Detail: "no failure or success seen for this signal since health.signal_stale_after; shown as INFO until it is proven either way"})
			}
		}
		return out
	}
}

// signalCleared: the first success after the failure, or the kind's probe.
func signalCleared(kind Kind) func(f *Facts, key Key) bool {
	return func(f *Facts, key Key) bool {
		st, ok := f.Signals[SignalKey{Kind: kind, Subject: key.Subject}]
		return succeeded(st, ok) || f.Probes[key]
	}
}

func desiresRun(t *TaskFacts) bool { return t != nil && t.Desired == desiredRun }

func deliberatelyStopped(t *TaskFacts) bool {
	return t != nil && t.Desired == desiredStop && t.State == stateStopped
}

// liveness is the task's clock when this node drives the task with desired
// RUN; nil otherwise (stopped, FAILED, owned elsewhere, or no clock).
func liveness(f *Facts) *Liveness {
	t := f.Task
	if !desiresRun(t) || t.OwnedElsewhere || t.Liveness == nil || t.Liveness.LastRead.IsZero() {
		return nil
	}
	return t.Liveness
}

func dumpStalledAfter(f *Facts) time.Duration {
	if f.DumpStalledAfter > 0 {
		return f.DumpStalledAfter
	}
	return DefaultDumpStalledAfter
}

func unreachableAfter(f *Facts) time.Duration {
	if f.UnreachableAfter > 0 {
		return f.UnreachableAfter
	}
	return DefaultUnreachableAfter
}

// silenceOf is how long the source has sent nothing, rounded for details.
func silenceOf(f *Facts, l *Liveness) time.Duration { return f.Now.Sub(l.LastRead).Round(time.Second) }

// unreachable: a connection attempt failed at least unreachable_after after
// the last byte read, and nothing was read since. Measured between two
// observed facts, never from a task state, so backoff that keeps the task
// out of RUNNING after the source is back does not count (#383 QA F3).
func unreachable(f *Facts) bool {
	l := liveness(f)
	return l != nil && l.LastFailure.After(l.LastRead) && l.LastFailure.Sub(l.LastRead) >= unreachableAfter(f)
}

// stalled: nothing read for dump_stalled_after, whatever the task state,
// and SOURCE_UNREACHABLE does not already speak for it.
func stalled(f *Facts) bool {
	l := liveness(f)
	return l != nil && f.Now.Sub(l.LastRead) >= dumpStalledAfter(f) && !unreachable(f)
}

func dumpStalledDerive(f *Facts) []Instance {
	if !stalled(f) {
		return nil
	}
	return []Instance{{Detail: "no byte or heartbeat read from the source for " + silenceOf(f, liveness(f)).String() +
		" (dump_stalled_after " + dumpStalledAfter(f).String() + "); the task is " + f.Task.State}}
}

// DUMP_STALLED closes when a byte is read again, the task is stopped or
// owned elsewhere, or SOURCE_UNREACHABLE opens in the same pass (HANDED_OFF).
func dumpStalledCleared(f *Facts, key Key) bool { return !stalled(f) }

func dumpStalledReason(f *Facts, key Key) string {
	if unreachable(f) {
		return ReasonHandedOff
	}
	return ""
}

func sourceUnreachableDerive(f *Facts) []Instance {
	if !unreachable(f) {
		return nil
	}
	l := liveness(f)
	detail := "no byte or heartbeat read from the source for " + silenceOf(f, l).String() +
		"; the latest connection attempt failed " + f.Now.Sub(l.LastFailure).Truncate(time.Second).String() + " ago"
	if strings.TrimSpace(l.FailureDetail) != "" {
		detail += ": " + l.FailureDetail
	}
	return []Instance{{Detail: detail}}
}

// SOURCE_UNREACHABLE closes once a byte is read after the latest failure,
// or the task is stopped, FAILED (TASK_FAILED takes over) or owned elsewhere.
func sourceUnreachableCleared(f *Facts, key Key) bool { return !unreachable(f) }

func taskFailedDerive(f *Facts) []Instance {
	t := f.Task
	if t == nil || t.State != stateFailed || !t.FailedCurrent {
		return nil
	}
	return []Instance{{Key: Key{Subject: ClassifyFailure(t.LastError)}, Detail: "task FAILED: " + t.LastError}}
}

// taskUnadoptedDerive: a standalone leftover directory that no task record
// drives (#385). Replication for it is stopped until the operator adopts it.
func taskUnadoptedDerive(f *Facts) []Instance {
	t := f.Task
	if t == nil || !t.Unadopted {
		return nil
	}
	return []Instance{{Detail: TaskUnadoptedDetail(f.TaskID)}}
}

// TaskUnadoptedDetail is the TASK_UNADOPTED detail for a leftover task
// directory; the legacy sink uses the same text while the evaluator is off.
func TaskUnadoptedDetail(taskID string) string {
	return "data_dir/" + taskID + " holds a backup with no task metadata (standalone, after a restart or a delete): " +
		"replication for it is stopped and the backup falls behind the source; POST /api/tasks/" + taskID +
		"/adopt with the source and cluster_key to resume it, or move the directory out of data_dir"
}

func stalledAfter(f *Facts) time.Duration {
	if f.StalledAfter > 0 {
		return f.StalledAfter
	}
	return DefaultStalledAfter
}

func transitionStalledDerive(f *Facts) []Instance {
	t := f.Task
	if t == nil || (t.State != stateStarting && t.State != stateStopping) || t.RunHere || t.OwnedElsewhere || t.StateSince.IsZero() {
		return nil
	}
	if age := f.Now.Sub(t.StateSince); age >= stalledAfter(f) {
		return []Instance{{Detail: "task is in " + t.State + " for " + age.Round(time.Second).String() + " with no dump driving it"}}
	}
	return nil
}

func noProgressAfter(f *Facts) time.Duration {
	if f.NoProgressAfter > 0 {
		return f.NoProgressAfter
	}
	return DefaultNoProgressAfter
}

// noProgress (design 0009 §24.9) is the backstop for a task that should be
// running and is not getting anywhere, whatever the cause: a local run that
// has not advanced under this worker's data_dir (nor been confirmed caught
// up with the source) since it started, or a desired-RUN task that no
// worker runs. It returns how long, and the detail.
func noProgress(f *Facts) (time.Duration, string, bool) {
	t := f.Task
	if !desiresRun(t) || t.State == stateFailed || t.OwnedElsewhere || unreachable(f) {
		return 0, "", false
	}
	after := noProgressAfter(f)
	threshold := " (no_progress_after " + after.String() + ")"
	if p := t.Progress; p != nil {
		since := p.RunSince
		if p.InDataDir && p.LastAdvance.After(since) {
			since = p.LastAdvance
		}
		if since.IsZero() {
			return 0, "", false
		}
		idle := f.Now.Sub(since)
		if idle < after {
			return 0, "", false
		}
		d := idle.Round(time.Second).String()
		switch {
		case p.File != "" && !p.InDataDir && !p.LastAdvance.IsZero():
			return idle, "this node runs the task (state " + t.State + ") but for " + d + " nothing it wrote is under this worker's data_dir: " +
				"the run's current segment " + p.File + " is not under this worker's data_dir" + threshold +
				"; the backup is not advancing here. Cluster nodes must share data_dir at the same path", true
		case p.LastAdvance.IsZero():
			return idle, "this node runs the task (state " + t.State + ") but has written nothing and has not been confirmed at the source tip for " + d + threshold, true
		default:
			return idle, "this node runs the task (state " + t.State + ") but it has not advanced under data_dir or been confirmed at the source tip for " + d + threshold, true
		}
	}
	if t.RunHere || t.StateSince.IsZero() {
		return 0, "", false // a node with no runner keeps no progress book
	}
	idle := f.Now.Sub(t.StateSince)
	if idle < after {
		return 0, "", false
	}
	owner := t.Owner
	if owner == "" {
		owner = "none"
	} else if t.LeaseExpired {
		owner += " (lease expired)"
	}
	return idle, "desired RUN but no worker is running the task: state " + t.State + ", owner " + owner + ", for " + idle.Round(time.Second).String() + threshold, true
}

func noProgressDerive(f *Facts) []Instance {
	if _, detail, ok := noProgress(f); ok {
		return []Instance{{Detail: detail}}
	}
	return nil
}

func noProgressCleared(f *Facts, key Key) bool {
	_, _, ok := noProgress(f)
	return !ok
}
