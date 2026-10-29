// Package tasks provides module-level functionality for tasks.
// input: a temp data_dir, a fake catalog, and the cluster lease-holder gate on the verifier
// output: proof that VerifyAllows is true only for the task lease holder (epoch > 0) and that a real Verifier.Pass through it checks a held task and skips an unheld one without metrics or findings
// pos: ADR 0009 phase 3 §10 gate tests for ADR 0011 per_node (docs/design/0009-phase3-verifier.md)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestVerifyAllows_LeaseHolderOnly is the ADR 0011 seam phase 3 §10 binds:
// the holder verifies, every other node skips. Epoch 0 and no worker id
// are "not held" too.
func TestVerifyAllows_LeaseHolderOnly(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(WithDataDir(dir), WithFileStore(newFakeFileStore()), WithClusterWorkerID("worker-a"))
	task, err := s.CreateTask("v", "k")
	if err != nil {
		t.Fatal(err)
	}
	allows := func() bool {
		ok, err := s.VerifyAllows(context.Background(), task.ID)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if allows() {
		t.Fatal("unheld task allowed")
	}
	s.mu.Lock()
	s.tasks[task.ID] = Task{ID: task.ID, OwnerWorkerID: "worker-a", Epoch: 3}
	s.mu.Unlock()
	if !allows() {
		t.Fatal("holder not allowed")
	}
	s.mu.Lock()
	s.tasks[task.ID] = Task{ID: task.ID, OwnerWorkerID: "worker-b", Epoch: 3}
	s.mu.Unlock()
	if allows() {
		t.Fatal("another holder allowed")
	}
	s.mu.Lock()
	s.tasks[task.ID] = Task{ID: task.ID, OwnerWorkerID: "worker-a"}
	s.mu.Unlock()
	if allows() {
		t.Fatal("epoch 0 allowed")
	}
	plain := NewScheduler(WithDataDir(t.TempDir()), WithFileStore(newFakeFileStore()))
	if ok, _ := plain.VerifyAllows(context.Background(), task.ID); ok {
		t.Fatal("no worker id allowed")
	}
}

// TestVerifier_ClusterOwnerVerifies wires the real gate through a pass:
// a held task is checked (metrics update), an unheld one is skipped.
func TestVerifier_ClusterOwnerVerifies(t *testing.T) {
	dir := t.TempDir()
	files := newFakeFileStore()
	s := NewScheduler(WithDataDir(dir), WithFileStore(files), WithFindingStore(NewFileFindingStore(filepath.Join(dir, FindingFileName))), WithClusterWorkerID("worker-a"))
	task, err := s.CreateTask("verify", "verify-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, task.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	putSealed(files, task.ID, "mysql-bin.000001", dir, BinlogFile{State: "SEALED", SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	v := NewVerifier(s, VerifierConfig{DataDir: dir, BudgetBytesPerSec: -1, Allows: s.VerifyAllows})
	s.SetVerifier(v)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	v.Pass(context.Background(), now)
	if _, ok := s.VerifyMetrics().Tasks[task.ID]; ok {
		t.Fatal("unheld task was checked")
	}
	s.mu.Lock()
	s.tasks[task.ID] = Task{ID: task.ID, OwnerWorkerID: "worker-a", Epoch: 3}
	s.mu.Unlock()
	v.Pass(context.Background(), now)
	if _, ok := s.VerifyMetrics().Tasks[task.ID]; !ok {
		t.Fatal("held task was not checked")
	}
}
