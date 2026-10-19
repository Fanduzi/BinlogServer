// Package config provides module-level functionality for config.
// input: repo-root example YAML, .goreleaser.yml archive file list, scripts/release-assets.sh copy list
// output: proof that every shipped example config loads through LoadConfig
// pos: regression guard for packaged configuration samples
// note: if this file changes, update this header and module README.md.
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig_ShippedExamples(t *testing.T) {
	root := repoRoot(t)
	paths := shippedExampleConfigs(t, root)
	if len(paths) < 2 {
		t.Fatalf("expected config.example.yaml and config.production.example.yaml, got %v", paths)
	}
	seen := map[string]bool{}
	for _, rel := range paths {
		seen[rel] = true
	}
	for _, required := range []string{"config.example.yaml", "config.production.example.yaml"} {
		if !seen[required] {
			t.Fatalf("shipped example %s is not packaged", required)
		}
	}

	// Production example enables auth and keeps the token as ${ENV}.
	t.Setenv("BINLOG_SERVER_API_AUTH_BEARER_TOKEN", "shipped-example-token")
	t.Setenv("BINLOG_SERVER_META_DSN", "")
	t.Setenv("BINLOG_SERVER_UPLOAD_ACCESS_KEY", "")
	t.Setenv("BINLOG_SERVER_UPLOAD_SECRET_KEY", "")

	for _, rel := range paths {
		t.Run(rel, func(t *testing.T) {
			cfg, err := LoadConfig(filepath.Join(root, rel))
			if err != nil {
				t.Fatalf("LoadConfig(%s): %v", rel, err)
			}
			if cfg.ListenAddr == "" || cfg.DataDir == "" {
				t.Fatalf("LoadConfig(%s) returned an empty runtime config: %+v", rel, cfg)
			}
		})
	}

	cfg, err := LoadConfig(filepath.Join(root, "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:8080" {
		t.Fatalf("listen_addr = %q", cfg.ListenAddr)
	}
	if cfg.HTTP.WorkerHealth.ReadHeaderTimeoutSec != 3 || cfg.HTTP.WorkerHealth.IdleTimeoutSec != 30 {
		t.Fatalf("worker_health timeouts = %+v", cfg.HTTP.WorkerHealth)
	}
	if cfg.Standalone.TaskRecords != "on" {
		t.Fatalf("standalone.task_records = %q", cfg.Standalone.TaskRecords)
	}
	if cfg.Health.Evaluator != "off" || cfg.Health.SweepIntervalSec != 60 {
		t.Fatalf("health = %+v", cfg.Health)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// shippedExampleConfigs returns every example YAML the repo ships and every
// YAML the release tarball packages (GoReleaser files list and release-assets.sh).
func shippedExampleConfigs(t *testing.T, root string) []string {
	t.Helper()
	set := map[string]struct{}{}
	add := func(rel string) {
		rel = filepath.ToSlash(strings.TrimSpace(rel))
		if rel == "" || strings.Contains(rel, "*") {
			return
		}
		if strings.HasSuffix(rel, ".yaml") || strings.HasSuffix(rel, ".yml") {
			set[rel] = struct{}{}
		}
	}

	matches, err := filepath.Glob(filepath.Join(root, "*example*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, match := range matches {
		rel, err := filepath.Rel(root, match)
		if err != nil {
			t.Fatal(err)
		}
		add(rel)
	}

	goreleaser, err := os.ReadFile(filepath.Join(root, ".goreleaser.yml"))
	if err != nil {
		t.Fatal(err)
	}
	inFiles := false
	for _, line := range strings.Split(string(goreleaser), "\n") {
		trim := strings.TrimSpace(line)
		if !inFiles {
			if trim == "files:" {
				inFiles = true
			}
			continue
		}
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			break
		}
		if !strings.HasPrefix(trim, "- ") {
			continue
		}
		item := strings.Trim(strings.TrimPrefix(trim, "- "), `"'`)
		expanded, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(item)))
		if err != nil {
			t.Fatal(err)
		}
		if len(expanded) == 0 && (strings.HasSuffix(item, ".yaml") || strings.HasSuffix(item, ".yml")) {
			add(item)
			continue
		}
		for _, match := range expanded {
			rel, err := filepath.Rel(root, match)
			if err != nil {
				t.Fatal(err)
			}
			add(rel)
		}
	}

	script, err := os.ReadFile(filepath.Join(root, "scripts", "release-assets.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range strings.Fields(string(script)) {
		field = strings.Trim(field, `"'`)
		field = strings.TrimPrefix(field, "$ROOT_DIR/")
		if strings.HasSuffix(field, ".yaml") || strings.HasSuffix(field, ".yml") {
			add(field)
		}
	}

	if len(set) == 0 {
		t.Fatal("no shipped example configs discovered")
	}
	out := make([]string, 0, len(set))
	for rel := range set {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Fatalf("packaged config %s: %v", rel, err)
		}
		out = append(out, rel)
	}
	return out
}
