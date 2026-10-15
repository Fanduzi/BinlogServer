package tasks

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// slowExitRunner keeps working for a while after its context ends, the way
// the real runner closes and syncs its open segment before returning.
type slowExitRunner struct {
	started   chan Task
	exited    atomic.Bool
	cancelled atomic.Bool
}

func (r *slowExitRunner) Run(ctx context.Context, task Task) error {
	select {
	case r.started <- task:
	default:
	}
	<-ctx.Done()
	r.cancelled.Store(true)
	time.Sleep(150 * time.Millisecond)
	r.exited.Store(true)
	return context.Canceled
}

func startedTask(t *testing.T, s *Scheduler, r *slowExitRunner) Task {
	t.Helper()
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "x", Flavor: "mysql"}); err != nil {
		t.Fatal(err)
	}
	if err := s.StartTask(task.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.started:
	case <-time.After(3 * time.Second):
		t.Fatal("runner not started")
	}
	return task
}

// Runners belong to the process run: ending the run context cancels them,
// WaitRuns returns only after each runner has returned, and shutdown is not
// an operator Stop (desired RUN stays, so the next start resumes the task).
func TestScheduler_RunContextOwnsRunners(t *testing.T) {
	r := &slowExitRunner{started: make(chan Task, 1)}
	s := NewScheduler(WithRunner(r))
	ctx, cancel := context.WithCancel(context.Background())
	s.SetRunContext(ctx)
	task := startedTask(t, s, r)

	cancel()
	if !s.WaitRuns(3 * time.Second) {
		t.Fatal("WaitRuns timed out")
	}
	if !r.cancelled.Load() || !r.exited.Load() {
		t.Fatalf("WaitRuns returned before the runner finished (cancelled=%v exited=%v)", r.cancelled.Load(), r.exited.Load())
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DesiredRun != TaskDesiredRun || got.State == StateStopped {
		t.Fatalf("shutdown must not record a Stop: desired=%s state=%s", got.DesiredRun, got.State)
	}
	fresh, err := s.CreateTask("cluster-b", "cluster-b-key")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.ConfigureSource(fresh.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "x", Flavor: "mysql"})
	if err := s.StartTask(fresh.ID); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("start after the run context ended: %v, want ErrShuttingDown", err)
	}
}

// WaitRuns bounds the wait: a runner that ignores cancellation does not
// hang shutdown.
func TestScheduler_WaitRunsIsBounded(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	s := NewScheduler(WithRunner(runnerFunc(func(ctx context.Context, _ Task) error { <-block; return nil })))
	ctx, cancel := context.WithCancel(context.Background())
	s.SetRunContext(ctx)
	task, _ := s.CreateTask("c", "k")
	_ = s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "x", Flavor: "mysql"})
	if err := s.StartTask(task.ID); err != nil {
		t.Fatal(err)
	}
	cancel()
	start := time.Now()
	if s.WaitRuns(100 * time.Millisecond) {
		t.Fatal("WaitRuns reported done while a runner is still running")
	}
	if time.Since(start) > time.Second {
		t.Fatal("WaitRuns did not honour its timeout")
	}
}

type runnerFunc func(context.Context, Task) error

func (f runnerFunc) Run(ctx context.Context, t Task) error { return f(ctx, t) }
