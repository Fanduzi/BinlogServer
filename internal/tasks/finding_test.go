// Package tasks provides module-level functionality for tasks.
// input: injected store, event-store, and disk failures
// output: proof that each phase-0 failure path ends in a finding (ADR 0009, S1/S2/S3/S13/S15) and that the sink retries its own failed writes
// pos: fault-injection tests for the finding sink
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type memFindingStore struct {
	mu   sync.Mutex
	rows map[findingKey]Finding
	fail error
}

func newMemFindingStore() *memFindingStore {
	return &memFindingStore{rows: map[findingKey]Finding{}}
}

func (m *memFindingStore) UpsertFinding(_ context.Context, f Finding) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	key := findingKey{f.TaskID, f.Kind, f.Subject}
	if prev, ok := m.rows[key]; ok && prev.FirstSeen.Before(f.FirstSeen) {
		f.FirstSeen = prev.FirstSeen
	}
	m.rows[key] = f
	return nil
}

func (m *memFindingStore) ListFindings(_ context.Context, taskID string, includeResolved bool, limit int) ([]Finding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Finding
	for k, f := range m.rows {
		if k.task == taskID && (includeResolved || f.ResolvedAt == nil) {
			out = append(out, f)
		}
	}
	return out, nil
}

func (m *memFindingStore) ListOpenFindings(_ context.Context, limit int) ([]Finding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Finding
	for _, f := range m.rows {
		if f.ResolvedAt == nil {
			out = append(out, f)
		}
	}
	return out, nil
}

func (m *memFindingStore) get(task string, kind FindingKind, subject string) (Finding, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.rows[findingKey{task, kind, subject}]
	return f, ok
}

func openFinding(s *Scheduler, task string, kind FindingKind) (Finding, bool) {
	for _, f := range s.FindingStats().Open {
		if f.TaskID == task && f.Kind == kind {
			return f, true
		}
	}
	return Finding{}, false
}

func TestFindingSink_RetriesFailedWritesAndKeepsNewest(t *testing.T) {
	store := newMemFindingStore()
	s := NewScheduler(WithFindingStore(store))
	store.fail = errors.New("meta down")
	s.Report(Finding{TaskID: "7", Kind: FindingTaskPersistFailed, Detail: "first"})
	s.FlushFindings(context.Background())
	st := s.FindingStats()
	if st.SinkErrors != 1 || st.PendingOps != 1 || len(st.Open) != 1 {
		t.Fatalf("after failed write: %+v", st)
	}
	s.Report(Finding{TaskID: "7", Kind: FindingTaskPersistFailed, Detail: "second"})
	store.fail = nil
	s.FlushFindings(context.Background())
	got, ok := store.get("7", FindingTaskPersistFailed, "")
	if !ok || got.Detail != "second" || got.Count != 2 || !got.Open() {
		t.Fatalf("stored %+v ok=%v", got, ok)
	}
	if s.FindingStats().PendingOps != 0 {
		t.Fatalf("pending left")
	}
	s.Resolve("7", FindingTaskPersistFailed, "")
	s.FlushFindings(context.Background())
	if got, _ := store.get("7", FindingTaskPersistFailed, ""); got.Open() {
		t.Fatalf("not resolved: %+v", got)
	}
	if st := s.FindingStats(); len(st.Open) != 0 || st.Reported[FindingTaskPersistFailed] != 2 {
		t.Fatalf("stats %+v", st)
	}
}

type failingEventStore struct {
	*fakeEventStore
	appendErr error
	listErr   error
}

func (f *failingEventStore) AppendEvent(ctx context.Context, e TaskEvent) error {
	if f.appendErr != nil {
		return f.appendErr
	}
	return f.fakeEventStore.AppendEvent(ctx, e)
}

func (f *failingEventStore) ListEvents(ctx context.Context, id string, limit int) ([]TaskEvent, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.fakeEventStore.ListEvents(ctx, id, limit)
}

// S1: an event the store refused is a finding, not a dropped error.
func TestS1_EventPersistFailureIsFinding(t *testing.T) {
	events := &failingEventStore{fakeEventStore: newFakeEventStore(), appendErr: errors.New("insert task_events: read-only")}
	s := NewScheduler(WithEventStore(events))
	task, err := s.CreateTask("s1", "s1-key")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	f, ok := openFinding(s, task.ID, FindingEventPersistFailed)
	if !ok || f.Subject != "TASK_CREATED" {
		t.Fatalf("no EVENT_PERSIST_FAILED for TASK_CREATED: %+v", s.FindingStats().Open)
	}
}

