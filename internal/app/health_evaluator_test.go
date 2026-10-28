// Package app provides module-level functionality for app.
// input: standalone and cluster-role configs fed to startHealthEvaluator and healthEvaluatorIdentity
// output: the evaluator is always constructed and started; standalone persists the node id and writes .health.json; a control-plane-only node resolves a node id (cluster.worker_id, else persisted) yet never evaluates as a task holder
// pos: P2-3 / design 0009 §24.13 wiring tests
// note: if this file changes, update this header and module README.md.
package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"binlog_server/internal/config"
	"binlog_server/internal/tasks"
)

// P2-3: the evaluator always runs; in standalone it persists the node id
// and writes .health.json.
func TestStartHealthEvaluatorStandalone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	cfg := config.Config{DataDir: dir}
	cfg.Health = config.HealthConfig{SweepIntervalSec: 60, RehashBytesPerSec: 1 << 20}
	if err := startHealthEvaluator(ctx, cfg, tasks.NewScheduler(tasks.WithDataDir(dir)), nil, "", func(f func()) { go f() }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, tasks.NodeIDFileName)); err != nil {
		t.Fatalf("evaluator did not persist the node id: %v", err)
	}
}

// QA r3 (b), design 0009 §24.13: a control-plane-only node resolves no
// worker id, yet its evaluator must have a node id (cluster.worker_id, else
// the persisted one) to hold _health, and never evaluate as a task holder.
func TestHealthEvaluatorIdentity(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{DataDir: dir, Mode: "cluster"}
	cfg.Cluster.Role = "control-plane"
	id, cpOnly, err := healthEvaluatorIdentity(cfg, "")
	if err != nil || id == "" || !cpOnly {
		t.Fatalf("control-plane without worker_id: id=%q cpOnly=%v err=%v", id, cpOnly, err)
	}
	again, _, err := healthEvaluatorIdentity(cfg, "")
	if err != nil || again != id {
		t.Fatalf("control-plane node id not stable: %q then %q (%v)", id, again, err)
	}
	cfg.Cluster.WorkerID = "n2"
	if id, cpOnly, err = healthEvaluatorIdentity(cfg, ""); err != nil || id != "n2" || !cpOnly {
		t.Fatalf("control-plane with worker_id: id=%q cpOnly=%v err=%v", id, cpOnly, err)
	}
	cfg.Cluster.Role = "all-in-one"
	if id, cpOnly, err = healthEvaluatorIdentity(cfg, "n1"); err != nil || id != "n1" || cpOnly {
		t.Fatalf("worker: id=%q cpOnly=%v err=%v", id, cpOnly, err)
	}
	standalone := config.Config{DataDir: t.TempDir()}
	if id, cpOnly, err = healthEvaluatorIdentity(standalone, "standalone"); err != nil || id == "" || id == "standalone" || cpOnly {
		t.Fatalf("standalone: id=%q cpOnly=%v err=%v", id, cpOnly, err)
	}
}
