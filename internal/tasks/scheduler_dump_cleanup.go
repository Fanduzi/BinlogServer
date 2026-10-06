// Package tasks provides module-level functionality for tasks.
// input: a failed or successful KILL of one Binlog Dump connection id, the task row, and the current source password
// output: pending_dump_cleanup on the task, one DUMP_CLEANUP_PENDING event, one DUMP_CLEANUP_CLEARED event, and a 5s-30s retry until the thread is gone or the task is deleted
// pos: scheduler side of an operator Stop that could not reach the source
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"fmt"
	"log"
	"time"
)

const (
	dumpCleanupRetryMin = 5 * time.Second
	dumpCleanupRetryMax = 30 * time.Second
)

// DumpCleanupBinder is the runner hook that reports each KILL attempt.
type DumpCleanupBinder interface {
	BindDumpCleanup(func(taskID string, source SourceConfig, connectionID uint32, killErr error))
}

// DumpThreadKiller KILL one connection id with the source it is given.
type DumpThreadKiller interface {
	KillDumpThread(source SourceConfig, connectionID uint32) error
}

// pendingDumpWriter stores the marker without rewriting the rest of the task row.
type pendingDumpWriter interface {
	PendingDumpColumn() bool
	SavePendingDumpCleanup(ctx context.Context, taskID, previous, next string) (bool, error)
}

// RunDumpCleanupRetry KILL leftover dump threads until ctx is cancelled.
// The first wait is 5s. A failed pass waits twice as long, up to 30s.
// A pass that finds nothing to kill, or kills what it found, waits 5s again.
func (s *Scheduler) RunDumpCleanupRetry(ctx context.Context) {
	if s == nil || ctx == nil {
		return
	}
	s.mu.Lock()
	killer := s.dumpKiller
	s.mu.Unlock()
	if killer == nil {
		return
	}
	wait := dumpCleanupRetryMin
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if s.retryPendingDumpCleanups() {
			if wait < dumpCleanupRetryMax {
				wait *= 2
				if wait > dumpCleanupRetryMax {
					wait = dumpCleanupRetryMax
				}
			}
		} else {
			wait = dumpCleanupRetryMin
		}
		timer.Reset(wait)
	}
}

// retryPendingDumpCleanups tries each idle task's leftover connection once.
// It returns true when a KILL still could not reach the source.
func (s *Scheduler) retryPendingDumpCleanups() bool {
	if s.store != nil {
		if err := s.syncTasksFromStore(); err != nil {
			log.Printf("dump cleanup sync tasks: %v", err)
		}
	}
	s.mu.Lock()
	killer := s.dumpKiller
	batch := make([]Task, 0)
	for _, task := range s.tasks {
		if task.PendingDumpCleanup == nil || task.PendingDumpCleanup.ConnectionID == 0 {
			continue
		}
		if task.State != StateStopped && task.State != StateFailed {
			continue
		}
		if done, ok := s.runs[task.ID]; ok && !isClosed(done) {
			continue
		}
		batch = append(batch, task)
	}
	s.mu.Unlock()
	if killer == nil || len(batch) == 0 {
		return false
	}
	failed := false
	for _, task := range batch {
		current, err := s.taskForCleanup(task.ID)
		if err != nil {
			continue
		}
		if current.PendingDumpCleanup == nil || current.PendingDumpCleanup.ConnectionID == 0 {
			continue
		}
		src := current.Source
		if current.PendingDumpCleanup.Host != "" {
			src.Host = current.PendingDumpCleanup.Host
			src.Port = current.PendingDumpCleanup.Port
		}
		id := current.PendingDumpCleanup.ConnectionID
		killErr := killer(src, id)
		if killErr != nil {
			failed = true
			log.Printf("binlog dump cleanup retry task=%s conn=%d err=%v", task.ID, id, killErr)
		}
		s.noteDumpCleanup(task.ID, src, id, killErr)
	}
	return failed
}

func (s *Scheduler) taskForCleanup(id string) (Task, error) {
	if s.store != nil {
		task, err := s.GetTask(id)
		if err != nil {
			return Task{}, err
		}
		return task, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok {
		return Task{}, ErrTaskNotFound
	}
	return task, nil
}

func (s *Scheduler) noteDumpCleanup(taskID string, source SourceConfig, connectionID uint32, killErr error) {
	s.mu.Lock()
	task, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return
	}
	current := DumpCleanup{}
	if task.PendingDumpCleanup != nil {
		current = *task.PendingDumpCleanup
	}
	attempted := DumpCleanup{}
	if killErr != nil && connectionID != 0 {
		attempted = DumpCleanup{ConnectionID: connectionID, Host: source.Host, Port: source.Port}
	} else if connectionID != 0 {
		attempted = DumpCleanup{ConnectionID: connectionID}
	}
	updated, kind := ApplyDumpCleanup(current, attempted, killErr)
	if kind == "" {
		s.mu.Unlock()
		return
	}
	previous := EncodeDumpCleanup(current)
	next := EncodeDumpCleanup(updated)
	store := s.store
	s.mu.Unlock()

	changed := true
	if writer, ok := store.(pendingDumpWriter); ok && writer.PendingDumpColumn() {
		ctx, cancel := s.withWriteTimeout(context.Background())
		var err error
		changed, err = writer.SavePendingDumpCleanup(ctx, taskID, previous, next)
		cancel()
		if err != nil {
			log.Printf("pending dump cleanup write task=%s err=%v", taskID, err)
			changed = true
		}
	}

	s.mu.Lock()
	task, ok = s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return
	}
	if updated.ConnectionID == 0 {
		task.PendingDumpCleanup = nil
	} else {
		warned := updated.warned()
		task.PendingDumpCleanup = &warned
	}
	s.tasks[taskID] = task
	if changed {
		switch kind {
		case dumpCleanupPending:
			detail := fmt.Sprintf("connection_id=%d host=%s port=%d", updated.ConnectionID, updated.Host, updated.Port)
			s.appendEventLocked(taskID, "DUMP_CLEANUP_PENDING", task.PendingDumpCleanup.Warning, detail)
		case dumpCleanupCleared:
			s.appendEventLocked(taskID, "DUMP_CLEANUP_CLEARED", DumpCleanupClearedMessage(connectionID), "")
			log.Printf("binlog dump cleanup cleared task=%s conn=%d", taskID, connectionID)
		}
	}
	s.flushPendingEventsLocked()
	s.mu.Unlock()
}

func (s *Scheduler) persistsPendingDumpLocked() bool {
	writer, ok := s.store.(pendingDumpWriter)
	return ok && writer.PendingDumpColumn()
}

// keepPendingDump copies an in-memory marker forward when the store row cannot hold it.
func (s *Scheduler) keepPendingDump(id string, task Task) Task {
	if s.persistsPendingDumpLocked() || task.PendingDumpCleanup != nil {
		return task
	}
	if mem, ok := s.tasks[id]; ok && mem.PendingDumpCleanup != nil {
		task.PendingDumpCleanup = mem.PendingDumpCleanup
	}
	return task
}
