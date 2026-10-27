// Package config provides module-level functionality for config.
// input: every file under docs/landing and the repo VERSION file
// output: a failure when the landing page drifts from its fixed shape: extra files or sections, a version other than VERSION, a version pin outside the four allowed sites, dates, checksums, network loads, or size
// pos: allowlist contract that keeps the landing page a product page with one release version
// note: if this file changes, update this header and module README.md.
package config

import (
	"fmt"
	"html"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

// The landing page is a product page. Release history lives in docs/releases,
// CHANGELOG.md and GitHub Releases. This guard is an allowlist: the file set,
// sections, headings and element ids are fixed below, and the only version the
// page may name is VERSION, at exactly the four sites in landingVersionSites.
// Changing the page's shape means editing these lists on purpose.

const (
	landingDir          = "docs/landing"
	landingHTML         = "index.html"
	landingHTMLMaxBytes = 96 << 10  // the single page, inline CSS and JS included
	landingMaxBytes     = 700 << 10 // everything under docs/landing, screenshots included
)

// landingFiles is the complete file set under docs/landing.
var landingFiles = []string{
	"_redirects", // Cloudflare Pages: 301 /docs/images/* to /images/* (old screenshot paths)
	"images/console-dashboard.png",
	"images/swagger.png",
	"images/task-detail.png",
	landingHTML,
}

// landingIDs is every element id on the page, in order. "changelog" keeps old
// /#changelog links landing on the line that links the CHANGELOG.
var landingIDs = []string{
	"top", "i18n-root", "console", "deployment", "guarantees", "topologies", "pipeline",
	"positioning", "fit", "trust", "checklist", "changelog", "backToTop",
}

// landingSections counts <section> elements: hero, the nine id'd sections, CTA.
const landingSections = 11

// landingHeadings is the data-i18n key of every <h1>-<h6>, in order.
var landingHeadings = []string{
	"heroTitle",
	"consoleTitle", "gallery1Title", "gallery2Title", "gallery3Title",
	"deployTitle", "step1Title", "step2Title", "step3Title", "step4Title",
	"guaranteesTitle", "g1Title", "g2Title", "g3Title", "g4Title",
	"topologiesTitle", "topo1Title", "topo2Title", "topo3Title",
	"pipelineTitle", "flow1Title", "flow2Title", "flow3Title",
	"positioningTitle",
	"fitTitle", "fitGoodTitle", "fitBadTitle", "fitDecisionTitle",
	"trustTitle", "trust1Title", "trust2Title", "trust3Title", "trust4Title",
	"changelogTitle", "releaseRightTitle",
	"ctaTitle",
}

// landingSrcs is every src= value; landingLinkHrefs is every <link href>.
var (
	landingSrcs      = []string{"images/console-dashboard.png", "images/task-detail.png", "images/swagger.png"}
	landingLinkHrefs = []string{
		"https://fonts.googleapis.com",
		"https://fonts.gstatic.com",
		"https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700;800;900&family=IBM+Plex+Mono:wght@400;500;600&display=swap",
	}
)

// landingVersionSites are the only places the release version appears, outside
// HTML comments. <html data-version> is the source the script reads; the other
// three are the no-JS text the script overwrites, and release-check looks for
// the badge and CTA strings. The EN and zh strings use {ver}.
func landingVersionSites(ver string) []string {
	return []string{
		`<html lang="en" data-version="` + ver + `">`,
		`data-i18n="heroBadge">v` + ver + ` — latest release</span>`,
		`data-i18n="step1Code">VER=` + ver + ` OS=`,
		`data-i18n="ctaPrimary">Get Started with v` + ver + `</a>`,
		`.replaceAll('{ver}', version)`,
	}
}

// The version appears four times in the source (the sites above) and three
// times in the text once tags are removed (data-version is an attribute).
const (
	landingVersionsInSource = 4
	landingVersionsInText   = 3
)

// landingAddresses are the only dotted numbers with four or more parts: the
// example addresses in the quick start and production baseline snippets.
var landingAddresses = []string{"127.0.0.1", "0.0.0.0", "192.168.1.50", "10.0.0.15"}

var (
	landingCommentPattern = regexp.MustCompile(`(?s)<!--.*?(?:-->|$)`)
	landingTagPattern     = regexp.MustCompile(`(?s)<[^>]*>`)
	landingJoinPattern    = regexp.MustCompile("[\\s'\"`+]")
	// Any dotted run of three or more numbers, optional v/V, spaces allowed.
	landingDottedPattern = regexp.MustCompile(`[vV]?\d+(?:\s*\.\s*\d+){2,}`)
	landingHexPattern    = regexp.MustCompile(`(?i)[0-9a-f]{40,}`)
	landingDatePatterns  = []*regexp.Regexp{
		regexp.MustCompile(`\b(?:19|20)\d\d\s*[-/.年]\s*(?:0?[1-9]|1[0-2])\s*(?:[-/.月]|$)`),
		regexp.MustCompile(`(?i)\b(?:jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+\d{1,2}(?:st|nd|rd|th)?,?\s+(?:19|20)\d\d\b`),
		regexp.MustCompile(`(?i)\b\d{1,2}\s+(?:jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?,?\s+(?:19|20)\d\d\b`),
	}
	landingIDPattern      = regexp.MustCompile(`(?i)\bid\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	landingSrcPattern     = regexp.MustCompile(`(?i)\bsrc\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	landingLinkPattern    = regexp.MustCompile(`(?i)<link\b[^>]*>`)
	landingHrefPattern    = regexp.MustCompile(`(?i)\bhref\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	landingSectionPattern = regexp.MustCompile(`(?i)<section\b`)
	landingHeadingPattern = regexp.MustCompile(`(?i)<h[1-6]\b[^>]*>`)
	landingI18nPattern    = regexp.MustCompile(`(?i)\bdata-i18n\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	landingNetPattern     = regexp.MustCompile(`\b(?:fetch|XMLHttpRequest|sendBeacon|EventSource|WebSocket|importScripts|import)\b|(?i)@import|\burl\s*\(`)
)

func TestLandingPageIsAProductPage(t *testing.T) {
	root := repoRoot(t)
	ver := strings.TrimSpace(mustRead(t, filepath.Join(root, "VERSION")))
	assertLanding(t, os.DirFS(filepath.Join(root, landingDir)), ver)
}

func assertLanding(t testing.TB, fsys fs.FS, ver string) {
	t.Helper()
	for _, p := range checkLanding(fsys, ver) {
		t.Error(p)
	}
}

// checkLanding returns one line per rule the tree breaks.
func checkLanding(fsys fs.FS, ver string) []string {
	var problems []string
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	const why = "the landing page sells the product; release history goes to docs/releases, CHANGELOG.md and GitHub Releases"

	var files []string
	total := 0
	texts := map[string]string{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		files = append(files, p)
		total += len(b)
		if !strings.HasSuffix(p, ".png") {
			texts[p] = string(b)
		}
		return nil
	})
	if err != nil {
		return []string{fmt.Sprintf("read %s: %v", landingDir, err)}
	}
	sort.Strings(files)
	if !slices.Equal(files, landingFiles) {
		bad("%s holds %v, want exactly %v: one self-contained page plus its screenshots; add a file by listing it in landingFiles", landingDir, files, landingFiles)
	}
	if total > landingMaxBytes {
		bad("%s is %d bytes in total, over the %d-byte bound: %s", landingDir, total, landingMaxBytes, why)
	}
	page := texts[landingHTML]
	if len(page) > landingHTMLMaxBytes {
		bad("%s is %d bytes, over the %d-byte bound: %s", landingHTML, len(page), landingHTMLMaxBytes, why)
	}

	inSource, inText := 0, 0
	for _, name := range sortedKeys(texts) {
		raw := texts[name]
		// Text with tags removed, entities decoded, and whitespace, quotes and
		// + dropped, so values split across lines, tags or JS string concatenation
		// read as one run.
		joined := landingJoinPattern.ReplaceAllString(html.UnescapeString(landingTagPattern.ReplaceAllString(raw, "")), "")
		for _, view := range []struct {
			label, text string
			count       *int
		}{{"source", raw, &inSource}, {"text", joined, &inText}} {
			for _, m := range landingDottedPattern.FindAllString(view.text, -1) {
				tok := strings.TrimLeft(strings.Join(strings.Fields(m), ""), "vV")
				switch {
				case tok == ver:
					*view.count++
				case strings.Count(tok, ".") >= 3 && slices.Contains(landingAddresses, tok):
				default:
					bad("%s (%s) names %q; the only version on the landing page is VERSION %s, and the only addresses are %v: %s", name, view.label, m, ver, landingAddresses, why)
				}
			}
			if m := landingHexPattern.FindString(view.text); m != "" {
				bad("%s (%s) holds a %d-character hex run %.16s…; checksums belong in checksums.txt, which the install steps verify", name, view.label, len(m), m)
			}
			if m := landingNetPattern.FindString(view.text); m != "" {
				bad("%s (%s) loads something at runtime (%q); the page loads nothing beyond its screenshots and fonts", name, view.label, m)
			}
		}
		for _, re := range landingDatePatterns {
			if m := re.FindString(raw); m != "" {
				bad("%s has a date (%q); dated entries are release history: %s", name, m, why)
			}
		}
	}
	if inSource != landingVersionsInSource {
		bad("the landing source names VERSION %s %d times, want %d (the sites in landingVersionSites); EN and zh strings use {ver}", ver, inSource, landingVersionsInSource)
	}
	if inText != landingVersionsInText {
		bad("the landing text names VERSION %s %d times, want %d; a section about the current release is still release history", ver, inText, landingVersionsInText)
	}

	visible := landingCommentPattern.ReplaceAllString(page, "")
	for _, site := range landingVersionSites(ver) {
		if !strings.Contains(visible, site) {
			bad("%s lacks %q outside HTML comments; set the version in these sites only", landingHTML, site)
		}
	}
	if ids := attrValues(landingIDPattern, page); !slices.Equal(ids, landingIDs) {
		bad("%s element ids are %v, want %v; add a section by listing it in landingIDs", landingHTML, ids, landingIDs)
	}
	if n := len(landingSectionPattern.FindAllString(page, -1)); n != landingSections {
		bad("%s has %d <section> elements, want %d; add a section by changing landingSections", landingHTML, n, landingSections)
	}
	var headings []string
	for _, tag := range landingHeadingPattern.FindAllString(page, -1) {
		key := attrValues(landingI18nPattern, tag)
		headings = append(headings, strings.Join(key, ","))
	}
	if !slices.Equal(headings, landingHeadings) {
		bad("%s headings (data-i18n keys) are %v, want %v; add a heading by listing it in landingHeadings", landingHTML, headings, landingHeadings)
	}
	for _, src := range attrValues(landingSrcPattern, page) {
		if !slices.Contains(landingSrcs, src) {
			bad("%s loads src=%q; only the screenshots %v may load", landingHTML, src, landingSrcs)
		}
	}
	for _, tag := range landingLinkPattern.FindAllString(page, -1) {
		hrefs := attrValues(landingHrefPattern, tag)
		if len(hrefs) != 1 || !slices.Contains(landingLinkHrefs, hrefs[0]) {
			bad("%s has %s; only the font links %v may load", landingHTML, tag, landingLinkHrefs)
		}
	}
	return problems
}

func attrValues(re *regexp.Regexp, s string) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1]+m[2]+m[3])
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestLandingGuardRejects feeds checkLanding the real tree with one edit each:
// the 24 regrowth paths QA found on #565 r1 (B*, H*, X*, V*, F*), then one case
// per rule so every rule has a case only it explains.
func TestLandingGuardRejects(t *testing.T) {
	root := repoRoot(t)
	ver := strings.TrimSpace(mustRead(t, filepath.Join(root, "VERSION")))
	base := fstest.MapFS{}
	dir := filepath.Join(root, landingDir)
	if err := fs.WalkDir(os.DirFS(dir), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(filepath.Join(dir, p))
		base[p] = &fstest.MapFile{Data: b}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := checkLanding(base, ver); len(got) != 0 {
		t.Fatalf("the unedited tree must pass: %v", got)
	}

	parts := strings.Split(ver, ".")
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	next := fmt.Sprintf("%s.%s.%d", parts[0], parts[1], patch+1)
	const hex = "24c9d9cc572607bfb9fb21c24fac546e4748723010fa35c733777c79adce8c8d"
	const anchor = `<section id="checklist"`

	type edit func(t *testing.T, m fstest.MapFS)
	replace := func(old, new string) edit {
		return func(t *testing.T, m fstest.MapFS) {
			page := string(m[landingHTML].Data)
			if strings.Count(page, old) != 1 {
				t.Fatalf("%q appears %d times, want 1", old, strings.Count(page, old))
			}
			m[landingHTML] = &fstest.MapFile{Data: []byte(strings.Replace(page, old, new, 1))}
		}
	}
	inject := func(s string) edit { return replace(anchor, s+"\n    "+anchor) }
	put := func(p string, data string) edit {
		return func(t *testing.T, m fstest.MapFS) { m[p] = &fstest.MapFile{Data: []byte(data)} }
	}
	drop := func(p string) edit {
		return func(t *testing.T, m fstest.MapFS) { delete(m, p) }
	}
	bump := func(skip string) edit {
		return func(t *testing.T, m fstest.MapFS) {
			page := string(m[landingHTML].Data)
			if skip != "" {
				page = strings.Replace(page, skip+ver, skip+"@KEEP@", 1)
			}
			page = strings.ReplaceAll(strings.ReplaceAll(page, ver, next), "@KEEP@", ver)
			m[landingHTML] = &fstest.MapFile{Data: []byte(page)}
		}
	}

	cases := []struct {
		name string
		edit edit
		ver  string // defaults to VERSION
		want string
	}{
		// QA #565 r1 regrowth paths.
		{"B1 renamed timeline section", inject(`<section id="history"><h3>Release History</h3><ol><li><span class="milestone-date">Oct 2026</span> health metrics</li></ol></section>`), "", "element ids are"},
		{"B2 table with bare versions", inject(`<table><tr><td>0.5.82</td><td>webhooks</td></tr></table>`), "", `names "0.5.82"`},
		{"B3 spaced version", inject(`<p>v0 . 5 . 82 added webhooks</p>`), "", `names "v0 . 5 . 82"`},
		{"B4 uppercase V", inject(`<p>V0.5.82 added webhooks</p>`), "", `names "V0.5.82"`},
		{"B5 release 0.5.82", inject(`<p>Since release 0.5.82 the webhook ships.</p>`), "", `names "0.5.82"`},
		{"B6 archive name with underscore", inject(`<p>binlog-server_v0.5.82</p>`), "", `names "v0.5.82"`},
		{"B8 month-name date", inject(`<div class="timeline-date">Oct 27, 2026</div><div>webhooks</div>`), "", `has a date ("Oct 27, 2026")`},
		{"B9 id before class, ISO date", inject(`<div id="t1" class='timeline-date'>2026-10-27</div>`), "", `has a date ("2026-10-`},
		{"B10 id='changelog' single quotes", inject(`<section id='releases'></section>`), "", "element ids are"},
		{"B12 Latest Releases heading", inject(`<h3>Latest Releases</h3>`), "", "headings (data-i18n keys)"},
		{"H1 uppercase hex", inject(`<pre>` + strings.ToUpper(hex) + `  x.tar.gz</pre>`), "", "64-character hex run"},
		{"H2 hex split across lines", inject("<pre>" + hex[:32] + "\n" + hex[32:] + "</pre>"), "", "(text) holds a"},
		{"H3 hex split across tags", inject(`<code>` + hex[:32] + `</code><code>` + hex[32:] + `</code>`), "", "(text) holds a"},
		{"H4 hex built in JS", inject(`<script>var s='` + hex[:32] + `'+'` + hex[32:] + `';</script>`), "", "(text) holds a"},
		{"H5 sha1", inject(`<pre>da39a3ee5e6b4b0d3255bfef95601890afd80709  x.tar.gz</pre>`), "", "40-character hex run"},
		{"H6 sha512", inject(`<pre>` + hex + hex + `</pre>`), "", "128-character hex run"},
		{"H7 hex after underscore", inject(`<pre>sha_` + hex + `</pre>`), "", "64-character hex run"},
		{"H8 fetch checksums.txt", inject(`<script>fetch('https://github.com/Fanduzi/BinlogServer/releases/download/v` + ver + `/checksums.txt')</script>`), "", `loads something at runtime ("fetch")`},
		{"X1 external script", inject(`<script src="history.js"></script>`), "", `loads src="history.js"`},
		{"X2 external stylesheet", inject(`<link rel="stylesheet" href="landing.css">`), "", "only the font links"},
		{"V1 current rc", inject(`<p>v` + ver + `-rc1</p>`), "", "source names VERSION " + ver + " 5 times"},
		{"V2 stale VER= pin", replace("VER="+ver+" OS=", "VER=0.5.82 OS="), "", `names "0.5.82"`},
		{"F1 second HTML page", put("releases.html", `<h1>Releases</h1>`), "", "want exactly"},
		{"F2 external history.js", put("history.js", "var r=['v0.5.82'];/*"+strings.Repeat("x", 200000)+"*/"), "", "want exactly"},
		{"F3 whats new in the current version", inject(`<div><b>What's new in v` + ver + `</b><ul><li>Webhooks</li></ul></div>`), "", "text names VERSION " + ver + " 4 times"},

		// One case per rule.
		{"file set: _redirects removed", drop("_redirects"), "", "want exactly"},
		{"size: page over 96 KiB", inject("<!--" + strings.Repeat("x", 20000) + "-->"), "", "bytes, over the 98304-byte bound"},
		{"size: tree over 700 KiB", put("images/swagger.png", strings.Repeat("x", 300000)), "", "bytes in total"},
		{"version: four-part non-address", inject(`<p>` + ver + `.1</p>`), "", `names "` + ver + `.1"`},
		{"version: old version in a comment", inject(`<!-- v0.5.82 -->`), "", `(source) names "v0.5.82"`},
		{"version: old version in an attribute", inject(`<div data-v="v0.5.82"></div>`), "", `(source) names "v0.5.82"`},
		{"version: old version split by a tag", inject(`<p>0.<b>5</b>.82</p>`), "", `(text) names "0.5.82"`},
		{"version: old version as entities", inject(`<p>&#48;.5.82</p>`), "", `(text) names "0.5.82"`},
		{"count: current version in an attribute", inject(`<div data-v="` + ver + `"></div>`), "", "source names VERSION " + ver + " 5 times"},
		{"count: current version split by a tag", inject(`<p>v` + strings.Replace(ver, ".", ".<b></b>", 1) + `</p>`), "", "text names VERSION " + ver + " 4 times"},
		{"count: zh string hardcodes the version", replace("开始使用 v{ver}", "开始使用 v"+ver), "", "source names VERSION " + ver + " 5 times"},
		{"site: CTA only in a comment", replace(`<a href="#deployment" class="btn-primary" data-i18n="ctaPrimary">Get Started with v`+ver+`</a>`, `<!-- <a href="#deployment" class="btn-primary" data-i18n="ctaPrimary">Get Started with v`+ver+`</a> -->`), "", `lacks "data-i18n=\"ctaPrimary\">Get Started with v` + ver},
		{"site: CTA version moved elsewhere", replace(`>Get Started with v`+ver+`<`, `>Get Started<`+`/a><p>v`+ver+`</p><a hidden>`), "", `lacks "data-i18n=\"ctaPrimary\">Get Started with v` + ver},
		{"site: {ver} never substituted", replace(".replaceAll('{ver}', version)", ""), "", "lacks \".replaceAll('{ver}', version)\""},
		{"count: data-version removed", replace(` data-version="`+ver+`"`, ""), "", "source names VERSION " + ver + " 3 times"},
		{"count: CTA text in a comment", replace(`>Get Started with v`+ver+`<`, `><!-- Get Started with v`+ver+` -->Get Started<`), "", "text names VERSION " + ver + " 2 times"},
		{"sections: one removed", replace(`<section class="cta-section">`, `<div class="cta-section">`), "", "has 10 <section> elements"},
		{"ids: extra id", inject(`<div id="t1"></div>`), "", "element ids are"},
		{"sections: section without id", inject(`<section class="news"><p>soon</p></section>`), "", "<section> elements"},
		{"sections: uppercase tag", inject(`<SECTION ID="releases"></SECTION>`), "", "<section> elements"},
		{"headings: unkeyed h4", inject(`<h4>History</h4>`), "", "headings (data-i18n keys)"},
		{"dates: day first", inject(`<p>27 Oct 2026</p>`), "", `has a date ("27 Oct 2026")`},
		{"dates: zh", inject(`<p>2026年10月27日</p>`), "", `has a date ("2026年10月`},
		{"runtime: fetch via string concat", inject(`<script>window['fe'+'tch']('x')</script>`), "", `(text) loads something at runtime ("fetch")`},
		{"runtime: css url()", inject(`<style>.x{background:url(h.png)}</style>`), "", "loads something at runtime"},
		{"runtime: css @IMPORT in capitals", inject(`<style>@IMPORT "h.css";</style>`), "", "loads something at runtime"},
		{"runtime: dynamic import", inject(`<script>import('./history.js')</script>`), "", `loads something at runtime ("import")`},
		{"src: iframe", inject(`<iframe src='https://example.com/releases'></iframe>`), "", `loads src="https://example.com/releases"`},

		// A release bump: all four sites move together, or the guard fails.
		{"bump: VER= left behind (#567)", bump("VER="), next, `names "` + ver + `"`},
		{"bump: data-version left behind", bump(`data-version="`), next, `names "` + ver + `"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := fstest.MapFS{}
			for k, v := range base {
				m[k] = v
			}
			tc.edit(t, m)
			v := tc.ver
			if v == "" {
				v = ver
			}
			got := checkLanding(m, v)
			for _, p := range got {
				if strings.Contains(p, tc.want) {
					return
				}
			}
			t.Errorf("want a problem containing %q, got %q", tc.want, got)
		})
	}

	t.Run("a failing tree fails the on-disk test", func(t *testing.T) {
		m := fstest.MapFS{}
		for k, v := range base {
			m[k] = v
		}
		drop("_redirects")(t, m)
		r := &recordingTB{TB: t}
		assertLanding(r, m, ver)
		if !r.failed {
			t.Error("assertLanding did not report a broken tree")
		}
	})

	t.Run("bump: all four sites", func(t *testing.T) {
		m := fstest.MapFS{}
		for k, v := range base {
			m[k] = v
		}
		bump("")(t, m)
		if got := checkLanding(m, next); len(got) != 0 {
			t.Errorf("a complete bump to %s must pass: %v", next, got)
		}
	})
}

// recordingTB records Error calls so a test can assert that assertLanding fails.
type recordingTB struct {
	testing.TB
	failed bool
}

func (r *recordingTB) Error(args ...any) { r.failed = true }
