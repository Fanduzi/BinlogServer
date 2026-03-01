// Package meta provides module-level functionality for meta.
// input: MySQL connections, SQL schema/contracts, retry/lease timing policies
// output: persistent metadata operations for tasks, leases, runs, and checkpoints
// pos: metadata persistence layer between domain scheduler and MySQL storage engine
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestWithRetry_RetryOnTransientErrors 验证相关行为。
func TestWithRetry_RetryOnTransientErrors(t *testing.T) {
	var attempts int
	errTransient := errors.New("transient")

	policy := RetryPolicy{
		BaseDelay:  1 * time.Millisecond,
		MaxDelay:   2 * time.Millisecond,
		MaxRetries: 5,
		Jitter:     0,
		IsTransient: func(err error) bool {
			return errors.Is(err, errTransient)
		},
	}

	err := WithRetry(context.Background(), policy, func() error {
		attempts++
		if attempts < 3 {
			return errTransient
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithRetry returned error: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("expected attempts=3, got %d", attempts)
	}
}

// TestWithRetry_StopOnPermanentErrors 验证相关行为。
func TestWithRetry_StopOnPermanentErrors(t *testing.T) {
	var attempts int
	baseErr := errors.New("bad request")
	policy := RetryPolicy{
		BaseDelay:  1 * time.Millisecond,
		MaxDelay:   2 * time.Millisecond,
		MaxRetries: 5,
		Jitter:     0,
		IsTransient: func(err error) bool {
			return false
		},
	}

	err := WithRetry(context.Background(), policy, func() error {
		attempts++
		return Permanent(baseErr)
	})
	if err == nil {
		t.Fatal("expected permanent error")
	}
	if !errors.Is(err, baseErr) {
		t.Fatalf("expected wrapped base error, got %v", err)
	}
	if attempts != 1 {
		t.Fatalf("expected attempts=1, got %d", attempts)
	}
}

// TestWithRetry_DeadlineExceeded 验证相关行为。
func TestWithRetry_DeadlineExceeded(t *testing.T) {
	var attempts int
	errTransient := errors.New("temporary down")
	policy := RetryPolicy{
		BaseDelay:  20 * time.Millisecond,
		MaxDelay:   20 * time.Millisecond,
		MaxRetries: 100,
		Jitter:     0,
		IsTransient: func(err error) bool {
			return errors.Is(err, errTransient)
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancel()

	err := WithRetry(ctx, policy, func() error {
		attempts++
		return errTransient
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded, got %v", err)
	}
	if attempts < 1 {
		t.Fatalf("expected at least one attempt, got %d", attempts)
	}
}
