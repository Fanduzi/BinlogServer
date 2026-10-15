// Package health provides module-level functionality for health.
// input: a task's signal book, task row facts, the stored open keys and this pass's probes
// output: the P2-4 Derive and ClearRule of every signal and task-state kind: a signal opens on a failure and closes on the first later success or its §5.5 probe, an open signal with no news for signal_stale_after shows as INFO; DUMP_STALLED with its #366 rules and HANDED_OFF to SOURCE_UNREACHABLE; SOURCE_UNREACHABLE (threshold + two-pass debounce); TASK_FAILED by failure class; TASK_TRANSITION_STALLED; ClassifyFailure
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
	DefaultStalledAfter     = 60 * time.Second
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

// dumpStalledGone: the stall no longer speaks for the task (S12, #366):
// data arrived, the task was deliberately stopped, or the lease moved on.
func dumpStalledGone(f *Facts, st SignalState, ok bool) bool {
	if succeeded(st, ok) || deliberatelyStopped(f.Task) {
		return true
	}
	return f.Task != nil && st.Epoch > 0 && f.Task.Epoch > 0 && f.Task.Epoch != st.Epoch
}

func handedOff(f *Facts) bool {
	return f.Task != nil && f.Open[Key{TaskID: f.TaskID, Kind: SourceUnreachable}]
}

func dumpStalledDerive(f *Facts) []Instance {
	var out []Instance
	for k, st := range f.Signals {
		if k.Kind != DumpStalled || !failing(st) || dumpStalledGone(f, st, true) || handedOff(f) {
			continue
		}
		if f.Task != nil && !desiresRun(f.Task) && f.Task.State != stateFailed {
			continue
		}
		out = append(out, Instance{Key: Key{Subject: k.Subject}, Detail: st.Detail, FactAt: st.FactAt})
	}
	return out
}

// DUMP_STALLED leaving RUNNING for STARTING/RETRY_BACKOFF does not close it;
// it closes on data, a deliberate stop, a newer lease epoch, or once
// SOURCE_UNREACHABLE is open (HANDED_OFF: one of the two is always open).
func dumpStalledCleared(f *Facts, key Key) bool {
	st, ok := f.Signals[SignalKey{Kind: DumpStalled, Subject: key.Subject}]
	return dumpStalledGone(f, st, ok) || handedOff(f)
}

func dumpStalledReason(f *Facts, key Key) string {
	st, ok := f.Signals[SignalKey{Kind: DumpStalled, Subject: key.Subject}]
	if !dumpStalledGone(f, st, ok) && handedOff(f) {
		return ReasonHandedOff
	}
	return ""
}

func unreachableAfter(f *Facts) time.Duration {
	if f.UnreachableAfter > 0 {
		return f.UnreachableAfter
	}
	return DefaultUnreachableAfter
}

func sourceUnreachableDerive(f *Facts) []Instance {
	t := f.Task
	if !desiresRun(t) || t.UnreachablePasses < 2 || t.UnreachableFor < unreachableAfter(f) {
		return nil
	}
	detail := "task has been in STARTING/RETRY_BACKOFF for " + t.UnreachableFor.Truncate(time.Second).String() + " with desired RUN"
	if strings.TrimSpace(t.LastError) != "" {
		detail += ": " + t.LastError
	}
	return []Instance{{Detail: detail}}
}

// SOURCE_UNREACHABLE closes once a dump receives data again (the
// DUMP_STALLED success signal) and the task is RUNNING, on a deliberate
// stop, or when the task FAILED (TASK_FAILED takes over).
func sourceUnreachableCleared(f *Facts, key Key) bool {
	t := f.Task
	if t == nil {
		return false
	}
	if deliberatelyStopped(t) || t.State == stateFailed {
		return true
	}
	if t.State != stateRunning {
		return false
	}
	for k, st := range f.Signals {
		if k.Kind == DumpStalled && succeeded(st, true) {
			return true
		}
	}
	return false
}

func taskFailedDerive(f *Facts) []Instance {
	t := f.Task
	if t == nil || t.State != stateFailed || !t.FailedCurrent {
		return nil
	}
	return []Instance{{Key: Key{Subject: ClassifyFailure(t.LastError)}, Detail: "task FAILED: " + t.LastError}}
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
