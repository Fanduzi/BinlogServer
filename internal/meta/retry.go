// Package meta provides module-level functionality for meta.
// input: MySQL connections, SQL schema/contracts, retry/lease timing policies
// output: persistent metadata operations for tasks, leases, runs, and checkpoints
// pos: metadata persistence layer between domain scheduler and MySQL storage engine
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
)

// Policy 描述一次重试流程的退避和判错策略。
type Policy struct {
	// BaseDelay 是首次重试等待时间。
	BaseDelay time.Duration
	// MaxDelay 是退避上限。
	MaxDelay time.Duration
	// MaxRetries 是最大重试次数（不含首次执行）。
	MaxRetries int
	// Jitter 是随机抖动比例（0~1）。
	Jitter float64
	// IsTransient 判断错误是否可重试。
	IsTransient func(error) bool
}

// RetryPolicy 保持对现有调用方的兼容别名。
type RetryPolicy = Policy

// RetryExecutor 定义统一重试执行接口，屏蔽第三方类型细节。
type RetryExecutor interface {
	Do(ctx context.Context, policy Policy, fn func() error) error
}

type backoffRetryExecutor struct{}

type permanentError struct {
	err error
}

// Error 返回原始错误文本。
func (e permanentError) Error() string {
	return e.err.Error()
}

// Unwrap 支持 errors.Is/errors.As 解包。
func (e permanentError) Unwrap() error {
	return e.err
}

// Permanent 将错误包装为“不可重试”。
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

var defaultRetryExecutor RetryExecutor = backoffRetryExecutor{}

// WithRetry 按策略执行带重试的函数。
func WithRetry(ctx context.Context, policy RetryPolicy, fn func() error) error {
	return defaultRetryExecutor.Do(ctx, normalizeRetryPolicy(policy), fn)
}

// DefaultMySQLRetryPolicy 返回适用于 MySQL 元数据操作的默认重试策略。
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

// IsTransientMySQLError 基于错误文本做瞬时错误判定。
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

// isPermanentError 判断错误链上是否包含 permanent 标记。
func isPermanentError(err error) bool {
	var perr permanentError
	return errors.As(err, &perr)
}

// normalizeRetryPolicy 补全重试策略默认值并修正非法配置。
func normalizeRetryPolicy(policy Policy) Policy {
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

// Do 执行带退避与重试上限的函数调用。
func (e backoffRetryExecutor) Do(ctx context.Context, policy Policy, fn func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	policy = normalizeRetryPolicy(policy)

	exp := backoff.NewExponentialBackOff()
	exp.InitialInterval = policy.BaseDelay
	exp.RandomizationFactor = policy.Jitter
	exp.Multiplier = 2
	exp.MaxInterval = policy.MaxDelay
	exp.MaxElapsedTime = 0
	exp.Reset()

	strategy := backoff.WithContext(backoff.WithMaxRetries(exp, uint64(policy.MaxRetries)), ctx)
	return backoff.Retry(func() error {
		err := fn()
		if err == nil {
			return nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return backoff.Permanent(err)
		}
		if isPermanentError(err) {
			return backoff.Permanent(err)
		}
		if policy.IsTransient != nil && !policy.IsTransient(err) {
			return backoff.Permanent(err)
		}
		return err
	}, strategy)
}
