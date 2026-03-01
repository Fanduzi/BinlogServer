// Package tasks provides module-level functionality for tasks.
// input: task commands/events, runner callbacks, store/lease/uploader dependencies
// output: task state transitions, scheduling decisions, and execution coordination
// pos: core domain orchestration layer governing backup task lifecycle and policies
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"testing"
)

type fakeEventStore struct {
	events      map[string][]TaskEvent
	appendCalls int
}

// newFakeEventStore 实现对应功能逻辑。
func newFakeEventStore() *fakeEventStore {
	return &fakeEventStore{events: make(map[string][]TaskEvent)}
}

// AppendEvent 实现对应功能逻辑。
func (f *fakeEventStore) AppendEvent(_ context.Context, event TaskEvent) error {
	f.appendCalls++
	f.events[event.TaskID] = append(f.events[event.TaskID], event)
	return nil
}

// ListEvents 实现对应功能逻辑。
func (f *fakeEventStore) ListEvents(_ context.Context, taskID string, limit int) ([]TaskEvent, error) {
	items := f.events[taskID]
	if limit <= 0 || limit >= len(items) {
		out := make([]TaskEvent, len(items))
		copy(out, items)
		return out, nil
	}
	out := make([]TaskEvent, limit)
	copy(out, items[len(items)-limit:])
	return out, nil
}

// TestScheduler_UsesEventStore 验证相关行为。
func TestScheduler_UsesEventStore(t *testing.T) {
	eventStore := newFakeEventStore()
	s := NewScheduler(
		WithEventStore(eventStore),
		WithRunner(&fakeRunner{started: make(chan Task, 1)}),
	)

	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}
	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask returned error: %v", err)
	}

	events, err := s.ListEvents(task.ID, 10)
	if err != nil {
		t.Fatalf("ListEvents returned error: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected events from event store")
	}
	if eventStore.appendCalls == 0 {
		t.Fatal("expected append event called")
	}
}
