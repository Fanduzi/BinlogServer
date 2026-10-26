// Package tasks provides module-level functionality for tasks.
// input: a finding store whose write is slow or blocked, and a scheduler sink
// output: WaitFindingSink stays in the final flush until the store returns, and quiet stays false while that flush is in flight
// pos: shutdown and #490 coverage for the finding sink's in-flight bit
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// slowFindingStore delays each write so the sink's flush after cancel is
// still in flight when shutdown begins.
type slowFindingStore struct {
	FindingStore
	writes atomic.Int32
}

func (s *slowFindingStore) UpsertFinding(ctx context.Context, f Finding) error {
	time.Sleep(150 * time.Millisecond)
	s.writes.Add(1)
	return s.FindingStore.UpsertFinding(ctx, f)
}

// Shutdown must not return while the sink is still writing its last flush:
// in standalone that write lands in data_dir after Run returned and raced
// the caller's cleanup (TestApp_StartupReadsUploadedSegment flaked in CI on
// #395 at 56b83812).
func TestFindingSink_WaitCoversFinalFlush(t *testing.T) {
	store := &slowFindingStore{FindingStore: NewFileFindingStore(t.TempDir() + "/f.json")}
	s := NewScheduler(WithFindingStore(store))
	ctx, cancel := context.WithCancel(context.Background())
	s.StartFindingSink(ctx, time.Hour)
	s.Report(Finding{TaskID: "7", Kind: FindingTaskPersistFailed, Detail: "last"})
	cancel()
	if !s.WaitFindingSink(5 * time.Second) {
		t.Fatal("WaitFindingSink timed out")
	}
	if store.writes.Load() == 0 {
		t.Fatal("WaitFindingSink returned before the final flush wrote the finding")
	}
	if !NewScheduler().WaitFindingSink(time.Millisecond) {
		t.Fatal("WaitFindingSink without a started sink must return at once")
	}
}

// blockFindingStore holds UpsertFinding until release is closed.
type blockFindingStore struct {
	*memFindingStore
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockFindingStore) UpsertFinding(ctx context.Context, f Finding) error {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return b.memFindingStore.UpsertFinding(ctx, f)
}

// TestFindingSink_QuietSeesInflight fails when quiet ignores a flush that
// has already taken its batch (pending empty, inflight still set) or when
// quiet is always true.
func TestFindingSink_QuietSeesInflight(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	stop := func() { releaseOnce.Do(func() { close(release) }) }
	store := &blockFindingStore{
		memFindingStore: newMemFindingStore(),
		entered:         make(chan struct{}),
		release:         release,
	}
	s := NewScheduler(WithFindingStore(store))
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		stop()
		cancel()
		s.WaitFindingSink(2 * time.Second)
	}()
	s.StartFindingSink(ctx, time.Hour)
	s.Report(Finding{TaskID: "1", Kind: FindingTaskPersistFailed, Detail: "held"})
	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("flush did not reach the store")
	}
	if s.findings.quiet() {
		t.Fatal("quiet while a flush is in flight")
	}
	stop()
	deadline := time.Now().Add(2 * time.Second)
	for !s.findings.quiet() {
		if time.Now().After(deadline) {
			t.Fatal("not quiet after the flush finished")
		}
		time.Sleep(time.Millisecond)
	}
}
