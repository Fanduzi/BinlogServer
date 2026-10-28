// Package config provides module-level functionality for config.
// input: README.md, README_ZH.md, the repo VERSION file, docs/guide/admin/upgrade.md and upgrade.zh-CN.md
// output: a failure when a README names a version of the current release line other than VERSION, has an h1/h2 heading outside its list, outgrows its byte cap, leaves less room under the cap than the largest routine edit (the headroom check) or has a cap more than 4 KB above its size, or its Upgrading section leaves the two-line template; or when an upgrade guide is not newest first with VERSION on top
// pos: documentation contract that keeps per-version history out of the READMEs
// note: if this file changes, update this header and module README.md.
package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// Threat model: the READMEs used to collect one upgrade note per release.
// This guard stops that from coming back through ordinary edits: a README
// keeps a fixed list of h1/h2 headings, a two-line Upgrading section and a
// byte cap, and names no version of the current release line except VERSION.
// It is not a defense against deliberate obfuscation (spelled-out versions,
// look-alike characters, markup tricks). Stale versions in release surfaces
// are the release check's job. Per-version notes live in the upgrade guides.
// After a minor or major bump, a leftover version of the previous line
// (0.5.83 once VERSION is 0.6.0) is not this guard's to catch: the release
// check's previous-release scan (TestVersionPinsMatchVERSIONFile) flags it.

// readmeRule names the rule a problem breaks, so tests can assert which one.
type readmeRule string

const (
	ruleVersion   readmeRule = "version"
	ruleHeading   readmeRule = "heading"
	ruleSize      readmeRule = "size"
	ruleUpgrading readmeRule = "upgrading"
)

type readmeProblem struct {
	rule readmeRule
	msg  string
}

type readmeContract struct {
	rel      string
	guide    string
	maxBytes int
	// headings lists every h1 and h2 in order, as written after the #s.
	headings []string
	// upgrading is the h2 of the template section.
	upgrading string
	// pinPrefix + VERSION + pinSuffix + summary is the first line of the section.
	pinPrefix, pinSuffix string
	// pointer is the second and last line of the section.
	pointer string
}

// readmeSummaryMaxRunes bounds the one-line summary after the pin.
const readmeSummaryMaxRunes = 200

// readmeGrowth is how much a README may grow before its cap has to move:
// routine edits (an h3, an install snippet, a FAQ paragraph, a checklist
// bullet), not a history section. A cap is set to the README size plus
// readmeGrowth plus the largest routine edit, because checkReadmeAccepts
// applies each routine edit on top of the README as it is; without that
// margin a README grown by readmeGrowth would fail the accept test.
const readmeGrowth = 2048

// readmeCapMaxMargin bounds how far a cap may sit above its README; after a
// PR shrinks a README the cap may lag by up to 4 KB.
const readmeCapMaxMargin = 4096

var readmeContracts = []readmeContract{
	{
		rel:      "README.md",
		guide:    "docs/guide/admin/upgrade.md",
		maxBytes: 36231,
		headings: []string{
			"# BinlogServer",
			"## What Problem Does It Solve?",
			"## Architectural Guarantees (DBA First)",
			"## Deployment Topologies",
			"## Quick Start (Release Operators)",
			"## Production Deployment Checklist",
			"## Upgrading",
			"## Cluster session timeouts (ADR 0011 PR-2)",
			"## Architecture",
			"## Repository Map",
			"## Source Build &amp; Verification",
		},
		upgrading: "## Upgrading",
		pinPrefix: "Latest release: `v",
		pinSuffix: "`. ",
		pointer:   "Before upgrading, read every note between your version and the latest release in the [upgrade guide](docs/guide/admin/upgrade.md); when a release has a migration, run `./migrate up` with the new `migrate` before you start it. Full detail is in the [release notes](docs/releases/) and the [CHANGELOG](CHANGELOG.md).",
	},
	{
		rel:      "README_ZH.md",
		guide:    "docs/guide/admin/upgrade.zh-CN.md",
		maxBytes: 33903,
		headings: []string{
			"# BinlogServer",
			"## 解决的核心痛点",
			"## 四大架构设计保证 (DBA First)",
			"## 推荐部署架构",
			"## Quick Start (面向生产运维)",
			"## 生产部署安全检查清单",
			"## 升级",
			"## 架构",
			"## 仓库入口导航",
			"## 开发与源码构建",
		},
		upgrading: "## 升级",
		pinPrefix: "最新版本：`v",
		pinSuffix: "`。",
		pointer:   "升级前，按 [升级指南](docs/guide/admin/upgrade.zh-CN.md) 读完从你的版本到最新版本之间的每一条说明；有迁移的版本，先用新版 `migrate` 执行 `./migrate up` 再启动。完整内容见 [发布说明](docs/releases/) 和 [CHANGELOG](CHANGELOG.md)。",
	},
}

