package tasks

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeRunner struct {
	started chan Task
}

func (f *fakeRunner) Run(ctx context.Context, task Task) error {
	select {
	case f.started <- task:
	default:
	}
	<-ctx.Done()
	return context.Canceled
}

type failOnceRunner struct {
	mu              sync.Mutex
	calls           int
	secondRunNotify chan struct{}
}

func (r *failOnceRunner) Run(ctx context.Context, _ Task) error {
	r.mu.Lock()
	r.calls++
	call := r.calls
	r.mu.Unlock()

	if call == 1 {
		return errors.New("temporary network failure")
	}

	select {
	case <-r.secondRunNotify:
	default:
		close(r.secondRunNotify)
	}

	<-ctx.Done()
	return context.Canceled
}

func TestScheduler_StartTaskWithRunnerRequiresSource(t *testing.T) {
	s := NewScheduler(WithRunner(&fakeRunner{started: make(chan Task, 1)}))
	task, err := s.CreateTask("cluster-a")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}

	err = s.StartTask(task.ID)
	if err == nil {
		t.Fatal("expected error when source is missing, got nil")
	}
}

func TestScheduler_StartTaskInvokesRunner(t *testing.T) {
	runner := &fakeRunner{started: make(chan Task, 1)}
	s := NewScheduler(WithRunner(runner))

	task, err := s.CreateTask("cluster-a")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	err = s.ConfigureSource(task.ID, SourceConfig{
		Host:     "127.0.0.1",
		Port:     3306,
		User:     "repl",
		Password: "secret",
		Flavor:   "mysql",
		ServerID: 200001,
	})
	if err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}

	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask returned error: %v", err)
	}

	select {
	case startedTask := <-runner.started:
		if startedTask.ID != task.ID {
			t.Fatalf("expected task id %s, got %s", task.ID, startedTask.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner was not invoked")
	}
}

func TestScheduler_StopTaskCancelsRunner(t *testing.T) {
	runner := &fakeRunner{started: make(chan Task, 1)}
	s := NewScheduler(WithRunner(runner))

	task, err := s.CreateTask("cluster-a")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	err = s.ConfigureSource(task.ID, SourceConfig{
		Host:     "127.0.0.1",
		Port:     3306,
		User:     "repl",
		Password: "secret",
		Flavor:   "mysql",
		ServerID: 200001,
	})
	if err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}

	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask returned error: %v", err)
	}
	<-runner.started

	if err := s.StopTask(task.ID); err != nil {
		t.Fatalf("StopTask returned error: %v", err)
	}

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.State != StateStopped {
		t.Fatalf("expected state %s, got %s", StateStopped, got.State)
	}
}

func TestScheduler_ConfigureSourceRejectsInvalid(t *testing.T) {
	s := NewScheduler()
	task, err := s.CreateTask("cluster-a")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}

	err = s.ConfigureSource(task.ID, SourceConfig{})
	if !errors.Is(err, ErrInvalidSourceConfig) {
		t.Fatalf("expected ErrInvalidSourceConfig, got %v", err)
	}
}

func TestScheduler_AutoRetryAfterRunnerError(t *testing.T) {
	runner := &failOnceRunner{secondRunNotify: make(chan struct{})}
	s := NewScheduler(
		WithRunner(runner),
		WithRetryBackoff(10*time.Millisecond, 20*time.Millisecond),
	)

	task, err := s.CreateTask("cluster-a")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	err = s.ConfigureSource(task.ID, SourceConfig{
		Host:     "127.0.0.1",
		Port:     3306,
		User:     "repl",
		Password: "secret",
		Flavor:   "mysql",
		ServerID: 200001,
	})
	if err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}

	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask returned error: %v", err)
	}

	select {
	case <-runner.secondRunNotify:
	case <-time.After(2 * time.Second):
		t.Fatal("expected scheduler to auto retry and invoke runner second time")
	}

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.State != StateRunning {
		t.Fatalf("expected state %s after retry, got %s", StateRunning, got.State)
	}
	if got.LastError != "" {
		t.Fatalf("expected last error cleared after retry, got %q", got.LastError)
	}

	if err := s.StopTask(task.ID); err != nil {
		t.Fatalf("StopTask returned error: %v", err)
	}
}
