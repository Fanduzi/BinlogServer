// Package config provides module-level functionality for config.
// input: quoted YAML keys in config.go, docs/guide/admin/configuration.md, and the repo VERSION file
// output: a failure when a loader key is undocumented or a shipped version pin disagrees with VERSION
// pos: documentation contract for configuration keys and the current release pin
// note: if this file changes, update this header and module README.md.
package config

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var documentedKeyPattern = regexp.MustCompile(`"((?:listen_addr|data_dir|meta_dsn|mode)|(?:[a-z][a-z0-9_]*(?:\.[a-z0-9_]+)+))"`)

func TestDocumentedConfigKeys(t *testing.T) {
	root := repoRoot(t)
	src, err := os.ReadFile(filepath.Join(root, "internal", "config", "config.go"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile(filepath.Join(root, "docs", "guide", "admin", "configuration.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(doc)
	var missing []string
	seen := map[string]struct{}{}
	for _, match := range documentedKeyPattern.FindAllSubmatch(src, -1) {
		key := string(match[1])
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if !strings.Contains(text, key) {
			missing = append(missing, key)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no config keys extracted")
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("configuration.md is missing %d keys:\n%s", len(missing), strings.Join(missing, "\n"))
	}
}

func TestVersionPinsMatchVERSIONFile(t *testing.T) {
	root := repoRoot(t)
	ver := strings.TrimSpace(mustRead(t, filepath.Join(root, "VERSION")))
	if ver == "" || strings.Contains(ver, "\n") {
		t.Fatalf("VERSION = %q", ver)
	}
	checks := []struct {
		rel  string
		need string
	}{
		{"README.md", "VER=" + ver},
		{"README_ZH.md", "VER=" + ver},
		{"docs/guide/admin/deployment.md", "VER=" + ver},
		{"config.example.yaml", "v" + ver},
		{"config.production.example.yaml", "v" + ver},
		{"docs/landing/index.html", "v" + ver + " (Current)"},
		{"docs/landing/index.html", "v" + ver + " — latest release"},
		{"docs/landing/index.html", "Get Started with v" + ver},
	}
	for _, check := range checks {
		body := mustRead(t, filepath.Join(root, check.rel))
		if !strings.Contains(body, check.need) {
			t.Errorf("%s missing %q", check.rel, check.need)
		}
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