var (
	// readmeNumberRun is a dotted run of ASCII numbers with an optional
	// -prerelease suffix. A leading v is irrelevant: the digits match without it.
	readmeNumberRun = regexp.MustCompile(`\d+(?:\.\d+)+(?:-[0-9A-Za-z]+)?`)
	atxHeading      = regexp.MustCompile(`^ {0,3}(#{1,2})(?:[ \t]+(.*?))?$`)
	atxMinorHeading = regexp.MustCompile(`^ {0,3}#{3,6}(?:[ \t]|$)`)
	setextUnderline = regexp.MustCompile(`^ {0,3}(=+|-+)[ \t]*$`)
	htmlHeading     = regexp.MustCompile(`(?i)<h[12][\s>]`)
	codeFence       = regexp.MustCompile("^ {0,3}(```|~~~)")
)

// readmeStrayVersions returns every three-part number that starts with
// VERSION's major.minor (X.Y.N, vX.Y.N, X.Y.N-rc1) and is not VERSION. Other
// numbers pass: two parts (0.99, 0.5), four parts (addresses), other lines
// (MySQL 8.4.13, deadcode@v0.52.0), and anything pinned after @ (a Go tool
// at module@vX.Y.N).
func readmeStrayVersions(body, ver string) []string {
	line := strings.Join(strings.SplitN(ver, ".", 3)[:2], ".") + "."
	seen := map[string]bool{}
	var out []string
	for _, loc := range readmeNumberRun.FindAllStringIndex(body, -1) {
		m := body[loc[0]:loc[1]]
		before := strings.TrimRight(body[:loc[0]], "vV")
		if strings.HasSuffix(before, "@") || strings.Count(m, ".") != 2 {
			continue
		}
		if strings.HasPrefix(m, line) && m != ver && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

func readmeProblems(body, ver string, c readmeContract) []readmeProblem {
	var problems []readmeProblem
	add := func(rule readmeRule, format string, args ...any) {
		problems = append(problems, readmeProblem{rule, fmt.Sprintf(format, args...)})
	}
	hint := "per-version notes go in " + c.guide + "; the README names only the latest release"

	if stray := readmeStrayVersions(body, ver); len(stray) > 0 {
		add(ruleVersion, "%s names %s, which is not VERSION %s; %s", c.rel, strings.Join(stray, ", "), ver, hint)
	}
	if got := readmeHeadings(body); !equalStrings(got, c.headings) {
		add(ruleHeading, "%s h1/h2 headings are %q, want %q; a new top-level section needs a deliberate edit of readmeContracts", c.rel, got, c.headings)
	}
	if len(body) > c.maxBytes {
		add(ruleSize, "%s is %d bytes, %d over its %d-byte cap; move detail into docs/guide, or if the growth is legitimate raise maxBytes for %s in readmeContracts in the same PR", c.rel, len(body), len(body)-c.maxBytes, c.maxBytes, c.rel)
	} else if margin := c.maxBytes - len(body); margin > readmeCapMaxMargin {
		add(ruleSize, "%s is %d bytes, %d under its %d-byte cap, more than %d; lower maxBytes in readmeContracts to the size plus %d plus the largest routine edit (TestReadmeHeadroom prints it)", c.rel, len(body), margin, c.maxBytes, readmeCapMaxMargin, readmeGrowth)
	}
	for _, msg := range upgradingTemplateProblems(body, ver, c) {
		add(ruleUpgrading, "%s %s section: %s; %s", c.rel, c.upgrading, msg, hint)
	}
	return problems
}

// readmeHeadings returns every h1 and h2 outside fenced code, in order,
// including setext and HTML headings.
func readmeHeadings(body string) []string {
	var out []string
	inFence := false
	prev := ""
	for _, line := range strings.Split(body, "\n") {
		if codeFence.MatchString(line) {
			inFence = !inFence
			prev = ""
			continue
		}
		if inFence {
			continue
		}
		if m := atxHeading.FindStringSubmatch(line); m != nil {
			out = append(out, strings.TrimSpace(m[1]+" "+m[2]))
		} else if setextUnderline.MatchString(line) && strings.TrimSpace(prev) != "" {
			out = append(out, "<setext heading> "+strings.TrimSpace(prev))
		}
		if htmlHeading.MatchString(line) {
			out = append(out, "<html heading> "+strings.TrimSpace(line))
		}
		prev = line
	}
	return out
}

// upgradingTemplateProblems checks that the section, which runs to the next
// h1 or h2, holds exactly the pin line with a one-line summary, then the fixed
// pointer line. An h3 or deeper heading does not end it and is not allowed.
func upgradingTemplateProblems(body, ver string, c readmeContract) []string {
	lines := strings.Split(body, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimRight(line, " \t") == c.upgrading {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return []string{"missing"}
	}
	var content, problems []string
	for _, line := range lines[start:] {
		if atxHeading.MatchString(line) {
			break
		}
		if atxMinorHeading.MatchString(line) {
			problems = append(problems, fmt.Sprintf("has the heading %q; the section takes no subsections", strings.TrimSpace(line)))
		}
		if t := strings.TrimSpace(line); t != "" && t != "---" {
			content = append(content, t)
		}
	}
	if len(content) != 2 {
		return append(problems, fmt.Sprintf("has %d lines, want 2 (the %q line and the upgrade guide pointer)", len(content), c.pinPrefix+ver+c.pinSuffix))
	}
	pin := c.pinPrefix + ver + c.pinSuffix
	if summary, ok := strings.CutPrefix(content[0], pin); !ok {
		problems = append(problems, fmt.Sprintf("first line must start with %q", pin))
	} else if n := utf8.RuneCountInString(summary); n == 0 || n > readmeSummaryMaxRunes {
		problems = append(problems, fmt.Sprintf("summary after the pin is %d characters, want 1 to %d", n, readmeSummaryMaxRunes))
	}
	if content[1] != c.pointer {
		problems = append(problems, fmt.Sprintf("second line must be the fixed pointer %q", c.pointer))
	}
	return problems
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func repoVersion(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(mustRead(t, filepath.Join(repoRoot(t), "VERSION")))
}

// readmeFixture is what the guard suites run on: a VERSION and the READMEs
// and upgrade guides that go with it, keyed by repo-relative path.
type readmeFixture struct {
	ver   string
	files map[string]string
}

func realReadmeFixture(t *testing.T) readmeFixture {
	t.Helper()
	root := repoRoot(t)
	f := readmeFixture{ver: repoVersion(t), files: map[string]string{}}
	for _, c := range readmeContracts {
		f.files[c.rel] = mustRead(t, filepath.Join(root, c.rel))
		f.files[c.guide] = mustRead(t, filepath.Join(root, c.guide))
	}
	return f
}

// bumped is the fixture as a correct release of next leaves it: VERSION
// replaced everywhere in the READMEs and a note for next on top of each guide.
func (f readmeFixture) bumped(t *testing.T, next string) readmeFixture {
	t.Helper()
	out := readmeFixture{ver: next, files: map[string]string{}}
	for _, c := range readmeContracts {
		out.files[c.rel] = strings.ReplaceAll(f.files[c.rel], f.ver, next)
		guide := f.files[c.guide]
		loc := guideVersionedNote.FindStringIndex(guide)
		if loc == nil {
			t.Fatalf("%s has no note to put v%s above", c.guide, next)
		}
		out.files[c.guide] = guide[:loc[0]] + "- **v" + next + ":** Test release.\n" + guide[loc[0]:]
	}
	return out
}

// other returns the i-th version (i >= 1) on VERSION's major.minor line that
// is not VERSION: patch-i when that is not negative, else patch+i.
func (f readmeFixture) other(t *testing.T, i int) string {
	t.Helper()
	parts := strings.Split(f.ver, ".")
	patch, err := strconv.Atoi(parts[2])
	if len(parts) != 3 || err != nil {
		t.Fatalf("VERSION %q is not X.Y.Z", f.ver)
	}
	if patch-i >= 0 {
		patch -= i
	} else {
		patch += i
	}
	return parts[0] + "." + parts[1] + "." + strconv.Itoa(patch)
}

func TestReadmeMatchesContract(t *testing.T) {
	checkReadmeMatchesContract(t, realReadmeFixture(t))
}

func TestReadmeCapBounds(t *testing.T)    { checkReadmeCapBounds(t, realReadmeFixture(t)) }
func TestReadmeGuardRejects(t *testing.T) { checkReadmeRejects(t, realReadmeFixture(t)) }
func TestReadmeGuardAccepts(t *testing.T) { checkReadmeAccepts(t, realReadmeFixture(t)) }
func TestUpgradingNamesSubsection(t *testing.T) {
	checkUpgradingNamesSubsection(t, realReadmeFixture(t))
}

// TestReadmeGuardSurvivesMinorAndMajorBumps runs every suite as a correct
// release of the next minor and the next major would leave the files (from
// 0.5.83: 0.6.0 and 1.0.0), so a bump past the current release line does not
// break the guard's own tests.
func TestReadmeGuardSurvivesMinorAndMajorBumps(t *testing.T) {
	real := realReadmeFixture(t)
	for _, next := range readmeBumpTargets(real.ver) {
		t.Run(next, func(t *testing.T) {
			f := real.bumped(t, next)
			if f.files["README.md"] == real.files["README.md"] {
				t.Fatalf("bump to %s changed nothing", next)
			}
			checkReadmeMatchesContract(t, f)
			checkReadmeCapBounds(t, f)
			checkReadmeRejects(t, f)
			checkReadmeAccepts(t, f)
			checkUpgradingNamesSubsection(t, f)
		})
	}
}

// readmeBumpTargets returns the next minor and the next major after ver.
func readmeBumpTargets(ver string) []string {
	var n [3]int
	for i, p := range strings.SplitN(ver, ".", 3) {
		n[i], _ = strconv.Atoi(p)
	}
	return []string{fmt.Sprintf("%d.%d.0", n[0], n[1]+1), fmt.Sprintf("%d.0.0", n[0]+1)}
}

func TestReadmeFixtureHelpers(t *testing.T) {
	for ver, want := range map[string]string{"0.5.83": "0.6.0 1.0.0", "1.9.2": "1.10.0 2.0.0"} {
		if got := strings.Join(readmeBumpTargets(ver), " "); got != want {
			t.Errorf("readmeBumpTargets(%s) = %s, want %s", ver, got, want)
		}
	}
	for _, k := range []struct {
		ver  string
		i    int
		want string
	}{{"0.5.83", 1, "0.5.82"}, {"0.5.83", 4, "0.5.79"}, {"0.6.0", 1, "0.6.1"}, {"0.6.2", 2, "0.6.0"}, {"0.6.2", 3, "0.6.5"}} {
		if got := (readmeFixture{ver: k.ver}).other(t, k.i); got != k.want {
			t.Errorf("other(%s, %d) = %s, want %s", k.ver, k.i, got, k.want)
		}
	}
}

func checkReadmeMatchesContract(t *testing.T, f readmeFixture) {
	t.Helper()
	for _, c := range readmeContracts {
		for _, p := range readmeProblems(f.files[c.rel], f.ver, c) {
			t.Error(p.msg)
		}
		for _, p := range guideOrderProblems(f.files[c.guide], f.ver) {
			t.Errorf("%s: %s", c.guide, p)
		}
	}
}

// checkReadmeCapBounds moves the cap around the real size: over the cap
// fails, and so does a cap more than 4 KB above it.
func checkReadmeCapBounds(t *testing.T, f readmeFixture) {
	t.Helper()
	for _, c := range readmeContracts {
		body := f.files[c.rel]
		n := strconv.Itoa(len(body))
		for _, k := range []struct {
			margin int
			want   []string
		}{
			{-1, []string{c.rel + " is " + n + " bytes, 1 over its " + strconv.Itoa(len(body)-1) + "-byte cap", "raise maxBytes for " + c.rel + " in readmeContracts"}},
			{-300, []string{c.rel + " is " + n + " bytes, 300 over its"}},
			{0, nil},
			{2048, nil},
			{4096, nil},
			{4097, []string{c.rel + " is " + n + " bytes, 4097 under its " + strconv.Itoa(len(body)+4097) + "-byte cap"}},
		} {
			loose := c
			loose.maxBytes = len(body) + k.margin
			var msgs []string
			for _, p := range readmeProblems(body, f.ver, loose) {
				msgs = append(msgs, p.msg)
			}
			got := strings.Join(msgs, "\n")
			ok := (len(k.want) == 0) == (got == "")
			for i, w := range k.want {
				if i == 0 && !strings.HasPrefix(got, w) || !strings.Contains(got, w) {
					ok = false
				}
			}
			if !ok {
				t.Errorf("%s with the cap at size%+d: %q, want %q", c.rel, k.margin, got, k.want)
			}
		}
	}
}

func TestReadmeStrayVersions(t *testing.T) {
	const ver = "0.5.83"
	for in, want := range map[string]string{
		"v0.5.83 and 0.5.83":                               "",
		"v0.5.82":                                          "0.5.82",
		"V0.5.20, 0.5.20 again":                            "0.5.20",
		"0.5.83-rc1 and 0.5.830 and (0.5.81)":              "0.5.83-rc1,0.5.830,0.5.81",
		"histogram_quantile(0.99, x) ratio 0.5 0.5s 0.25":  "",
		"deadcode@v0.52.0 v0.4.9 8.4.13 0.0.0.0 10.0.5.82": "",
		"0.5.83.1 0.5.1.2 10.0.5.82.1":                     "",
		"example.com/tool@v0.5.3 tool@0.5.4 tool@V0.5.5":   "",
		"`v0.5.81` and @ v0.5.80":                          "0.5.81,0.5.80",
	} {
		if got := strings.Join(readmeStrayVersions(in, ver), ","); got != want {
			t.Errorf("readmeStrayVersions(%q) = %q, want %q", in, got, want)
		}
	}
	if got := readmeStrayVersions("v1.2.3 v1.2.4", "1.2.3"); len(got) != 1 || got[0] != "1.2.4" {
		t.Errorf("major 1: got %q, want [1.2.4]", got)
	}
}

// checkReadmeRejects feeds the guard every ordinary way history came back
// in QA, plus one case per rule that breaks only that rule. Versions are
// derived from VERSION: {1}..{4} are other patches on its major.minor line.
func checkReadmeRejects(t *testing.T, f readmeFixture) {
	t.Helper()
	ver := f.ver
	expand := strings.NewReplacer("{1}", f.other(t, 1), "{2}", f.other(t, 2), "{3}", f.other(t, 3), "{4}", f.other(t, 4)).Replace
	type tc struct {
		name string
		edit func(body string, c readmeContract) string
		want []readmeRule
	}
	pin := func(c readmeContract) string { return c.pinPrefix + ver + c.pinSuffix }
	insert := func(text string) func(string, readmeContract) string {
		return func(body string, c readmeContract) string {
			anchor := c.headings[len(c.headings)-2] + "\n"
			return strings.Replace(body, anchor, expand(text)+"\n\n"+anchor, 1)
		}
	}
	replaceLine := func(old func(readmeContract) string, repl func(readmeContract) string) func(string, readmeContract) string {
		return func(body string, c readmeContract) string {
			return strings.Replace(body, old(c), expand(repl(c)), 1)
		}
	}
	pinLine := func(c readmeContract) string {
		for _, line := range strings.Split(f.files[c.rel], "\n") {
			if strings.HasPrefix(line, c.pinPrefix) {
				return line
			}
		}
		t.Fatalf("%s has no pin line", c.rel)
		return ""
	}
	pointer := func(c readmeContract) string { return c.pointer }
	// historyNotes is the real guide's notes, unbolded, renumbered onto
	// VERSION's line so they read as that line's history.
	historyNotes := func(c readmeContract) string {
		var notes []string
		for _, line := range strings.Split(f.files[c.guide], "\n") {
			if guideVersionedNote.MatchString(line) {
				v := f.other(t, len(notes)+1)
				notes = append(notes, guideVersionedNote.ReplaceAllString(strings.ReplaceAll(line, "**", ""), "- v"+v))
			}
		}
		if len(notes) < 20 {
			t.Fatalf("%s has %d notes, want at least 20", c.guide, len(notes))
		}
		return strings.Join(notes, "\n")
	}
	bullets := "- v{1}: health metrics.\n- v{2}: fast shutdown.\n- v{3}: TLS everywhere."
	cases := []tc{
		// History shapes QA found in the first two reviews.
		{"unbolded bullets under Release history", insert("## Release history\n\n" + bullets), []readmeRule{ruleHeading, ruleVersion}},
		{"unbolded bullets under 版本历史", insert("## 版本历史\n\n" + bullets), []readmeRule{ruleHeading, ruleVersion}},
		{"Upgrading notes", insert("## Upgrading notes\n\n" + bullets), []readmeRule{ruleHeading, ruleVersion}},
		{"numbered bold list under Older releases", insert("## Older releases\n\n1. **v{1}** metrics\n2. **v{2}** shutdown"), []readmeRule{ruleHeading, ruleVersion}},
		{"version table under a new heading", insert("## Releases\n\n| Version | Notes |\n|---|---|\n| {1} | metrics |\n| {2} | shutdown |"), []readmeRule{ruleHeading, ruleVersion}},
		{"v-less paragraphs inside Upgrading", replaceLine(pointer, func(c readmeContract) string {
			return c.pointer + "\n\n{1} adds metrics.\n\n{2} shuts down fast."
		}), []readmeRule{ruleUpgrading, ruleVersion}},
		{"the real guide notes, unbolded, under a new heading", func(body string, c readmeContract) string {
			return insert("## Release history\n\n"+historyNotes(c))(body, c)
		}, []readmeRule{ruleHeading, ruleVersion, ruleSize}},
		{"renamed heading with the version", insert("## Upgrade notes (v{1})\n\n- **v{1}:** new."), []readmeRule{ruleHeading, ruleVersion}},
		{"full-width colon heading", insert("## 升级说明：v{1}\n\n- **v{1}：** 新。"), []readmeRule{ruleHeading, ruleVersion}},
		{"notes after a rule in Upgrading", replaceLine(pointer, func(c readmeContract) string { return c.pointer + "\n\n---\n\n" + bullets }), []readmeRule{ruleUpgrading, ruleVersion}},
		{"notes in details", insert("<details><summary>Older</summary>\n\n" + bullets + "\n\n</details>"), []readmeRule{ruleVersion}},
		{"notes in an HTML comment", insert("<!-- v{1} -->"), []readmeRule{ruleVersion}},
		{"h3 upgrade notes", insert("### Upgrade Notes\n\n- **v{1}:** metrics."), []readmeRule{ruleVersion}},
		{"nested and quoted bullets", insert("- tips\n  - **v{1}:** a\n> - **v{2}:** b"), []readmeRule{ruleVersion}},
		// One rule each: versions.
		{"plain older version", insert("Fixed in v{4}."), []readmeRule{ruleVersion}},
		{"capital V", insert("Fixed in V{1}."), []readmeRule{ruleVersion}},
		{"no v", insert("Fixed in {1}."), []readmeRule{ruleVersion}},
		{"code span", insert("Fixed in `v{1}`."), []readmeRule{ruleVersion}},
		{"bold", insert("Fixed in **v{1}**."), []readmeRule{ruleVersion}},
		{"link to an older tag", insert("See [notes](https://github.com/Fanduzi/BinlogServer/releases/tag/v{1})."), []readmeRule{ruleVersion}},
		{"autolink to an older tag", insert("<https://github.com/Fanduzi/BinlogServer/releases/tag/v{1}>"), []readmeRule{ruleVersion}},
		{"attribute value", insert(`<a title="v{1}: health metrics">notes</a>`), []readmeRule{ruleVersion}},
		{"rc of VERSION", insert(ver + "-rc1"), []readmeRule{ruleVersion}},
		{"longer patch", insert("v" + ver + "0"), []readmeRule{ruleVersion}},
		// One rule each: headings.
		{"new h2 without versions", insert("## Release history\n\nSee the guide."), []readmeRule{ruleHeading}},
		{"new h1", insert("# History"), []readmeRule{ruleHeading}},
		{"indented h2", insert("   ## History"), []readmeRule{ruleHeading}},
		{"h2 with closing hashes", insert("## History ##"), []readmeRule{ruleHeading}},
		{"setext h2", insert("Release history\n---------------"), []readmeRule{ruleHeading}},
		{"setext h1", insert("Release history\n==============="), []readmeRule{ruleHeading}},
		{"HTML h2", insert("<h2>History</h2>"), []readmeRule{ruleHeading}},
		{"HTML h1 with attributes", insert(`<H1 id="history">History</H1>`), []readmeRule{ruleHeading}},
		{"renamed allowed heading", func(body string, c readmeContract) string {
			h := c.headings[1]
			return strings.Replace(body, h+"\n", h+" Now\n", 1)
		}, []readmeRule{ruleHeading}},
		{"dropped allowed heading", func(body string, c readmeContract) string {
			h := c.headings[len(c.headings)-1]
			return strings.Replace(body, h+"\n", "\n", 1)
		}, []readmeRule{ruleHeading}},
		{"swapped headings", func(body string, c readmeContract) string {
			a, b := c.headings[1], c.headings[2]
			body = strings.Replace(body, a+"\n", "\x00\n", 1)
			body = strings.Replace(body, b+"\n", a+"\n", 1)
			return strings.Replace(body, "\x00\n", b+"\n", 1)
		}, []readmeRule{ruleHeading}},
		// One rule each: the Upgrading template.
		{"extra line", replaceLine(pointer, func(c readmeContract) string { return c.pointer + "\n\nAlso read the FAQ." }), []readmeRule{ruleUpgrading}},
		{"bullet in the section", replaceLine(pointer, func(c readmeContract) string { return c.pointer + "\n\n- see the guide" }), []readmeRule{ruleUpgrading}},
		{"h3 then plain text inside Upgrading", replaceLine(pointer, func(c readmeContract) string {
			return c.pointer + "\n\n### Previous releases\n\nOlder notes are in the guide."
		}), []readmeRule{ruleUpgrading}},
		{"h3 then older notes inside Upgrading", replaceLine(pointer, func(c readmeContract) string {
			return c.pointer + "\n\n### Previous releases\n\n" + bullets
		}), []readmeRule{ruleUpgrading, ruleVersion}},
		{"h4 alone inside Upgrading", replaceLine(pointer, func(c readmeContract) string { return c.pointer + "\n\n#### Older" }), []readmeRule{ruleUpgrading}},
		{"h6 alone inside Upgrading", replaceLine(pointer, func(c readmeContract) string { return c.pointer + "\n\n###### Older" }), []readmeRule{ruleUpgrading}},
		{"pointer changed", replaceLine(pointer, func(c readmeContract) string {
			return strings.Replace(c.pointer, "CHANGELOG.md", "docs/CHANGELOG.md", 1)
		}), []readmeRule{ruleUpgrading}},
		{"pointer missing", replaceLine(pointer, func(readmeContract) string { return "" }), []readmeRule{ruleUpgrading}},
		{"lines swapped", func(body string, c readmeContract) string {
			p := pinLine(c)
			body = strings.Replace(body, p, "\x00", 1)
			body = strings.Replace(body, c.pointer, p, 1)
			return strings.Replace(body, "\x00", c.pointer, 1)
		}, []readmeRule{ruleUpgrading}},
		{"empty summary", replaceLine(pinLine, func(c readmeContract) string { return strings.TrimRight(pin(c), " ") }), []readmeRule{ruleUpgrading}},
		{"summary one over the cap", replaceLine(pinLine, func(c readmeContract) string {
			return pin(c) + strings.Repeat("x", 201)
		}), []readmeRule{ruleUpgrading}},
		{"pin without the v", replaceLine(pinLine, func(c readmeContract) string {
			return strings.Replace(pinLine(c), "`v"+ver, "`"+ver, 1)
		}), []readmeRule{ruleUpgrading}},
		{"stale pin", replaceLine(pinLine, func(c readmeContract) string {
			return strings.Replace(pinLine(c), ver, "{1}", 1)
		}), []readmeRule{ruleUpgrading, ruleVersion}},
		{"section renamed", func(body string, c readmeContract) string {
			return strings.Replace(body, c.upgrading+"\n", c.upgrading+"s\n", 1)
		}, []readmeRule{ruleHeading, ruleUpgrading}},
		// One rule: size.
		{"one byte over the cap", func(body string, c readmeContract) string {
			return insert(strings.Repeat("x", c.maxBytes-len(body)-1))(body, c)
		}, []readmeRule{ruleSize}},
	}
	for _, c := range readmeContracts {
		readme := f.files[c.rel]
		if got := readmeProblems(readme, ver, c); len(got) != 0 {
			t.Fatalf("%s: README already fails: %v", c.rel, got)
		}
		for _, k := range cases {
			body := k.edit(readme, c)
			if body == readme {
				t.Fatalf("%s: case %q did not change the README", c.rel, k.name)
			}
			got := map[readmeRule]bool{}
			for _, p := range readmeProblems(body, ver, c) {
				got[p.rule] = true
				if !strings.HasPrefix(p.msg, c.rel+" ") {
					t.Errorf("%s: case %q: message %q does not start with the file name", c.rel, k.name, p.msg)
				}
			}
			want := map[readmeRule]bool{}
			for _, r := range k.want {
				want[r] = true
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("%s: case %q broke rules %v, want %v (VERSION %s)", c.rel, k.name, got, want, ver)
			}
		}
	}
}

func checkUpgradingNamesSubsection(t *testing.T, f readmeFixture) {
	t.Helper()
	for _, c := range readmeContracts {
		readme := f.files[c.rel]
		for _, h := range []string{"### Previous releases", "#### Older", "###### Deep"} {
			body := strings.Replace(readme, c.pointer, c.pointer+"\n\n"+h, 1)
			got := strings.Join(upgradingTemplateProblems(body, f.ver, c), "\n")
			if !strings.Contains(got, fmt.Sprintf("has the heading %q", h)) || !strings.Contains(got, "has 3 lines") {
				t.Errorf("%s: %s inside the section: problems %q, want the heading named and the line count", c.rel, h, got)
			}
		}
	}
}
