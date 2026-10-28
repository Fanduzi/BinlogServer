package releaseprep

import (
	"slices"
	"strings"
	"testing"
)

// The manifest edits the README cleanup and the landing-page cleanup make
// when they land on top of this gate. This gate's PR body quotes them; these
// tests prove the API expresses them.
// Both helpers are idempotent: once the PR has made the edit in Surfaces and
// History, applying them again changes nothing.

// manifest563 is Surfaces and History after the README cleanup: the README names the
// latest release once, the upgrade guides keep the per-version notes, and
// the README history rules go.
func manifest563() ([]Surface, []HistoryRule) {
	s := slices.Clone(Surfaces)
	for i := range s {
		switch s[i].Path {
		case "README.md":
			s[i].Visible = []string{"Latest release: `v{v}`"}
		case "README_ZH.md":
			s[i].Visible = []string{"最新版本：`v{v}`"}
		}
	}
	for _, g := range []string{"docs/guide/admin/upgrade.md", "docs/guide/admin/upgrade.zh-CN.md"} {
		if !slices.ContainsFunc(s, func(x Surface) bool { return x.Path == g }) {
			s = append(s, Surface{Path: g, NewestNote: GuideNote})
		}
	}
	h := slices.DeleteFunc(slices.Clone(History), func(r HistoryRule) bool {
		return (r.Path == "README.md" || r.Path == "README_ZH.md")
	})
	return s, h
}

// manifest565 is Surfaces and History after the landing-page cleanup: the landing page names the
// version at its four sites only, its JS strings use {ver}, and the timeline
// (with its "(Current)" marker and history rules) goes.
func manifest565() ([]Surface, []HistoryRule) {
	s := slices.Clone(Surfaces)
	for i := range s {
		if s[i].Path == landingPath {
			s[i] = Surface{
				Path:                  landingPath,
				Attrs:                 []Attr{{"html", "data-version"}},
				Visible:               []string{"v{v} — latest release", "Get Started with v{v}"},
				VERPins:               true,
				OnlyThisVersion:       true,
				ScriptUsesPlaceholder: true,
				Mentions:              4,
			}
		}
	}
	h := slices.DeleteFunc(slices.Clone(History), func(r HistoryRule) bool { return r.Path == landingPath })
	return s, h
}

func withManifest(t *testing.T, s []Surface, h []HistoryRule) {
	t.Helper()
	oldS, oldH := Surfaces, History
	Surfaces, History = s, h
	t.Cleanup(func() { Surfaces, History = oldS, oldH })
}

const ver, prev = "0.5.100", "0.5.99" // the follow-up fixtures

func fixture563() map[string]string {
	f := Fixture(ver, prev)
	r := strings.NewReplacer("@V@", ver, "@P@", prev)
	cut := func(path, from string) {
		f[path] = f[path][:strings.Index(f[path], from)]
	}
	cut("README.md", "## Upgrade Notes")
	cut("README_ZH.md", "## 升级须知")
	f["README.md"] += r.Replace("## Upgrading\n\nLatest release: `v@V@`. No migration; in-place upgrade from the previous release is supported.\n")
	f["README_ZH.md"] += r.Replace("## 升级\n\n最新版本：`v@V@`。无需迁移，支持从上一个版本原地升级。\n")
	f["docs/guide/admin/upgrade.md"] = r.Replace("# Upgrading\n\nRead every note, newest first. Versioned notes start at v0.0.1.\n\n## Per-version notes\n\n" +
		"- **v@V@: retention marker, no migration:** in-place upgrade from v@P@ is tested. v@P@ runs on the same schema.\n" +
		"- **v@P@: health metrics:** optional keys.\n- **Standalone: a note with no version.**\n- **v0.0.1: first.**\n")
	f["docs/guide/admin/upgrade.zh-CN.md"] = r.Replace("# 升级\n\n## 逐版本说明\n\n- **v@V@：保留策略标记，没有迁移：** 测试过从 v@P@ 原地升级。\n- **v@P@：健康指标。**\n")
	return f
}

