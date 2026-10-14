package lintguard

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// guarded are the backup data path packages (ADR 0009 phase 0).
var guarded = []string{"internal/tasks", "internal/replication", "internal/binlog", "internal/upload"}

// TestNoNewIgnoredErrors fails on any `_ = call()` or log-only `if err != nil`
// in the guarded packages that is not in allowlist (allowlist.go) with its
// reviewed count. A new silent path must report a finding, return the error,
// or be added to the allowlist with a reason in the same PR.
func TestNoNewIgnoredErrors(t *testing.T) {
	counts := map[string]int{}
	lines := map[string][]int{}
	for _, dir := range guarded {
		sites, err := Scan("../..", "../../"+dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range sites {
			counts[s.Key()]++
			lines[s.Key()] = append(lines[s.Key()], s.Line)
		}
	}
	var bad []string
	for key, n := range counts {
		entry, ok := allowlist[key]
		if !ok || n > entry.Count {
			bad = append(bad, fmt.Sprintf("%s lines %v (found %d, allowed %d)", key, lines[key], n, entry.Count))
		}
	}
	for key, entry := range allowlist {
		if counts[key] < entry.Count {
			bad = append(bad, fmt.Sprintf("%s: allowlist says %d, found %d; lower the count", key, entry.Count, counts[key]))
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("ignored errors not in internal/lintguard/allowlist.go:\n%s", strings.Join(bad, "\n"))
	}
}
