// Package releaseprep finds every copy of the previous release outside release history.
// input: the version being released, the previous release, and every tracked file (decoded, never skipped; symlinks not followed)
// output: one failed Finding per stale line (file:line) that no History rule allows
// pos: the primary release-prep gate; the Surfaces manifest is the positive check next to it
// note: if this file changes, update this header and README.md.
package releaseprep

import (
	"fmt"
	"html"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// HistoryRule allows the previous release to be named on purpose, in one of
// three scopes:
//   - no Line and no Section: the whole file;
//   - Section: every line of the section whose heading matches it (to the next
//     heading of the same or a higher level, code fences included);
//   - Line: only inside a match of Line on the line (normalized as
//     findVersion reads it), so the rule is anchored to one construct and a
//     second copy elsewhere on the same line still fails. Unless, when set,
//     turns the rule off for a line it matches (an install pin there is never
//     history).
//
// In Line, Unless and Section, {v} stands for this release and {p} for the
// previous one (both quoted).
type HistoryRule struct {
	// Path is a repo path, "dir/**" (everything under dir), "**/name" (a file
	// name anywhere) or a path.Match pattern.
	Path    string
	Line    string
	Unless  string
	Section string
	Why     string
}

// History is the release-history allowlist. A line naming the previous release
// anywhere else is a copy the release forgot to bump. A new legitimate mention
// of an older release goes here, with its reason; the failure message says so.
// Each rule covers a file that names a release today or has a reason to.
var History = []HistoryRule{
	// Release history by definition.
	{Path: changelogPath, Why: "the CHANGELOG keeps a section per release"},
	{Path: "docs/releases/**", Why: "release notes describe each release and what it upgrades from"},
	{Path: "docs/adr/**", Why: "an ADR is a dated record of a decision and the release it shipped in; it is not edited per release"},
	{Path: "docs/design/**", Why: "a design doc is a dated record of the release a design shipped in; it is not edited per release"},
	{Path: "docs/guide/admin/upgrade.md", Section: guideHistory, Why: "the per-version notes keep one note per release, newest first (#563)"},
	{Path: "docs/guide/admin/upgrade.zh-CN.md", Section: guideHistory, Why: "the per-version notes keep one note per release, newest first (#563)"},

	// Tests and dependency pins never advertise the release.
	{Path: "**/*_test.go", Why: "tests name fixture versions and the release a behaviour came from; they do not ship"},
	{Path: "migrations/**", Why: "a migration names the release that added it and is never edited after it ships"},
	{Path: "go.mod", Why: "dependency versions, not the release"},
	{Path: "go.sum", Why: "dependency versions, not the release"},
	{Path: "**/package-lock.json", Why: "resolved dependency versions, not the release"},

	// The in-place upgrade e2e leg starts from the previous release on purpose.
	{Path: ".github/workflows/e2e.yml", Line: `old_ref: v{p}\b|in-place upgrade from v{p}\b`, Why: "the plain upgrade leg starts from the previous release"},
	{Path: "scripts/e2e/upgrade-legacy-failback.sh", Line: `plain from v{p}\b`, Why: "the plain upgrade leg starts from the previous release"},
	{Path: "docs/production-readiness.md", Line: "`plain` 从 v{p}\\b", Why: "the plain upgrade leg starts from the previous release"},
	{Path: "scripts/e2e/README.md", Line: "`plain` 从 `v{p}`|从 `v{p}` 直接升级", Why: "the plain upgrade leg starts from the previous release"},

	// The release skill's worked example: OLD is the previous release by
	// definition (OLD=${LAST_TAG#v}), so the example names it on purpose.
	{Path: ".agents/skills/binlogserver-release/SKILL.md", Line: `e\.g\. {p}\b`, Why: "the release runbook's OLD is the previous release by definition"},

	// The landing page release timeline until the landing-page cleanup removes
	// it (and these three rules), each one element only. The first entry is this
	// release, and its "(Current)" marker is a Surface.
	{Path: landingPath, Line: `<div class="timeline-version">v[0-9]+\.[0-9]+\.[0-9]+</div>`, Why: "an older timeline entry (#565 removes the timeline)"},
	{Path: landingPath, Line: `<p data-i18n="release[2-9]">[^<]*</p>|\brelease[2-9]: '(?:[^'\\\n]|\\.)*'`, Why: "an older timeline entry (#565 removes the timeline)"},
	{Path: landingPath, Line: `<p data-i18n="release1">v{v}[:：][^<]*</p>|\brelease1: 'v{v}[:：](?:[^'\\\n]|\\.)*'`, Why: "this release's timeline entry says what it upgrades from"},
}

// guideHistory is the upgrade guides' history section.
const guideHistory = `^## (?:Per-version notes|逐版本说明)\s*$`

// GuideNote is one upgrade-guide note: a list item, heading or quoted list
// item that starts with a version (the three parts are its capture groups).
// The upgrade-guide Surfaces use it as NewestNote.
const GuideNote = `^[ \t]*(?:[-*+]|\d+[.)]|#{1,6}|>[ \t]*[-*+])[ \t]+(?:\*\*|__)?[vV]?(\d+)\.(\d+)\.(\d+)`

// checkStale fails every line of every tracked file that names prev (any
// spelling, see findVersion) unless a History rule allows each copy. Every
// file is read (decodeText); symlinks are not followed and are listed.
func (c *checker) checkStale(prev, what string) {
	files, links, err := c.src.Files()
	if err != nil {
		c.err = err
		return
	}
	rules, err := compileHistory(History, c.ver, prev)
	if err != nil {
		c.err = err
		return
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	stale := 0
	for _, p := range paths {
		lines := strings.Split(decodeText(files[p]), "\n")
		inSection := rules.sections(p, lines)
		for i, line := range lines {
			norm := normalizeLine(line)
			spans := versionSpans(norm, prev)
			if len(spans) == 0 || inSection[i] || rules.allows(p, norm, spans) {
				continue
			}
			stale++
			c.fail("%s:%d names %s %s: %s; bump it to %s, or if it is release history add a releaseprep.History rule with the reason", p, i+1, what, prev, snippet(line, prev), c.ver)
		}
	}
	if stale == 0 {
		c.pass("no copy of %s v%s outside release history (%d files)", what, prev, len(paths))
	}
	if len(links) > 0 {
		sort.Strings(links)
		c.pass("not followed: %d symlink(s) %s (their targets are scanned as files)", len(links), strings.Join(links, ", "))
	}
}

type compiledRule struct {
	path                  string
	line, unless, section *regexp.Regexp
}

type compiledRules []compiledRule

func compileHistory(rules []HistoryRule, ver, prev string) (compiledRules, error) {
	r := strings.NewReplacer("{v}", regexp.QuoteMeta(ver), "{p}", regexp.QuoteMeta(prev))
	compile := func(h HistoryRule, expr string) (*regexp.Regexp, error) {
		if expr == "" {
			return nil, nil
		}
		re, err := regexp.Compile(r.Replace(expr))
		if err != nil {
			return nil, fmt.Errorf("history rule %s: %w", h.Path, err)
		}
		return re, nil
	}
	out := make(compiledRules, 0, len(rules))
	for _, h := range rules {
		if h.Why == "" {
			return nil, fmt.Errorf("history rule %s has no reason", h.Path)
		}
		if h.Line != "" && h.Section != "" {
			return nil, fmt.Errorf("history rule %s sets both Line and Section", h.Path)
		}
		cr := compiledRule{path: h.Path}
		var err error
		if cr.line, err = compile(h, h.Line); err != nil {
			return nil, err
		}
		if cr.unless, err = compile(h, h.Unless); err != nil {
			return nil, err
		}
		if cr.section, err = compile(h, h.Section); err != nil {
			return nil, err
		}
		out = append(out, cr)
	}
	return out, nil
}

// sections marks the lines of p that a whole-file or Section rule allows.
func (rs compiledRules) sections(p string, lines []string) []bool {
	out := make([]bool, len(lines))
	for _, r := range rs {
		if !pathMatches(r.path, p) || r.line != nil {
			continue
		}
		start, end := 0, len(lines)
		if r.section != nil {
			if start, end = section(lines, r.section); start < 0 {
				continue
			}
		}
		for i := start; i < end; i++ {
			out[i] = true
		}
	}
	return out
}

// allows reports whether Line rules cover every copy (spans) on the line.
func (rs compiledRules) allows(p, norm string, spans [][2]int) bool {
	var ok [][]int
	for _, r := range rs {
		if r.line == nil || !pathMatches(r.path, p) || (r.unless != nil && r.unless.MatchString(norm)) {
			continue
		}
		ok = append(ok, r.line.FindAllStringIndex(norm, -1)...)
	}
	for _, s := range spans {
		covered := false
		for _, m := range ok {
			if m[0] <= s[0] && s[1] <= m[1] {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

func pathMatches(pattern, p string) bool {
	if dir, ok := strings.CutSuffix(pattern, "/**"); ok {
		if strings.HasPrefix(dir, "**/") {
			return strings.Contains("/"+p, "/"+strings.TrimPrefix(dir, "**/")+"/")
		}
		return strings.HasPrefix(p, dir+"/")
	}
	if name, ok := strings.CutPrefix(pattern, "**/"); ok {
		m, _ := path.Match(name, path.Base(p))
		return m
	}
	m, _ := path.Match(pattern, p)
	return m
}

// findVersion returns the spans (in normalizeLine(line)) where ver (X.Y.Z)
// appears in any spelling: with or without v, with ., _ or - between the parts
// (archive names and dirs), leading zeros, and whatever normalizeLine folds.
// A longer number does not count: 10.1.2.3, 1.2.30 and 1.2.3.1 are not
// 1.2.3.
func findVersion(line, ver string) [][2]int { return versionSpans(normalizeLine(line), ver) }

func versionSpans(s, ver string) [][2]int {
	var out [][2]int
	for _, m := range versionRE(ver).FindAllStringIndex(s, -1) {
		before := byte(' ')
		if m[0] > 0 {
			before = s[m[0]-1]
		}
		if s[m[0]] == 'v' || s[m[0]] == 'V' {
			if isWordByte(before) { // "xv1.2.3": the v belongs to a word
				continue
			}
		} else if isWordByte(before) || before == '.' { // 10.1.2.3, x1.2.3
			continue
		}
		rest := s[m[1]:]
		if len(rest) > 0 && isDigit(rest[0]) { // 1.2.30
			continue
		}
		if len(rest) > 1 && rest[0] == '.' && isDigit(rest[1]) { // 1.2.3.1
			continue
		}
		// The same separator on into another number: a digit-grouped
		// 1_000_000_000 is one longer number, not a version inside it.
		sep := s[m[0]+strings.IndexAny(s[m[0]:m[1]], "._-")]
		if len(rest) > 1 && rest[0] == sep && isDigit(rest[1]) || m[0] > 1 && before == sep && isDigit(s[m[0]-2]) {
			continue
		}
		out = append(out, [2]int{m[0], m[1]})
	}
	return out
}

var (
	versionREMu    sync.Mutex
	versionRECache = map[string]*regexp.Regexp{}
)

func versionRE(ver string) *regexp.Regexp {
	versionREMu.Lock()
	defer versionREMu.Unlock()
	re, ok := versionRECache[ver]
	if !ok {
		parts := strings.Split(ver, ".")
		for i, p := range parts {
			if p = strings.TrimLeft(p, "0"); p == "" {
				parts[i] = "0+"
			} else {
				parts[i] = "0*" + regexp.QuoteMeta(p)
			}
		}
		re = regexp.MustCompile(`[vV]?` + strings.Join(parts, `[._-]`))
		versionRECache[ver] = re
	}
	return re
}

func isDigit(b byte) bool    { return b >= '0' && b <= '9' }
func isWordByte(b byte) bool { return isDigit(b) || (b|0x20 >= 'a' && b|0x20 <= 'z') }

// dotLike are characters that render as a dot between version parts;
// dashLike as a dash.
var (
	dotLike  = map[rune]bool{'．': true, '。': true, '｡': true, '·': true, '・': true, '․': true, '‧': true, '﹒': true, '∙': true, 'ˌ': true, 'ꓸ': true, '٫': true}
	dashLike = map[rune]bool{'‐': true, '‑': true, '‒': true, '–': true, '—': true, '―': true, '−': true, '﹣': true, '－': true}
)

var (
	// inlineSplitRE is an inline tag or comment between two characters of a
	// version, as in v1.<b>2</b>.3 or 1.2<!-- -->.3.
	inlineSplitRE = regexp.MustCompile(`(?i)([0-9vV._-])((?:<!--.*?-->|</?(?:span|b|i|em|strong|code|a|small|sup|sub|mark|wbr|s|u|kbd|samp|var)\b[^<>]*>)+)([0-9._-])`)
	percentRE     = regexp.MustCompile(`%(?:2[EeDd]|5[Ff])`)
)

// normalizeLine folds the spellings a version can be split into: inline tags
// and comments between its characters, %2E/%2D/%5F escapes, HTML entities,
// zero-width characters, Unicode decimal digits (to ASCII), and dot and dash
// look-alikes (to '.' and '-').
func normalizeLine(line string) string {
	for {
		next := inlineSplitRE.ReplaceAllString(line, "$1$3")
		if next == line {
			break
		}
		line = next
	}
	line = percentRE.ReplaceAllString(line, ".") // %2E, %2D and %5F all separate parts
	line = html.UnescapeString(line)
	var b strings.Builder
	for _, r := range line {
		switch {
		case r == '\u200b' || r == '\u200c' || r == '\u200d' || r == '\u2060' || r == '\ufeff' || r == '\u00ad':
		case dotLike[r]:
			b.WriteByte('.')
		case dashLike[r]:
			b.WriteByte('-')
		case r > unicode.MaxASCII && unicode.Is(unicode.Nd, r):
			b.WriteByte(byte('0' + digitValue(r)))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// digitValue is the value of a Unicode decimal digit. Nd digits come in runs
// of ten starting at zero; the mathematical digits are five runs back to back.
func digitValue(r rune) int {
	if r >= 0x1D7CE && r <= 0x1D7FF {
		return int(r-0x1D7CE) % 10
	}
	zero := r
	for zero > 0 && unicode.Is(unicode.Nd, zero-1) && r-zero < 9 {
		zero--
	}
	return int(r - zero)
}

// snippet quotes the line as written, cut to about 120 bytes around the
// version when it is long.
func snippet(line, ver string) string {
	s := strings.TrimSpace(line)
	if utf8.RuneCountInString(s) <= 120 {
		return fmt.Sprintf("%q", s)
	}
	at := strings.Index(s, ver)
	if at < 0 { // an unusual spelling: place it by its offset in the normalized line
		if spans := findVersion(s, ver); len(spans) > 0 {
			at = spans[0][0] * len(s) / max(1, len(normalizeLine(s)))
		}
		at = max(at, 0)
	}
	lo, hi := max(0, at-50), min(len(s), at+70)
	for lo > 0 && !utf8.RuneStart(s[lo]) {
		lo--
	}
	for hi < len(s) && !utf8.RuneStart(s[hi]) {
		hi++
	}
	out := s[lo:hi]
	if lo > 0 {
		out = "…" + out
	}
	if hi < len(s) {
		out += "…"
	}
	return fmt.Sprintf("%q", out)
}