func fixture565() map[string]string {
	f := Fixture(ver, prev)
	r := strings.NewReplacer("@V@", ver, "@P@", prev)
	f[landingPath] = r.Replace(`<!doctype html>
<html lang="en" data-version="@V@">
<head><meta name="description" content="BinlogServer"><style>.hidden { display: none }</style></head>
<body>
<span class="tag tag-new" data-i18n="heroBadge">v@V@ — latest release</span>
<code data-i18n="step1Code">VER=@V@ OS=linux ARCH=amd64
curl -fsSL -O "https://github.com/x/releases/download/v${VER}/binlog-server_${VER}_${OS}_${ARCH}.tar.gz"</code>
<a href="#deployment" class="btn-primary" data-i18n="ctaPrimary">Get Started with v@V@</a>
<p>MySQL 8.4 LTS, MariaDB 10.3+, 127.0.0.1:8080</p>
<script>
const en = { heroBadge: 'v{ver} — latest release', step1Code: 'VER={ver} OS=linux\ncurl "download/v${VER}/binlog-server_${VER}_${OS}.tar.gz"', ctaPrimary: 'Get Started with v{ver}' };
const zh = { heroBadge: 'v{ver} — 最新发布版本', step1Code: 'VER={ver} OS=linux', ctaPrimary: '开始使用 v{ver}' };
// The release version lives once, in <html data-version>; strings use {ver}.
const version = document.documentElement.dataset.version;
</script>
</body></html>
`)
	return f
}

func checkAll(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	tagFindings, err := Check(mapSource(files), ptr(published("v"+prev)), "v"+ver)
	if err != nil {
		t.Fatal(err)
	}
	prFindings, err := Check(mapSource(files), nil, "v"+ver)
	if err != nil {
		t.Fatal(err)
	}
	return failures(tagFindings), failures(prFindings)
}

func TestManifest563(t *testing.T) {
	s, h := manifest563()
	withManifest(t, s, h)
	if tag, pr := checkAll(t, fixture563()); tag != "" || pr != "" {
		t.Fatalf("#563-shaped tree must pass:\ntag gate:\n%s\nPR check:\n%s", tag, pr)
	}
	// Prose in the per-version notes names older releases by rule (Section).
	f := fixture563()
	f["docs/guide/admin/upgrade.md"] += "\nUpgrading from v" + prev + " needs no migration.\n"
	if tag, pr := checkAll(t, f); tag != "" || pr != "" {
		t.Fatalf("prose in the notes section must pass:\n%s\n%s", tag, pr)
	}
	// Applying the helper again (after the rebase) changes nothing.
	s2, h2 := manifest563()
	if len(s2) != len(s) || len(h2) != len(h) {
		t.Fatalf("manifest563 is not idempotent: %d/%d surfaces, %d/%d rules", len(s2), len(s), len(h2), len(h))
	}
	cases := []struct {
		name string
		edit func(map[string]string)
		want string
	}{
		{"README latest release stale", func(f map[string]string) {
			f["README.md"] = strings.Replace(f["README.md"], "Latest release: `v"+ver+"`", "Latest release: `v"+prev+"`", 1)
		}, "README.md does not show \"Latest release: `v" + ver + "`\""},
		{"README latest release only in a comment", func(f map[string]string) {
			f["README.md"] = strings.Replace(f["README.md"], "Latest release: `v"+ver+"`.", "<!-- Latest release: `v"+ver+"`. -->", 1)
		}, "README.md does not show \"Latest release: `v" + ver + "`\""},
		{"README latest release longer", func(f map[string]string) {
			f["README.md"] = strings.Replace(f["README.md"], "`v"+ver+"`. No", "`v"+ver+"`.1 No", 1)
			f["README.md"] = strings.Replace(f["README.md"], "Latest release: `v"+ver+"`", "Latest release: `v"+ver+"0`", 1)
		}, "README.md does not show \"Latest release: `v" + ver + "`\""},
		{"zh latest release missing", func(f map[string]string) {
			f["README_ZH.md"] = strings.Replace(f["README_ZH.md"], "最新版本：`v"+ver+"`", "最新版本：v"+ver, 1)
		}, "README_ZH.md does not show \"最新版本：`v" + ver + "`\""},
		{"README keeps an old upgrade note", func(f map[string]string) {
			f["README.md"] += "\n- **v" + prev + ": health metrics:** optional keys.\n"
		}, "README.md:"},
		{"upgrade guide newest note is the previous release", func(f map[string]string) {
			g := f["docs/guide/admin/upgrade.md"]
			f["docs/guide/admin/upgrade.md"] = g[:strings.Index(g, "- **v"+ver)] + g[strings.Index(g, "- **v"+prev):]
		}, "docs/guide/admin/upgrade.md newest note is v" + prev + ", want v" + ver + " at the top"},
		{"upgrade guide out of order", func(f map[string]string) {
			f["docs/guide/admin/upgrade.md"] += "- **v0.0.2: late.**\n"
		}, "docs/guide/admin/upgrade.md note v0.0.2 comes after v0.0.1"},
		{"upgrade guide duplicate note", func(f map[string]string) {
			f["docs/guide/admin/upgrade.zh-CN.md"] += "- **v" + prev + "：又一条。**\n"
		}, "upgrade.zh-CN.md note v" + prev + " comes after v" + prev},
		{"upgrade guide newest note only in a comment", func(f map[string]string) {
			f["docs/guide/admin/upgrade.zh-CN.md"] = strings.Replace(f["docs/guide/admin/upgrade.zh-CN.md"], "- **v"+ver, "<!-- - **v"+ver, 1)
			f["docs/guide/admin/upgrade.zh-CN.md"] = strings.Replace(f["docs/guide/admin/upgrade.zh-CN.md"], "原地升级。\n", "原地升级。 -->\n", 1)
		}, "upgrade.zh-CN.md newest note is v" + prev},
		{"upgrade guide newest note only in a fence", func(f map[string]string) {
			f["docs/guide/admin/upgrade.md"] = strings.Replace(f["docs/guide/admin/upgrade.md"], "## Per-version notes\n\n", "## Per-version notes\n\n~~~\n- **v9.9.9: example**\n~~~\n", 1)
			f["docs/guide/admin/upgrade.md"] = strings.Replace(f["docs/guide/admin/upgrade.md"], "- **v"+ver, "- **v0.5.98", 1)
		}, "upgrade.md newest note is v0.5.98"},
		{"upgrade guide without notes", func(f map[string]string) {
			f["docs/guide/admin/upgrade.zh-CN.md"] = "# 升级\n"
		}, "upgrade.zh-CN.md has no versioned note"},
		{"upgrade guide missing", func(f map[string]string) { delete(f, "docs/guide/admin/upgrade.md") }, "docs/guide/admin/upgrade.md missing"},
		{"upgrade guide prose outside the notes names the previous release", func(f map[string]string) {
			f["docs/guide/admin/upgrade.md"] = strings.Replace(f["docs/guide/admin/upgrade.md"], "# Upgrading\n\n", "# Upgrading\n\nThe latest release is v"+prev+".\n\n", 1)
		}, "docs/guide/admin/upgrade.md:3 names the previous release"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := fixture563()
			c.edit(f)
			tag, pr := checkAll(t, f)
			if !strings.Contains(tag, c.want) || !strings.Contains(pr, c.want) {
				t.Fatalf("want %q from both the tag gate and the PR check:\ntag:\n%s\npr:\n%s", c.want, tag, pr)
			}
		})
	}
}

