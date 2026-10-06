// Package tasks provides module-level functionality for tasks.
// input: a failed or successful KILL of one Binlog Dump connection id, the task row, and the current source password
// output: a process-local pending-dump registry, pending_dump_cleanup on the task, one DUMP_CLEANUP_PENDING event, one DUMP_CLEANUP_CLEARED event, and a 5s-30s KILL retry that continues after the runner is gone until the thread is gone or the task is deleted; a schema-4 empty column drops this process's copy so another process does not keep a cleared warning
// pos: scheduler side of an operator Stop that could not reach the source
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
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
// The registry is independent of the runner. A closed run at STOPPED or FAILED
// is still retried. It returns true when a KILL still could not reach the source.
func (s *Scheduler) retryPendingDumpCleanups() bool {
	if s.store != nil {
		if err := s.syncTasksFromStore(); err != nil {
			log.Printf("dump cleanup sync tasks: %v", err)
		}
	}
	s.mu.Lock()
	killer := s.dumpKiller
	markers := s.pendingMarkersLocked()
	type pendingKill struct {
		id     string
		marker DumpCleanup
	}
	batch := make([]pendingKill, 0, len(markers))
	for id, marker := range markers {
		task, ok := s.tasks[id]
		if !ok {
			delete(s.pendingDumps, id)
			continue
		}
		if task.State != StateStopped && task.State != StateFailed {
			continue
		}
		if done, ok := s.runs[id]; ok && !isClosed(done) {
			continue
		}
		batch = append(batch, pendingKill{id: id, marker: marker})
	}
	s.mu.Unlock()
	if killer == nil || len(batch) == 0 {
		return false
	}
	failed := false
	for _, item := range batch {
		current, err := s.taskForCleanup(item.id)
		if err != nil {
			if errors.Is(err, ErrTaskNotFound) {
				s.mu.Lock()
				s.forgetPendingDumpLocked(item.id)
				s.mu.Unlock()
			}
			continue
		}
		src := current.Source
		if item.marker.Host != "" {
			src.Host = item.marker.Host
			src.Port = item.marker.Port
		}
		id := item.marker.ConnectionID
		killErr := killer(src, id)
		if killErr != nil {
			failed = true
			log.Printf("binlog dump cleanup retry task=%s conn=%d err=%v", item.id, id, killErr)
		}
		s.noteDumpCleanup(item.id, src, id, killErr)
	}
	return failed
}

