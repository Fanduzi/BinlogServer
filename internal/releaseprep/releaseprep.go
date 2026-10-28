// Package releaseprep checks that a release-prep commit carries every release surface for one version.
// input: a tag vX.Y.Z, the repo's release tags, and files read from the working tree or a git ref
// output: one Finding per check; any failed Finding blocks the release. Check is the tag gate (rel from git and GitHub) and, with a nil rel, the per-PR run against VERSION; both check the notes and CHANGELOG and share checkTree (stale scan + Surfaces)
// pos: release gate behind `make release-check` and the release-prep job in release.yml
// note: if this file changes, update this header and README.md.
package releaseprep

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Release notes and the CHANGELOG are checked here; every other file a release
// edits is a Surface in manifest.go.
const (
	changelogPath = "CHANGELOG.md"

	minNotesBodyRunes = 80 // a title plus a date line is not release notes
	minZhHanRunes     = 20 // the zh notes must be written in Chinese, not only titled in it
)

func notesEN(tag string) string { return "docs/releases/release-notes-" + tag + ".md" }
func notesZH(tag string) string { return "docs/releases/" + tag + ".zh-CN.md" }

// Prerelease tags (v1.2.3-rc1) are rejected on purpose: every tag publishes a
// release that the README and landing page point at.
var tagRE = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// Version is a parsed vX.Y.Z release tag.
type Version [3]int

// ParseTag parses vX.Y.Z and rejects every other form.
func ParseTag(tag string) (Version, error) {
	m := tagRE.FindStringSubmatch(tag)
	if m == nil {
		return Version{}, fmt.Errorf("version must look like vX.Y.Z (no prerelease suffix), got %q", tag)
	}
	var v Version
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return Version{}, err
		}
		v[i] = n
	}
	return v, nil
}

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

// Compare orders versions numerically, so 1.10.0 > 1.9.0.
func (v Version) Compare(o Version) int { return slices.Compare(v[:], o[:]) }

// Source reads one repo file as stored (ok is false when it does not exist),
// and every tracked file for the stale-version scan, symlinks apart (links).
type Source interface {
	Read(path string) (body string, ok bool, err error)
	Files() (files map[string]string, links []string, err error)
}

// Finding is the result of one check.
type Finding struct {
	OK  bool
	Msg string
}

type checker struct {
	src      Source
	tag, ver string
	prev     string
	// unpublished are the versions between prev and this one (scanVersions).
	unpublished []string
	out         []Finding
	err         error
}

func (c *checker) pass(format string, a ...any) {
	c.out = append(c.out, Finding{OK: true, Msg: fmt.Sprintf(format, a...)})
}

func (c *checker) fail(format string, a ...any) {
	c.out = append(c.out, Finding{Msg: fmt.Sprintf(format, a...)})
}

// read returns the file body, or records a failure when it is missing.
func (c *checker) read(path string) (string, bool) {
	body, ok, err := c.src.Read(path)
	if err != nil {
		c.err = err
		return "", false
	}
	if !ok {
		c.fail("%s missing", path)
	}
	return decodeText(body), ok
}

// Releases is what the tag gate knows about earlier releases.
type Releases struct {
	// Merged are the v* tags that are ancestors of the release commit
	// (MergedTags); a tag on another branch is never the previous release.
	Merged []string
	// Published are the tags with a published GitHub release. nil means
	// unknown (offline): the previous release is then the newest CHANGELOG
	// section older than the tag whose tag is in Merged.
	Published []string
}

// Check runs every release-prep check for tag against src; an error means
// the check could not run. rel is the tag gate's view (release-check): the
// tags in the release commit's history and, unless offline, the published
// GitHub releases. A nil rel is what every PR runs against VERSION
// (internal/config's TestVersionPinsMatchVERSIONFile): the same checks without
// the tags, so a release-prep PR that bumps VERSION fails in CI, before the
// tag, when its notes, CHANGELOG section or any surface is missing, and
// scanVersions takes the previous release from the CHANGELOG. Both go through
// this one code path.
func Check(src Source, rel *Releases, tag string) ([]Finding, error) {
	v, err := ParseTag(tag)
	if err != nil {
		return nil, err
	}
	c := &checker{src: src, tag: tag, ver: v.String()}
	if rel != nil {
		c.checkHistoryTags(v, rel.Merged)
	}
	changelog := c.changelogVersions()
	if c.err != nil {
		return nil, c.err
	}
	prev, extra, from := scanVersions(v, rel, changelog)
	if prev == nil {
		c.fail("no release older than %s among the %s; cannot look for stale copies", c.tag, from)
	} else {
		c.prev = prev.String()
		c.pass("%s follows the previous release v%s (newest older one among the %s)", c.tag, c.prev, from)
		for _, x := range extra {
			c.unpublished = append(c.unpublished, x.String())
		}
		if len(extra) > 0 {
			c.pass("also scanning v%s: tagged or in %s, newer than v%s and never published", strings.Join(c.unpublished, ", v"), changelogPath, c.prev)
		}
	}
	c.checkTree()
	return c.out, c.err
}

