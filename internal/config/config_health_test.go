// Package config provides module-level functionality for config.
// input: YAML health blocks and the built-in defaults
// output: proof that health.evaluator, sweep, rehash, signal, no-progress and stale_after keys load, and that illegal values are rejected (design 0009 §28 for stale_after)
// pos: config tests for the health evaluator keys
// note: if this file changes, update this header and module README.md.
package config

import (
	"os"
	"path/filepath"
	"testing"
)

// P2-3: health.evaluator off|shadow|on (default off), health.sweep_interval_sec
// (default 60) and health.rehash_bytes_per_sec (default 4 MiB/s).
func TestLoadConfig_HealthDefaultsAndOverrides(t *testing.T) {
	setRequiredAuthEnv(t)
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Health.Evaluator != "off" || cfg.Health.SweepIntervalSec != 60 || cfg.Health.RehashBytesPerSec != 4<<20 {
		t.Fatalf("defaults %+v", cfg.Health)
	}
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("health:\n  evaluator: shadow\n  sweep_interval_sec: 20\n  rehash_bytes_per_sec: 1048576\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Health.Evaluator != "shadow" || cfg.Health.SweepIntervalSec != 20 || cfg.Health.RehashBytesPerSec != 1<<20 {
		t.Fatalf("overrides %+v", cfg.Health)
	}
	if err := os.WriteFile(path, []byte("health:\n  evaluator: maybe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("unknown health.evaluator accepted")
	}
}

// P2-4: health.unreachable_after_sec (default 90), a per-task override map,
// and health.signal_stale_after_sec (default 24h).
func TestLoadConfig_HealthSignalKeys(t *testing.T) {
	setRequiredAuthEnv(t)
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Health.UnreachableAfterSec != 90 || cfg.Health.SignalStaleAfterSec != 86400 || len(cfg.Health.UnreachableAfterTaskSec) != 0 {
		t.Fatalf("defaults %+v", cfg.Health)
	}
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("health:\n  unreachable_after_sec: 120\n  signal_stale_after_sec: 3600\n  unreachable_after_task_sec:\n    \"7\": 300\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Health.UnreachableAfterSec != 120 || cfg.Health.SignalStaleAfterSec != 3600 || cfg.Health.UnreachableAfterTaskSec["7"] != 300 {
		t.Fatalf("overrides %+v", cfg.Health)
	}
	for _, bad := range []string{"health:\n  unreachable_after_sec: -1\n", "health:\n  unreachable_after_task_sec:\n    \"7\": 0\n", "health:\n  unreachable_after_task_sec:\n    \"7\": soon\n"} {
		if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

// P2-6d: health.no_progress_after_sec (default 120, not negative) feeds
// TASK_NO_PROGRESS (design 0009 §24.9).
func TestLoadConfig_HealthNoProgressAfter(t *testing.T) {
	setRequiredAuthEnv(t)
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Health.NoProgressAfterSec != 120 {
		t.Fatalf("default %d", cfg.Health.NoProgressAfterSec)
	}
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("health:\n  no_progress_after_sec: 300\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err = LoadConfig(path); err != nil || cfg.Health.NoProgressAfterSec != 300 {
		t.Fatalf("override %v %+v", err, cfg)
	}
	if err := os.WriteFile(path, []byte("health:\n  no_progress_after_sec: -1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("negative health.no_progress_after_sec accepted")
	}
}

// P2-6.1: health.stale_after_sec defaults to 0 (5×sweep inside the evaluator)
// and a negative value is rejected (design 0009 §28).
func TestLoadConfig_HealthStaleAfter(t *testing.T) {
	setRequiredAuthEnv(t)
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Health.StaleAfterSec != 0 {
		t.Fatalf("default %d, want 0", cfg.Health.StaleAfterSec)
	}
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("health:\n  stale_after_sec: 120\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err = LoadConfig(path); err != nil || cfg.Health.StaleAfterSec != 120 {
		t.Fatalf("override %v %+v", err, cfg.Health.StaleAfterSec)
	}
	if err := os.WriteFile(path, []byte("health:\n  stale_after_sec: -1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("negative health.stale_after_sec accepted")
	}
}
