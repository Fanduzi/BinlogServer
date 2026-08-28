// Package tasks provides module-level functionality for tasks.
// input: scheduler metadata endpoint policy and task source configurations
// output: regression coverage preventing metadata/source replication feedback loops
// pos: public task-service boundary tests for metadata source isolation
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestScheduler_CreateRejectsMetadataSource(t *testing.T) {
	s := NewScheduler(WithMetadataSourceEndpoint("127.0.0.1", 3306))

	_, err := s.CreateTaskFromSpec(
		"unsafe-source",
		"unsafe-source",
		&SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret"},
		nil,
		nil,
	)

	if !errors.Is(err, ErrInvalidSourceConfig) {
		t.Fatalf("expected ErrInvalidSourceConfig, got %v", err)
	}
	if !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("expected operator-facing metadata conflict, got %q", err)
	}
	if got := len(s.ListTasks()); got != 0 {
		t.Fatalf("rejected create persisted %d tasks", got)
	}
}

func TestScheduler_StartRejectsPersistedMetadataSource(t *testing.T) {
	store := newFakeStore()
	store.tasks["18"] = Task{
		ID:         "18",
		Name:       "legacy-unsafe-source",
		ClusterKey: "legacy-unsafe-source",
		State:      StateStopped,
		Source:     SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret"},
		Start:      StartConfig{Mode: StartModeLatest},
	}
	s := NewScheduler(WithStore(store), WithMetadataSourceEndpoint("127.0.0.1", 3306))
	if err := s.Restore(context.Background()); err != nil {
		t.Fatalf("Restore returned error: %v", err)
	}

	err := s.StartTask("18")

	if !errors.Is(err, ErrInvalidSourceConfig) {
		t.Fatalf("expected ErrInvalidSourceConfig, got %v", err)
	}
	got, err := s.GetTask("18")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.State != StateStopped {
		t.Fatalf("rejected start changed state to %s", got.State)
	}
}

func TestScheduler_ConfigureRejectsMetadataSource(t *testing.T) {
	s := NewScheduler(WithMetadataSourceEndpoint("127.0.0.1", 3306))
	task, err := s.CreateTask("safe-source", "safe-source")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}

	err = s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret"})

	if !errors.Is(err, ErrInvalidSourceConfig) {
		t.Fatalf("expected ErrInvalidSourceConfig, got %v", err)
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.Source.Host != "" {
		t.Fatalf("rejected source mutated task: %+v", got.Source)
	}
}

func TestScheduler_UpdateRejectsMetadataSource(t *testing.T) {
	s := NewScheduler(WithMetadataSourceEndpoint("127.0.0.1", 3306))
	safe := SourceConfig{Host: "127.0.0.1", Port: 3307, User: "repl", Password: "secret"}
	task, err := s.CreateTaskFromSpec("safe-source", "safe-source", &safe, nil, nil)
	if err != nil {
		t.Fatalf("CreateTaskFromSpec returned error: %v", err)
	}
	unsafe := SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}

	_, err = s.UpdateTask(task.ID, TaskPatch{ClusterKey: task.ClusterKey, Source: &unsafe})

	if !errors.Is(err, ErrInvalidSourceConfig) {
		t.Fatalf("expected ErrInvalidSourceConfig, got %v", err)
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.Source.Host != safe.Host || got.Source.Port != safe.Port {
		t.Fatalf("rejected update mutated source: %+v", got.Source)
	}
}
