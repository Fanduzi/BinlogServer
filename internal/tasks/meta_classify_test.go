// Package tasks provides module-level functionality for tasks.
// input: runner errors from a source outage whose text looks like a metadata blip, and a leftover dump while a run is live
// output: proof that only errors tagged at the metadata-store boundary raise METADATA_WRITE_BLOCKED, and that dump cleanup retries do not pause while the task runs
// pos: #338 re-QA-2 P2 and the cleanup-retry gap
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// sourceOutageRunner fails with go-mysql's dump errors, then runs.
type sourceOutageRunner struct {
	mu      sync.Mutex
	calls   int
	errs    []error
	running chan struct{}
}

func (r *sourceOutageRunner) Run(context.Context, Task) error { return errors.New("RunWithNotify required") }

func (r *sourceOutageRunner) RunWithNotify(ctx context.Context, _ Task, onReady func()) error {
	r.mu.Lock()
	call := r.calls
	r.calls++
	r.mu.Unlock()
	if call < len(r.errs) {
		return r.errs[call]
	}
	// Not ready: a finding opened by the errors above stays open.
	select {
	case r.running <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return context.Canceled
}

// #338 re-QA-2 P2: a source dump error that merely contains "timeout",
// "bad connection" or "EOF" is not a refused metadata write.
func TestSourceOutageIsNotMetadataWriteBlocked(t *testing.T) {
	runner := &sourceOutageRunner{running: make(chan struct{}, 1), errs: []error{
		errors.New("io.ReadFull(header) failed. err EOF: connection was bad"),
		errors.New("read tcp 127.0.0.1:41430->127.0.0.1:19897: i/o timeout"),
		errors.New("driver: bad connection"),
	}}
	s := NewScheduler(WithRunner(runner), WithRetryBackoff(10*time.Millisecond, 10*time.Millisecond))
	task, _ := s.CreateTask("src", "src-key")
	_ = s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"})
	if err := s.StartTask(task.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.running:
	case <-time.After(5 * time.Second):
		t.Fatal("task did not retry through the source errors")
	}
	if f, ok := openFinding(s, task.ID, FindingMetadataWriteBlocked); ok {
		t.Fatalf("source outage raised %s: %s", f.Kind, f.Detail)
	}
	_ = s.StopTask(task.ID)
}

// #338 re-QA-2: a leftover dump (one Close could not KILL) was not retried
// for minutes while the task's new run was live.
func TestDumpCleanupRetriesWhileRunIsLive(t *testing.T) {
	s := NewScheduler()
	task := s.mustTask(t)
	s.mu.Lock()
	item := s.tasks[task.ID]
	item.State = StateRunning
	pending := DumpCleanup{ConnectionID: 77, Host: "10.0.0.8", Port: 3306}.warned()
	item.PendingDumpCleanup = &pending
	s.tasks[task.ID] = item
	if s.runs == nil {
		s.runs = map[string]chan struct{}{}
	}
	s.runs[task.ID] = make(chan struct{})
	s.mu.Unlock()
	var killed uint32
	s.dumpKiller = func(_ SourceConfig, id uint32) error {
		killed = id
		return nil
	}
	s.retryPendingDumpCleanups()
	if killed != 77 {
		t.Fatalf("leftover dump not retried while the run is live (killed %d)", killed)
	}
}
