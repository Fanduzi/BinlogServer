// Package releaseprep lists every file a release edits and what each must say.
// input: a version X.Y.Z
// output: Surfaces, the one release-surface manifest (the positive check), and checkSurfaces, which applies it
// pos: shared by the release-check gate (Check) and internal/config's doc pin test, so the two cannot drift
// note: if this file changes, update this header and README.md. A PR that adds or changes a release surface edits Surfaces only.
package releaseprep

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Surface is one file the release-prep commit edits: the positive half of the
// gate (the files must say this version). The inverse half, no stale copy of
// the previous release anywhere, is checkStale with the History allowlist.
// Every rule reads the file with HTML comments stripped.
type Surface struct {
	Path string
	// Visible strings must appear in the text; "{v}" stands for X.Y.Z. For
	// .html and .md files that is shownText: no HTML comments, <template>,
	// <script> or <style> content, and no tag attributes (<meta content>).
	// CSS or attribute hiding is out of scope on purpose (README.md). A string
	// ending in the version must not be followed by more of a version
	// (v1.2.30, v1.2.3.1, v1.2.3-rc1).
	Visible []string
	// Attrs: attributes that must equal X.Y.Z exactly, such as <html data-version>.
	Attrs []Attr
	// VERPins: at least one visible VER= pin, and every VER= value anywhere in
	// the file, in any quoting, is exactly X.Y.Z (the {ver} placeholder aside).
	VERPins bool
	// InstallSection: the install section (the first h2-h4 heading with
	// "unpack" or "解压", to the next heading of the same or higher level, code
	// fences included) names this version in its heading and no other version
	// anywhere (vX.Y.Z, VER= and archive names, so the checksum lines too).
	InstallSection bool
	// OnlyThisVersion: every version mention in the file is X.Y.Z.
	OnlyThisVersion bool
	// Exact: the file is exactly "X.Y.Z" with an optional trailing newline.
	Exact bool
	// NewestNote is a regexp (multiline) matching one versioned note; its
	// capture groups, joined with ".", are the note's version. Outside comments
	// and code fences the first note must be X.Y.Z and each later note strictly
	// older: the upgrade guides keep one note per release, newest first.
	NewestNote string
	// ScriptUsesPlaceholder: no <script> names any version, this one included;
	// JS strings write {ver} and fill it from <html data-version>.
	ScriptUsesPlaceholder bool
	// Mentions, when above zero, is how many times X.Y.Z (any spelling) appears
	// outside HTML comments: the page's version sites, no more and no fewer.
	Mentions int
}

// Attr names one element attribute: the first <Elem> element's Name.
type Attr struct{ Elem, Name string }

const (
	deployPath  = "docs/guide/admin/deployment.md"
	landingPath = "docs/landing/index.html"
	versionPath = "VERSION"
)

// Surfaces is the release-surface manifest. The release-prep gate and
// TestVersionPinsMatchVERSIONFile in internal/config both apply it (through
// Check, for the tag and for every PR), so a surface added here is checked on every PR
// (against VERSION) and on the tag.
var Surfaces = []Surface{
	{Path: "README.md", VERPins: true, InstallSection: true},
	{Path: "README_ZH.md", VERPins: true, InstallSection: true},
	{Path: deployPath, VERPins: true},
	{Path: "config.example.yaml", Visible: []string{"BinlogServer v{v}"}, OnlyThisVersion: true},
	{Path: "config.production.example.yaml", Visible: []string{"BinlogServer v{v}"}, OnlyThisVersion: true},
	{
		Path:    landingPath,
		Attrs:   []Attr{{"html", "data-version"}},
		Visible: []string{"v{v} — latest release", "Get Started with v{v}"},
		VERPins: true,
	},
	{Path: versionPath, Exact: true},
}

func (c *checker) checkSurfaces(surfaces []Surface) {
	for _, s := range surfaces {
		body, ok := c.read(s.Path)
		if !ok {
			continue
		}
		c.checkSurface(s, body)
	}
}

