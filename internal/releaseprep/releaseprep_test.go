package releaseprep

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type mapSource map[string]string

func (m mapSource) Files() (map[string]string, []string, error) { return m, nil, nil }

func (m mapSource) Read(path string) (string, bool, error) {
	body, ok := m[path]
	return body, ok, nil
}

// Multi-digit versions catch string ordering, prefix and unescaped-dot bugs.
var versions = []struct{ ver, prev string }{
	{"0.5.100", "0.5.99"},
	{"0.10.0", "0.9.12"},
	{"0.6.0", "0.5.83"}, // minor bump: a leftover previous-minor version still fails
}

// published is a clone where every tag is in the release's history and has a
// published GitHub release.
func published(tags ...string) Releases { return Releases{Merged: tags, Published: tags} }

func ptr(r Releases) *Releases { return &r }

// checkSurfacesOnly applies only the Surfaces manifest, without the notes,
// the CHANGELOG or the stale scan.
func checkSurfacesOnly(src Source, ver string) ([]Finding, error) {
	c := &checker{src: src, tag: "v" + ver, ver: ver}
	c.checkSurfaces(Surfaces)
	return c.out, c.err
}

func run(t *testing.T, files map[string]string, tags []string, tag string) ([]Finding, string) {
	t.Helper()
	return runRel(t, files, published(tags...), tag)
}

func runRel(t *testing.T, files map[string]string, rel Releases, tag string) ([]Finding, string) {
	t.Helper()
	findings, err := Check(mapSource(files), &rel, tag)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	var fails []string
	for _, f := range findings {
		if !f.OK {
			fails = append(fails, f.Msg)
		}
	}
	return findings, strings.Join(fails, "\n")
}

func TestCompleteReleasePasses(t *testing.T) {
	for _, v := range versions {
		files := Fixture(v.ver, v.prev)
		findings, fails := run(t, files, []string{"v" + v.prev, "v0.1.0", "v" + v.ver + "-rc1"}, "v"+v.ver)
		if fails != "" {
			t.Fatalf("v%s: complete release failed:\n%s", v.ver, fails)
		}
		if len(findings) < 15 {
			t.Fatalf("v%s: only %d checks ran", v.ver, len(findings))
		}
		// A tag run sees its own tag too.
		if _, fails := run(t, files, []string{"v" + v.prev, "v" + v.ver}, "v"+v.ver); fails != "" {
			t.Fatalf("v%s at its own tag:\n%s", v.ver, fails)
		}
	}
}

// edit changes one file of a complete fixture.
type edit func(f map[string]string, ver, prev string)

func set(path, body string) edit {
	return func(f map[string]string, ver, prev string) {
		r := strings.NewReplacer("@V@", ver, "@P@", prev)
		f[r.Replace(path)] = r.Replace(body)
	}
}

func replace(path, old, new string) edit {
	return func(f map[string]string, ver, prev string) {
		r := strings.NewReplacer("@V@", ver, "@P@", prev)
		o, n := r.Replace(old), r.Replace(new)
		path := r.Replace(path)
		if !strings.Contains(f[path], o) {
			panic("fixture " + path + " lacks " + o)
		}
		f[path] = strings.Replace(f[path], o, n, 1)
	}
}

func del(path string) edit {
	return func(f map[string]string, _, _ string) { delete(f, path) }
}

