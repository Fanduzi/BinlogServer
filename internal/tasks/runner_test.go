// Package tasks provides module-level functionality for tasks.
// input: task commands/events, retryable/permanent runner callbacks, store/lease/uploader dependencies
// output: runner invocation, code-specific retry cap/reset, readiness, stop, and permanent-failure assertions
// pos: public Scheduler seam tests for runner-driven task lifecycle behavior
// note: if this file changes, update this header and module README.md.
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

// Run 实现对应功能逻辑。
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

type permanentFailureRunner struct {
	mu    sync.Mutex
	calls int
}

type unreachableRunner struct {
	mu        sync.Mutex
	calls     int
	attempted chan int
	release   chan struct{}
}

type readyResetRunner struct {
	mu      sync.Mutex
	calls   int
	resumed chan struct{}
}

type otherRetryableSourceRunner struct {
	mu       sync.Mutex
	calls    int
	eleventh chan struct{}
}

func (r *otherRetryableSourceRunner) Run(ctx context.Context, _ Task) error {
	r.mu.Lock()
	r.calls++
	call := r.calls
	r.mu.Unlock()
	if call == 11 {
		close(r.eleventh)
		<-ctx.Done()
		return ctx.Err()
	}
	return NewRetryableSourceError("SOURCE_THROTTLED", "source busy")
}

func (r *readyResetRunner) Run(context.Context, Task) error {
	return errors.New("RunWithNotify required")
}

func (r *readyResetRunner) RunWithNotify(ctx context.Context, _ Task, onReady func()) error {
	r.mu.Lock()
	r.calls++
	call := r.calls
	r.mu.Unlock()

	if call < 10 {
		return NewRetryableSourceError(CodeSourceUnreachable, "dial tcp: connection refused")
	}
	onReady()
	if call == 10 {
		return NewRetryableSourceError(CodeSourceUnreachable, "read tcp: connection reset")
	}
	close(r.resumed)
	<-ctx.Done()
	return ctx.Err()
}

