package cmd

import (
	"context"
	"testing"
	"time"
)

// After a signal the watchdog restores default handlers (stop) and exits 2
// if Run has not returned within the limit.
func TestShutdownWatchdog_ExitsWhenRunHangs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	code := make(chan int, 1)
	go shutdownWatchdog(ctx, func() { close(stopped) }, make(chan struct{}), 50*time.Millisecond, func(c int) { code <- c })
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("default signal handlers not restored after the first signal")
	}
	select {
	case c := <-code:
		if c != 2 {
			t.Fatalf("exit code %d, want 2", c)
		}
	case <-time.After(time.Second):
		t.Fatal("watchdog did not bound the shutdown")
	}
}

func TestShutdownWatchdog_QuietWhenRunReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	called := make(chan int, 1)
	go shutdownWatchdog(ctx, func() {}, runDone, 50*time.Millisecond, func(c int) { called <- c })
	cancel()
	close(runDone)
	select {
	case <-called:
		t.Fatal("watchdog exited although Run returned")
	case <-time.After(200 * time.Millisecond):
	}
}
