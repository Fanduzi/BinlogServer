// Package config provides module-level functionality for config.
// input: docs/guide/admin/upgrade.md and upgrade.zh-CN.md, the repo VERSION file
// output: a failure when an upgrade guide's versioned notes are not newest first with VERSION on top, naming the line when VERSION's note is in a shape the check does not read
// pos: documentation contract for the upgrade guides that hold the per-version notes the READMEs no longer carry
// note: if this file changes, update this header and module README.md.
package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// guideVersionedNote is a top-level list item, heading or quoted list item
// that starts with a version, after emphasis, a bracket, a code span, or a
// Release / Version / 版本 label. Nested sub-bullets and table rows (such as
// a compatibility table) are not notes.
var guideVersionedNote = regexp.MustCompile("(?m)^(?:[-*+]|\\d+[.)]|#{1,6}|>[ \\t]*[-*+])[ \\t]+(?:[*_\\[(`][ \\t]*|(?i:release|version):?[ \\t]+|版本[ \\t]*)*[vV]?(\\d+)\\.(\\d+)\\.(\\d+)")

const guideNoteShapes = "a note is a top-level list item, heading or quoted list item that starts with the version, optionally after emphasis, a bracket, a code span or a Release / Version / 版本 label, such as `- **vX.Y.Z:** …`, `1. vX.Y.Z …`, `### Release vX.Y.Z` or `> - [vX.Y.Z] …`"

func guideOrderProblems(body, ver string) []string {
	matches := guideVersionedNote.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 || strings.Join(matches[0][1:], ".") != ver {
		top := "no note"
		if len(matches) > 0 {
			top = "the top note is v" + strings.Join(matches[0][1:], ".")
		}
		if n, line := guideLineNaming(body, ver); n > 0 && !guideVersionedNote.MatchString(line) {
			return []string{fmt.Sprintf("%s, want v%s (VERSION) on top; line %d %q names v%s but is not a note this check reads: %s", top, ver, n, line, ver, guideNoteShapes)}
		}
		return []string{fmt.Sprintf("%s, want v%s (VERSION) on top; add its note above the others: %s", top, ver, guideNoteShapes)}
	}
	var problems []string
	for i := 1; i < len(matches); i++ {
		if compareSemver(matches[i-1][1:], matches[i][1:]) <= 0 {
			problems = append(problems, fmt.Sprintf("v%s comes before v%s; keep notes newest first with one note per version",
				strings.Join(matches[i-1][1:], "."), strings.Join(matches[i][1:], ".")))
		}
	}
	return problems
}

// guideLineNaming returns the first line, 1-based, that names ver.
func guideLineNaming(body, ver string) (int, string) {
	for i, line := range strings.Split(body, "\n") {
		for _, m := range readmeNumberRun.FindAllString(line, -1) {
			if m == ver {
				return i + 1, line
			}
		}
	}
	return 0, ""
}

