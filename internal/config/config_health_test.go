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