func TestManifest565(t *testing.T) {
	s, h := manifest565()
	withManifest(t, s, h)
	if tag, pr := checkAll(t, fixture565()); tag != "" || pr != "" {
		t.Fatalf("#565-shaped landing must pass:\ntag gate:\n%s\nPR check:\n%s", tag, pr)
	}
	if s2, h2 := manifest565(); len(s2) != len(s) || len(h2) != len(h) || !slices.EqualFunc(s2, s, func(a, b Surface) bool { return a.Path == b.Path && a.Mentions == b.Mentions }) {
		t.Fatal("manifest565 is not idempotent")
	}
	sub := func(old, new string) func(map[string]string) {
		return func(f map[string]string) {
			if !strings.Contains(f[landingPath], old) {
				panic("fixture565 lacks " + old)
			}
			f[landingPath] = strings.Replace(f[landingPath], old, new, 1)
		}
	}
	cases := []struct {
		name string
		edit func(map[string]string)
		want string
	}{
		{"zh string hardcodes the release", sub("'开始使用 v{ver}'", "'开始使用 v"+ver+"'"), "index.html <script> hardcodes " + ver},
		{"zh string hardcodes the release without v", sub("'开始使用 v{ver}'", "'开始使用 "+ver+"'"), "index.html <script> hardcodes " + ver},
		{"zh string hardcodes the previous release", sub("'v{ver} — 最新发布版本'", "'v"+prev+" — 最新发布版本'"), "index.html <script> hardcodes " + prev},
		{"JS pin hardcoded", sub("step1Code: 'VER={ver} OS=linux'", "step1Code: 'VER="+ver+" OS=linux'"), "index.html <script> hardcodes " + ver},
		{"JS archive hardcoded", sub(`binlog-server_${VER}_${OS}.tar.gz`, "binlog-server_"+ver+"_${OS}.tar.gz"), "index.html <script> hardcodes " + ver},
		{"fifth site", sub("<p>MySQL", "<h2>Quick Start (v"+ver+")</h2><p>MySQL"), "index.html names " + ver + " 5 times outside comments, want exactly 4"},
		{"site lost", sub("Get Started with v"+ver, "Get Started"), "index.html names " + ver + " 3 times outside comments, want exactly 4"},
		{"old timeline entry", sub("<p>MySQL", `<div class="timeline-version">v`+prev+`</div><p>MySQL`), "docs/landing/index.html:"},
		{"badge only in a template", sub(`<span class="tag tag-new" data-i18n="heroBadge">v`+ver+` — latest release</span>`, `<template><span>v`+ver+` — latest release</span></template>`), `does not show "v` + ver + ` — latest release"`},
		{"data-version stale", sub(`data-version="`+ver+`"`, `data-version="`+prev+`"`), `<html data-version="` + prev + `"> must be "` + ver + `"`},
		{"other version in a script", sub("const version", "const old = 'v0.0.1';\nconst version"), "index.html <script> hardcodes 0.0.1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := fixture565()
			c.edit(f)
			tag, pr := checkAll(t, f)
			if !strings.Contains(tag, c.want) || !strings.Contains(pr, c.want) {
				t.Fatalf("want %q from both the tag gate and the PR check:\ntag:\n%s\npr:\n%s", c.want, tag, pr)
			}
		})
	}
}

