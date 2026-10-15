package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"binlog_server/internal/config"
	"binlog_server/internal/tasks"
)

// P2-3: off wires nothing; shadow/on in standalone persist the node id and
// write .health.json; an unknown mode is refused.
func TestStartHealthEvaluatorStandalone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	cfg := config.Config{DataDir: dir}
	cfg.Health = config.HealthConfig{Evaluator: "off", SweepIntervalSec: 60, RehashBytesPerSec: 1 << 20}
	if err := startHealthEvaluator(ctx, cfg, tasks.NewScheduler(tasks.WithDataDir(dir)), nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, tasks.NodeIDFileName)); !os.IsNotExist(err) {
		t.Fatalf("off created a node id: %v", err)
	}
	cfg.Health.Evaluator = "shadow"
	if err := startHealthEvaluator(ctx, cfg, tasks.NewScheduler(tasks.WithDataDir(dir)), nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, tasks.NodeIDFileName)); err != nil {
		t.Fatalf("shadow did not persist the node id: %v", err)
	}
	cfg.Health.Evaluator = "maybe"
	if err := startHealthEvaluator(ctx, cfg, tasks.NewScheduler(tasks.WithDataDir(dir)), nil, ""); err == nil {
		t.Fatal("unknown mode accepted")
	}
}
