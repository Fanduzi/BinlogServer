package tasks

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
	"binlog_server/internal/taskrecords"
)

// fullHealthStore is .health.json on a full data_dir: reads work, every write
// fails with ENOSPC until full is cleared.
type fullHealthStore struct {
	*healthstore.FileStore
	full atomic.Bool
}

func (f *fullHealthStore) ApplyTransitions(ctx context.Context, id string, fence health.Fence, ts []health.Transition, h health.TaskHealth) (healthstore.Applied, error) {
	if f.full.Load() {
		return healthstore.Applied{}, fmt.Errorf("write %s: %w", filepath.Base(healthstore.HealthFileName), syscall.ENOSPC)
	}
	return f.FileStore.ApplyTransitions(ctx, id, fence, ts, h)
}

func (f *fullHealthStore) Unwrap() healthstore.Store { return f.FileStore }

// fullFindingStore is .findings.json on the same full disk.
type fullFindingStore struct {
	FindingStore
	full atomic.Bool
}

func (f *fullFindingStore) UpsertFinding(ctx context.Context, it Finding) error {
	if f.full.Load() {
		return fmt.Errorf("write .findings.json: %w", syscall.ENOSPC)
	}
	return f.FindingStore.UpsertFinding(ctx, it)
}

// #395 QA P2: with data_dir full, TASK_RECORDS_FAILED/write must still be
// served by GET /api/findings and degrade GET /api/health. The in-memory
// finding state is authoritative; the failed persistence is a finding of
// its own, and both clear once the disk has room again.
func TestFullDisk_RecordsWriteFindingServedFromMemory(t *testing.T) {
	data := recordDir(t)
	hs := &fullHealthStore{FileStore: healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))}
	fs := &fullFindingStore{FindingStore: NewFileFindingStore(filepath.Join(data, FindingFileName))}
	hs.full.Store(true)
	fs.full.Store(true)
	file := &failingRecordFile{Store: taskrecords.Open(data, recordKey(t, 1), nil), fail: true}
	s, _ := recordProcess(t, data, file, nil, WithFindingStore(fs))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.StartFindingSink(ctx, 10*time.Millisecond)
	e := NewEvaluator(s, EvaluatorConfig{Mode: EvaluatorOn, Store: hs, NodeID: "n", Instance: "i", Debounce: time.Millisecond})
	if _, err := s.CreateTaskFromSpec("d", "d-key", recordSourceCfg(), nil, nil); err == nil {
		t.Fatal("create accepted with a failing records save")
	}
	_ = e.SweepNow(context.Background())

	open, err := s.OpenFindings(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var write, persist *Finding
	for i := range open {
		switch {
		case open[i].Kind == FindingTaskRecordsFailed && open[i].Subject == RecordsWrite:
			write = &open[i]
		case open[i].Kind == FindingFindingsPersistFailed:
			persist = &open[i]
		}
	}
	if write == nil {
		t.Fatalf("TASK_RECORDS_FAILED/write not served while the health store is full: %+v", open)
	}
	if persist == nil || persist.Severity != FindingCritical || !strings.Contains(persist.Detail, "no space left") {
		t.Fatalf("no CRITICAL %s naming ENOSPC: %+v", FindingFindingsPersistFailed, persist)
	}
	if got := s.HealthSummary(context.Background()); got.Status != HealthDegraded {
		t.Fatalf("health %+v while data_dir is full, want %s", got, HealthDegraded)
	}

	// Room again: the state lands in the store and the persist finding closes.
	hs.full.Store(false)
	fs.full.Store(false)
	file.fail = false
	s.RetryRecordSave()
	for i := 0; i < 3; i++ {
		_ = e.SweepNow(context.Background())
	}
	s.FlushFindings(context.Background())
	open, _ = s.OpenFindings(context.Background(), 100)
	for _, f := range open {
		if f.Kind == FindingFindingsPersistFailed || (f.Kind == FindingTaskRecordsFailed && f.Subject == RecordsWrite) {
			t.Fatalf("%s/%s still open after the disk has room: %+v", f.Kind, f.Subject, open)
		}
	}
	if got := s.HealthSummary(context.Background()); got.Status != HealthOK {
		t.Fatalf("health %+v after recovery, want %s", got, HealthOK)
	}
}

// #395 QA P3: a startup reason left open by an earlier process
// (wrong_key) closes when this start finds a different one (key_missing).
func TestTaskRecords_StaleStartupReasonClosesWhenReasonChanges(t *testing.T) {
	data := recordDir(t)
	s1, _ := recordProcess(t, data, taskrecords.Open(data, recordKey(t, 1), nil), nil)
	if _, err := s1.CreateTaskFromSpec("r", "r-key", recordSourceCfg(), &StartConfig{Mode: StartModeLatest}, nil); err != nil {
		t.Fatal(err)
	}
	store := healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))
	sweep := func(file RecordFile, keyErr error) {
		t.Helper()
		s := NewScheduler(WithDataDir(data), WithRunner(newRecordRunner()), WithClusterLeaseManager(NewMemoryLease()),
			WithClusterWorkerID("standalone"), WithTaskRecords(file, keyErr))
		e := NewEvaluator(s, EvaluatorConfig{Mode: EvaluatorOn, Store: store, NodeID: "n", Instance: "i", Debounce: time.Millisecond})
		if err := s.Restore(context.Background()); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if err := e.SweepNow(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
	}
	sweep(taskrecords.Open(data, recordKey(t, 2), nil), nil) // wrong key
	if _, ok := openOf(t, store, ClusterFindingTask)[string(FindingTaskRecordsFailed)+"/"+RecordsWrongKey]; !ok {
		t.Fatalf("setup: no wrong_key: %+v", openOf(t, store, ClusterFindingTask))
	}
	sweep(taskrecords.Open(data, nil, nil), taskrecords.ErrKeyMissing) // key gone
	open := openOf(t, store, ClusterFindingTask)
	if _, ok := open[string(FindingTaskRecordsFailed)+"/"+RecordsKeyMissing]; !ok {
		t.Fatalf("no key_missing: %+v", open)
	}
	if _, ok := open[string(FindingTaskRecordsFailed)+"/"+RecordsWrongKey]; ok {
		t.Fatalf("stale wrong_key from the earlier start is still open: %+v", open)
	}
}
