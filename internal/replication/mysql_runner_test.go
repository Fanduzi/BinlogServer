package replication

import (
	"testing"

	"binlog_server/internal/tasks"
)

func TestBuildSyncerConfig_Defaults(t *testing.T) {
	cfg := buildSyncerConfig(tasks.Task{
		ID: "12",
		Source: tasks.SourceConfig{
			Host:     "127.0.0.1",
			Port:     3306,
			User:     "repl",
			Password: "secret",
		},
	})

	if cfg.Host != "127.0.0.1" {
		t.Fatalf("unexpected host: %s", cfg.Host)
	}
	if cfg.Port != 3306 {
		t.Fatalf("unexpected port: %d", cfg.Port)
	}
	if cfg.Flavor != "mysql" {
		t.Fatalf("expected default flavor mysql, got %s", cfg.Flavor)
	}
	if cfg.ServerID == 0 {
		t.Fatal("expected non-zero server id")
	}
}

func TestBuildSyncerConfig_UsesTaskServerID(t *testing.T) {
	cfg := buildSyncerConfig(tasks.Task{
		ID: "88",
		Source: tasks.SourceConfig{
			Host:     "10.0.0.1",
			Port:     3306,
			User:     "repl",
			Password: "secret",
			ServerID: 330099,
			Flavor:   "mysql",
		},
	})

	if cfg.ServerID != 330099 {
		t.Fatalf("expected server id 330099, got %d", cfg.ServerID)
	}
}
