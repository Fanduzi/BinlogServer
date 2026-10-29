// Package tasks provides module-level functionality for tasks.
// input: a store that reports an open refusal, and a cluster gate that reports a storage mode or an older protocol
// output: proof that a claim does not Acquire while a stored refusal is open, the registered storage mode differs, or a live registration is still below protocol 11
// pos: the guards in front of Acquire for ADR 0011 F11 and the stored half of F12
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type refusalStore struct {
	*schedulerTestStore
	open bool
}

func (s *refusalStore) RefusalOpen(context.Context, string, string) (bool, error) {
	return s.open, nil
}

func (s *refusalStore) DeleteTaskRefusals(context.Context, string) error { return nil }

type claimGate struct {
	registered string
	older      int
}

func (g claimGate) RegisterClusterStorage(context.Context, string) (string, error) {
	if g.registered == "" {
		return "per_node", nil
	}
	return g.registered, nil
}

func (g claimGate) CountOlderProtocols(context.Context, string, int64) (int, error) {
	return g.older, nil
}

func newClaimScheduler(t *testing.T, store TaskStore, gate ClusterGate) (*Scheduler, *fakeLeaseManager) {
	t.Helper()
	lease := &fakeLeaseManager{acquireEpoch: 4, acquireOK: true}
	runner := &fakeRunner{started: make(chan Task, 1)}
	s := NewScheduler(
		WithStore(store),
		WithRunner(runner),
		WithClusterLeaseManager(lease),
		WithClusterWorkerID("worker-b"),
		WithDataDir(t.TempDir()),
		WithClusterGate(gate),
	)
	return s, lease
}

func prepareClaim(t *testing.T, s *Scheduler) {
	t.Helper()
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatal(err)
	}
}

func TestStoredRefusalBlocksClaim(t *testing.T) {
	store := &refusalStore{schedulerTestStore: &schedulerTestStore{tasks: map[string]Task{}}, open: true}
	s, lease := newClaimScheduler(t, store, nil)
	prepareClaim(t, s)
	task, err := s.GetTask("1")
	if err != nil {
		t.Fatal(err)
	}
	err = s.StartTask(task.ID)
	if !errors.Is(err, ErrLeaseNotAcquired) {
		t.Fatalf("StartTask err=%v, want ErrLeaseNotAcquired", err)
	}
	if lease.acquireCalls != 0 {
		t.Fatalf("Acquire calls=%d, want 0", lease.acquireCalls)
	}
}

func TestMixedVersionRefusesClaim(t *testing.T) {
	store := &schedulerTestStore{tasks: map[string]Task{}}
	s, lease := newClaimScheduler(t, store, claimGate{older: 1})
	prepareClaim(t, s)
	err := s.StartTask("1")
	if err == nil || !strings.Contains(err.Error(), "mixed version") {
		t.Fatalf("StartTask err=%v, want mixed version", err)
	}
	if lease.acquireCalls != 0 {
		t.Fatalf("Acquire calls=%d, want 0", lease.acquireCalls)
	}
}

// The gate is a sweep-level fence, not a per-candidate one: the _cluster
// MIXED_VERSION finding must fire while an old-protocol node still dumps,
// even when this worker has nothing claimable. A claim loop that only
// checks per candidate would never open the finding under a live lease.
func TestMixedVersionSweepFiresWithoutCandidates(t *testing.T) {
	store := &schedulerTestStore{tasks: map[string]Task{}}
	s, _ := newClaimScheduler(t, store, claimGate{older: 1})
	claimed, err := s.ClaimRunnableTasks()
	if err == nil || !strings.Contains(err.Error(), "mixed version") {
		t.Fatalf("ClaimRunnableTasks err=%v, want mixed version", err)
	}
	if claimed != 0 {
		t.Fatalf("claimed=%d, want 0", claimed)
	}
}

func TestStorageMismatchRefusesClaim(t *testing.T) {
	store := &schedulerTestStore{tasks: map[string]Task{}}
	s, lease := newClaimScheduler(t, store, claimGate{registered: "shared"})
	prepareClaim(t, s)
	err := s.StartTask("1")
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("StartTask err=%v, want storage mismatch", err)
	}
	if lease.acquireCalls != 0 {
		t.Fatalf("Acquire calls=%d, want 0", lease.acquireCalls)
	}
}

func TestClaimProceedsWhenGateAgrees(t *testing.T) {
	store := &refusalStore{schedulerTestStore: &schedulerTestStore{tasks: map[string]Task{}}, open: false}
	s, lease := newClaimScheduler(t, store, claimGate{registered: "per_node"})
	prepareClaim(t, s)
	if err := s.StartTask("1"); err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	lease.mu.Lock()
	calls := lease.acquireCalls
	lease.mu.Unlock()
	if calls == 0 {
		t.Fatal("matching gate and a closed refusal must Acquire")
	}
	_ = s.StopTask("1")
}
