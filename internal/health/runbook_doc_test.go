// Package health — contract between the CRITICAL finding registry and the
// operator runbook.
// input: DefaultRegistry() severity, the runbook section of
//
//	docs/guide/admin/troubleshooting.md, and bash -n
//
// output: a failure when a CRITICAL finding kind has no `### KIND` runbook
//
//	entry, when an entry carries no bash block, or when a runbook bash block
//	does not parse
//
// pos: documentation contract test for production-readiness I2 (every
//
//	CRITICAL finding has a runbook and its commands are executable)
package health

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// nonRegistryCritical lists CRITICAL findings emitted outside the health
// registry (the tasks-package signal kinds and the claim guard) plus
// UPLOAD_LATE, whose severity turns Critical only past its threshold.
var nonRegistryCritical = []Kind{
	"MIXED_VERSION",
	"META_LOCK_STALLED",
	"RETRY_LOOP_FAILED",
	"FINDINGS_PERSIST_FAILED",
	UploadLate,
}

// criticalKinds returns every kind an operator can see at CRITICAL severity.
func criticalKinds() []string {
	seen := map[string]bool{}
	for kind, spec := range DefaultRegistry() {
		if spec.Severity == Critical {
			seen[string(kind)] = true
		}
	}
	for _, kind := range nonRegistryCritical {
		seen[string(kind)] = true
	}
	kinds := make([]string, 0, len(seen))
	for k := range seen {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds
}

func runbookRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// runbookSection returns the finding-runbook section: the last "## " heading
// whose body holds "### <KIND>" subsections, split per kind.
func runbookSubsections(t *testing.T, doc string) map[string]string {
	t.Helper()
	idx := strings.Index(doc, "## 9. CRITICAL finding")
	if idx < 0 {
		t.Fatal("troubleshooting.md has no '## 9. CRITICAL finding' section")
	}
	section := doc[idx:]
	sub := map[string]string{}
	re := regexp.MustCompile(`(?m)^### ([A-Z][A-Z0-9_]+)\s*$`)
	matches := re.FindAllStringSubmatchIndex(section, -1)
	for i, m := range matches {
		end := len(section)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		sub[section[m[2]:m[3]]] = section[m[0]:end]
	}
	if len(sub) == 0 {
		t.Fatal("runbook section has no ### KIND subsections")
	}
	return sub
}

var bashFence = regexp.MustCompile("(?s)```(?:bash|sh)\\s*\n(.*?)```")

// TestCriticalFindingsHaveRunbooks asserts every CRITICAL finding kind maps
// to a `### KIND` subsection carrying at least one runnable-command block.
func TestCriticalFindingsHaveRunbooks(t *testing.T) {
	root := runbookRoot(t)
	doc, err := os.ReadFile(filepath.Join(root, "docs", "guide", "admin", "troubleshooting.md"))
	if err != nil {
		t.Fatal(err)
	}
	sub := runbookSubsections(t, string(doc))
	var missing, empty []string
	for _, kind := range criticalKinds() {
		body, ok := sub[kind]
		if !ok {
			missing = append(missing, kind)
			continue
		}
		if len(bashFence.FindAllStringSubmatch(body, -1)) == 0 {
			empty = append(empty, kind)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("troubleshooting.md runbook is missing CRITICAL kinds:\n%s", strings.Join(missing, "\n"))
	}
	if len(empty) > 0 {
		t.Fatalf("runbook entries without a command block:\n%s", strings.Join(empty, "\n"))
	}
}

// TestRunbookCommandsParse runs bash -n over every command block in the
// runbook section so documented commands cannot rot into syntax errors.
func TestRunbookCommandsParse(t *testing.T) {
	root := runbookRoot(t)
	doc, err := os.ReadFile(filepath.Join(root, "docs", "guide", "admin", "troubleshooting.md"))
	if err != nil {
		t.Fatal(err)
	}
	idx := strings.Index(string(doc), "## 9. CRITICAL finding")
	if idx < 0 {
		t.Fatal("troubleshooting.md has no '## 9. CRITICAL finding' section")
	}
	blocks := bashFence.FindAllStringSubmatch(string(doc)[idx:], -1)
	if len(blocks) == 0 {
		t.Fatal("runbook section has no bash blocks")
	}
	for i, b := range blocks {
		tmp := filepath.Join(t.TempDir(), "cmd.sh")
		if err := os.WriteFile(tmp, []byte(b[1]), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("bash", "-n", tmp).CombinedOutput(); err != nil {
			t.Fatalf("runbook bash block %d does not parse: %v\n%s\n---\n%s", i, err, out, b[1])
		}
	}
}
