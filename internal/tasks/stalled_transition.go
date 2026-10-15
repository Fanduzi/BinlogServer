// Package tasks provides module-level functionality for tasks.
// input: the in-memory task rows, this process's open runs, and the lease owner of each row
// output: a watchdog pass that finds a task left in STARTING or STOPPING with no run of this process driving it for longer than stalledAfter, raises TASK_TRANSITION_STALLED (legacy only; since P2-4 the evaluator derives it from the task row while it owns the managed kinds), and hands it to the control loop (convergeTask) to start or finish the stop; the finding resolves once the task leaves that state or a run drives it
// pos: safety net under commitTransitionLocked (tip 53a9 P1): runs on every finding-sink tick; a dispatched STARTING row on a process without a runner, and a row another worker owns, are driven elsewhere and skipped
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"fmt"
	"time"
)

// defaultStalledAfter is how long STARTING or STOPPING may last without a
// run before the watchdog takes the task on. A start writes RUNNING once the
// dump is ready and a stop writes STOPPED once the run closed; both are
// seconds, so this is far past any healthy transition.
const defaultStalledAfter = 60 * time.Second

func (s *Scheduler) reconcileStalledTransitions() {
	s.mu.Lock()
	if s.runner == nil {
		s.mu.Unlock()
		return
	}
	now := time.Now()
	var stalled []Task
	for id, task := range s.tasks {
		if s.transitionStalledLocked(id, task, now) {
			stalled = append(stalled, task)
			continue
		}
		s.resolveUnmanaged(id, FindingTransitionStalled, "")
	}
	s.mu.Unlock()
	for _, task := range stalled {
		detail := fmt.Sprintf("task was in %s for %s with no dump driving it; the control loop reconciled it", task.State, now.Sub(task.UpdatedAt).Round(time.Second))
		if _, err := s.convergeTask(task.ID); err != nil {
			detail = fmt.Sprintf("task is in %s for %s with no dump driving it; reconciling failed and is retried every tick: %v", task.State, now.Sub(task.UpdatedAt).Round(time.Second), err)
		}
		s.reportUnmanaged(Finding{TaskID: task.ID, Kind: FindingTransitionStalled, Severity: FindingWarning, Detail: detail})
	}
}

// transitionStalledLocked: STARTING or STOPPING, older than stalledAfter, no
// open run here, and not a row another worker owns. Caller holds s.mu.
func (s *Scheduler) transitionStalledLocked(id string, task Task, now time.Time) bool {
	if task.State != StateStarting && task.State != StateStopping {
		return false
	}
	if now.Sub(task.UpdatedAt) < s.stalledAfter {
		return false
	}
	if done, ok := s.runs[id]; ok && !isClosed(done) {
		return false
	}
	if s.leaseManager != nil && task.OwnerWorkerID != "" && task.OwnerWorkerID != s.clusterWorkerID {
		return false
	}
	return true
}