func (r *unreachableRunner) Run(ctx context.Context, _ Task) error {
	r.mu.Lock()
	r.calls++
	call := r.calls
	r.mu.Unlock()
	if r.attempted != nil {
		select {
		case r.attempted <- call:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-r.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return NewRetryableSourceError(CodeSourceUnreachable, "dial tcp: connection refused")
}

func (r *unreachableRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// Run returns an unrecoverable source error.
func (r *permanentFailureRunner) Run(context.Context, Task) error {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return NewPermanentError(CodeSourceAccessDenied, "denied")
}

func (r *permanentFailureRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// Run 实现对应功能逻辑。
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

type readyGateRunner struct {
	entered chan struct{}
	release chan struct{}
}

// Run 实现对应功能逻辑。
func (r *readyGateRunner) Run(_ context.Context, _ Task) error {
	return nil
}

// RunWithNotify 实现对应功能逻辑。
func (r *readyGateRunner) RunWithNotify(ctx context.Context, _ Task, onReady func()) error {
	select {
	case <-r.entered:
	default:
		close(r.entered)
	}
	<-r.release
	onReady()
	<-ctx.Done()
	return context.Canceled
}

type delayedStopRunner struct {
	started chan Task
	delay   time.Duration
}

// Run 实现对应功能逻辑。
func (r *delayedStopRunner) Run(ctx context.Context, task Task) error {
	select {
	case r.started <- task:
	default:
	}
	<-ctx.Done()
	time.Sleep(r.delay)
	return context.Canceled
}

// TestScheduler_StartTaskWithRunnerRequiresSource 验证相关行为。
func TestScheduler_StartTaskWithRunnerRequiresSource(t *testing.T) {
	s := NewScheduler(WithRunner(&fakeRunner{started: make(chan Task, 1)}))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}

	err = s.StartTask(task.ID)
	if err == nil {
		t.Fatal("expected error when source is missing, got nil")
	}
}

// TestScheduler_StartTaskInvokesRunner 验证相关行为。
func TestScheduler_StartTaskInvokesRunner(t *testing.T) {
	runner := &fakeRunner{started: make(chan Task, 1)}
	s := NewScheduler(WithRunner(runner))

	task, err := s.CreateTask("cluster-a", "cluster-a-key")
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

// TestScheduler_StopTaskCancelsRunner 验证相关行为。
func TestScheduler_StopTaskCancelsRunner(t *testing.T) {
	runner := &fakeRunner{started: make(chan Task, 1)}
	s := NewScheduler(WithRunner(runner))

	task, err := s.CreateTask("cluster-a", "cluster-a-key")
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

	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := s.GetTask(task.ID)
		if err != nil {
			t.Fatalf("GetTask returned error: %v", err)
		}
		if got.State == StateStopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected state %s, got %s", StateStopped, got.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestScheduler_ConfigureSourceRejectsInvalid 验证相关行为。
func TestScheduler_ConfigureSourceRejectsInvalid(t *testing.T) {
	s := NewScheduler()
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}

	err = s.ConfigureSource(task.ID, SourceConfig{})
	if !errors.Is(err, ErrInvalidSourceConfig) {
		t.Fatalf("expected ErrInvalidSourceConfig, got %v", err)
	}
}

// TestScheduler_AutoRetryAfterRunnerError 验证相关行为。
func TestScheduler_AutoRetryAfterRunnerError(t *testing.T) {
	runner := &failOnceRunner{secondRunNotify: make(chan struct{})}
	s := NewScheduler(
		WithRunner(runner),
		WithRetryBackoff(10*time.Millisecond, 20*time.Millisecond),
	)

	task, err := s.CreateTask("cluster-a", "cluster-a-key")
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

// TestScheduler_PermanentRunnerErrorTransitionsToFailed verifies that permanent
// runner errors stop the retry loop and retain the operator-facing reason.
func TestScheduler_PermanentRunnerErrorTransitionsToFailed(t *testing.T) {
	runner := &permanentFailureRunner{}
	s := NewScheduler(WithRunner(runner))

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

	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := s.GetTask(task.ID)
		if err != nil {
			t.Fatalf("GetTask returned error: %v", err)
		}
		if got.State == StateFailed {
			if got.LastError != "SOURCE_ACCESS_DENIED: denied" {
				t.Fatalf("expected permanent error detail, got %q", got.LastError)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected state %s, got %s", StateFailed, got.State)
		}
		time.Sleep(10 * time.Millisecond)
	}

	events, err := s.ListEvents(task.ID, 0)
	if err != nil {
		t.Fatalf("ListEvents returned error: %v", err)
	}
	var runnerErrorIndex, failedIndex = -1, -1
	for i, event := range events {
		switch event.Type {
		case "TASK_RUNNER_ERROR":
			runnerErrorIndex = i
		case "TASK_FAILED":
			failedIndex = i
		case "TASK_RETRY_BACKOFF":
			t.Fatal("permanent runner error must not enter retry backoff")
		}
	}
	if runnerErrorIndex < 0 || failedIndex != runnerErrorIndex+1 {
		t.Fatalf("expected TASK_RUNNER_ERROR immediately followed by TASK_FAILED, got %#v", events)
	}
	if calls := runner.callCount(); calls != 1 {
		t.Fatalf("expected runner called once, got %d", calls)
	}
}

func TestScheduler_UnreachableSourceFailsAfterTenConsecutiveAttempts(t *testing.T) {
	task := Task{
		ID:         "37",
		Name:       "restored-task",
		ClusterKey: "cluster-a-key",
		State:      StateRetryBackoff,
		LastError:  "SOURCE_UNREACHABLE: previous process",
		Source:     SourceConfig{Host: "203.0.113.1", Port: 3306, User: "repl"},
	}
	store := &schedulerTestStore{tasks: map[string]Task{task.ID: task}}
	runner := &unreachableRunner{attempted: make(chan int), release: make(chan struct{})}
	s := NewScheduler(WithStore(store), WithRunner(runner), WithRetryBackoff(time.Millisecond, time.Millisecond))
	if err := s.Restore(context.Background()); err != nil {
		t.Fatalf("Restore returned error: %v", err)
	}
	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask returned error: %v", err)
	}
	t.Cleanup(func() { _ = s.StopTask(task.ID) })

	expectAttempt := func(want int) {
		t.Helper()
		select {
		case got := <-runner.attempted:
			if got != want {
				t.Fatalf("expected attempt %d, got %d", want, got)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for attempt %d", want)
		}
	}
	assertEventCounts := func(wantRunnerErrors, wantRetries, wantFailed int) {
		t.Helper()
		events, err := s.ListEvents(task.ID, 0)
		if err != nil {
			t.Fatalf("ListEvents returned error: %v", err)
		}
		counts := map[string]int{}
		for _, event := range events {
			counts[event.Type]++
		}
		if counts["TASK_RUNNER_ERROR"] != wantRunnerErrors || counts["TASK_RETRY_BACKOFF"] != wantRetries || counts["TASK_RETRYING"] != wantRetries || counts["TASK_FAILED"] != wantFailed {
			t.Fatalf("unexpected retry event counts at attempt %d: %#v", wantRunnerErrors, counts)
		}
	}

	expectAttempt(1)
	runner.release <- struct{}{}
	expectAttempt(2)
	assertEventCounts(1, 1, 0)

	for attempt := 2; attempt < 9; attempt++ {
		runner.release <- struct{}{}
		expectAttempt(attempt + 1)
	}
	runner.release <- struct{}{}
	expectAttempt(10)
	assertEventCounts(9, 9, 0)

	runner.release <- struct{}{}

	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := s.GetTask(task.ID)
		if err != nil {
			t.Fatalf("GetTask returned error: %v", err)
		}
		if got.State == StateFailed {
			if got.LastError != "SOURCE_UNREACHABLE: dial tcp: connection refused" {
				t.Fatalf("expected stable source error detail, got %q", got.LastError)
			}
			break
		}
		if time.Now().After(deadline) {
			_ = s.StopTask(task.ID)
			t.Fatalf("expected state %s after retry cap, got %s", StateFailed, got.State)
		}
		time.Sleep(time.Millisecond)
	}
	assertEventCounts(10, 9, 1)
	if calls := runner.callCount(); calls != 10 {
		t.Fatalf("expected 10 runner attempts, got %d", calls)
	}
	events, err := s.ListEvents(task.ID, 0)
	if err != nil {
		t.Fatalf("ListEvents returned error: %v", err)
	}
	if n := len(events); n < 2 || events[n-2].Type != "TASK_RUNNER_ERROR" || events[n-1].Type != "TASK_FAILED" {
		t.Fatalf("expected exhaustion to end with runner error then failed, got %#v", events)
	}
}

func TestScheduler_RunnerReadyResetsConsecutiveSourceFailures(t *testing.T) {
	runner := &readyResetRunner{resumed: make(chan struct{})}
	s := NewScheduler(WithRunner(runner), WithRetryBackoff(time.Millisecond, time.Millisecond))

	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "203.0.113.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}
	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask returned error: %v", err)
	}

	select {
	case <-runner.resumed:
	case <-time.After(2 * time.Second):
		got, _ := s.GetTask(task.ID)
		t.Fatalf("expected retry budget reset after ready, state=%s", got.State)
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.State != StateRunning || got.LastError != "" {
		t.Fatalf("expected resumed runner to be RUNNING without error, got state=%s last_error=%q", got.State, got.LastError)
	}
	if err := s.StopTask(task.ID); err != nil {
		t.Fatalf("StopTask returned error: %v", err)
	}
}

func TestScheduler_OtherRetryableSourceCodeIsNotCapped(t *testing.T) {
	runner := &otherRetryableSourceRunner{eleventh: make(chan struct{})}
	s := NewScheduler(WithRunner(runner), WithRetryBackoff(time.Millisecond, time.Millisecond))

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

	select {
	case <-runner.eleventh:
	case <-time.After(2 * time.Second):
		got, _ := s.GetTask(task.ID)
		t.Fatalf("expected non-SOURCE_UNREACHABLE retries to continue, state=%s", got.State)
	}
	if err := s.StopTask(task.ID); err != nil {
		t.Fatalf("StopTask returned error: %v", err)
	}
}

// TestScheduler_StartTaskTransitionsFromStartingAfterRunnerReady 验证相关行为。
func TestScheduler_StartTaskTransitionsFromStartingAfterRunnerReady(t *testing.T) {
	runner := &readyGateRunner{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	s := NewScheduler(WithRunner(runner))

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

	select {
	case <-runner.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not enter")
	}

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.State != StateStarting {
		t.Fatalf("expected state %s before runner ready, got %s", StateStarting, got.State)
	}

	close(runner.release)

	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err = s.GetTask(task.ID)
		if err != nil {
			t.Fatalf("GetTask returned error: %v", err)
		}
		if got.State == StateRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected state %s after runner ready, got %s", StateRunning, got.State)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := s.StopTask(task.ID); err != nil {
		t.Fatalf("StopTask returned error: %v", err)
	}
}

// TestScheduler_StopTaskTransitionsStoppingToStopped 验证相关行为。
func TestScheduler_StopTaskTransitionsStoppingToStopped(t *testing.T) {
	runner := &delayedStopRunner{
		started: make(chan Task, 1),
		delay:   80 * time.Millisecond,
	}
	s := NewScheduler(WithRunner(runner))

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
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner was not invoked")
	}

	if err := s.StopTask(task.ID); err != nil {
		t.Fatalf("StopTask returned error: %v", err)
	}

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.State != StateStopping {
		t.Fatalf("expected immediate state %s, got %s", StateStopping, got.State)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err = s.GetTask(task.ID)
		if err != nil {
			t.Fatalf("GetTask returned error: %v", err)
		}
		if got.State == StateStopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected eventual state %s, got %s", StateStopped, got.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
