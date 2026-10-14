// Package tasks provides module-level functionality for tasks.
// input: a metadata store that refuses writes (read-only) and then recovers
// output: proof that a dump stopped by a refused metadata write is a CRITICAL finding and not a healthy RUNNING state, and that task events refused by the event store are replayed later
// pos: regression tests for the #336 re-QA P2/P3 (read-only meta)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// metaBlockedRunner fails its first run the way a refused checkpoint write
// does, then runs normally.
type metaBlockedRunner struct {
	mu      sync.Mutex
	calls   int
	running chan struct{}
}

func (r *metaBlockedRunner) Run(context.Context, Task) error {
	return errors.New("RunWithNotify required")
}

func (r *metaBlockedRunner) RunWithNotify(ctx context.Context, _ Task, onReady func()) error {
	r.mu.Lock()
	r.calls++
	call := r.calls
	r.mu.Unlock()
	if call == 1 {
		return errors.New("save checkpoint: Error 1290 (HY000): The MySQL server is running with the --super-read-only option")
	}
	onReady()
	select {
	case r.running <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return context.Canceled
}

// A dump stopped because the metadata store refused a write is surfaced as a
// CRITICAL METADATA_WRITE_BLOCKED finding and the task is not shown RUNNING;
// the finding resolves once the task runs again with its RUNNING row written.
func TestMetadataWriteBlockedIsCriticalAndNotRunning(t *testing.T) {
	runner := &metaBlockedRunner{running: make(chan struct{}, 1)}
	s := NewScheduler(WithRunner(runner), WithRetryBackoff(300*time.Millisecond, 300*time.Millisecond))
	task, _ := s.CreateTask("ro", "ro-key")
	_ = s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"})
	if err := s.StartTask(task.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, ok := openFinding(s, task.ID, FindingMetadataWriteBlocked)
		return ok
	})
	f, _ := openFinding(s, task.ID, FindingMetadataWriteBlocked)
	if f.Severity != FindingCritical {
		t.Fatalf("severity %s", f.Severity)
	}
	if got, _ := s.GetTask(task.ID); got.State == StateRunning {
		t.Fatal("task shown RUNNING while its dump is stopped")
	}
	<-runner.running
	waitFor(t, func() bool {
		_, ok := openFinding(s, task.ID, FindingMetadataWriteBlocked)
		return !ok
	})
}

// Events the store refused are kept and written on a later sink pass; the
// EVENT_PERSIST_FAILED finding resolves once they land.
func TestRefusedEventsAreReplayed(t *testing.T) {
	events := &failingEventStore{fakeEventStore: newFakeEventStore(), appendErr: errors.New("insert task_events: read-only")}
	s := NewScheduler(WithEventStore(events))
	task, err := s.CreateTask("ev", "ev-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := openFinding(s, task.ID, FindingEventPersistFailed); !ok {
		t.Fatal("no EVENT_PERSIST_FAILED while the store refuses events")
	}
	events.appendErr = nil
	s.republishUnpublished()
	got, _ := events.fakeEventStore.ListEvents(context.Background(), task.ID, 0)
	if len(got) == 0 || got[0].Type != "TASK_CREATED" {
		t.Fatalf("TASK_CREATED not replayed: %+v", got)
	}
	if _, ok := openFinding(s, task.ID, FindingEventPersistFailed); ok {
		t.Fatal("EVENT_PERSIST_FAILED still open after the replay")
	}
}
