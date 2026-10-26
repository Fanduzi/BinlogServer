// Package config provides module-level functionality for config.
// input: an empty config and a notify block
// output: proof of notify defaults and rejection of a bad URL or an out-of-range attempt count
// pos: config tests for the F2 webhook keys
// note: if this file changes, update this header and module README.md.
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig_NotifyDefaultsAndRejects(t *testing.T) {
	setRequiredAuthEnv(t)
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.WebhookURL != "" || cfg.Notify.WebhookToken != "" || cfg.Notify.TimeoutSec != 5 || cfg.Notify.Attempts != 3 || cfg.Notify.BackoffMs != 200 {
		t.Fatalf("defaults %+v", cfg.Notify)
	}

	path := filepath.Join(t.TempDir(), "c.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("notify:\n  webhook_url: ftp://hooks.example/binlog\n")
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "notify.webhook_url") {
		t.Fatalf("bad url err %v", err)
	}
	write("notify:\n  attempts: 9\n")
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "notify.attempts") {
		t.Fatalf("attempts err %v", err)
	}
	write("notify:\n  webhook_url: https://hooks.example/binlog\n  webhook_token: sekrit\n  timeout_sec: 10\n  attempts: 2\n  backoff_ms: 100\n")
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.WebhookURL != "https://hooks.example/binlog" || cfg.Notify.WebhookToken != "sekrit" || cfg.Notify.TimeoutSec != 10 || cfg.Notify.Attempts != 2 || cfg.Notify.BackoffMs != 100 {
		t.Fatalf("overrides %+v", cfg.Notify)
	}
}