func TestIncompleteReleaseFails(t *testing.T) {
	en, zh := "docs/releases/release-notes-v@V@.md", "docs/releases/v@V@.zh-CN.md"
	cases := []struct {
		name string
		edit edit
		want string
	}{
		// Every surface must exist.
		{"no English notes", delNotes(false), "release-notes-v@V@.md missing"},
		{"no Chinese notes", delNotes(true), "v@V@.zh-CN.md missing"},
		{"no CHANGELOG", del(changelogPath), "CHANGELOG.md missing"},
		{"no README", del("README.md"), "README.md missing"},
		{"no README_ZH", del("README_ZH.md"), "README_ZH.md missing"},
		{"no deployment guide", del(deployPath), "deployment.md missing"},
		{"no VERSION", del(versionPath), "VERSION missing"},
		{"no landing page", del(landingPath), "index.html missing"},

		// Release notes: exact title on the first line, a body, zh in Chinese.
		{"English title names the previous version", replace(en, "# Binlog Server v@V@", "# Binlog Server v@P@"), "must open with a title"},
		{"Chinese title names the previous version", replace(zh, "# Binlog Server v@V@ 中文", "# Binlog Server v@P@ 中文"), "must open with a title"},
		{"English title names a longer version", replace(en, "# Binlog Server v@V@", "# Binlog Server v@V@0"), "must open with a title"},
		{"English title names an extra segment", replace(en, "# Binlog Server v@V@", "# Binlog Server v@V@.1"), "must open with a title"},
		{"Chinese title names an rc", replace(zh, "v@V@ 中文", "v@V@-rc1 中文"), "must open with a title"},
		{"Chinese title names two versions", replace(zh, "v@V@ 中文", "v@V@ / v@P@ 中文"), "must open with a title"},
		{"English title for another product", replace(en, "# Binlog Server v@V@", "# Binlog Viz v@V@"), "must open with a title"},
		{"Chinese title is not a heading", replace(zh, "# Binlog Server", "Binlog Server"), "must open with a title"},
		{"English title below the first line", replace(en, "# Binlog Server", "Draft\n# Binlog Server"), "must open with a title"},
		{"English title only in a comment", replace(en, "# Binlog Server v@V@", "<!-- # Binlog Server v@V@ -->\n# Binlog Server v@P@"), "must open with a title"},
		{"English notes are only a title", set(en, "# Binlog Server v@V@\n"), "have no body"},
		{"Chinese notes are only a title", set(zh, "# v@V@\n"), "have no body"},
		{"English body only in a comment", set(en, "# Binlog Server v@V@\n\n<!-- v@V@ keeps every sealed segment verified before upload and fixes a retention marker. -->\n"), "have no body"},
		{"English body is only headings", set(en, "# Binlog Server v@V@\n\n## Fixed\n\n## Added because the retention marker and every sealed segment were fixed here\n"), "have no body"},
		{"Chinese notes written in English", set(zh, "# Binlog Server v@V@\n\nv@V@ keeps every sealed segment verified before upload and fixes a retention marker after migrate up.\n"), "not written in Chinese"},
		{"Chinese only in the title", set(zh, "# Binlog Server v@V@ 中文发布说明中文发布说明中文发布说明中文发布说明\n\nv@V@ keeps every sealed segment verified before upload and fixes a retention marker.\n"), "not written in Chinese"},

		// CHANGELOG: an anchored, exact heading with a body, outside comments and fences.
		{"CHANGELOG without the section", replace(changelogPath, "## [v@V@] - 2026-01-01", "## [v1.2.3] - 2026-01-01"), "has no '## [v@V@]' section"},
		{"CHANGELOG names it only in a bullet", replace(changelogPath, "## [v@V@] - 2026-01-01", "- see [v@V@] - 2026-01-01"), "has no '## [v@V@]' section"},
		{"CHANGELOG heading for a longer version", replace(changelogPath, "## [v@V@]", "## [v@V@0]"), "has no '## [v@V@]' section"},
		{"CHANGELOG plain heading for a longer version", replace(changelogPath, "## [v@V@]", "## v@V@0"), "has no '## [v@V@]' section"},
		{"CHANGELOG heading with dots replaced", replace(changelogPath, "## [v@V@]", "## [v"+"@D@"+"]"), "has no '## [v@V@]' section"},
		{"CHANGELOG heading one level down", replace(changelogPath, "## [v@V@]", "### [v@V@]"), "has no '## [v@V@]' section"},
		{"CHANGELOG heading in a comment", replace(changelogPath, "## [v@V@] - 2026-01-01", "<!--\n## [v@V@] - 2026-01-01\n-->"), "has no '## [v@V@]' section"},
		{"CHANGELOG heading in a code fence", replace(changelogPath, "## [v@V@] - 2026-01-01", "```\n## [v@V@] - 2026-01-01\n```"), "has no '## [v@V@]' section"},
		{"CHANGELOG heading with no body", replace(changelogPath, "### Fixed\n\n- retention marker\n\n", ""), "section for v@V@ is empty"},
		{"CHANGELOG heading last and empty", set(changelogPath, "# Changelog\n\n## [v@V@] - 2026-01-01\n\n"), "section for v@V@ is empty"},
		{"CHANGELOG body only in a comment", replace(changelogPath, "### Fixed\n\n- retention marker\n\n", "<!-- - retention marker -->\n\n"), "section for v@V@ is empty"},

		// README pins: every VER= form must equal the version exactly.
		{"stale README pin", replace("README.md", "VER=@V@", "VER=@P@"), "README.md pins @P@ instead"},
		{"stale README_ZH pin", replace("README_ZH.md", "VER=@V@", "VER=@P@"), "README_ZH.md pins @P@ instead"},
		{"double-quoted stale pin", replace("README.md", "VER=@V@", `VER="@P@"`), "pins @P@ instead"},
		{"single-quoted stale pin", replace("README.md", "VER=@V@", `VER='@P@'`), "pins @P@ instead"},
		{"v-prefixed pin", replace("README.md", "VER=@V@", "VER=v@V@"), "pins v@V@ instead"},
		{"pin with an extra segment", replace("README.md", "VER=@V@", "VER=@V@.1"), "pins @V@.1 instead"},
		{"rc pin", replace("README.md", "VER=@V@", "VER=@V@-rc1"), "pins @V@-rc1 instead"},
		{"pin of a longer version", replace("README.md", "VER=@V@", "VER=@V@0"), "pins @V@0 instead"},
		{"pin of a version prefix", replace("README.md", "VER=@V@", "VER=@T@"), "instead of VER=@V@"},
		{"stale pin before a good one", replace("README.md", "VER=@V@\n", "VER=@P@\nVER=@V@\n"), "pins @P@ instead"},
		{"stale pin after a good one", replace("README.md", "### 2. Run it", "`VER=@P@`\n\n### 2. Run it"), "pins @P@ instead"},
		{"README without a pin", replace("README.md", "VER=@V@\n", ""), "README.md has no visible install pin"},
		{"README pin only in a comment", replace("README.md", "VER=@V@\n", "<!-- VER=@V@ -->\n"), "README.md has no visible install pin"},
		{"stale deployment guide pin", set(deployPath, "VER=@P@\n"), "deployment.md pins @P@"},

		// README install section: heading, checksums and archive names.
		{"install heading names the previous version", replace("README.md", "unpack v@V@", "unpack v@P@"), "install heading"},
		{"zh install heading names the previous version", replace("README_ZH.md", "解压 v@V@", "解压 v@P@"), "README_ZH.md install heading"},
		{"checksum line names the previous archive", replace("README.md", "  binlog-server_@V@_linux", "  binlog-server_@P@_linux"), "install section names @P@ instead of @V@ (still the previous release v@P@)"},
		{"zh checksum line names the previous archive", replace("README_ZH.md", "  binlog-server_@V@_linux", "  binlog-server_@P@_linux"), "README_ZH.md install section names @P@"},
		{"archive tree names the previous version", replace("README.md", "binlog-server_@V@_linux_amd64/", "binlog-server_@P@_linux_amd64/"), "install section names @P@"},
		{"checksums sentence names the previous version", replace("README.md", "Published `v@V@`", "Published `v@P@`"), "install section names @P@"},
		{"stale archive after a shell comment in a fence", replace("README.md", "# fetch the archive\n", "# fetch the archive\n# binlog-server_@P@_linux_amd64.tar.gz\n"), "install section names @P@"},
		{"no install section", replace("README.md", "### 1. Download, verify, and unpack v@V@", "### 1. Download v@V@"), "has no install section"},

		// VERSION is exactly one line.
		{"stale VERSION", set(versionPath, "@P@\n"), "VERSION must be exactly one line"},
		{"VERSION with two lines", set(versionPath, "@P@\n@V@\n"), "VERSION must be exactly one line"},
		{"VERSION with the version first of two lines", set(versionPath, "@V@\n@P@\n"), "VERSION must be exactly one line"},
		{"VERSION with a v prefix", set(versionPath, "v@V@\n"), "VERSION must be exactly one line"},
		{"VERSION of a longer version", set(versionPath, "@V@0\n"), "VERSION must be exactly one line"},
		{"VERSION with a trailing blank line", set(versionPath, "@V@\n\n"), "VERSION must be exactly one line"},

		// Landing page: the two required strings, visibly, and no other version.
		{"landing badge names the previous version", replace(landingPath, ">v@V@ — latest release", ">v@P@ — latest release"), `does not show "v@V@ — latest release"`},
		{"landing CTA names the previous version", replace(landingPath, "Get Started with v@V@", "Get Started with v@P@"), `does not show "Get Started with v@V@"`},
		{"landing badge only in a comment", replace(landingPath, `<span class="tag tag-new" data-i18n="heroBadge">v@V@ — latest release</span>`, "<!-- v@V@ — latest release -->"), `does not show "v@V@ — latest release"`},
		{"landing changed only by a comment", setLandingStaleWithComment(), `does not show "v@V@ — latest release"`},
		{"landing badge names a longer version", replace(landingPath, ">v@V@ — latest", ">v@V@0 — latest"), `does not show "v@V@ — latest release"`},
		{"landing CTA names a longer version", replace(landingPath, "Get Started with v@V@", "Get Started with v@V@0"), `does not show "Get Started with v@V@"`},
		{"landing CTA names an extra segment", replace(landingPath, "Get Started with v@V@", "Get Started with v@V@.1"), `does not show "Get Started with v@V@"`},
		{"landing CTA names an rc", replace(landingPath, "Get Started with v@V@", "Get Started with v@V@-rc1"), `does not show "Get Started with v@V@"`},
		{"landing data-version names the previous version", replace(landingPath, `data-version="@V@"`, `data-version="@P@"`), `<html data-version="@P@"> must be "@V@"`},
		{"landing data-version names a longer version", replace(landingPath, `data-version="@V@"`, `data-version="@V@0"`), `<html data-version="@V@0"> must be "@V@"`},
		{"landing data-version names an extra segment", replace(landingPath, `data-version="@V@"`, `data-version="@V@.1"`), `<html data-version="@V@.1"> must be "@V@"`},
		{"landing without data-version", replace(landingPath, ` data-version="@V@"`, ""), `<html data-version=""> must be "@V@"`},
		{"landing data-version only in a comment", replace(landingPath, `<html lang="en" data-version="@V@">`, `<html lang="en"><!-- data-version="@V@" -->`), `<html data-version=""> must be "@V@"`},
		{"landing pins the previous version", replace(landingPath, "VER=@V@ OS", "VER=@P@ OS"), "index.html pins @P@ instead"},
		{"landing without a pin", replace(landingPath, "VER=@V@ OS", "OS"), "index.html has no visible install pin"},

		// Config examples: the header names this version and no other.
		{"config example names the previous version", replace("config.example.yaml", "BinlogServer v@V@", "BinlogServer v@P@"), `config.example.yaml does not show "BinlogServer v@V@"`},
		{"config example names a longer version", replace("config.example.yaml", "BinlogServer v@V@", "BinlogServer v@V@0"), `config.example.yaml does not show "BinlogServer v@V@"`},
		{"production example names the previous version", replace("config.production.example.yaml", "BinlogServer v@V@", "BinlogServer v@P@"), `config.production.example.yaml does not show "BinlogServer v@V@"`},
		{"production example also names the previous version", replace("config.production.example.yaml", "listen_addr", "# upgraded from v@P@\nlisten_addr"), "config.production.example.yaml names @P@ instead"},
		{"config example names an rc elsewhere", replace("config.example.yaml", "listen_addr", "# v@V@-rc1\nlisten_addr"), "config.example.yaml names @V@-rc1 instead"},
		{"config example names an extra segment elsewhere", replace("config.example.yaml", "listen_addr", "# v@V@.1\nlisten_addr"), "config.example.yaml names @V@.1 instead"},
		{"config example pins the previous version", replace("config.example.yaml", "listen_addr", "# VER=@P@\nlisten_addr"), "config.example.yaml names @P@ instead"},
		{"no config example", del("config.example.yaml"), "config.example.yaml missing"},
		{"no production example", del("config.production.example.yaml"), "config.production.example.yaml missing"},
		{"landing unclosed comment hides the CTA", replace(landingPath, "<a href", "<!-- <a href"), `does not show "Get Started with v@V@"`},

		// r2 mutation survivors (QA issuecomment-6442026786 P2-4).
		{"CHANGELOG heading in a ~~~ fence", replace(changelogPath, "## [v@V@] - 2026-01-01", "~~~\n## [v@V@] - 2026-01-01\n~~~"), "has no '## [v@V@]' section"},
		{"~~~ fence in the install section hides no heading", replace("README.md", "# fetch the archive\n", "# fetch the archive\n```\n\n~~~\n## not a heading\n~~~\n\n```text\nbinlog-server_0.0.1_linux_amd64.tar.gz\n"), "README.md install section names 0.0.1"},
		{"empty VER= pin", replace("README.md", "VER=@V@\n", "VER=@V@\nVER=\n"), "README.md pins  instead of VER=@V@"},
		{"empty quoted VER= pin", replace("README.md", "VER=@V@\n", "VER=@V@\nVER=\"\"\n"), "README.md pins  instead of VER=@V@"},
		{"empty landing VER= pin", replace(landingPath, "VER=@V@ OS", "VER=@V@ OS\nVER= OS"), "index.html pins  instead of VER=@V@"},
		{"placeholder in another case is a pin", replace(landingPath, "VER=@V@ OS", "VER=@V@ OS\nVER={VER} OS"), "index.html pins {VER} instead"},
		{"placeholder with a suffix is a pin", replace(landingPath, "VER=@V@ OS", "VER=@V@ OS\nVER={ver}1 OS"), "index.html pins {ver}1 instead"},
		{"longer placeholder is a pin", replace(landingPath, "VER=@V@ OS", "VER=@V@ OS\nVER={version} OS"), "index.html pins {version} instead"},
		{"install heading at h5", replace("README.md", "### 1. Download, verify, and unpack v@V@", "##### 1. Download, verify, and unpack v@V@"), "README.md has no install section"},
		{"install heading at h1", replace("README.md", "### 1. Download, verify, and unpack v@V@", "# 1. Download, verify, and unpack v@V@"), "README.md has no install section"},
		{"install heading at h6", replace("README_ZH.md", "### 1. 下载、校验并解压 v@V@", "###### 1. 下载、校验并解压 v@V@"), "README_ZH.md has no install section"},

		// Comments, <template>, <script>, <style> and tag attributes (<meta
		// content>) are not required sites; CSS or attribute hiding is out of
		// scope (TestVisualHidingIsOutOfScope).
		{"badge in a template", hideBadge(`<template>%s</template>`), `does not show "v@V@ — latest release"`},
		{"badge in an unclosed template", hideBadge(`<template>%s`), `does not show "v@V@ — latest release"`},
		{"badge in a script comment", hideBadge(`<script>/* %s */</script>`), `does not show "v@V@ — latest release"`},
		{"badge in a script string", hideBadge(`<script type="module">const s = '%s';</script>`), `does not show "v@V@ — latest release"`},
		{"badge in a style comment", hideBadge(`<STYLE>/* %s */</STYLE>`), `does not show "v@V@ — latest release"`},
		{"badge in meta content", hideBadge(`<meta name="description" content="%s">`), `does not show "v@V@ — latest release"`},
		{"badge in a title attribute", hideBadge(`<span title="%s">x</span>`), `does not show "v@V@ — latest release"`},
		{"badge in an HTML comment", hideBadge(`<!-- %s -->`), `does not show "v@V@ — latest release"`},
		{"badge split across blocks", hideBadge(`<p>v@V@</p><p> — latest release</p>%.0s`), `does not show "v@V@ — latest release"`},
		{"pin only in a script", replace(landingPath, `<code data-i18n="step1Code">VER=@V@ OS`, `<script>const c = 'VER=@V@';</script><code data-i18n="step1Code">OS`), "index.html has no visible install pin"},
		{"README pin only in a style", replace("README.md", "VER=@V@\n", "<style>/* VER=@V@ */</style>\n"), "README.md has no visible install pin"},
		{"data-version only on a commented html tag", replace(landingPath, `<html lang="en" data-version="@V@">`, `<!-- <html data-version="@V@"> --><html lang="en">`), `<html data-version=""> must be "@V@"`},
		{"data-version only on another element", replace(landingPath, `<html lang="en" data-version="@V@">`, `<x-v data-version="@V@"></x-v><html lang="en">`), `<html data-version=""> must be "@V@"`},
		{"data-version only on a later html tag", replace(landingPath, `<html lang="en" data-version="@V@">`, `<html lang="en"><body><html data-version="@V@">`), `<html data-version=""> must be "@V@"`},
		{"config header without the version", replace("config.example.yaml", "# BinlogServer v@V@ Comprehensive", "# BinlogServer Comprehensive"), `config.example.yaml does not show "BinlogServer v@V@"`},
		{"badge names the previous version", replace(landingPath, "v@V@ — latest release", "v@P@ — latest release"), `names the previous release @P@`},
	}
	for _, v := range versions {
		for _, tc := range cases {
			t.Run(v.ver+"/"+tc.name, func(t *testing.T) {
				files := Fixture(v.ver, v.prev)
				r := strings.NewReplacer("@V@", v.ver, "@P@", v.prev, "@D@", strings.ReplaceAll(v.ver, ".", "-"), "@T@", v.ver[:len(v.ver)-1])
				tc.edit(files, v.ver, v.prev)
				for k, body := range files {
					files[k] = r.Replace(body)
				}
				_, fails := run(t, files, []string{"v" + v.prev}, "v"+v.ver)
				want := r.Replace(tc.want)
				if !strings.Contains(fails, want) {
					t.Fatalf("want a failure containing %q, got:\n%s", want, fails)
				}
			})
		}
	}
}

