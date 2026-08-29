// Package tasks provides module-level functionality for tasks.
// input: scheduler metadata endpoint policy and loopback source configurations
// output: regression coverage preventing metadata/source replication feedback loops across endpoint spellings
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

func TestScheduler_CreateRejectsLocalhostMetadataSource(t *testing.T) {
	s := NewScheduler(WithMetadataSourceEndpoint("127.0.0.1", 3306))

	_, err := s.CreateTaskFromSpec(
		"localhost-source",
		"localhost-source",
		&SourceConfig{Host: "localhost", Port: 3306, User: "repl", Password: "secret"},
		nil,
		nil,
	)

	if !errors.Is(err, ErrInvalidSourceConfig) {
		t.Fatalf("expected ErrInvalidSourceConfig, got %v", err)
	}
	if got := len(s.ListTasks()); got != 0 {
		t.Fatalf("rejected create persisted %d tasks", got)
	}
}

func TestScheduler_CreateRejectsLoopbackMetadataSources(t *testing.T) {
	tests := []struct {
		name         string
		metadataHost string
		sourceHost   string
	}{
		{name: "ipv4 loopback alias", metadataHost: "127.0.0.1", sourceHost: "127.0.0.2"},
		{name: "ipv4 loopback lower bound", metadataHost: "localhost", sourceHost: "127.0.0.0"},
		{name: "ipv4 loopback upper bound", metadataHost: "localhost", sourceHost: "127.255.255.255"},
		{name: "ipv6 loopback", metadataHost: "localhost", sourceHost: "::1"},
		{name: "bracketed ipv6 loopback", metadataHost: "[::1]", sourceHost: "[::1]"},
		{name: "expanded ipv6 loopback", metadataHost: "localhost", sourceHost: "0:0:0:0:0:0:0:1"},
		{name: "uppercase localhost", metadataHost: "127.0.0.1", sourceHost: "LOCALHOST"},
		{name: "trailing dot localhost", metadataHost: "127.0.0.1", sourceHost: "localhost."},
		{name: "metadata uppercase and trailing dot", metadataHost: "LOCALHOST.", sourceHost: "127.0.0.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewScheduler(WithMetadataSourceEndpoint(tt.metadataHost, 3306))
			_, err := s.CreateTaskFromSpec(
				"unsafe-source",
				"unsafe-source",
				&SourceConfig{Host: tt.sourceHost, Port: 3306, User: "repl", Password: "secret"},
				nil,
				nil,
			)

			if !errors.Is(err, ErrInvalidSourceConfig) {
				t.Fatalf("expected ErrInvalidSourceConfig, got %v", err)
			}
			if got := len(s.ListTasks()); got != 0 {
				t.Fatalf("rejected create persisted %d tasks", got)
			}
		})
	}
}

func TestScheduler_AllowsLoopbackMetadataSourceOnDifferentPort(t *testing.T) {
	s := NewScheduler(WithMetadataSourceEndpoint("127.0.0.1", 3306))

	task, err := s.CreateTaskFromSpec(
		"different-port-source",
		"different-port-source",
		&SourceConfig{Host: "localhost", Port: 3307, User: "repl", Password: "secret"},
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("CreateTaskFromSpec returned error: %v", err)
	}
	if task.Source.Host != "localhost" || task.Source.Port != 3307 {
		t.Fatalf("unexpected source after allowed create: %+v", task.Source)
	}
}

func TestScheduler_AllowsBracketedHostnameOnMetadataPort(t *testing.T) {
	s := NewScheduler(WithMetadataSourceEndpoint("localhost", 3306))

	task, err := s.CreateTaskFromSpec(
		"bracketed-hostname-source",
		"bracketed-hostname-source",
		&SourceConfig{Host: "[localhost]", Port: 3306, User: "repl", Password: "secret"},
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("CreateTaskFromSpec returned error: %v", err)
	}
	if task.Source.Host != "[localhost]" {
		t.Fatalf("unexpected source after allowed create: %+v", task.Source)
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

func TestScheduler_UpdateRejectsLoopbackMetadataSource(t *testing.T) {
	s := NewScheduler(WithMetadataSourceEndpoint("localhost", 3306))
	safe := SourceConfig{Host: "192.0.2.10", Port: 3307, User: "repl", Password: "secret"}
	task, err := s.CreateTaskFromSpec("safe-source", "safe-source", &safe, nil, nil)
	if err != nil {
		t.Fatalf("CreateTaskFromSpec returned error: %v", err)
	}
	unsafe := SourceConfig{Host: "127.255.255.254", Port: 3306, User: "repl"}

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

func TestScheduler_ConfigureRejectsBracketedIPv6MetadataSource(t *testing.T) {
	s := NewScheduler(WithMetadataSourceEndpoint("::1", 3306))
	task, err := s.CreateTask("safe-source", "safe-source")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}

	err = s.ConfigureSource(task.ID, SourceConfig{Host: "[::1]", Port: 3306, User: "repl", Password: "secret"})

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

func TestScheduler_StartRejectsLoopbackMetadataSource(t *testing.T) {
	store := newFakeStore()
	store.tasks["33"] = Task{
		ID:         "33",
		Name:       "legacy-unsafe-source",
		ClusterKey: "legacy-unsafe-source",
		State:      StateStopped,
		Source:     SourceConfig{Host: "127.0.0.2", Port: 3306, User: "repl", Password: "secret"},
		Start:      StartConfig{Mode: StartModeLatest},
	}
	s := NewScheduler(WithStore(store), WithMetadataSourceEndpoint("localhost", 3306))
	if err := s.Restore(context.Background()); err != nil {
		t.Fatalf("Restore returned error: %v", err)
	}

	err := s.StartTask("33")

	if !errors.Is(err, ErrInvalidSourceConfig) {
		t.Fatalf("expected ErrInvalidSourceConfig, got %v", err)
	}
	got, err := s.GetTask("33")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.State != StateStopped {
		t.Fatalf("rejected start changed state to %s", got.State)
	}
}