func compareSemver(a, b []string) int {
	for i := range a {
		x, _ := strconv.Atoi(a[i])
		y, _ := strconv.Atoi(b[i])
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

func TestUpgradeGuideNewestFirst(t *testing.T) {
	f := realReadmeFixture(t)
	for _, c := range readmeContracts {
		for _, p := range guideOrderProblems(f.files[c.guide], f.ver) {
			t.Errorf("%s: %s", c.guide, p)
		}
	}
}

func TestUpgradeGuideOrderCases(t *testing.T) {
	const ver = "0.5.83"
	cases := []struct {
		name, body string
		fail       bool
	}{
		{"newest first", "- **v0.5.83:** a\n- **v0.5.82:** b\n- unversioned\n- **v0.5.9:** c", false},
		{"numeric not lexical", "- **v0.5.83:** a\n- **v0.5.10:** b\n- **v0.5.9:** c", false},
		{"minor and major order", "- **v0.5.83:** a\n- **v0.4.99:** b\n- **v0.3.100:** c", false},
		{"labels newest first", "- **Release v0.5.83:** a\n- **[v0.5.82]** b\n- 版本 0.5.81: c\n- `v0.5.80`: d", false},
		{"unversioned bullet naming MySQL", "- **v0.5.83:** a\n- Supported: MySQL 8.4.13\n- **v0.5.82:** b", false},
		{"nested sub-bullet is not a note", "- **v0.5.83:** a\n- **v0.5.82:** b\n  - v0.5.80 users: set use_ssl false first\n- **v0.5.81:** c", false},
		{"compatibility table is not a note", "- **v0.5.83:** a\n\n| From | Rollback |\n|---|---|\n| 0.5.80 | ok |\n| 0.5.84 | n/a |\n\n- **v0.5.82:** b", false},
		{"no notes", "- unversioned only", true},
		{"top is not VERSION", "- **v0.5.82:** a\n- **v0.5.81:** b", true},
		{"top is newer than VERSION", "- **v0.5.84:** a\n- **v0.5.83:** b", true},
		{"top is a prefix of VERSION", "- **v0.5.8:** a", true},
		{"out of order", "- **v0.5.83:** a\n- **v0.5.81:** b\n- **v0.5.82:** c", true},
		{"unbolded out of order", "- **v0.5.83:** a\n- **v0.5.82:** b\n- v0.5.90: c", true},
		{"older unbolded note above", "- v0.5.20: x\n- **v0.5.83:** a", true},
		{"numbered out of order", "- **v0.5.83:** a\n1. **v0.5.84** b", true},
		{"numbered with a paren out of order", "- **v0.5.83:** a\n2) **v0.5.84** b", true},
		{"star out of order", "- **v0.5.83:** a\n* **v0.5.84:** b", true},
		{"plus out of order", "- **v0.5.83:** a\n+ **v0.5.84:** b", true},
		{"underscore emphasis out of order", "- **v0.5.83:** a\n- __v0.5.84:__ b", true},
		{"h1 out of order", "- **v0.5.83:** a\n# v0.5.84", true},
		{"h3 out of order", "- **v0.5.83:** a\n### v0.5.84", true},
		{"h6 out of order", "- **v0.5.83:** a\n###### v0.5.84", true},
		{"quoted dash out of order", "- **v0.5.83:** a\n> - **v0.5.84:** b", true},
		{"quoted star out of order", "- **v0.5.83:** a\n> * **v0.5.84:** b", true},
		{"quoted plus out of order", "- **v0.5.83:** a\n> + **v0.5.84:** b", true},
		{"tab after the marker out of order", "- **v0.5.83:** a\n-\t**v0.5.84:** b", true},
		{"equal versions", "- **v0.5.83:** a\n- **v0.5.83:** again", true},
		{"minor out of order", "- **v0.5.83:** a\n- **v0.6.0:** b", true},
		{"major out of order", "- **v0.5.83:** a\n- **v1.0.0:** b", true},
		{"capital V out of order", "- **v0.5.83:** a\n- **V0.5.84:** b", true},
		{"no v out of order", "- **v0.5.83:** a\n- 0.5.84: b", true},
		{"Release label out of order", "- **v0.5.83:** a\n- **Release v0.5.20:** old\n- **v0.5.82:** b", true},
		{"Release colon label out of order", "- **v0.5.83:** a\n- Release: 0.5.20 old\n- **v0.5.82:** b", true},
		{"Version label out of order", "- **v0.5.83:** a\n- Version 0.5.84: b", true},
		{"Chinese label out of order", "- **v0.5.83：** a\n- **版本 v0.5.84：** b", true},
		{"bracketed out of order", "- **v0.5.83:** a\n- **[v0.5.20]** old\n- **v0.5.82:** b", true},
		{"parenthesized out of order", "- **v0.5.83:** a\n- (v0.5.84) b", true},
		{"code span out of order", "- **v0.5.83:** a\n- `v0.5.84`: b", true},
		{"bold code span out of order", "- **v0.5.83:** a\n- **`v0.5.84`:** b", true},
	}
	for _, k := range cases {
		got := guideOrderProblems(k.body, ver)
		if (len(got) > 0) != k.fail {
			t.Errorf("%s: problems %v, want fail=%v", k.name, got, k.fail)
		}
	}
}

// TestUpgradeGuideNamesUnreadNote checks the message when VERSION's note is
// written in a shape the check does not read: it names the line and lists
// the shapes it does read, instead of asking for a note that is already there.
func TestUpgradeGuideNamesUnreadNote(t *testing.T) {
	const ver = "0.5.84"
	for _, k := range []struct{ name, body, line string }{
		{"Released prefix", "Released v0.5.84 with metrics.\n\n- **v0.5.83:** a", "line 1 \"Released v0.5.84 with metrics.\""},
		{"product name prefix", "- **v0.5.83:** a\n- BinlogServer 0.5.84: metrics", "line 2 \"- BinlogServer 0.5.84: metrics\""},
		{"nested, no notes", "  - v0.5.84: metrics", "line 1 \"  - v0.5.84: metrics\""}, {"Chinese verb", "- 发布 v0.5.84：新增指标", "line 1 \"- 发布 v0.5.84：新增指标\""},
	} {
		got := strings.Join(guideOrderProblems(k.body, ver), "\n")
		if !strings.Contains(got, k.line) || !strings.Contains(got, "is not a note this check reads") || !strings.Contains(got, "`- **vX.Y.Z:** …`") {
			t.Errorf("%s: %q, want it to name %s and list the accepted shapes", k.name, got, k.line)
		}
	}
	for _, k := range []struct{ name, body, want string }{
		{"note missing", "- **v0.5.83:** a", "the top note is v0.5.83, want v0.5.84 (VERSION) on top; add its note above the others: a note is"},
		{"no notes at all", "Nothing here.", "no note, want v0.5.84 (VERSION) on top; add its note above the others: a note is"},
		{"wrong note on top names VERSION", "- **v0.5.85:** a, after v0.5.84", "the top note is v0.5.85, want v0.5.84 (VERSION) on top; add its note above the others"},
	} {
		if got := strings.Join(guideOrderProblems(k.body, ver), "\n"); !strings.HasPrefix(got, k.want) {
			t.Errorf("%s: %q, want prefix %q", k.name, got, k.want)
		}
	}
}
