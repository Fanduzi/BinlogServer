// Package tasks provides module-level functionality for tasks.
// input: a runner that fails permanently, an unlisted source error, and the terminal allowlist clock
// output: proof that SOURCE_AUTH/LOG_BIN_OFF retry only inside terminalRetry, that an error outside the allowlist yields on the first attempt, and that STORAGE_INCONSISTENT is FAILED instead
// pos: scheduler retry policy coverage for the terminal allowlist, split from runner_test.go
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"testing"
	"time"
)

func TestAllowlistRetriesUntilTerminalWindow(t *testing.T) {
	runner := &permanentFailureRunner{}
	s := NewScheduler(WithRunner(runner), WithRetryBackoff(time.Millisecond, time.Millisecond))
	s.terminalRetry = 80 * time.Millisecond
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatalf("ConfigureSource: %v", err)
	}
	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	sawRetry := false
	for {
		got, err := s.GetTask(task.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got.State == StateRetryBackoff {
			sawRetry = true
		}
		if got.State == StateFailed {
			if !sawRetry {
				t.Fatal("allowlist error failed before the terminal window")
			}
			if got.DesiredRun != TaskDesiredRun || got.LastError != "SOURCE_ACCESS_DENIED: denied" {
				t.Fatalf("state=%s desired=%s last_error=%q", got.State, got.DesiredRun, got.LastError)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected FAILED after the window, state=%s", got.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestScheduler_UnlistedSourceCodeFailsOnce(t *testing.T) {
	runner := &scriptedRunner{fn: func(context.Context, Task, int) error {
		return NewRetryableSourceError("SOURCE_THROTTLED", "source busy")
	}}
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

	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := s.GetTask(task.ID)
		if err != nil {
			t.Fatalf("GetTask returned error: %v", err)
		}
		if got.State == StateLeaseDegraded {
			if got.DesiredRun != TaskDesiredRun || got.LastError != "SOURCE_THROTTLED: source busy" {
				t.Fatalf("last_error=%q desired=%s", got.LastError, got.DesiredRun)
			}
			break
		}
		if got.State == StateRetryBackoff {
			t.Fatalf("unlisted source code entered RETRY_BACKOFF: %s", got.LastError)
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected LEASE_DEGRADED, state=%s", got.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if calls := runner.callCount(); calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
}

func TestScheduler_StorageInconsistentIsTerminalNotYielded(t *testing.T) {
	leases := NewMemoryLease()
	runner := &scriptedRunner{fn: func(context.Context, Task, int) error {
		return NewPermanentError(CodeStorageInconsistent, "object fbup/mysql-bin.000004 could not be read (file does not exist)")
	}}
	s := NewScheduler(
		WithRunner(runner),
		WithClusterLeaseManager(leases),
		WithClusterWorkerID("worker-a"),
		WithClusterLease(time.Hour, time.Hour, time.Hour),
		WithRetryBackoff(time.Millisecond, time.Millisecond),
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

	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := s.GetTask(task.ID)
		if err != nil {
			t.Fatalf("GetTask returned error: %v", err)
		}
		if got.State == StateFailed {
			if got.FailedReason != "STORAGE_INCONSISTENT" {
				t.Fatalf("failed_reason=%q", got.FailedReason)
			}
			break
		}
		if got.State == StateLeaseDegraded {
			t.Fatalf("a shared-storage contradiction was yielded instead of FAILED: %s", got.LastError)
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected FAILED, state=%s", got.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if calls := runner.callCount(); calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
}

// TestScheduler_StartTaskTransitionsFromStartingAfterRunnerReady 验证相关行为。
