// Package tasks provides module-level functionality for tasks.
// input: task row write results from persistTaskLocked
// output: TASK_PERSIST_FAILED findings (S2/S3) and a retry of the in-memory row on the sink tick until a write succeeds
// pos: makes a lost state transition a visible, retried condition instead of a log line
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"errors"
	"fmt"
)

// notePersistFailureLocked records that the store does not hold this task's
// latest state. Caller holds s.mu.
func (s *Scheduler) notePersistFailureLocked(id string, state State, err error) {
	if err == nil || errors.Is(err, ErrStaleTaskWrite) {
		return
	}
	s.unpublished[id] = struct{}{}
	s.Report(Finding{
		TaskID:   id,
		Kind:     FindingTaskPersistFailed,
		Severity: FindingCritical,
		Detail:   fmt.Sprintf("state %s not written to metadata store: %v", state, err),
	})
}

// notePersistedLocked clears the condition after a successful row write.
// Caller holds s.mu.
func (s *Scheduler) notePersistedLocked(id string) {
	if _, ok := s.unpublished[id]; !ok {
		return
	}
	delete(s.unpublished, id)
	s.Resolve(id, FindingTaskPersistFailed, "")
}

// republishUnpublished writes the current in-memory row of every task whose
// last write failed. persistTaskLocked keeps the newest snapshot, so this
// never writes an older state over a newer one from this process.
func (s *Scheduler) republishUnpublished() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store == nil || len(s.unpublished) == 0 {
		return
	}
	ids := make([]string, 0, len(s.unpublished))
	for id := range s.unpublished {
		ids = append(ids, id)
	}
	for _, id := range ids {
		task, ok := s.tasks[id]
		if !ok {
			delete(s.unpublished, id)
			s.Resolve(id, FindingTaskPersistFailed, "")
			continue
		}
		// Errors are recorded by persistTaskLocked itself.
		if err := s.persistTaskLocked(task); err != nil {
			continue
		}
	}
}