// scanVersions is the single rule for which earlier versions the stale scan
// looks for, shared by the tag gate and every PR's go test:
//   - prev, the last published release: the newest version older than v among
//     the published GitHub releases that are tags in the release commit's
//     history; when published releases are unknown (rel nil, or
//     rel.Published nil offline) among the CHANGELOG sections (that are tags
//     in its history, when the history is known);
//   - extra, every version in the CHANGELOG or tagged in that history that is
//     newer than prev and older than v: a release that was tagged or prepared
//     but never published still had its version written everywhere.
//
// from names where prev came from.
func scanVersions(v Version, rel *Releases, changelog []string) (prev *Version, extra []Version, from string) {
	var merged []string
	candidates, from := changelog, changelogPath+" sections"
	if rel != nil {
		merged = rel.Merged
		if rel.Published != nil {
			candidates, from = rel.Published, "published GitHub releases"
		} else {
			from += " (offline: published releases unknown)"
		}
		var both []string
		for _, t := range candidates {
			if slices.Contains(merged, t) {
				both = append(both, t)
			}
		}
		candidates, from = both, from+" that are tags in its history"
	}
	if prev = previousTag(v, candidates); prev == nil {
		return nil, nil, from
	}
	seen := map[Version]bool{}
	for _, t := range append(slices.Clone(changelog), merged...) {
		tv, err := ParseTag(t)
		if err == nil && tv.Compare(*prev) > 0 && tv.Compare(v) < 0 && !seen[tv] {
			seen[tv] = true
			extra = append(extra, tv)
		}
	}
	slices.SortFunc(extra, Version.Compare)
	return prev, extra, from
}

// checkHistoryTags fails a clone without tags (shallow: the previous release
// cannot be found) and a tag older than one already in its history.
func (c *checker) checkHistoryTags(v Version, merged []string) {
	for _, t := range merged {
		if tv, err := ParseTag(t); err == nil && tv.Compare(v) > 0 {
			c.fail("%s is older than the release tag %s already in its history", c.tag, t)
			return
		}
	}
	if len(merged) == 0 {
		c.fail("no release tag is an ancestor of %s in this clone; a shallow clone (fetch-depth 1) has no tags: run `git fetch --tags`", c.tag)
	}
}

// checkTree is the part of the gate both entry points share: the inverse
// check (no stale copy of the previous release) and the Surfaces manifest.
func (c *checker) checkTree() {
	c.checkNotes(notesEN(c.tag), false)
	c.checkNotes(notesZH(c.tag), true)
	c.checkChangelog()
	if c.prev != "" {
		c.checkStale(c.prev, "the previous release")
	}
	for _, x := range c.unpublished {
		c.checkStale(x, "the unpublished release")
	}
	c.checkSurfaces(Surfaces)
}

func previousTag(v Version, tags []string) *Version {
	var prev *Version
	for _, t := range tags {
		tv, err := ParseTag(t)
		if err == nil && tv.Compare(v) < 0 && (prev == nil || tv.Compare(*prev) > 0) {
			p := tv
			prev = &p
		}
	}
	return prev
}

var changelogVersionRE = regexp.MustCompile(`(?m)^## \[?(v[0-9]+\.[0-9]+\.[0-9]+)\]?(?:\s|$)`)

// changelogVersions lists the CHANGELOG's release sections (vX.Y.Z).
func (c *checker) changelogVersions() []string {
	body, ok, err := c.src.Read(changelogPath)
	if err != nil {
		c.err = err
		return nil
	}
	if !ok {
		return nil
	}
	var tags []string
	for _, m := range changelogVersionRE.FindAllStringSubmatch(stripFences(stripHTMLComments(decodeText(body))), -1) {
		tags = append(tags, m[1])
	}
	return tags
}

func (c *checker) checkNotes(path string, zh bool) {
	body, ok := c.read(path)
	if !ok {
		return
	}
	lines := strings.Split(stripHTMLComments(body), "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i == len(lines) || !c.titleOK(lines[i], zh) {
		c.fail("release notes %s must open with a title naming only %s", path, c.tag)
		return
	}
	var content strings.Builder
	for _, l := range lines[i+1:] {
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") {
			content.WriteString(t)
		}
	}
	text := content.String()
	switch {
	case len([]rune(text)) < minNotesBodyRunes:
		c.fail("release notes %s have no body under the title (%d characters, need %d)", path, len([]rune(text)), minNotesBodyRunes)
	case zh && countHan(text) < minZhHanRunes:
		c.fail("release notes %s are not written in Chinese (%d Han characters, need %d)", path, countHan(text), minZhHanRunes)
	default:
		c.pass("release notes %s", path)
	}
}

func (c *checker) titleOK(line string, zh bool) bool {
	if !zh {
		rest, ok := strings.CutPrefix(line, "# Binlog Server v"+c.ver)
		return ok && (rest == "" || unicode.IsSpace([]rune(rest)[0]))
	}
	return strings.HasPrefix(line, "# ") && slices.Equal(versionMentions(line), []string{c.ver})
}

func countHan(s string) int {
	n := 0
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			n++
		}
	}
	return n
}

