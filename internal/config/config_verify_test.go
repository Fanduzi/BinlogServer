// Package config provides module-level functionality for config.
// input: YAML verify blocks and the built-in defaults
// output: proof that the phase 3 verifier keys load, that verify.recent_share_percent rejects 0 and values outside 1–100, and that a negative budget is rejected while 0 is kept
// pos: config tests for the verifier keys (docs/design/0009-phase3-verifier.md §9)
// note: if this file changes, update this header and module README.md.
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_VerifyDefaultsAndShare(t *testing.T) {
	setRequiredAuthEnv(t)
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	v := cfg.Verify
	if v.EverySec != 86400 || v.BudgetBytesPerSec != 4<<20 || v.RotationPeriods != 20 || v.RecentSharePercent != 25 ||
		v.PassDeadlineSec != 0 || v.PassMaxBytes != 0 || v.ObjectRecentSec != 86400 || v.DrillDSN != "" || v.DrillEverySec != 604800 || v.DrillCheckSQL != "" {
		t.Fatalf("defaults %+v", v)
	}
	path := filepath.Join(t.TempDir(), "c.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("verify:\n  every_sec: 3600\n  budget_bytes_per_sec: 0\n  rotation_periods: 0\n  recent_share_percent: 100\n  pass_deadline_sec: 10\n  pass_max_bytes: 4096\n  object_recent_sec: 60\n  drill_every_sec: 0\n  drill_check_sql: \"SELECT 1\"\n")
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	v = cfg.Verify
	if v.EverySec != 3600 || v.BudgetBytesPerSec != 0 || v.RotationPeriods != 0 || v.RecentSharePercent != 100 ||
		v.PassDeadlineSec != 10 || v.PassMaxBytes != 4096 || v.ObjectRecentSec != 60 || v.DrillEverySec != 0 || v.DrillCheckSQL != "SELECT 1" {
		t.Fatalf("overrides %+v", v)
	}
	for _, bad := range []string{
		"verify:\n  every_sec: 0\n",
		"verify:\n  budget_bytes_per_sec: -1\n",
		"verify:\n  rotation_periods: -1\n",
		"verify:\n  recent_share_percent: 0\n",
		"verify:\n  recent_share_percent: 101\n",
		"verify:\n  recent_share_percent: -1\n",
		"verify:\n  pass_deadline_sec: -1\n",
		"verify:\n  pass_max_bytes: -1\n",
		"verify:\n  object_recent_sec: -1\n",
		"verify:\n  drill_every_sec: -1\n",
	} {
		write(bad)
		if _, err := LoadConfig(path); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