// hideBadge removes the visible landing badge and puts the badge text inside
// the given markup (a %s verb).
func hideBadge(markup string) edit {
	return func(f map[string]string, ver, prev string) {
		badge := "v" + ver + " — latest release"
		visible := `<span class="tag tag-new" data-i18n="heroBadge">` + badge + `</span>`
		if !strings.Contains(f[landingPath], visible) {
			panic("fixture landing lacks the badge")
		}
		f[landingPath] = strings.Replace(f[landingPath], visible, fmt.Sprintf(strings.ReplaceAll(markup, "@V@", ver), badge), 1)
	}
}

func delNotes(zh bool) edit {
	return func(f map[string]string, ver, _ string) {
		if zh {
			delete(f, notesZH("v"+ver))
		} else {
			delete(f, notesEN("v"+ver))
		}
	}
}

// The QA case: the page still shows the previous release; only a comment names the new one.
func setLandingStaleWithComment() edit {
	return func(f map[string]string, ver, prev string) {
		f[landingPath] = "<!-- v" + ver + " — latest release Get Started with v" + ver + " -->\n" + strings.ReplaceAll(f[landingPath], ver, prev)
	}
}

// Positive checks ignore comments and older releases; the stale scan reads
// comments too (a stale copy in a comment is still a stale copy), so the
// comments here name a release older than the previous one.
func TestCommentsAndHistoryDoNotFail(t *testing.T) {
	const old = "0.0.1"
	for _, v := range versions {
		files := Fixture(v.ver, v.prev)
		files["README.md"] += "\n<!-- VER=" + old + " binlog-server_" + old + "_linux -->\n"
		files[landingPath] += "<!-- v" + old + " — latest release -->\n"
		files[changelogPath] += "\n<!-- ## [v" + v.ver + "] -->\n"
		// Addresses are not versions.
		files[landingPath] += "<code>curl http://127.0.0.1:8080/healthz; host 192.168.1.50</code>\n"
		files["README.md"] = strings.Replace(files["README.md"], "### 2. Run it", "Listen on 127.0.0.1:8080.\n\n### 2. Run it", 1)
		if _, fails := run(t, files, []string{"v" + v.prev}, "v"+v.ver); fails != "" {
			t.Fatalf("v%s: comments and older versions outside the install section must not fail:\n%s", v.ver, fails)
		}
		files["README.md"] += "<!-- VER=" + v.prev + " -->\n"
		if _, fails := run(t, files, []string{"v" + v.prev}, "v"+v.ver); !strings.Contains(fails, "README.md:") || !strings.Contains(fails, "names the previous release "+v.prev) {
			t.Fatalf("v%s: a stale copy inside a comment must fail the stale scan, got:\n%s", v.ver, fails)
		}
	}
}

