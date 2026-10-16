// Package tasks provides module-level functionality for tasks.
// input: task row write results from persistTaskLocked
// output: TASK_PERSIST_FAILED findings (S2/S3) and a retry of the in-memory row on the sink tick until a write succeeds
// pos: makes a lost state transition a visible, retried condition instead of a log line
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"fmt"

	"binlog_server/internal/healthstore"
)

// notePersistFailureLocked records that the store does not hold this task's
// latest state. Caller holds s.mu.
func (s *Scheduler) notePersistFailureLocked(id string, state State, err error) {
	if err == nil {
		return
	}
	if errors.Is(err, ErrStaleTaskWrite) || errors.Is(err, healthstore.ErrFenced) {
		// The store's fence says another epoch owns the row now; its row wins
		// and there is nothing of ours left to republish (#360 / P2-6a).
		if _, ok := s.unpublished[id]; ok {
			delete(s.unpublished, id)
			s.SignalSuccess(id, FindingTaskPersistFailed, "")
		}
		return
	}
	s.unpublished[id] = struct{}{}
	s.SignalFailure(Finding{
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
	s.SignalSuccess(id, FindingTaskPersistFailed, "")
}

// republishUnpublished writes the current in-memory row of every task whose
// last write failed. persistTaskLocked keeps the newest snapshot, so this
// never writes an older state over a newer one from this process.
func (s *Scheduler) republishUnpublished() {
	s.replayRefusedEvents()
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
			s.SignalSuccess(id, FindingTaskPersistFailed, "")
			continue
		}
		// Errors are recorded by persistTaskLocked itself.
		if err := s.persistTaskLocked(task); err != nil {
			continue
		}
	}
}

// maxRefusedEvents bounds the replay buffer; older events beyond it are
// dropped and their EVENT_PERSIST_FAILED finding stays open.
const maxRefusedEvents = 10000

func (s *Scheduler) keepRefusedEvent(e TaskEvent) {
	s.refusedMu.Lock()
	defer s.refusedMu.Unlock()
	if len(s.refusedEvents) >= maxRefusedEvents {
		s.refusedEvents = s.refusedEvents[1:]
	}
	s.refusedEvents = append(s.refusedEvents, e)
}

// replayRefusedEvents writes refused events again, oldest first, and stops at
// the first refusal so they keep their order. A finding resolves once no
// refused event of that task and type is left.
func (s *Scheduler) replayRefusedEvents() {
	store := s.eventStore
	if store == nil {
		return
	}
	// Held for the pass (each write is bounded) so a concurrent refusal or a
	// full-buffer trim cannot shift what was written.
	s.refusedMu.Lock()
	written := 0
	for _, e := range s.refusedEvents {
		ctx, cancel := withTimeout(context.Background(), s.internalWriteTimeout)
		err := store.AppendEvent(ctx, e)
		cancel()
		if err != nil {
			break
		}
		written++
	}
	done := append([]TaskEvent(nil), s.refusedEvents[:written]...)
	s.refusedEvents = s.refusedEvents[written:]
	left := make(map[[2]string]bool, len(s.refusedEvents))
	for _, e := range s.refusedEvents {
		left[[2]string{e.TaskID, e.Type}] = true
	}
	s.refusedMu.Unlock()
	for _, e := range done {
		if !left[[2]string{e.TaskID, e.Type}] {
			s.SignalSuccess(e.TaskID, FindingEventPersistFailed, e.Type)
		}
	}
}
