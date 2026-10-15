package tasks

import (
	"context"
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
