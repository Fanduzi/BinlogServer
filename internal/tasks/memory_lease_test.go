// Package tasks provides module-level functionality for tasks.
// input: MemoryLease Acquire/Release sequences between two workers
// output: exclusive grant, immediate free after Release, hold during unexpired ownership
// pos: in-process ownership-door tests without MySQL
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"testing"
	"time"
)

func TestMemoryLease_OtherWorkerCannotAcquireUntilRelease(t *testing.T) {
	leases := NewMemoryLease()
	ctx := context.Background()
	ttl := time.Hour

	epoch, ok, err := leases.Acquire(ctx, "1", "worker-a", ttl)
	if err != nil || !ok || epoch != 1 {
		t.Fatalf("worker-a acquire: epoch=%d ok=%v err=%v", epoch, ok, err)
	}

	_, ok, err = leases.Acquire(ctx, "1", "worker-b", ttl)
	if err != nil {
		t.Fatalf("worker-b acquire error: %v", err)
	}
	if ok {
		t.Fatal("worker-b acquired while worker-a still holds the lease")
	}

	released, err := leases.Release(ctx, "1", "worker-a", epoch)
	if err != nil || !released {
		t.Fatalf("release: released=%v err=%v", released, err)
	}

	epochB, ok, err := leases.Acquire(ctx, "1", "worker-b", ttl)
	if err != nil || !ok {
		t.Fatalf("worker-b acquire after release: ok=%v err=%v", ok, err)
	}
	if epochB != epoch+1 {
		t.Fatalf("expected epoch %d after takeover, got %d", epoch+1, epochB)
	}
}

func TestMemoryLease_SameWorkerCanReacquireWithoutRelease(t *testing.T) {
	leases := NewMemoryLease()
	ctx := context.Background()
	ttl := time.Hour

	epoch, ok, err := leases.Acquire(ctx, "1", "worker-a", ttl)
	if err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}
	again, ok, err := leases.Acquire(ctx, "1", "worker-a", ttl)
	if err != nil || !ok {
		t.Fatalf("reacquire: ok=%v err=%v", ok, err)
	}
	if again != epoch {
		t.Fatalf("same-worker reacquire changed epoch %d -> %d", epoch, again)
	}
}

func TestMemoryLease_VerifyFalseAfterRelease(t *testing.T) {
	leases := NewMemoryLease()
	ctx := context.Background()
	epoch, ok, err := leases.Acquire(ctx, "1", "worker-a", time.Hour)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	held, err := leases.Verify(ctx, "1", "worker-a", epoch)
	if err != nil || !held {
		t.Fatalf("verify before release: held=%v err=%v", held, err)
	}
	if _, err := leases.Release(ctx, "1", "worker-a", epoch); err != nil {
		t.Fatalf("release: %v", err)
	}
	held, err = leases.Verify(ctx, "1", "worker-a", epoch)
	if err != nil {
		t.Fatalf("verify after release: %v", err)
	}
	if held {
		t.Fatal("verify still true after Release")
	}
}
