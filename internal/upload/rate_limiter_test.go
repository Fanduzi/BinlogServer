// Package upload provides module-level functionality for upload.
// input: local binlog files, object store credentials/config, upload retry context
// output: object storage upload operations and upload status/error outcomes
// pos: outbound storage adapter layer for sealed binlog artifact distribution
// note: if this file changes, update this header and module README.md.
package upload

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestNewUploadRateLimiter_ZeroBurstDefaultsToOne 验证 burst=0 时自动修正为 1。
func TestNewUploadRateLimiter_ZeroBurstDefaultsToOne(t *testing.T) {
	rl := NewUploadRateLimiter(10, 0)
	if rl == nil {
		t.Fatal("expected non-nil limiter")
	}
	// burst=1：第一次 Wait 应立即返回。
	ctx := context.Background()
	if err := rl.Wait(ctx); err != nil {
		t.Fatalf("first Wait unexpectedly failed: %v", err)
	}
}

// TestNewUploadRateLimiter_Unlimited 验证 uploadsPerSecond<=0 时不限速。
func TestNewUploadRateLimiter_Unlimited(t *testing.T) {
	rl := NewUploadRateLimiter(0, 1)
	ctx := context.Background()
	// rate.Inf 下多次 Wait 应立即返回，无阻塞。
	for i := 0; i < 100; i++ {
		if err := rl.Wait(ctx); err != nil {
			t.Fatalf("Wait %d failed on unlimited limiter: %v", i, err)
		}
	}
}

// TestNewUploadRateLimiter_NegativeRateUnlimited 验证负速率同样不限速。
func TestNewUploadRateLimiter_NegativeRateUnlimited(t *testing.T) {
	rl := NewUploadRateLimiter(-5, 2)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if err := rl.Wait(ctx); err != nil {
			t.Fatalf("Wait %d failed on negative-rate limiter: %v", i, err)
		}
	}
}

// TestUploadRateLimiter_BurstAllowedImmediately 验证令牌桶满时突发请求立即通过。
func TestUploadRateLimiter_BurstAllowedImmediately(t *testing.T) {
	const burst = 5
	// 速率极低（0.001/s），仅靠 burst 令牌。
	rl := NewUploadRateLimiter(0.001, burst)
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < burst; i++ {
		if err := rl.Wait(ctx); err != nil {
			t.Fatalf("burst Wait %d failed: %v", i, err)
		}
	}
	// burst 内不应阻塞超过 100ms。
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("burst requests took too long: %v", elapsed)
	}
}

// TestUploadRateLimiter_ExceedBurstBlocks 验证超出 burst 后 Wait 会阻塞直至 ctx 取消。
func TestUploadRateLimiter_ExceedBurstBlocks(t *testing.T) {
	// burst=1, rate=0.001/s：第二次 Wait 需等待约 1000s，用 ctx 取消代替。
	rl := NewUploadRateLimiter(0.001, 1)
	ctx := context.Background()
	// 消耗唯一令牌。
	if err := rl.Wait(ctx); err != nil {
		t.Fatalf("first Wait failed: %v", err)
	}
	// 第二次 Wait 应阻塞；用超时 ctx 触发取消。
	ctxTimeout, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := rl.Wait(ctxTimeout); err == nil {
		t.Fatal("expected Wait to fail after burst exhausted")
	}
}

// TestUploadRateLimiter_ContextCancelledBeforeWait 验证已取消的 ctx 立即返回错误。
func TestUploadRateLimiter_ContextCancelledBeforeWait(t *testing.T) {
	rl := NewUploadRateLimiter(1, 1)
	// 消耗令牌。
	if err := rl.Wait(context.Background()); err != nil {
		t.Fatalf("first Wait failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 提前取消
	if err := rl.Wait(ctx); err == nil {
		t.Fatal("expected error for pre-cancelled context")
	}
}

// TestUploadRateLimiter_Concurrent 验证并发调用安全（race detector）。
func TestUploadRateLimiter_Concurrent(t *testing.T) {
	rl := NewUploadRateLimiter(1000, 1000)
	ctx := context.Background()
	const goroutines = 50
	var (
		wg      sync.WaitGroup
		success atomic.Int64
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if err := rl.Wait(ctx); err == nil {
				success.Add(1)
			}
		}()
	}
	wg.Wait()
	if success.Load() != goroutines {
		t.Fatalf("expected %d successes, got %d", goroutines, success.Load())
	}
}

// TestUploadRateLimiter_LargeRate 验证极大速率（1e9/s）不 panic 且正常工作。
func TestUploadRateLimiter_LargeRate(t *testing.T) {
	rl := NewUploadRateLimiter(1e9, 100)
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		if err := rl.Wait(ctx); err != nil {
			t.Fatalf("Wait %d failed on large-rate limiter: %v", i, err)
		}
	}
}
