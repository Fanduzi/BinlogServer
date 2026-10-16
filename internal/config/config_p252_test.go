package config

import (
	"os"
	"path/filepath"
	"testing"
)

// design 0009 §21: P2-5a upload grace and the P2-5.2 health keys.
func TestLoadConfig_P252Keys(t *testing.T) {
	setRequiredAuthEnv(t)
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UploadGraceSec != 900 || len(cfg.UploadGraceTaskSec) != 0 || cfg.Health.UploadLateCriticalAfterSec != 86400 ||
		cfg.Health.ObjectListIntervalSec != 300 || cfg.Health.ObjectListLimit != 10000 {
		t.Fatalf("defaults grace=%d %v health=%+v", cfg.UploadGraceSec, cfg.UploadGraceTaskSec, cfg.Health)
	}
	path := filepath.Join(t.TempDir(), "c.yaml")
	body := "upload:\n  grace_sec: 120\n  grace_task_sec:\n    \"7\": 1800\n" +
		"health:\n  upload_late_critical_after_sec: 3600\n  upload_late_critical_after_task_sec:\n    \"7\": 7200\n  object_list_interval_sec: 60\n  object_list_limit: 10\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err = LoadConfig(path); err != nil {
		t.Fatal(err)
	}
	if cfg.UploadGraceSec != 120 || cfg.UploadGraceTaskSec["7"] != 1800 || cfg.Health.UploadLateCriticalAfterSec != 3600 ||
		cfg.Health.UploadLateCriticalAfterTaskSec["7"] != 7200 || cfg.Health.ObjectListIntervalSec != 60 || cfg.Health.ObjectListLimit != 10 {
		t.Fatalf("overrides grace=%d %v health=%+v", cfg.UploadGraceSec, cfg.UploadGraceTaskSec, cfg.Health)
	}
	for _, bad := range []string{
		"upload:\n  grace_sec: -1\n",
		"upload:\n  grace_task_sec:\n    \"7\": 0\n",
		"health:\n  upload_late_critical_after_sec: -5\n",
		"health:\n  upload_late_critical_after_task_sec:\n    \"7\": 0\n",
		"health:\n  object_list_limit: -1\n",
		"health:\n  object_list_interval_sec: -1\n",
	} {
		if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
