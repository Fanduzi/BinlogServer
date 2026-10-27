// Package config provides module-level functionality for config.
// input: quoted YAML keys in config.go, docs/guide/admin/configuration.md, and the repo VERSION file
// output: a failure when a loader key is undocumented, a shipped version pin disagrees with VERSION, or the landing page grows a release history
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

// The landing page is a product page. Release history lives in docs/releases and
// CHANGELOG.md; the landing page names only the current version, so a release PR
// changes the pins and nothing else.
const landingMaxBytes = 96 << 10

var (
	landingVersionPattern  = regexp.MustCompile(`\bv\d+\.\d+\.\d+\b`)
	landingChecksumPattern = regexp.MustCompile(`\b[0-9a-f]{64}\b`)
	landingHistoryPattern  = regexp.MustCompile(`(?i)class="timeline-date"[^>]*>\s*\d{4}-\d{2}-\d{2}|id="(changelog|releases)"|Recent Releases`)
)

func TestLandingPageHasNoReleaseHistory(t *testing.T) {
	root := repoRoot(t)
	ver := strings.TrimSpace(mustRead(t, filepath.Join(root, "VERSION")))
	const rel = "docs/landing/index.html"
	body := mustRead(t, filepath.Join(root, rel))
	const why = "the landing page sells the product; put release history in docs/releases and CHANGELOG.md and link to GitHub Releases"
	if len(body) > landingMaxBytes {
		t.Errorf("%s is %d bytes, over the %d-byte bound: %s", rel, len(body), landingMaxBytes, why)
	}
	if m := landingHistoryPattern.FindString(body); m != "" {
		t.Errorf("%s has a release timeline (%q): %s", rel, m, why)
	}
	if m := landingChecksumPattern.FindString(body); m != "" {
		t.Errorf("%s hardcodes a checksum (%s); the install steps verify checksums.txt instead", rel, m)
	}
	stale := map[string]struct{}{}
	for _, m := range landingVersionPattern.FindAllString(body, -1) {
		if strings.TrimPrefix(m, "v") != ver {
			stale[m] = struct{}{}
		}
	}
	if len(stale) > 0 {
		var list []string
		for v := range stale {
			list = append(list, v)
		}
		sort.Strings(list)
		t.Errorf("%s names versions other than VERSION %s: %s; %s", rel, ver, strings.Join(list, ", "), why)
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
