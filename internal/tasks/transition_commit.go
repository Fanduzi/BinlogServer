// Package tasks provides module-level functionality for tasks.
// input: the next task row an operator action (Create, Start, Stop, Delete, Update, Adopt, Configure*) wants, the state it moves to, and the event that records it
// output: commitTransitionLocked and commitDeleteLocked, the only way an operator action changes a task: the store write happens first and memory, events and the dump are touched only after it lands; a refused write leaves memory, the stored row and findings as they were, records a *_REJECTED event, and returns ErrTransitionNotRecorded; every commit marks the task dirty for the health evaluator (TRANSITION)
// pos: tip 53a9 dogfood P1 (Start in RETRY_BACKOFF on read-only meta left a STARTING zombie) and #336 (Stop zombie); lintguard's task-state test keeps every operator path on these helpers
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"fmt"
)

// ErrTransitionNotRecorded wraps a store refusal of an operator action. The
// action was not applied: memory, the stored row and the dump are unchanged.
var ErrTransitionNotRecorded = errors.New("not recorded")

// transitionRecord is the event a committed transition appends.
type transitionRecord struct {
	// Action names the operator action in the refusal ("start", "stop").
	Action  string
	Type    string
	Message string
	Detail  string
}

// commitTransitionLocked writes next with State to, and only after the
// store accepts it puts next in memory and appends rec. Caller holds s.mu;
// the lock is released during the write. On refusal nothing changes: the
// memory row stays what it was (the store still holds it), the write is not
// left for republishing, and a <TYPE>_REJECTED event is recorded. A fenced
// write (ErrStaleTaskWrite) is returned as is.
func (s *Scheduler) commitTransitionLocked(next Task, to State, rec transitionRecord) error {
	next.State = to
	id := next.ID
	s.markHealthDirty(id, ReasonTransition)
	if s.store == nil {
		s.tasks[id] = next
		s.appendEventLocked(id, rec.Type, rec.Message, rec.Detail)
		s.flushPendingEventsLocked()
		return nil
	}
	prevPersisted, hadPersisted := s.persisted[id]
	_, wasUnpublished := s.unpublished[id]
	err := s.persistTaskLocked(next)
	if err != nil {
		if errors.Is(err, ErrStaleTaskWrite) {
			return err
		}
		if latest := s.persisted[id]; latest.task.UpdatedAt.Equal(next.UpdatedAt) && latest.task.SpecRevision == next.SpecRevision {
			if hadPersisted {
				latest.task = prevPersisted.task
				s.persisted[id] = latest
			} else {
				delete(s.persisted, id)
			}
		}
		if !wasUnpublished {
			delete(s.unpublished, id)
			s.Resolve(id, FindingTaskPersistFailed, "")
		}
		if _, known := s.tasks[id]; known {
			s.appendEventLocked(id, rec.Type+"_REJECTED", rec.Action+" not recorded; the "+rec.Action+" is not applied", err.Error())
			s.flushPendingEventsLocked()
		}
		return fmt.Errorf("%s %w; the %s is not applied and the task state is unchanged: %w", rec.Action, ErrTransitionNotRecorded, rec.Action, err)
	}
	if mem, ok := s.tasks[id]; !ok || mem.SpecRevision <= next.SpecRevision {
		s.tasks[id] = s.overlayPendingDumpLocked(next)
	}
	s.appendEventLocked(id, rec.Type, rec.Message, rec.Detail)
	// A loop transition that wrote during our unlocked write may have
	// published its older snapshot last. The operator row wins: the next
	// sink tick republishes memory (republishUnpublished).
	if latest := s.persisted[id]; !latest.task.UpdatedAt.Equal(next.UpdatedAt) {
		s.unpublished[id] = struct{}{}
	}
	s.flushPendingEventsLocked()
	return nil
}

// commitDeleteLocked removes the stored row first. Only after the store
// accepts it is the dump cancelled and the task dropped from memory. A
// refused delete changes nothing. Caller holds s.mu.
func (s *Scheduler) commitDeleteLocked(id string) error {
	if s.store != nil {
		store := s.store
		s.mu.Unlock()
		ctx, cancel := s.withWriteTimeout(context.Background())
		err := store.DeleteTask(ctx, id)
		cancel()
		s.mu.Lock()
		if err != nil {
			if _, known := s.tasks[id]; known {
				s.appendEventLocked(id, "TASK_DELETE_REJECTED", "delete not recorded; the delete is not applied", err.Error())
				s.flushPendingEventsLocked()
			}
			return fmt.Errorf("delete %w; the delete is not applied and the task state is unchanged: %w", ErrTransitionNotRecorded, err)
		}
	}
	if cancel, ok := s.cancels[id]; ok {
		cancel()
		delete(s.cancels, id)
	}
	delete(s.runs, id)
	delete(s.tasks, id)
	delete(s.events, id)
	delete(s.replica, id)
	delete(s.persisted, id)
	delete(s.unpublished, id)
	s.forgetPendingDumpLocked(id)
	return nil
}
