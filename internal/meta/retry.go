package meta

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"time"
)

type RetryPolicy struct {
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	MaxRetries  int
	Jitter      float64
	IsTransient func(error) bool
}

type permanentError struct {
	err error
}

func (e permanentError) Error() string {
	return e.err.Error()
}

func (e permanentError) Unwrap() error {
	return e.err
}

func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

func WithRetry(ctx context.Context, policy RetryPolicy, fn func() error) error {
	policy = normalizeRetryPolicy(policy)

	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}

		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if isPermanentError(err) {
			return err
		}
		if policy.IsTransient != nil && !policy.IsTransient(err) {
			return err
		}
		if attempt >= policy.MaxRetries {
			return err
		}

		wait := retryBackoff(policy, attempt)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func DefaultMySQLRetryPolicy() RetryPolicy {
	return RetryPolicy{
		BaseDelay:  100 * time.Millisecond,
		MaxDelay:   2 * time.Second,
		MaxRetries: 5,
		Jitter:     0.2,
		IsTransient: func(err error) bool {
			return IsTransientMySQLError(err)
		},
	}
}

func IsTransientMySQLError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "deadlock") ||
		strings.Contains(msg, "lock wait timeout") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "server has gone away") ||
		strings.Contains(msg, "read-only") ||
		strings.Contains(msg, "read only") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "eof")
}

func isPermanentError(err error) bool {
	var perr permanentError
	return errors.As(err, &perr)
}

func normalizeRetryPolicy(policy RetryPolicy) RetryPolicy {
	if policy.BaseDelay <= 0 {
		policy.BaseDelay = 50 * time.Millisecond
	}
	if policy.MaxDelay <= 0 {
		policy.MaxDelay = policy.BaseDelay
	}
	if policy.MaxDelay < policy.BaseDelay {
		policy.MaxDelay = policy.BaseDelay
	}
	if policy.MaxRetries < 0 {
		policy.MaxRetries = 0
	}
	if policy.Jitter < 0 {
		policy.Jitter = 0
	}
	if policy.Jitter > 1 {
		policy.Jitter = 1
	}
	return policy
}

func retryBackoff(policy RetryPolicy, attempt int) time.Duration {
	backoff := policy.BaseDelay
	for i := 0; i < attempt; i++ {
		backoff *= 2
		if backoff >= policy.MaxDelay {
			backoff = policy.MaxDelay
			break
		}
	}
	if policy.Jitter <= 0 {
		return backoff
	}
	min := 1 - policy.Jitter
	max := 1 + policy.Jitter
	factor := min + (max-min)*rand.Float64() // #nosec G404 -- jitter randomness is non-security related.
	return time.Duration(float64(backoff) * factor)
}
