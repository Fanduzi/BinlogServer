// Package tasks provides module-level functionality for tasks.
// input: the process run context from App.Run
// output: SetRunContext (every runner context derives from it), WaitRuns (bounded join of runTask goroutines), ErrShuttingDown for a start after the run context ended
// pos: task runner ownership and shutdown; a shutdown cancel is not an operator Stop, so desired RUN is kept and the next start resumes the task
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"time"
)

// ErrShuttingDown refuses a task start after the process run context ended.
var ErrShuttingDown = errors.New("binlog-server is shutting down; task not started")

// SetRunContext ties every task runner to ctx. When ctx ends, each runner's
// context is cancelled; WaitRuns then joins them. Call it before Restore.
func (s *Scheduler) SetRunContext(ctx context.Context) {
	s.mu.Lock()
	s.runBase = ctx
	s.mu.Unlock()
}

// WaitRuns blocks until every runTask goroutine has returned (its runner
// closed its segment and the task's last state write landed), or until
// timeout. It reports whether all of them finished.
func (s *Scheduler) WaitRuns(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.runWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