// TestManifestAppliesEverySurface: every surface is read, and one stale
// surface fails the manifest on its own.
func TestManifestAppliesEverySurface(t *testing.T) {
	for _, v := range versions {
		files := Fixture(v.ver, v.prev)
		findings, err := checkSurfacesOnly(mapSource(files), v.ver)
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) < len(Surfaces) {
			t.Fatalf("v%s: %d findings for %d surfaces", v.ver, len(findings), len(Surfaces))
		}
		for _, f := range findings {
			if !f.OK {
				t.Fatalf("v%s: complete tree failed: %s", v.ver, f.Msg)
			}
		}
		for _, s := range Surfaces {
			stale := Fixture(v.ver, v.prev)
			stale[s.Path] = strings.ReplaceAll(stale[s.Path], v.ver, v.prev)
			findings, err := checkSurfacesOnly(mapSource(stale), v.ver)
			if err != nil {
				t.Fatal(err)
			}
			failed := false
			for _, f := range findings {
				failed = failed || (!f.OK && strings.HasPrefix(f.Msg, s.Path))
			}
			if !failed {
				t.Errorf("v%s: %s at the previous version passed the manifest", v.ver, s.Path)
			}
		}
	}
}

func TestTags(t *testing.T) {
	files := Fixture("0.11.0", "0.10.0")
	stale := Fixture("0.11.0", "0.10.0")
	stale["docs/guide/admin/troubleshooting.md"] = "Upgrade to v0.10.0 first.\n"
	cases := []struct {
		name  string
		files map[string]string
		rel   Releases
		want  string // "" means pass
		ok    string
	}{
		{"no tags (shallow clone)", files, Releases{Published: []string{"v0.10.0"}}, "no release tag is an ancestor of v0.11.0 in this clone; a shallow clone (fetch-depth 1) has no tags: run `git fetch --tags`", ""},
		{"only the tag itself", files, published("v0.11.0"), "no release older than v0.11.0 among the published GitHub releases that are tags in its history", ""},
		{"only prerelease and foreign tags", files, published("v0.10.0-rc1", "0.10.0", "latest"), "no release older than v0.11.0", ""},
		{"previous chosen numerically", files, published("v0.9.0", "v0.10.0", "v0.2.0"), "", "v0.11.0 follows the previous release v0.10.0"},
		{"a newer tag in its history", files, published("v0.9.0", "v0.10.0", "v0.12.0"), "v0.11.0 is older than the release tag v0.12.0 already in its history", ""},
		// A tag left by a failed release (no published release) is skipped,
		// so the scan still looks for the last published release.
		{"leftover unpublished tag skipped", files, Releases{Merged: []string{"v0.9.0", "v0.10.0", "v0.10.1"}, Published: []string{"v0.9.0", "v0.10.0"}}, "", "v0.11.0 follows the previous release v0.10.0"},
		{"leftover tag skipped, stale copy found", stale, Releases{Merged: []string{"v0.10.0", "v0.10.1"}, Published: []string{"v0.10.0"}}, "troubleshooting.md:1 names the previous release 0.10.0", ""},
		// A published release that is not an ancestor (another branch) is not it.
		{"published release on another branch skipped", files, Releases{Merged: []string{"v0.9.0"}, Published: []string{"v0.9.0", "v0.10.0"}}, "", "v0.11.0 follows the previous release v0.9.0"},
		{"no published release at all", files, Releases{Merged: []string{"v0.10.0"}, Published: []string{}}, "no release older than v0.11.0 among the published GitHub releases", ""},
		// Offline: the newest older CHANGELOG section that is an ancestor tag.
		{"offline uses the CHANGELOG", files, Releases{Merged: []string{"v0.9.0", "v0.10.0", "v0.10.1"}}, "", "v0.11.0 follows the previous release v0.10.0 (newest older one among the CHANGELOG.md sections (offline: published releases unknown) that are tags in its history)"},
		{"offline CHANGELOG section without its tag", files, Releases{Merged: []string{"v0.9.0"}}, "no release older than v0.11.0 among the CHANGELOG.md sections (offline: published releases unknown) that are tags in its history", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings, fails := runRel(t, tc.files, tc.rel, "v0.11.0")
			if tc.want == "" && fails != "" {
				t.Fatalf("want pass, got:\n%s", fails)
			}
			if !strings.Contains(fails, tc.want) {
				t.Fatalf("want %q, got:\n%s", tc.want, fails)
			}
			if tc.ok != "" {
				found := false
				for _, f := range findings {
					found = found || (f.OK && strings.HasPrefix(f.Msg, tc.ok))
				}
				if !found {
					t.Fatalf("want ok %q in %+v", tc.ok, findings)
				}
			}
		})
	}
	// A dot-pattern tag such as v0x11y0 must not count as v0.11.0.
	if _, err := ParseTag("v0x11y0"); err == nil {
		t.Fatal("ParseTag accepted v0x11y0")
	}
}

