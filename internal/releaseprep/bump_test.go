package releaseprep

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The repo as it is today must be releasable by the documented bump alone:
// copy the real tracked tree, change only the manifest surfaces, the release
// notes and the CHANGELOG to the next patch, minor and major version, and the
// tag gate plus the internal/config and internal/releaseprep tests must pass.
// A doc that names the current version outside the manifest fails here, in
// the PR that adds it, not at the next release. Leaving out any one surface,
// note or CHANGELOG section must fail the gate and the PR check.

func realTree(t *testing.T) (files map[string]string, cur Version) {
	t.Helper()
	root := filepath.Join("..", "..")
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "-s").Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	files = map[string]string{}
	for _, rec := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		meta, path, ok := strings.Cut(rec, "\t")
		if !ok || strings.HasPrefix(meta, "120000 ") || strings.HasPrefix(meta, "160000 ") {
			continue // symlinks and submodules are not scanned
		}
		body, err := os.ReadFile(filepath.Join(root, path))
		if os.IsNotExist(err) {
			continue // deleted in the worktree, not yet committed
		}
		if err != nil {
			t.Fatal(err)
		}
		files[path] = string(body)
	}
	cur, err = ParseTag("v" + strings.TrimSpace(files["VERSION"]))
	if err != nil {
		t.Fatal(err)
	}
	return files, cur
}

// bumpEdits is the documented release-prep change from cur to next: every
// manifest surface with cur replaced, both release notes and a CHANGELOG
// section. Only those paths change.
func bumpEdits(t *testing.T, files map[string]string, cur, next string) map[string]string {
	t.Helper()
	edits := map[string]string{}
	for _, s := range Surfaces {
		body, ok := files[s.Path]
		if !ok {
			t.Fatalf("manifest surface %s is not tracked", s.Path)
		}
		edits[s.Path] = replaceVersion(body, cur, next)
	}
	edits["docs/releases/release-notes-v"+next+".md"] = "# Binlog Server v" + next + "\n\n" +
		"Release v" + next + " is a maintenance release. It carries the fixes merged since the previous release.\n"
	edits["docs/releases/v"+next+".zh-CN.md"] = "# Binlog Server v" + next + "\n\n" +
		"这是一个维护版本，包含上一个版本以来合入的所有修复和改进。升级方式与上一个版本相同，直接替换二进制文件并重启服务即可，不需要迁移元数据库，也不需要修改配置文件。请先阅读升级指南。\n"
	cl := files["CHANGELOG.md"]
	i := strings.Index(cl, "\n## [v")
	if i < 0 {
		t.Fatal("CHANGELOG.md has no release section")
	}
	edits["CHANGELOG.md"] = cl[:i+1] + "## [v" + next + "] - 2099-01-01\n\n### Fixed\n\n- maintenance\n\n" + cl[i+1:]
	return edits
}

// replaceVersion replaces every copy of cur not inside a longer number.
func replaceVersion(body, cur, next string) string {
	var b strings.Builder
	for {
		i := strings.Index(body, cur)
		if i < 0 {
			return b.String() + body
		}
		j := i + len(cur)
		inNumber := (i > 0 && strings.ContainsRune("0123456789.", rune(body[i-1]))) ||
			(j < len(body) && (body[j] >= '0' && body[j] <= '9' || j+1 < len(body) && body[j] == '.' && body[j+1] >= '0' && body[j+1] <= '9'))
		b.WriteString(body[:i])
		if inNumber {
			b.WriteString(cur)
		} else {
			b.WriteString(next)
		}
		body = body[j:]
	}
}

func TestReplaceVersion(t *testing.T) {
	got := replaceVersion("v1.2.3 VER=1.2.3 x_1.2.3_y 11.2.3 1.2.30 1.2.3.4 1.2.3.", "1.2.3", "1.2.4")
	if want := "v1.2.4 VER=1.2.4 x_1.2.4_y 11.2.3 1.2.30 1.2.3.4 1.2.4."; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func nextVersions(cur Version) map[string]string {
	return map[string]string{
		"patch": fmt.Sprintf("%d.%d.%d", cur[0], cur[1], cur[2]+1),
		"minor": fmt.Sprintf("%d.%d.0", cur[0], cur[1]+1),
		"major": fmt.Sprintf("%d.0.0", cur[0]+1),
	}
}

func writeFiles(t *testing.T, dir string, m map[string]string) {
	t.Helper()
	for path, body := range m {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// releasedCopy is a git repo holding the real tracked tree, committed and
// tagged as the current release.
func releasedCopy(t *testing.T, files map[string]string, cur Version) string {
	t.Helper()
	dir := t.TempDir()
	writeFiles(t, dir, files)
	gitIn(t, dir, "init", "-q", "-b", "main")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "v"+cur.String())
	gitIn(t, dir, "tag", "v"+cur.String())
	return dir
}

func TestRealTreeBumpsCleanly(t *testing.T) {
	if testing.Short() {
		t.Skip("copies the repo and runs go test in it")
	}
	files, cur := realTree(t)
	for kind, next := range nextVersions(cur) {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			dir := releasedCopy(t, files, cur)
			writeFiles(t, dir, bumpEdits(t, files, cur.String(), next))
			gitIn(t, dir, "add", "-A")
			gitIn(t, dir, "commit", "-qm", "v"+next)
			for _, args := range [][]string{{"-published=off", "v" + next}, {"-published=off", "v" + next, "HEAD"}} {
				if rc, out := call(dir, args...); rc != 0 {
					t.Fatalf("release-check %v on the bumped real tree: rc=%d\n%s", args, rc, out)
				}
			}
			// Everything but this test itself, which would copy the copy.
			cmd := exec.Command("go", "test", "-count=1", "-skip", "^TestRealTreeBumpsCleanly$", "./internal/config", "./internal/releaseprep")
			cmd.Dir = dir
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Run(); err != nil {
				t.Fatalf("go test on the tree bumped to v%s: %v\n%s", next, err, out.String())
			}
		})
	}
}

func TestRealTreeBumpMissingAnySiteFails(t *testing.T) {
	files, cur := realTree(t)
	next := nextVersions(cur)["patch"]
	edits := bumpEdits(t, files, cur.String(), next)
	rel := Releases{Merged: []string{"v" + cur.String()}, Published: []string{"v" + cur.String()}}
	bumped := func(skip string) mapSource {
		m := mapSource{}
		for p, b := range files {
			m[p] = b
		}
		for p, b := range edits {
			if p != skip {
				m[p] = b
			}
		}
		return m
	}
	for _, skip := range append([]string{""}, keys(edits)...) {
		name := "without " + skip
		if skip == "" {
			name = "complete bump"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			src := bumped(skip)
			gate, err := Check(src, &rel, "v"+next)
			if err != nil {
				t.Fatal(err)
			}
			pr, err := Check(src, nil, "v"+next)
			if err != nil {
				t.Fatal(err)
			}
			if skip == "" {
				if f := failures(gate) + failures(pr); f != "" {
					t.Fatalf("complete bump to v%s fails:\n%s", next, f)
				}
				return
			}
			if failures(gate) == "" {
				t.Errorf("bump to v%s without %s passes the tag gate", next, skip)
			}
			// The PR check reads the version from VERSION, so it cannot miss VERSION itself.
			if skip != "VERSION" && failures(pr) == "" {
				t.Errorf("bump to v%s without %s passes the PR check", next, skip)
			}
		})
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
