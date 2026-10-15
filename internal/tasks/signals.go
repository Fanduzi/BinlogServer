// Package tasks provides module-level functionality for tasks.
// input: hot-path signals
// output: P2-4 signal entry points (stub)
// pos: ADR 0009 phase 2 P2-4
// note: if this file changes, update this header and module README.md.
package tasks

// ReasonSignal marks a task dirty because a hot-path signal arrived.
const ReasonSignal EvalReason = "SIGNAL"

const (
	FindingSourceUnreachable FindingKind = "SOURCE_UNREACHABLE"
	FindingTaskFailed        FindingKind = "TASK_FAILED"
)

// SignalFailure (stub) is the legacy Report.
func (s *Scheduler) SignalFailure(f Finding) { s.Report(f) }

// SignalSuccess (stub) is the legacy Resolve.
func (s *Scheduler) SignalSuccess(taskID string, kind FindingKind, subject string) {
	s.Resolve(taskID, kind, subject)
}