// The gate catches a release that forgot a site, not a contributor hiding
// text on purpose: CSS and attribute hiding are out of scope (README.md), so
// a hidden but present badge passes. The stale scan still catches a hidden
// copy of the previous release.
func TestVisualHidingIsOutOfScope(t *testing.T) {
	for _, markup := range []string{`<span hidden>%s</span>`, `<span style="display:none">%s</span>`, `<noscript>%s</noscript>`} {
		files := Fixture("0.5.100", "0.5.99")
		hideBadge(markup)(files, "0.5.100", "0.5.99")
		if _, fails := run(t, files, []string{"v0.5.99"}, "v0.5.100"); fails != "" {
			t.Fatalf("%s: hiding is out of scope, want pass, got:\n%s", markup, fails)
		}
		files[landingPath] = strings.Replace(files[landingPath], "</body>", fmt.Sprintf(markup, "v0.5.99 — latest release")+"</body>", 1)
		if _, fails := run(t, files, []string{"v0.5.99"}, "v0.5.100"); !strings.Contains(fails, "index.html:") {
			t.Fatalf("%s: a hidden stale copy must fail the stale scan, got:\n%s", markup, fails)
		}
	}
}

func TestParseTag(t *testing.T) {
	for _, bad := range []string{"", "0.5.100", "V0.5.100", "v0.5.100-rc1", "v0.5", "v0.5.100.1", "v01.2.3", " v0.5.100", "v0.5.100\n"} {
		if _, err := ParseTag(bad); err == nil {
			t.Errorf("ParseTag(%q) accepted", bad)
		}
	}
	v, err := ParseTag("v0.10.100")
	if err != nil || v.String() != "0.10.100" {
		t.Fatalf("ParseTag(v0.10.100) = %v, %v", v, err)
	}
	if a, _ := ParseTag("v0.5.100"); a.Compare(Version{0, 5, 99}) <= 0 {
		t.Fatal("v0.5.100 must sort after v0.5.99")
	}
}