// pendingMarkersLocked is the registry plus any task-struct marker not copied yet.
// Caller holds s.mu.
func (s *Scheduler) pendingMarkersLocked() map[string]DumpCleanup {
	out := make(map[string]DumpCleanup, len(s.pendingDumps))
	for id, marker := range s.pendingDumps {
		if marker.ConnectionID != 0 {
			out[id] = marker
		}
	}
	for id, task := range s.tasks {
		if _, ok := out[id]; ok {
			continue
		}
		if task.PendingDumpCleanup == nil || task.PendingDumpCleanup.ConnectionID == 0 {
			continue
		}
		out[id] = DumpCleanup{
			ConnectionID: task.PendingDumpCleanup.ConnectionID,
			Host:         task.PendingDumpCleanup.Host,
			Port:         task.PendingDumpCleanup.Port,
		}
	}
	return out
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
	if marker, ok := s.pendingDumps[taskID]; ok && marker.ConnectionID != 0 {
		current = marker
	} else if task.PendingDumpCleanup != nil {
		current = *task.PendingDumpCleanup
	}
	attempted := DumpCleanup{}
	if killErr != nil && connectionID != 0 {
		attempted = DumpCleanup{ConnectionID: connectionID, Host: source.Host, Port: source.Port}
	} else if connectionID != 0 {
		attempted = DumpCleanup{ConnectionID: connectionID}
	}
	updated, kind := ApplyDumpCleanup(current, attempted, killErr)
	if updated.ConnectionID == 0 {
		s.forgetPendingDumpLocked(taskID)
	} else {
		s.rememberPendingDumpLocked(taskID, updated)
	}
	if kind == "" {
		if updated.ConnectionID != 0 && (task.PendingDumpCleanup == nil || task.PendingDumpCleanup.ConnectionID == 0) {
			task.PendingDumpCleanup = s.showPendingLocked(updated)
			s.tasks[taskID] = task
		}
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
		s.forgetPendingDumpLocked(taskID)
		task.PendingDumpCleanup = nil
	} else {
		s.rememberPendingDumpLocked(taskID, updated)
		task.PendingDumpCleanup = s.showPendingLocked(updated)
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

// keepPendingDump copies this process's marker forward when the store row cannot hold it.
func (s *Scheduler) keepPendingDump(id string, task Task) Task {
	if task.ID == "" {
		task.ID = id
	}
	return s.overlayPendingDumpLocked(task)
}

func (s *Scheduler) rememberPendingDumpLocked(id string, d DumpCleanup) {
	if id == "" || d.ConnectionID == 0 {
		return
	}
	if s.pendingDumps == nil {
		s.pendingDumps = make(map[string]DumpCleanup)
	}
	s.pendingDumps[id] = DumpCleanup{ConnectionID: d.ConnectionID, Host: d.Host, Port: d.Port}
}

func (s *Scheduler) forgetPendingDumpLocked(id string) {
	delete(s.pendingDumps, id)
}

// showPendingLocked fills the API warning. Schema 3 names the holding process.
// Caller holds s.mu.
func (s *Scheduler) showPendingLocked(d DumpCleanup) *DumpCleanup {
	shown := d.warned()
	if s.store != nil && !s.persistsPendingDumpLocked() {
		shown.ProcessLocal = true
		shown.Warning = DumpCleanupProcessLocalWarning(shown.ConnectionID)
	}
	return &shown
}

// overlayPendingDumpLocked puts this process's leftover dump on a task row.
// A schema-3 store read cannot clear it. Caller holds s.mu.
func (s *Scheduler) overlayPendingDumpLocked(task Task) Task {
	if task.ID == "" {
		return task
	}
	if s.persistsPendingDumpLocked() {
		// The column is shared. An empty read means this process's copy is stale:
		// another process already cleared it.
		if task.PendingDumpCleanup != nil && task.PendingDumpCleanup.ConnectionID != 0 {
			s.rememberPendingDumpLocked(task.ID, *task.PendingDumpCleanup)
			task.PendingDumpCleanup = s.showPendingLocked(*task.PendingDumpCleanup)
			return task
		}
		s.forgetPendingDumpLocked(task.ID)
		task.PendingDumpCleanup = nil
		return task
	}
	if marker, ok := s.pendingDumps[task.ID]; ok && marker.ConnectionID != 0 {
		task.PendingDumpCleanup = s.showPendingLocked(marker)
		return task
	}
	if s.store != nil {
		// The column is absent. Adopt a marker still on this struct or on the
		// in-memory task so the next store reload cannot drop it.
		if task.PendingDumpCleanup == nil || task.PendingDumpCleanup.ConnectionID == 0 {
			if mem, ok := s.tasks[task.ID]; ok && mem.PendingDumpCleanup != nil && mem.PendingDumpCleanup.ConnectionID != 0 {
				task.PendingDumpCleanup = mem.PendingDumpCleanup
			}
		}
		if task.PendingDumpCleanup != nil && task.PendingDumpCleanup.ConnectionID != 0 {
			s.rememberPendingDumpLocked(task.ID, *task.PendingDumpCleanup)
			task.PendingDumpCleanup = s.showPendingLocked(*task.PendingDumpCleanup)
		}
		return task
	}
	if task.PendingDumpCleanup != nil && task.PendingDumpCleanup.ConnectionID != 0 {
		s.rememberPendingDumpLocked(task.ID, *task.PendingDumpCleanup)
		return task
	}
	if marker, ok := s.pendingDumps[task.ID]; ok && marker.ConnectionID != 0 {
		task.PendingDumpCleanup = s.showPendingLocked(marker)
	}
	return task
}