func (c *checker) checkChangelog() {
	body, ok := c.read(changelogPath)
	if !ok {
		return
	}
	heading := regexp.MustCompile(`^## (?:\[v?` + regexp.QuoteMeta(c.ver) + `\]|v?` + regexp.QuoteMeta(c.ver) + `)(?:\s|$)`)
	lines := strings.Split(stripFences(stripHTMLComments(body)), "\n")
	for i, l := range lines {
		if !heading.MatchString(l) {
			continue
		}
		for _, next := range lines[i+1:] {
			if strings.HasPrefix(next, "## ") {
				break
			}
			if strings.TrimSpace(next) != "" {
				c.pass("%s has a %s section", changelogPath, c.tag)
				return
			}
		}
		c.fail("%s section for %s is empty", changelogPath, c.tag)
		return
	}
	c.fail("%s has no '## [%s]' section outside comments and code fences", changelogPath, c.tag)
}

// section returns the line range from the first heading matching re to the
// next heading of the same or a higher level, ignoring code fences.
func section(lines []string, re *regexp.Regexp) (int, int) {
	start, level := -1, 0
	fenced := false
	for i, l := range lines {
		if isFence(l) {
			fenced = !fenced
			continue
		}
		if fenced || !strings.HasPrefix(l, "#") {
			continue
		}
		hashes := len(l) - len(strings.TrimLeft(l, "#"))
		if start >= 0 && hashes <= level {
			return start, i
		}
		if start < 0 && re.MatchString(l) {
			start, level = i, hashes
		}
	}
	return start, len(lines)
}

var pinRE = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])VER=("[^"\n]*"|'[^'\n]*'|[^\s"'\x60;&|)]*)`)

// checkPins requires at least one VER= pin a reader sees (shown) and every
// pin anywhere outside comments (text), in any quoting, to equal the version.
func (c *checker) checkPins(path, text, shown string) {
	pins := pinValues(text)
	if len(pinValues(shown)) == 0 {
		c.fail("%s has no visible install pin (expected VER=%s)", path, c.ver)
		return
	}
	if wrong := others(pins, c.ver); len(wrong) > 0 {
		c.fail("%s pins %s instead of VER=%s", path, strings.Join(wrong, ", "), c.ver)
		return
	}
	c.pass("%s install pins are VER=%s", path, c.ver)
}

// Placeholder is the landing page's stand-in for the version in its JS
// strings (VER={ver}, v{ver}); the page fills it from <html data-version>.
// It is neither a pin nor a version.
const Placeholder = "{ver}"

func pinValues(text string) []string {
	var out []string
	for _, m := range pinRE.FindAllStringSubmatch(text, -1) {
		if v := strings.Trim(m[1], `"'`); v != Placeholder {
			out = append(out, v)
		}
	}
	return out
}

// checkMentions requires every version mention in text to be the version.
func (c *checker) checkMentions(what, text string, required bool) {
	got := versionMentions(text)
	if required && len(got) == 0 {
		c.fail("%s names no version (expected %s)", what, c.tag)
		return
	}
	if wrong := others(got, c.ver); len(wrong) > 0 {
		c.fail("%s names %s instead of %s%s", what, strings.Join(wrong, ", "), c.ver, c.prevHint(wrong))
		return
	}
	c.pass("%s names only %s", what, c.tag)
}

func (c *checker) prevHint(wrong []string) string {
	if c.prev != "" && slices.Contains(wrong, c.prev) {
		return " (still the previous release v" + c.prev + ")"
	}
	return ""
}

// versionMentions finds vX.Y.Z (any extra segments or prerelease suffix kept,
// so 1.2.3.1 and 1.2.3-rc1 do not read as 1.2.3), VER= pins and release
// archive names.
var (
	vTokenRE  = regexp.MustCompile(`v([0-9]+\.[0-9]+\.[0-9]+(?:\.[0-9]+)*(?:-[0-9A-Za-z][0-9A-Za-z.]*)?)`)
	archiveRE = regexp.MustCompile(`binlog-server_([^_\s/"'\x60$]+)_`)
	commentRE = regexp.MustCompile(`(?s)<!--.*?(?:-->|$)`)
)

func versionMentions(text string) []string {
	var out []string
	for _, re := range []*regexp.Regexp{vTokenRE, archiveRE} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			out = append(out, m[1])
		}
	}
	for _, p := range pinValues(text) {
		if !strings.Contains(p, "$") {
			out = append(out, p)
		}
	}
	return out
}

func others(got []string, want string) []string {
	var out []string
	for _, g := range got {
		if g != want && !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	slices.Sort(out)
	return out
}

// stripHTMLComments removes <!-- ... --> (an unclosed one runs to the end),
// keeping line breaks so line-based checks still see the same structure.
func stripHTMLComments(s string) string {
	return commentRE.ReplaceAllStringFunc(s, func(m string) string {
		return strings.Repeat("\n", strings.Count(m, "\n"))
	})
}

func isFence(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
}

// stripFences blanks every line inside a fenced code block.
func stripFences(s string) string {
	lines := strings.Split(s, "\n")
	fenced := false
	for i, l := range lines {
		if isFence(l) {
			fenced = !fenced
			lines[i] = ""
		} else if fenced {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}