// TestGitRefReadsTheCommit runs the check against a real repo: a ref check reads
// the commit, not the working tree, and missing files at the ref are missing.
func TestGitRefReadsTheCommit(t *testing.T) {
	dir := NewFixtureRepoDir(t, "0.5.100", "0.5.99")
	// Working tree and HEAD: complete. v0.5.99: no v0.5.100 surfaces.
	tags, err := MergedTags(dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(tags, " ") != "v0.5.99" {
		t.Fatalf("MergedTags = %v", tags)
	}
	for _, ref := range []string{"HEAD", "main"} {
		if _, fails := run2(t, GitRef{Dir: dir, Ref: ref}, tags); fails != "" {
			t.Fatalf("ref %s:\n%s", ref, fails)
		}
	}
	if err := os.Remove(filepath.Join(dir, notesZH("v0.5.100"))); err != nil {
		t.Fatal(err)
	}
	if _, fails := run2(t, GitRef{Dir: dir, Ref: "HEAD"}, tags); fails != "" {
		t.Fatalf("a dirty working tree changed a ref check:\n%s", fails)
	}
	if _, fails := run2(t, Dir(dir), tags); !strings.Contains(fails, "zh-CN.md missing") {
		t.Fatalf("working tree check missed the deleted notes:\n%s", fails)
	}
	_, fails := run2(t, GitRef{Dir: dir, Ref: "v0.5.99"}, tags)
	for _, want := range []string{"release-notes-v0.5.100.md missing", "VERSION must be exactly one line"} {
		if !strings.Contains(fails, want) {
			t.Fatalf("ref v0.5.99 must fail with %q, got:\n%s", want, fails)
		}
	}
	if err := ResolveRef(dir, "no-such-ref"); err == nil {
		t.Fatal("ResolveRef accepted an unknown ref")
	}
}

func run2(t *testing.T, src Source, tags []string) ([]Finding, string) {
	t.Helper()
	findings, err := Check(src, ptr(published(tags...)), "v0.5.100")
	if err != nil {
		t.Fatal(err)
	}
	var fails []string
	for _, f := range findings {
		if !f.OK {
			fails = append(fails, f.Msg)
		}
	}
	return findings, strings.Join(fails, "\n")
}

// Visible strings are matched after entities are decoded, so markup that
// writes the dash or an ampersand as an entity still shows the release.
func TestVisibleStringsDecodeEntities(t *testing.T) {
	if got := shownText("<p>v1.2.3 &mdash; latest &amp; greatest&#33;</p>"); !strings.Contains(got, "v1.2.3 — latest & greatest!") {
		t.Fatalf("shownText: %q", got)
	}
	for _, v := range versions {
		files := Fixture(v.ver, v.prev)
		files[landingPath] = strings.Replace(files[landingPath], "v"+v.ver+" — latest release", "v"+v.ver+" &mdash; latest release", 1)
		if !strings.Contains(files[landingPath], "&mdash;") {
			t.Fatal("fixture has no hero badge")
		}
		if _, fails := run(t, files, []string{"v" + v.prev}, "v"+v.ver); fails != "" {
			t.Fatalf("v%s: &mdash; in the hero badge must pass, got:\n%s", v.ver, fails)
		}
	}
}

// scanVersions: the last published release, plus every version tagged in the
// history or in the CHANGELOG between it and this one.
func TestScanVersions(t *testing.T) {
	v := Version{0, 5, 100}
	changelog := []string{"v0.5.100", "v0.5.98", "v0.5.97", "v0.5.95"}
	show := func(prev *Version, extra []Version) string {
		if prev == nil {
			return "none"
		}
		s := prev.String() + " +"
		for _, x := range extra {
			s += " " + x.String()
		}
		return s
	}
	for _, tc := range []struct {
		name string
		rel  *Releases
		want string
	}{
		// v0.5.99 tagged only, v0.5.98 in the CHANGELOG only, neither published.
		{"gh", &Releases{Merged: []string{"v0.5.95", "v0.5.97", "v0.5.99", "v0.5.101-rc1"}, Published: []string{"v0.5.95", "v0.5.97"}}, "0.5.97 + 0.5.98 0.5.99"},
		{"gh, nothing unpublished", &Releases{Merged: []string{"v0.5.95", "v0.5.98"}, Published: []string{"v0.5.95", "v0.5.98"}}, "0.5.98 +"},
		{"offline", &Releases{Merged: []string{"v0.5.95", "v0.5.97", "v0.5.99"}}, "0.5.97 + 0.5.98 0.5.99"},
		{"go test (no tags, no GitHub)", nil, "0.5.98 +"},
		{"nothing published", &Releases{Merged: []string{"v0.5.95"}, Published: []string{}}, "none"},
	} {
		prev, extra, _ := scanVersions(v, tc.rel, changelog)
		if got := show(prev, extra); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
