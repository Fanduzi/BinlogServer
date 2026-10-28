package releaseprep

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestFindVersionSpellings(t *testing.T) {
	const ver = "0.5.83"
	hit := []string{
		"0.5.83", "v0.5.83", "V0.5.83", "(v0.5.83)", "`v0.5.83`", "VER=0.5.83 OS=linux",
		"binlog-server_0.5.83_linux_amd64.tar.gz", "binlog-server-v0.5.83-linux", "dist/v0_5_83/", "0-5-83",
		"release-notes-v0.5.83.md", "v0.5.83.zh-CN.md", "v0.5.83-rc1", "v0.5.83:", "v0.5.83：",
		"v０.５.８３", "v٠.٥.٨٣", "v𝟎.𝟓.𝟖𝟑", "v𝟢.𝟧.𝟪𝟥", "v𝟶.𝟻.𝟾𝟹", "v0．5．83", "v0。5。83", "v0․5․83",
		"v0&#46;5&#46;83", "v0&period;5.83", "v0\u200b.5.83", "v0.5\u2060.83", "v0‐5‐83",
		"最新版本：v0.5.83。", "data-version=\"0.5.83\"",
		// Split spellings (P3): inline tags and comments inside the token,
		// percent escapes, Unicode dashes and dots, leading zeros.
		"v0.<b>5</b>.83", "v<span class=\"x\">0.5</span>.83", "0.5<!-- x -->.83", "v0<wbr>.5.83",
		"v0%2E5%2E83", "v0%2e5%2e83", "v0.\u00ad5.83", "binlog-server_0%2E5%2E83_linux", "v0%2D5%2D83", "v0%5F5%5F83",
		"v0–5–83", "v0—5—83", "v0−5−83", "v0‒5‒83", "v0－5－83", "v0ꓸ5ꓸ83", "v0٫5٫83", "v0ˌ5ˌ83",
		"v00.05.083", "v0.05.83", "0.5.083", "v000.5.83",
	}
	for _, s := range hit {
		if len(findVersion(s, ver)) == 0 {
			t.Errorf("findVersion(%q, %s) found nothing", s, ver)
		}
	}
	miss := []string{
		"10.0.5.83", "0.5.830", "0.5.83.1", "v0.5.8", "0.5.8.3", "v10.5.83", "x0.5.83", "av0.5.83",
		"1.0.5.83", "0.5.83.0", "v0.5.831", "0.15.83", "127.0.5.83",
		"v0.5.183", "v0.15.83", "v10.5.83", "v0.5<div>.83", "0.5<p>.83", "v0.5.8<b>3</b>1",
		"0_5_83_1", "1_0_5_83", "0-5-83-2", "7-0-5-83", "0_5_83.1", "9.0-5-83",
	}
	for _, s := range miss {
		if got := findVersion(s, ver); len(got) != 0 {
			t.Errorf("findVersion(%q, %s) = %v, want no match", s, ver, got)
		}
	}
	// A digit-grouped number is not a version in the same separator.
	for _, s := range []string{"Intn(1_000_000_000)", "2_1_0_0", "1-0-0-7"} {
		if got := findVersion(s, "1.0.0"); len(got) != 0 {
			t.Errorf("findVersion(%q, 1.0.0) = %v, want no match", s, got)
		}
	}
	for _, s := range []string{"v1.0.0-1", "x_1.0.0_y", "1_0_0.", "v1_0_0-rc1", "2-1_0_0", "binlog-server_1_0_0_linux"} {
		if len(findVersion(s, "1.0.0")) != 1 {
			t.Errorf("findVersion(%q, 1.0.0) must find one", s)
		}
	}
	if got := findVersion("v0.5.83 and 0.5.83, again v0_5_83", ver); len(got) != 3 {
		t.Errorf("want 3 matches, got %v", got)
	}
	// A part that is 0 matches any run of zeros, not nothing.
	if got := findVersion("v1.0.2", "1.0.2"); len(got) != 1 || len(findVersion("v1..2", "1.0.2")) != 0 || len(findVersion("v1.00.2", "1.0.2")) != 1 {
		t.Errorf("zero part: %v", got)
	}
	// Spans cover the version in the normalized line.
	if got := findVersion("see v0.<b>5</b>.83 now", ver); len(got) != 1 || got[0] != [2]int{4, 11} {
		t.Errorf("span = %v, want [[4 11]]", got)
	}
}

