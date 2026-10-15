// Package health provides module-level functionality for health.
// input: a task's signal book, task row facts, the stored open keys and this pass's probes
// output: the P2-4 Derive and ClearRule of every signal and task-state kind: a signal opens on a failure and closes on the first later success or its §5.5 probe, an open signal with no news for signal_stale_after shows as INFO; DUMP_STALLED with its #366 rules and HANDED_OFF to SOURCE_UNREACHABLE; SOURCE_UNREACHABLE (threshold + two-pass debounce); TASK_FAILED by failure class; TASK_TRANSITION_STALLED; ClassifyFailure
// pos: ADR 0009 phase 2 P2-4 (docs/design/0009-phase2-health-evaluator.md §5.5, §6); pure
// note: if this file changes, update this header and module README.md.
package health

import (
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

// ManagedKinds lists the kinds the evaluator owns (stub: P2-3 set).
func ManagedKinds() []Kind {
	var out []Kind
	for k, s := range DefaultRegistry() {
		if s.Managed {
			out = append(out, k)
		}
	}
	return out
}