// S2: a state transition the store refused is CRITICAL and is written again
// once the store is back; then the finding resolves.
func TestS2_TransitionPersistFailureIsFindingAndRepublished(t *testing.T) {
	store := newFakeStore()
	findings := newMemFindingStore()
	s := NewScheduler(WithStore(store), WithFindingStore(findings))
	task, err := s.CreateTask("s2", "s2-key")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	store.upsertErr = errors.New("server has gone away")
	s.mu.Lock()
	item := s.tasks[task.ID]
	item.State = StateFailed
	item.LastError = "boom"
	s.tasks[item.ID] = item
	err = s.persistTaskLocked(item)
	s.mu.Unlock()
	if err == nil {
		t.Fatalf("expected persist error")
	}
	if store.tasks[task.ID].State == StateFailed {
		t.Fatalf("fake store took the write")
	}
	f, ok := openFinding(s, task.ID, FindingTaskPersistFailed)
	if !ok || f.Severity != FindingCritical {
		t.Fatalf("no CRITICAL TASK_PERSIST_FAILED: %+v", s.FindingStats().Open)
	}
	store.upsertErr = nil
	s.republishUnpublished()
	if got := store.tasks[task.ID].State; got != StateFailed {
		t.Fatalf("store row state %s after republish, want FAILED", got)
	}
	if _, ok := openFinding(s, task.ID, FindingTaskPersistFailed); ok {
		t.Fatalf("finding still open after republish")
	}
	s.FlushFindings(context.Background())
	if got, ok := findings.get(task.ID, FindingTaskPersistFailed, ""); !ok || got.Open() {
		t.Fatalf("store finding %+v ok=%v", got, ok)
	}
}

type failingPendingStore struct {
	*pendingColumnStore
	saveErr error
}

func (f *failingPendingStore) SavePendingDumpCleanup(ctx context.Context, id, prev, next string) (bool, error) {
	if f.saveErr != nil {
		return false, f.saveErr
	}
	return f.pendingColumnStore.SavePendingDumpCleanup(ctx, id, prev, next)
}

// S3: the KILL-pending marker that did not reach the store is CRITICAL.
func TestS3_DumpCleanupMarkerWriteFailureIsFinding(t *testing.T) {
	store := &failingPendingStore{pendingColumnStore: newPendingColumnStore()}
	s := NewScheduler(WithStore(store), WithClusterWorkerID("worker-a"))
	task := s.mustTask(t)
	store.saveErr = errors.New("lock wait timeout")
	s.noteDumpHeld(task.ID, SourceConfig{Host: "10.0.0.8", Port: 3306}, 991, 1)
	if _, ok := openFinding(s, task.ID, FindingDumpCleanupPersistFailed); !ok {
		t.Fatalf("no DUMP_CLEANUP_PERSIST_FAILED: %+v", s.FindingStats().Open)
	}
}

// S13: a retry budget that could not be read is visible.
func TestS13_RetryBudgetReadFailureIsFinding(t *testing.T) {
	events := &failingEventStore{fakeEventStore: newFakeEventStore()}
	s := NewScheduler(WithEventStore(events))
	task, err := s.CreateTask("s13", "s13-key")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	events.listErr = errors.New("bad connection")
	if got := s.legacyUnreachableStreak(task.ID); got != 0 {
		t.Fatalf("streak %d", got)
	}
	if _, ok := openFinding(s, task.ID, FindingEventReadFailed); !ok {
		t.Fatalf("no EVENT_READ_FAILED: %+v", s.FindingStats().Open)
	}
	events.listErr = nil
	s.legacyUnreachableStreak(task.ID)
	if _, ok := openFinding(s, task.ID, FindingEventReadFailed); ok {
		t.Fatalf("EVENT_READ_FAILED not resolved after a good read")
	}
}

// S15: a data_dir that cannot be listed is visible.
func TestS15_DiskListFailureIsFinding(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewScheduler(WithDataDir(dir))
	s.ListTasks()
	if _, ok := openFinding(s, ClusterFindingTask, FindingDiskListFailed); !ok {
		t.Fatalf("no DISK_LIST_FAILED: %+v", s.FindingStats().Open)
	}
}