func TestPathMatches(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"CHANGELOG.md", "CHANGELOG.md", true},
		{"CHANGELOG.md", "docs/CHANGELOG.md", false},
		{"docs/releases/**", "docs/releases/v1.md", true},
		{"docs/releases/**", "docs/releases/a/b.md", true},
		{"docs/releases/**", "docs/releasesx/v1.md", false},
		{"docs/releases/**", "docs/releases", false},
		{"**/testdata/**", "internal/x/testdata/a.txt", true},
		{"**/testdata/**", "testdata/a.txt", true},
		{"**/testdata/**", "internal/mytestdata/a.txt", false},
		{"**/*_test.go", "main_test.go", true},
		{"**/*_test.go", "internal/a/b_test.go", true},
		{"**/*_test.go", "internal/a/b.go", false},
		{"**/*_test.go", "internal/a/b_test.go.md", false},
		{"**/package-lock.json", "web/package-lock.json", true},
		{"**/package-lock.json", "web/package.json", false},
		{"docs/*.md", "docs/a.md", true},
		{"docs/*.md", "docs/a/b.md", false},
	}
	for _, c := range cases {
		if got := pathMatches(c.pattern, c.path); got != c.want {
			t.Errorf("pathMatches(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestHistoryRulesNeedAReason(t *testing.T) {
	if _, err := compileHistory([]HistoryRule{{Path: "a.md"}}, "1.0.1", "1.0.0"); err == nil || !strings.Contains(err.Error(), "a.md has no reason") {
		t.Fatalf("a rule without Why must be an error, got %v", err)
	}
	if _, err := compileHistory([]HistoryRule{{Path: "a.md", Line: "(", Why: "x"}}, "1.0.1", "1.0.0"); err == nil {
		t.Fatal("a bad Line regex must be an error")
	}
	if _, err := compileHistory(History, "1.0.1", "1.0.0"); err != nil {
		t.Fatalf("History does not compile: %v", err)
	}
	if _, err := compileHistory([]HistoryRule{{Path: "a.md", Line: "x", Section: "y", Why: "x"}}, "1.0.1", "1.0.0"); err == nil {
		t.Fatal("a rule with both Line and Section must be an error")
	}
	for _, bad := range []HistoryRule{{Path: "a.md", Unless: "(", Why: "x"}, {Path: "a.md", Section: "(", Why: "x"}} {
		if _, err := compileHistory([]HistoryRule{bad}, "1.0.1", "1.0.0"); err == nil {
			t.Fatalf("a bad regex must be an error: %+v", bad)
		}
	}
	// {v} and {p} are literal versions, not regex dots.
	rs, _ := compileHistory([]HistoryRule{{Path: "a.md", Line: `old_ref: v{p}\b`, Why: "x"}}, "1.0.1", "1.0.0")
	if !lineAllowed(rs, "a.md", "  old_ref: v1.0.0", "1.0.0") || lineAllowed(rs, "b.md", "old_ref: v1.0.0", "1.0.0") {
		t.Fatal("{p} must expand to the quoted previous version, only on the rule's path")
	}
	rs, _ = compileHistory([]HistoryRule{{Path: "a.md", Line: `old_ref: v{p}\b`, Why: "x"}}, "1.0.1", "1x0x0")
	if lineAllowed(rs, "a.md", "old_ref: v1.0.0", "1.0.0") {
		t.Fatal("{p} must be quoted: 1x0x0 does not match 1.0.0")
	}
	// A rule covers only its construct: a second copy on the line still fails.
	rs, _ = compileHistory([]HistoryRule{{Path: "a.md", Line: `old_ref: v{p}\b`, Why: "x"}}, "1.0.1", "1.0.0")
	if lineAllowed(rs, "a.md", "old_ref: v1.0.0 # was v1.0.0", "1.0.0") {
		t.Fatal("a copy outside the rule's match must not be allowed")
	}
	// Unless turns the rule off for a line it matches.
	rs, _ = compileHistory([]HistoryRule{{Path: "a.md", Line: `^- v[0-9.]+:.*`, Unless: `VER=`, Why: "x"}}, "1.0.1", "1.0.0")
	if !lineAllowed(rs, "a.md", "- v1.0.0: notes", "1.0.0") || lineAllowed(rs, "a.md", "- v1.0.0: VER=1.0.0", "1.0.0") {
		t.Fatal("Unless must turn the rule off on a pin line only")
	}
}

func lineAllowed(rs compiledRules, p, line, prev string) bool {
	norm := normalizeLine(line)
	spans := versionSpans(norm, prev)
	return len(spans) > 0 && rs.allows(p, norm, spans)
}

// The stale copies QA found by bumping only the manifest sites
// (issuecomment-6442026786): each must fail by file:line.
func TestStaleCopiesFail(t *testing.T) {
	for _, v := range versions {
		cases := []struct {
			name, path, add string
		}{
			{"README upgrade heading", "README.md", "## Upgrade Notes (v@P@)"},
			{"README before-upgrading line", "README.md", "Before upgrading an existing deployment to `v@P@`, read the notes."},
			{"zh README heading", "README_ZH.md", "## 升级须知（v@P@）"},
			{"deployment archive", deployPath, "tar -xzf binlog-server_@P@_linux_amd64.tar.gz"},
			{"deployment dir", deployPath, "cd binlog-server_@P@_linux_amd64"},
			{"deployment no-v", deployPath, "Use version @P@ for this guide."},
			{"landing quick start", landingPath, "<h2>Quick Start (v@P@)</h2>"},
			{"landing download", landingPath, `<a href="https://github.com/x/releases/tag/v@P@">Download &amp; Verify</a>`},
			{"landing checksums", landingPath, "<code>curl -LO .../v@P@/checksums.txt</code>"},
			{"landing footer", landingPath, "<footer>BinlogServer v@P@</footer>"},
			{"landing zh badge", landingPath, "<script>const zh2 = { heroBadge: 'v@P@ — 最新发布版本' };</script>"},
			{"landing step title", landingPath, "<script>const s = { step1Title: 'Install v@P@' };</script>"},
			{"landing release1 not current", landingPath, `<p data-i18n="release1">v@P@: older entry.</p>`},
			{"stale copy in a comment", "README.md", "<!-- VER=@P@ -->"},
			{"entity spelling", landingPath, "<p>v@P@</p>"},
			{"new untracked doc", "docs/guide/new.md", "Install v@P@."},
			{"example config", "config.example.yaml", "# upgraded from v@P@"},
			{"upgrade guide prose outside its history section", "docs/guide/admin/upgrade.md", "Upgrade to v@P@ by replacing the binary."},
			{"e2e workflow outside old_ref", ".github/workflows/e2e.yml", "      new_ref: v@P@"},
			{"e2e old_ref line with a second copy", ".github/workflows/e2e.yml", "      old_ref: v@P@ # binlog-server_@P@"},
			// P2-3: only tests, migrations and dependency pins are whole-file history.
			{"non-test Go code", "internal/x/x.go", `const latest = "v@P@"`},
			{"testdata", "internal/x/testdata/a.txt", "v@P@"},
			{"console package version", "console/package.json", `  "version": "@P@",`},
			{"console bundle", "internal/ui/static/app.js", `var v="v@P@";`},
			{"develop plan", "docs/develop/plan.md", "Ship v@P@."},
			// Anchored line rules: the construct only, not the whole line.
			{"README note with a pin", "README.md", "- **v@P@: notes:** `VER=@P@`"},
			{"README notes link line with an archive", "README.md", "Full release notes: binlog-server_@P@_linux_amd64.tar.gz"},
			{"landing timeline entry with an adjacent copy", landingPath, `<div class="timeline-version">v@P@</div>@P@`},
			{"README note text not at the line start", "README.md", "See - **v@P@: health metrics:** in the guide."},
			{"e2e old_ref with another separator", ".github/workflows/e2e.yml", "      old_ref: v@P_@"},
			{"landing timeline entry with a trailing copy", landingPath, `<div class="timeline-version">v@P@</div> <span>Install v@P@</span>`},
			{"landing release2 with a trailing copy", landingPath, `<p data-i18n="release2">v@P@: older.</p><a>v@P@</a>`},
			{"landing JS release2 with a trailing copy", landingPath, `release2: 'v@P@: older.', step1: 'Install v@P@'`},
			{"landing release1 of another version", landingPath, `<p data-i18n="release1">v@P@: older entry.</p>`},
			{"production readiness outside the plain leg", "docs/production-readiness.md", "当前版本 v@P@"},
			// P1-1: no file is skipped over its encoding.
			{"invalid UTF-8 on the landing page", landingPath, "<p>\xa0 v@P@ — latest release</p>"},
			{"invalid UTF-8 in troubleshooting", "docs/guide/admin/troubleshooting.md", "Caf\xe9: install v@P@."},
			{"NUL byte in a doc", "docs/guide/admin/notes.txt", "a\x00b v@P@"},
		}
		for _, c := range cases {
			t.Run(v.ver+"/"+c.name, func(t *testing.T) {
				files := Fixture(v.ver, v.prev)
				add := strings.ReplaceAll(strings.ReplaceAll(c.add, "@P_@", strings.ReplaceAll(v.prev, ".", "_")), "@P@", v.prev)
				if c.name == "entity spelling" {
					add = strings.ReplaceAll(add, ".", "&#46;")
				}
				if strings.HasSuffix(c.name, "outside its history section") {
					files[c.path] = add + "\n" + files[c.path] // before the section, whatever the fixture holds
				} else {
					files[c.path] += "\n" + add + "\n"
				}
				line := strings.Count(files[c.path][:strings.Index(files[c.path], add)], "\n") + 1
				_, fails := run(t, files, []string{"v" + v.prev}, "v"+v.ver)
				want := c.path + ":" + strconv.Itoa(line) + " names the previous release " + v.prev
				if !strings.Contains(fails, want) {
					t.Fatalf("want %q, got:\n%s", want, fails)
				}
				// The every-PR check sees it too.
				findings, err := Check(mapSource(files), nil, "v"+v.ver)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(failures(findings), want) {
					t.Fatalf("PR check: want %q, got:\n%s", want, failures(findings))
				}
			})
		}
	}
}

// UTF-16 files are decoded (with a BOM) or read with their NULs dropped
// (without one): neither is skipped.
func TestUTF16FilesAreScanned(t *testing.T) {
	enc := func(s string, be bool) string {
		var b []byte
		for _, u := range utf16.Encode([]rune(s)) {
			if be {
				b = append(b, byte(u>>8), byte(u))
			} else {
				b = append(b, byte(u), byte(u>>8))
			}
		}
		return string(b)
	}
	for name, body := range map[string]string{
		// Full-width digits only read right when the UTF-16 is decoded.
		"LE with BOM":    "\xff\xfe" + enc("中文\nInstall v０.５.９９\n", false),
		"BE with BOM":    "\xfe\xff" + enc("中文\nInstall v０.５.９９\n", true),
		"LE without BOM": enc("x\nInstall v0.5.99\n", false),
		"UTF-8 BOM":      "\xef\xbb\xbfx\nInstall v0.5.99\n",
	} {
		files := Fixture("0.5.100", "0.5.99")
		files["docs/guide/admin/win.txt"] = body
		if _, fails := run(t, files, []string{"v0.5.99"}, "v0.5.100"); !strings.Contains(fails, "docs/guide/admin/win.txt:2 names the previous release 0.5.99") {
			t.Errorf("%s: want a stale finding on line 2, got:\n%s", name, fails)
		}
	}
	if got := decodeText("\xff\xfe" + enc("v1", false) + "\x00"); got != "v1" {
		t.Errorf("odd trailing byte: %q", got)
	}
	// A surface stored as UTF-16 is read as text too.
	files := Fixture("0.5.100", "0.5.99")
	files[versionPath] = "\xff\xfe" + enc("0.5.100\n", false)
	if _, fails := run(t, files, []string{"v0.5.99"}, "v0.5.100"); fails != "" {
		t.Errorf("UTF-16 VERSION: %s", fails)
	}
	// An editor's UTF-8 BOM does not break an exact surface.
	files = Fixture("0.5.100", "0.5.99")
	files[versionPath] = "\xef\xbb\xbf0.5.100\n"
	if _, fails := run(t, files, []string{"v0.5.99"}, "v0.5.100"); fails != "" {
		t.Errorf("VERSION with a UTF-8 BOM: %s", fails)
	}
	// A UTF-16 CHANGELOG still gives the previous release offline.
	files = Fixture("0.5.100", "0.5.99")
	files[changelogPath] = "\xff\xfe" + enc(files[changelogPath], false)
	findings, err := Check(mapSource(files), nil, "v0.5.100")
	if err != nil || !strings.Contains(passes(findings), "v0.5.100 follows the previous release v0.5.99 (newest older one among the CHANGELOG.md sections)") {
		t.Errorf("UTF-16 CHANGELOG: %v\n%s\n%s", err, passes(findings), failures(findings))
	}
}

func TestHistoryAllowsReleaseHistory(t *testing.T) {
	for _, v := range versions {
		files := Fixture(v.ver, v.prev)
		p := v.prev
		files["docs/releases/release-notes-v"+p+".md"] = "# Binlog Server v" + p + "\n"
		files["docs/adr/0001.md"] = "Decided in v" + p + ".\n"
		files["docs/design/0001.md"] = "Shipped in v" + p + ".\n"
		files["internal/x/x_test.go"] = `const since = "v` + p + `"` + "\n"
		files["migrations/000009_x.up.sql"] = "-- added in v" + p + "\n"
		files["go.sum"] = "example.com/m v" + p + " h1:x=\n"
		files["web/package-lock.json"] = `"version": "` + p + `"` + "\n"
		// The guides' history section allows prose naming any release (P3).
		files["docs/guide/admin/upgrade.md"] = "# Upgrade\n\n## Per-version notes\n\n- **v" + v.ver + ":** from v" + p + "\n\nUpgrading from v" + p + " needs no migration.\n\n### v" + p + "\n\nv" + p + " added keys.\n\n## Adding a note\n\nWrite one per release.\n"
		files["docs/guide/admin/upgrade.zh-CN.md"] = "# 升级\n\n## 逐版本说明\n\n- **v" + v.ver + "：** 从 v" + p + " 升级\n\n从 v" + p + " 升级无需迁移。\n"
		files[".github/workflows/e2e.yml"] = "jobs:\n  up:\n    # object-gone) plus a plain in-place upgrade from v" + p + ". object-only\n    with:\n      old_ref: v" + p + "\n"
		files["docs/production-readiness.md"] = "以及 `plain` 从 v" + p + "）；\n"
		files["scripts/e2e/README.md"] = "以及 `plain` 从 `v" + p + "`（FBUP），从 `v" + p + "` 直接升级\n"
		if _, fails := run(t, files, []string{"v" + p}, "v"+v.ver); fails != "" {
			t.Fatalf("v%s: release history must not fail:\n%s", v.ver, fails)
		}
		// A section rule ends at the next heading of the same level.
		files["docs/guide/admin/upgrade.md"] += "Still v" + p + " here.\n"
		_, fails := run(t, files, []string{"v" + p}, "v"+v.ver)
		if !strings.Contains(fails, "docs/guide/admin/upgrade.md:16 names the previous release") || strings.Contains(fails, "upgrade.md:7 ") {
			t.Fatalf("v%s: prose after the history section must fail, inside must not, got:\n%s", v.ver, fails)
		}
		// Before the section too.
		files["docs/guide/admin/upgrade.zh-CN.md"] = "# 升级\n\n最新 v" + p + "\n\n## 逐版本说明\n"
		if _, fails := run(t, files, []string{"v" + p}, "v"+v.ver); !strings.Contains(fails, "upgrade.zh-CN.md:3 names the previous release") {
			t.Fatalf("v%s: prose before the history section must fail, got:\n%s", v.ver, fails)
		}
	}
}

func TestPRCheckFindsThePreviousRelease(t *testing.T) {
	files := Fixture("0.5.100", "0.5.99")
	files["README.md"] += "\nsee v0.5.99\n"
	// Offline: the CHANGELOG's next-older section.
	findings, _ := Check(mapSource(files), nil, "v0.5.100")
	if !strings.Contains(passes(findings), "v0.5.100 follows the previous release v0.5.99 (newest older one among the CHANGELOG.md sections)") || !strings.Contains(failures(findings), "names the previous release 0.5.99") {
		t.Fatalf("CHANGELOG: got\n%s\n%s", passes(findings), failures(findings))
	}
	// A CHANGELOG section inside a fence or comment is not a release.
	files[changelogPath] = "# Changelog\n\n## [v0.5.100] - 2026-01-01\n\n- x\n\n<!-- ## [v0.5.99] -->\n```\n## [v0.5.98]\n```\n"
	findings, _ = Check(mapSource(files), nil, "v0.5.100")
	if !strings.Contains(failures(findings), "no release older than v0.5.100 among the CHANGELOG.md sections") {
		t.Fatalf("no previous release: got\n%s", failures(findings))
	}
	delete(files, changelogPath)
	findings, _ = Check(mapSource(files), nil, "v0.5.100")
	if !strings.Contains(failures(findings), "no release older than v0.5.100") {
		t.Fatalf("no CHANGELOG: got\n%s", failures(findings))
	}
	if _, err := Check(mapSource(files), nil, "vv0.5.100"); err == nil {
		t.Fatal("a VERSION with a v prefix is not a tag")
	}
}

// P2-4: a PR that bumps VERSION fails CI without its notes and CHANGELOG
// section, before anyone pushes the tag.
func TestPRCheckRequiresNotesAndChangelog(t *testing.T) {
	cases := map[string]struct {
		edit edit
		want string
	}{
		"no English notes":           {delNotes(false), "release-notes-v0.5.100.md missing"},
		"no Chinese notes":           {delNotes(true), "v0.5.100.zh-CN.md missing"},
		"no CHANGELOG section":       {replace(changelogPath, "## [v@V@] - 2026-01-01\n\n### Fixed\n\n- retention marker\n\n", ""), "CHANGELOG.md has no '## [v0.5.100]' section"},
		"English notes only a title": {set("docs/releases/release-notes-v@V@.md", "# Binlog Server v@V@\n"), "have no body"},
	}
	for name, tc := range cases {
		files := Fixture("0.5.100", "0.5.99")
		tc.edit(files, "0.5.100", "0.5.99")
		findings, err := Check(mapSource(files), nil, "v0.5.100")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(failures(findings), tc.want) {
			t.Errorf("%s: want %q, got:\n%s", name, tc.want, failures(findings))
		}
	}
}

func TestGitRefFilesListsTheCommit(t *testing.T) {
	dir := NewFixtureRepoDir(t, "0.5.100", "0.5.99")
	head, headLinks, err := GitRef{Dir: dir, Ref: "HEAD"}.Files()
	if err != nil {
		t.Fatal(err)
	}
	work, workLinks, err := Dir(dir).Files()
	if err != nil {
		t.Fatal(err)
	}
	if len(headLinks)+len(workLinks) != 0 {
		t.Fatalf("no symlinks in the fixture: %v %v", headLinks, workLinks)
	}
	keys := func(m map[string]string) []string {
		var k []string
		for p := range m {
			k = append(k, p)
		}
		slices.Sort(k)
		return k
	}
	if !slices.Equal(keys(head), keys(work)) || len(head) < 8 {
		t.Fatalf("HEAD files %v != working tree %v", keys(head), keys(work))
	}
	for p, body := range work {
		if head[p] != body {
			t.Fatalf("%s differs between HEAD and the working tree", p)
		}
	}
	old, _, err := GitRef{Dir: dir, Ref: "v0.5.99"}.Files()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := old[notesEN("v0.5.100")]; ok {
		t.Fatal("v0.5.99 must not list the v0.5.100 notes")
	}
}

func failures(fs []Finding) string { return messages(fs, false) }
func passes(fs []Finding) string   { return messages(fs, true) }

func messages(fs []Finding, ok bool) string {
	var out []string
	for _, f := range fs {
		if f.OK == ok {
			out = append(out, f.Msg)
		}
	}
	return strings.Join(out, "\n")
}

// Symlinks are not followed (P3): a link to a directory or to a file outside
// the repo is listed with a note, and neither source reads through it.
func TestSymlinksAreNotFollowed(t *testing.T) {
	dir := NewFixtureRepoDir(t, "0.5.100", "0.5.99")
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("v0.5.99\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{"docs/link-dir": "../docs", "docs/outside.md": outside} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=f", "-c", "user.email=f@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "links"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	for _, src := range []Source{Dir(dir), GitRef{Dir: dir, Ref: "HEAD"}} {
		files, links, err := src.Files()
		if err != nil {
			t.Fatalf("%T: %v", src, err)
		}
		slices.Sort(links)
		if !slices.Equal(links, []string{"docs/link-dir", "docs/outside.md"}) {
			t.Fatalf("%T links = %v", src, links)
		}
		for p := range files {
			if strings.HasPrefix(p, "docs/link-dir") || p == "docs/outside.md" {
				t.Fatalf("%T read through the symlink %s", src, p)
			}
		}
		findings, err := Check(src, ptr(published("v0.5.99")), "v0.5.100")
		if err != nil {
			t.Fatal(err)
		}
		if f := failures(findings); f != "" {
			t.Fatalf("%T: %s", src, f)
		}
		if !strings.Contains(passes(findings), "not followed: 2 symlink(s) docs/link-dir, docs/outside.md") {
			t.Fatalf("%T: want a symlink note, got:\n%s", src, passes(findings))
		}
	}
}
