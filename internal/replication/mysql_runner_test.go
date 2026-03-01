// input: source replication config, task state, checkpoint/file store dependencies
// output: replication run control, local binlog artifacts, and upload/recovery signals
// pos: data-plane runtime that consumes MySQL binlog stream and emits durable outputs
// note: if this file changes, update this header and module AGENTS.md.
package replication

import (
	"testing"

	"binlog_server/internal/tasks"
)

// TestBuildSyncerConfig_Defaults 验证相关行为。
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
	if cfg.SemiSyncEnabled {
		t.Fatal("expected semi-sync disabled by default")
	}
}

// TestBuildSyncerConfig_UsesTaskServerID 验证相关行为。
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

// TestBuildSyncerConfig_UsesTaskSemiSync 验证相关行为。
func TestBuildSyncerConfig_UsesTaskSemiSync(t *testing.T) {
	cfg := buildSyncerConfig(tasks.Task{
		ID: "89",
		Source: tasks.SourceConfig{
			Host:     "10.0.0.2",
			Port:     3306,
			User:     "repl",
			Password: "secret",
			Flavor:   "mysql",
			SemiSync: true,
		},
	})

	if !cfg.SemiSyncEnabled {
		t.Fatal("expected semi-sync enabled from task source config")
	}
}
