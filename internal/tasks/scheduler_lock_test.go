// Package tasks provides module-level functionality for tasks.
// input: a slow event store shared by two running tasks
// output: proof that one task's event read or insert does not stall Stop, Start, lease renewal, or progress on another task
// pos: regression for the scheduler lock held across metadata I/O
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gateEventStore blocks one task's event read or insert until released.
// Other tasks pass through. That is the cross-task stall #177 reproduced.
type gateEventStore struct {
	mu     sync.Mutex
	events map[string][]TaskEvent

	listTask    string
	listEntered chan struct{}
	listHold    chan struct{}

	appendTask    string
	appendEntered chan struct{}
	appendHold    chan struct{}
}

func (g *gateEventStore) AppendEvent(_ context.Context, event TaskEvent) error {
	g.mu.Lock()
	block := g.appendHold != nil && event.TaskID == g.appendTask
	entered := g.appendEntered
	hold := g.appendHold
	g.mu.Unlock()
	if block {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-hold
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.events[event.TaskID] = append(g.events[event.TaskID], event)
	return nil
}

func (g *gateEventStore) ListEvents(_ context.Context, taskID string, limit int) ([]TaskEvent, error) {
	g.mu.Lock()
	block := g.listHold != nil && taskID == g.listTask
	entered := g.listEntered
	hold := g.listHold
	g.mu.Unlock()
	if block {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-hold
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	items := g.events[taskID]
	if limit <= 0 || limit >= len(items) {
		out := make([]TaskEvent, len(items))
		copy(out, items)
		return out, nil
	}
	out := make([]TaskEvent, limit)
	copy(out, items[len(items)-limit:])
	return out, nil
}

func TestListEventsDoesNotStallOtherTask(t *testing.T) {
	listEntered := make(chan struct{})
	listHold := make(chan struct{})
	var releaseList sync.Once
	release := func() { releaseList.Do(func() { close(listHold) }) }
	store := &gateEventStore{
		events:      map[string][]TaskEvent{},
		listEntered: listEntered,
		listHold:    listHold,
	}
	t.Cleanup(release)

	var listed atomic.Bool
	lease := &fakeLeaseManager{
		acquireEpoch: 3,
		acquireOK:    true,
		renewFn: func(int) (bool, error) {
			if !listed.Load() {
				return true, nil
			}
			return false, errors.New("lease renew failed")
		},
	}
	s := NewScheduler(
		WithEventStore(store),
		WithRunner(&fakeRunner{started: make(chan Task, 4)}),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(time.Minute, 15*time.Millisecond, time.Minute),
	)
	first := startLockedTask(t, s, "cluster-a", "cluster-a-key")
	second := startLockedTask(t, s, "cluster-b", "cluster-b-key")
	store.mu.Lock()
	store.listTask = second.ID
	store.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.ListEvents(second.ID, 10)
	}()
	select {
	case <-listEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("ListEvents did not reach the event store")
	}
	listed.Store(true)

	deadline := time.Now().Add(500 * time.Millisecond)
	var state State
	for {
		var got Task
		within(t, 100*time.Millisecond, "GetTask during ListEvents", func() error {
			var err error
			got, err = s.GetTask(first.ID)
			return err
		})
		state = got.State
		if state == StateLeaseDegraded || state == StateStopping || state == StateStopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lease renew did not run while ListEvents blocked, state=%s", state)
		}
		time.Sleep(10 * time.Millisecond)
	}

	within(t, 100*time.Millisecond, "progress during ListEvents", func() error {
		s.ReportReplicationProgress(first.ID, time.Now().UTC(), "mysql-bin.000001", 4, true)
		return nil
	})
	within(t, 100*time.Millisecond, "StopTask during ListEvents", func() error {
		return s.StopTask(first.ID)
	})
	within(t, 100*time.Millisecond, "StartTask during ListEvents", func() error {
		task, err := s.CreateTask("cluster-c", "cluster-c-key")
		if err != nil {
			return err
		}
		if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret"}); err != nil {
			return err
		}
		return s.StartTask(task.ID)
	})

	release()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ListEvents did not return")
	}
}

func TestEventInsertDoesNotStallOtherTask(t *testing.T) {
	appendEntered := make(chan struct{})
	appendHold := make(chan struct{})
	var releaseAppend sync.Once
	release := func() { releaseAppend.Do(func() { close(appendHold) }) }
	store := &gateEventStore{
		events:        map[string][]TaskEvent{},
		appendEntered: appendEntered,
		appendHold:    appendHold,
	}
	t.Cleanup(release)

	s := NewScheduler(
		WithEventStore(store),
		WithRunner(&fakeRunner{started: make(chan Task, 4)}),
	)
	first := startLockedTask(t, s, "cluster-a", "cluster-a-key")
	second := startLockedTask(t, s, "cluster-b", "cluster-b-key")
	store.mu.Lock()
	store.appendTask = second.ID
	store.mu.Unlock()

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- s.StopTask(second.ID)
	}()
	select {
	case <-appendEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("event insert did not reach the event store")
	}

	within(t, 100*time.Millisecond, "progress during event insert", func() error {
		s.ReportReplicationProgress(first.ID, time.Now().UTC(), "mysql-bin.000001", 4, true)
		return nil
	})
	within(t, 100*time.Millisecond, "StopTask during event insert", func() error {
		return s.StopTask(first.ID)
	})

	release()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked StopTask did not return")
	}
}

func within(t *testing.T, budget time.Duration, what string, fn func() error) {
	t.Helper()
	errCh := make(chan error, 1)
	go func() {
		errCh <- fn()
	}()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(budget):
		t.Fatalf("%s blocked longer than %s", what, budget)
	}
}

func startLockedTask(t *testing.T, s *Scheduler, name, key string) Task {
	t.Helper()
	task, err := s.CreateTask(name, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret"}); err != nil {
		t.Fatal(err)
	}
	if err := s.StartTask(task.ID); err != nil {
		t.Fatal(err)
	}
	return task
}
