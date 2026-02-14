package config

import "testing"

func TestLoadConfig_DefaultValues(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Fatalf("expected :8080, got %s", cfg.ListenAddr)
	}
	if cfg.DataDir != "./data" {
		t.Fatalf("expected ./data, got %s", cfg.DataDir)
	}
	if cfg.MetaDSN != "" {
		t.Fatalf("expected empty meta dsn, got %s", cfg.MetaDSN)
	}
}