// P2-4 survivors that need their own boundary or a second file.

func TestNotesMinimums(t *testing.T) {
	en, zh := notesEN("v"+ver), notesZH("v"+ver)
	enNotes := func(n int) string { return "# Binlog Server v" + ver + "\n\n" + strings.Repeat("a", n) + "\n" }
	zhNotes := func(han int) string {
		return "# Binlog Server v" + ver + " 中文发布说明\n\n" + strings.Repeat("说", han) + strings.Repeat("a", 80) + "\n"
	}
	cases := []struct {
		path, body, want string
	}{
		{en, enNotes(79), "release notes " + en + " have no body under the title (79 characters, need 80)"},
		{en, enNotes(80), ""},
		{en, enNotes(40) + "## heading lines do not count toward the body at all, however long they are\n" + strings.Repeat("b", 39), "(79 characters, need 80)"},
		{zh, zhNotes(19), "release notes " + zh + " are not written in Chinese (19 Han characters, need 20)"},
		{zh, zhNotes(20), ""},
		{zh, "# Binlog Server v" + ver + " 中文发布说明\n\n" + strings.Repeat("说", 79) + "\n", "(79 characters, need 80)"},
	}
	for _, c := range cases {
		f := Fixture(ver, prev)
		f[c.path] = c.body
		_, fails := run(t, f, []string{"v" + prev}, "v"+ver)
		if c.want == "" && strings.Contains(fails, "release notes") {
			t.Errorf("%s: want pass, got:\n%s", c.body, fails)
		}
		if c.want != "" && !strings.Contains(fails, c.want) {
			t.Errorf("%s: want %q, got:\n%s", c.body, c.want, fails)
		}
	}
}

func TestInstallSectionEndsAtTheSameLevel(t *testing.T) {
	for _, v := range versions {
		files := Fixture(v.ver, v.prev)
		// "### 2. Run it" ends the h3 install section: an older version under it is fine.
		files["README.md"] = strings.Replace(files["README.md"], "### 2. Run it\n", "### 2. Run it\n\nTested against v0.0.1 and binlog-server_0.0.1_linux.\n", 1)
		if _, fails := run(t, files, []string{"v" + v.prev}, "v"+v.ver); fails != "" {
			t.Fatalf("v%s: a version under the next same-level heading must not fail:\n%s", v.ver, fails)
		}
		// A deeper heading does not end it.
		files = Fixture(v.ver, v.prev)
		files["README.md"] = strings.Replace(files["README.md"], "### 2. Run it\n", "#### Notes\n\nv0.0.1\n\n### 2. Run it\n", 1)
		if _, fails := run(t, files, []string{"v" + v.prev}, "v"+v.ver); !strings.Contains(fails, "README.md install section names 0.0.1") {
			t.Fatalf("v%s: a deeper heading must stay in the install section, got:\n%s", v.ver, fails)
		}
	}
}

func TestMissingFileDoesNotStopTheRest(t *testing.T) {
	for _, v := range versions {
		files := Fixture(v.ver, v.prev)
		delete(files, "README.md")
		delete(files, notesEN("v"+v.ver))
		files["README_ZH.md"] = strings.Replace(files["README_ZH.md"], "VER="+v.ver, "VER=0.0.1", 1)
		files["VERSION"] = "0.0.1\n"
		_, fails := run(t, files, []string{"v" + v.prev}, "v"+v.ver)
		for _, want := range []string{"README.md missing", notesEN("v"+v.ver) + " missing", "README_ZH.md pins 0.0.1", "VERSION must be exactly one line"} {
			if !strings.Contains(fails, want) {
				t.Errorf("v%s: want %q after a missing file, got:\n%s", v.ver, want, fails)
			}
		}
	}
}