func (c *checker) checkSurface(s Surface, body string) {
	if s.Exact {
		if body == c.ver || body == c.ver+"\n" {
			c.pass("%s is %s", s.Path, c.ver)
		} else {
			c.fail("%s must be exactly one line %q, got %q", s.Path, c.ver, body)
		}
		return
	}
	text := stripHTMLComments(body)
	shown := text
	if hasMarkup(s.Path) {
		shown = shownText(body)
	}
	for _, a := range s.Attrs {
		got, ok := attrOf(body, a.Elem, a.Name)
		if ok && got == c.ver {
			c.pass("%s <%s %s> is %s", s.Path, a.Elem, a.Name, c.ver)
		} else {
			c.fail("%s <%s %s=%q> must be %q", s.Path, a.Elem, a.Name, got, c.ver)
		}
	}
	for _, tmpl := range s.Visible {
		want := strings.ReplaceAll(tmpl, "{v}", c.ver)
		if containsWhole(shown, want) {
			c.pass("%s shows %q", s.Path, want)
		} else {
			c.fail("%s does not show %q (outside comments, <template>, <script>, <style> and tag attributes)", s.Path, want)
		}
	}
	if s.VERPins {
		c.checkPins(s.Path, text, shown)
	}
	if s.InstallSection {
		c.checkInstallSection(s.Path, text)
	}
	if s.OnlyThisVersion {
		c.checkMentions(s.Path, text, true)
	}
	if s.NewestNote != "" {
		c.checkNewestNote(s.Path, text, s.NewestNote)
	}
	if s.ScriptUsesPlaceholder {
		c.checkScripts(s.Path, body)
	}
	if s.Mentions > 0 {
		n := 0
		for _, line := range strings.Split(text, "\n") {
			n += len(findVersion(line, c.ver))
		}
		if n == s.Mentions {
			c.pass("%s names %s exactly %d times", s.Path, c.ver, n)
		} else {
			c.fail("%s names %s %d times outside comments, want exactly %d (its version sites)", s.Path, c.ver, n, s.Mentions)
		}
	}
}

func (c *checker) checkNewestNote(path, text, pattern string) {
	re, err := regexp.Compile("(?m)" + pattern)
	if err != nil {
		c.err = fmt.Errorf("%s NewestNote: %w", path, err)
		return
	}
	var notes []Version
	for _, m := range re.FindAllStringSubmatch(stripFences(text), -1) {
		var parts []string
		for _, g := range m[1:] {
			if g != "" {
				parts = append(parts, g)
			}
		}
		v, err := ParseTag("v" + strings.Join(parts, "."))
		if err != nil {
			c.fail("%s note %q has no X.Y.Z version", path, strings.TrimSpace(m[0]))
			return
		}
		notes = append(notes, v)
	}
	if len(notes) == 0 {
		c.fail("%s has no versioned note (expected one for %s at the top)", path, c.tag)
		return
	}
	if notes[0].String() != c.ver {
		c.fail("%s newest note is v%s, want %s at the top", path, notes[0], c.tag)
		return
	}
	for i := 1; i < len(notes); i++ {
		if notes[i].Compare(notes[i-1]) >= 0 {
			c.fail("%s note v%s comes after v%s; keep one note per release, newest first", path, notes[i], notes[i-1])
			return
		}
	}
	c.pass("%s newest note is %s (%d notes, newest first)", path, c.tag, len(notes))
}

// checkScripts fails any version written in a <script>, the release's own
// included: the page's JS strings use the {ver} placeholder.
func (c *checker) checkScripts(path, body string) {
	var found []string
	for _, js := range scriptBodies(body) {
		found = append(found, versionMentions(js)...)
		for _, line := range strings.Split(js, "\n") {
			if len(findVersion(line, c.ver)) > 0 {
				found = append(found, c.ver)
			}
		}
	}
	if len(found) > 0 {
		c.fail("%s <script> hardcodes %s; JS strings write %s", path, strings.Join(others(found, ""), ", "), Placeholder)
		return
	}
	c.pass("%s <script> names no version (uses %s)", path, Placeholder)
}

// containsWhole reports whether want occurs in text without the next
// characters continuing a version number.
func containsWhole(text, want string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], want)
		if j < 0 {
			return false
		}
		rest := text[i+j+len(want):]
		if !versionContinues.MatchString(rest) {
			return true
		}
		i += j + 1
	}
}

var versionContinues = regexp.MustCompile(`^(?:[0-9]|[.-][0-9A-Za-z])`)

var installHeadingRE = regexp.MustCompile(`^(#{2,4}) .*(?i:unpack|解压)`)

func (c *checker) checkInstallSection(path, text string) {
	lines := strings.Split(text, "\n")
	start, end := section(lines, installHeadingRE)
	if start < 0 {
		c.fail("%s has no install section (a heading with 'unpack' or '解压')", path)
		return
	}
	if !slices.Equal(versionMentions(lines[start]), []string{c.ver}) {
		c.fail("%s install heading %q does not name %s", path, lines[start], c.tag)
	}
	c.checkMentions(path+" install section", strings.Join(lines[start:end], "\n"), true)
}
