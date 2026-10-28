// Package config provides module-level functionality for config.
// input: quoted YAML keys in config.go, docs/guide/admin/configuration.md, the repo VERSION file, and internal/releaseprep (History allowlist and Surfaces manifest)
// output: a failure when a loader key is undocumented, a tracked file names the previous release outside release history, or a release surface disagrees with VERSION
// pos: documentation contract for configuration keys and the current release pin; the surface list itself lives in internal/releaseprep
// note: if this file changes, update this header and module README.md.
package config

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"binlog_server/internal/releaseprep"
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

// TestVersionPinsMatchVERSIONFile runs the release gate's checks
// (releaseprep.Check with no tags: release notes, the CHANGELOG section, the stale
// scan for the previous release and the Surfaces manifest, the same code
// `make release-check` runs on a tag, tags aside) against VERSION, so a PR
// that bumps VERSION without its notes or a surface fails here. The versions
// scanned come from releaseprep's scanVersions, the same rule as the tag
// gate; without tags the previous release is the newest older CHANGELOG
// section. A new
// surface goes in the manifest and a history mention in releaseprep.History,
// not here.
func TestVersionPinsMatchVERSIONFile(t *testing.T) {
	root := repoRoot(t)
	ver := strings.TrimSpace(mustRead(t, filepath.Join(root, "VERSION")))
	if ver == "" || strings.Contains(ver, "\n") {
		t.Fatalf("VERSION = %q", ver)
	}
	findings, err := releaseprep.Check(releaseprep.Dir(root), nil, "v"+ver)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) < len(releaseprep.Surfaces) {
		t.Fatalf("%d findings for %d surfaces", len(findings), len(releaseprep.Surfaces))
	}
	for _, f := range findings {
		if !f.OK {
			t.Errorf("%s", f.Msg)
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
