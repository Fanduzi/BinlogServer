package releaseprep

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Tests never call gh: by default the published releases are unknown, so
// -published=auto falls back to the CHANGELOG.
func TestMain(m *testing.M) {
	publishedReleases = func(string) ([]string, error) { return nil, errors.New("gh: not in tests") }
	os.Exit(m.Run())
}

func withPublished(t *testing.T, tags ...string) {
	t.Helper()
	old := publishedReleases
	publishedReleases = func(string) ([]string, error) { return tags, nil }
	t.Cleanup(func() { publishedReleases = old })
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=f", "-c", "user.email=f@example.invalid", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func call(dir string, args ...string) (int, string) {
	var out bytes.Buffer
	rc := Main(args, dir, &out, &out)
	return rc, out.String()
}

func TestRun(t *testing.T) {
	dir := NewFixtureRepoDir(t, "0.5.100", "0.5.99")
	cases := []struct {
		name string
		args []string
		rc   int
		want string
	}{
		{"working tree passes", []string{"v0.5.100"}, 0, "all release surfaces ready for v0.5.100\n"},
		{"tagged commit passes", []string{"v0.5.100", "HEAD"}, 0, "all release surfaces ready for v0.5.100 at HEAD"},
		{"previous commit fails", []string{"v0.5.100", "v0.5.99"}, 1, "check(s) failed for v0.5.100 at v0.5.99"},
		{"no arguments", nil, 2, "usage: release-check"},
		{"too many arguments", []string{"v0.5.100", "HEAD", "x"}, 2, "usage: release-check"},
		{"rc tag rejected on purpose", []string{"v0.5.100-rc1"}, 2, "no prerelease suffix"},
		{"version without v", []string{"0.5.100"}, 2, "must look like vX.Y.Z"},
		{"unknown ref", []string{"v0.5.100", "no-such-ref"}, 2, `unknown ref "no-such-ref"`},
		{"auto falls back to the CHANGELOG", []string{"v0.5.100"}, 0, "note published releases unknown (gh: not in tests); using the CHANGELOG"},
		{"off uses the CHANGELOG", []string{"-published=off", "v0.5.100"}, 0, "among the CHANGELOG.md sections (offline"},
		{"gh required", []string{"-published=gh", "v0.5.100"}, 2, "error: gh: not in tests"},
		{"unknown published mode", []string{"-published=yes", "v0.5.100"}, 2, "usage: release-check"},
		{"unknown flag", []string{"-x", "v0.5.100"}, 2, "usage: release-check"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc, out := call(dir, tc.args...)
			if rc != tc.rc || !strings.Contains(out, tc.want) {
				t.Fatalf("rc=%d (want %d), output lacks %q:\n%s", rc, tc.rc, tc.want, out)
			}
			if rc == 1 && !strings.Contains(out, "[release-check] FAIL ") {
				t.Fatalf("a failing run must print FAIL lines:\n%s", out)
			}
		})
	}
}

// With a ref, the tags that count are the ones in the ref's history, not
// HEAD's: v0.5.101 tagged on a later HEAD must not make v0.5.100 at its own
// tag look older than a release in its history.
func TestRefTagsComeFromTheRef(t *testing.T) {
	dir := NewFixtureRepoDir(t, "0.5.100", "0.5.99")
	gitIn(t, dir, "tag", "v0.5.100")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "next")
	gitIn(t, dir, "tag", "v0.5.101")
	if rc, out := call(dir, "-published=off", "v0.5.100", "v0.5.100"); rc != 0 {
		t.Fatalf("v0.5.100 at its tag: rc=%d:\n%s", rc, out)
	}
	if rc, out := call(dir, "-published=off", "v0.5.100", "HEAD"); rc != 1 || !strings.Contains(out, "v0.5.101") {
		t.Fatalf("v0.5.100 at HEAD (v0.5.101 in history) must fail naming it: rc=%d:\n%s", rc, out)
	}
}

// A depth-1 clone has no tags; the check must say so instead of exiting silently.
func TestShallowCloneSaysWhy(t *testing.T) {
	src := NewFixtureRepoDir(t, "0.5.100", "0.5.99")
	dst := filepath.Join(t.TempDir(), "shallow")
	if out, err := exec.Command("git", "clone", "-q", "--depth", "1", "--no-tags", "file://"+src, dst).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	rc, out := call(dst, "v0.5.100")
	if rc != 1 || !strings.Contains(out, "a shallow clone (fetch-depth 1) has no tags: run `git fetch --tags`") {
		t.Fatalf("rc=%d:\n%s", rc, out)
	}
	if _, err := os.Stat(filepath.Join(dst, "VERSION")); err != nil {
		t.Fatal(err)
	}
}

// -published=off never asks GitHub, even when it would answer.
func TestPublishedOffDoesNotAskGitHub(t *testing.T) {
	dir := NewFixtureRepoDir(t, "0.5.100", "0.5.99")
	publishedCalled := false
	old := publishedReleases
	publishedReleases = func(string) ([]string, error) { publishedCalled = true; return []string{"v0.5.99"}, nil }
	t.Cleanup(func() { publishedReleases = old })
	if rc, out := call(dir, "-published=off", "v0.5.100"); rc != 0 || publishedCalled || !strings.Contains(out, "CHANGELOG.md sections (offline") {
		t.Fatalf("rc=%d called=%v:\n%s", rc, publishedCalled, out)
	}
	if rc, out := call(dir, "v0.5.100"); rc != 0 || !publishedCalled || !strings.Contains(out, "among the published GitHub releases") {
		t.Fatalf("auto: rc=%d called=%v:\n%s", rc, publishedCalled, out)
	}
}

// P2-2: the previous release is the newest older tag that is an ancestor of
// the release commit and has a published GitHub release. A tag left by a
// failed release (v0.5.99 here, never published) is skipped, so the scan
// looks for the last published release, v0.5.98, and finds its stale copy.
func TestPreviousReleaseIsPublishedAncestor(t *testing.T) {
	dir := NewFixtureRepoDir(t, "0.5.100", "0.5.99")
	gitIn(t, dir, "tag", "v0.5.98", "v0.5.99")
	// A published release on another branch is not an ancestor.
	gitIn(t, dir, "checkout", "-q", "-b", "side", "v0.5.99")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "side")
	gitIn(t, dir, "tag", "v0.5.97")
	gitIn(t, dir, "checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(dir, "docs", "guide.md"), []byte("Install v0.5.98.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "stale")

	withPublished(t, "v0.5.98", "v0.5.97")
	for _, args := range [][]string{{"-published=gh", "v0.5.100"}, {"-published=gh", "v0.5.100", "HEAD"}} {
		rc, out := call(dir, args...)
		if rc != 1 || !strings.Contains(out, "docs/guide.md:1 names the previous release 0.5.98") || !strings.Contains(out, "follows the previous release v0.5.98") {
			t.Fatalf("%v: rc=%d, want the stale v0.5.98 copy:\n%s", args, rc, out)
		}
	}
	// Published v0.5.99 too: it is the previous release, and the tree passes.
	withPublished(t, "v0.5.99", "v0.5.98")
	if rc, out := call(dir, "-published=gh", "v0.5.100"); rc != 0 || !strings.Contains(out, "follows the previous release v0.5.99") {
		t.Fatalf("rc=%d:\n%s", rc, out)
	}
	// The only published release is on another branch: not an ancestor.
	withPublished(t, "v0.5.97")
	if rc, out := call(dir, "-published=gh", "v0.5.100"); rc != 1 || !strings.Contains(out, "no release older than v0.5.100 among the published GitHub releases that are tags in its history") {
		t.Fatalf("rc=%d:\n%s", rc, out)
	}
}

// A minor bump still scans the previous minor's last release: 0.5.83 left in
// a tarball path fails v0.6.0, from the published tag and offline.
func TestMinorBumpScansThePreviousMinor(t *testing.T) {
	dir := NewFixtureRepoDir(t, "0.6.0", "0.5.83")
	rc, out := call(dir, "-published=off", "v0.6.0")
	if rc != 0 || !strings.Contains(out, "follows the previous release v0.5.83") {
		t.Fatalf("clean minor bump: rc=%d:\n%s", rc, out)
	}
	readme := filepath.Join(dir, "README.md")
	raw, err := os.ReadFile(readme)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(readme, append(raw, "\ncurl -LO https://example.invalid/releases/download/v0.6.0/binlog-server_0.5.83_linux_amd64.tar.gz\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	line := strings.Count(string(raw), "\n") + 2
	gitIn(t, dir, "commit", "-qam", "leftover tarball path")
	want := fmt.Sprintf("README.md:%d names the previous release 0.5.83", line)
	withPublished(t, "v0.5.83")
	for _, args := range [][]string{{"-published=gh", "v0.6.0"}, {"-published=gh", "v0.6.0", "HEAD"}, {"-published=off", "v0.6.0"}} {
		if rc, out := call(dir, args...); rc != 1 || !strings.Contains(out, want) {
			t.Fatalf("%v: rc=%d, want %q:\n%s", args, rc, want, out)
		}
	}
}

// A release that was tagged but never published (v0.5.84) still wrote its
// version everywhere: bumping to v0.5.85 with the deployment guide left at
// v0.5.84 fails the tag gate (gh and offline) and every PR's go test alike.
func TestUnpublishedTaggedReleaseIsScanned(t *testing.T) {
	dir := NewFixtureRepoDir(t, "0.5.84", "0.5.83")
	gitIn(t, dir, "tag", "v0.5.84")
	const deploy = "docs/guide/admin/deployment.md"
	old, err := os.ReadFile(filepath.Join(dir, deploy))
	if err != nil {
		t.Fatal(err)
	}
	for path, body := range Fixture("0.5.85", "0.5.84") {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "v0.5.85")
	withPublished(t, "v0.5.83")
	if rc, out := call(dir, "-published=gh", "v0.5.85", "HEAD"); rc != 0 || !strings.Contains(out, "follows the previous release v0.5.83") || !strings.Contains(out, "also scanning v0.5.84") {
		t.Fatalf("complete bump: rc=%d:\n%s", rc, out)
	}
	if err := os.WriteFile(filepath.Join(dir, deploy), old, 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "commit", "-qam", "deployment guide left at v0.5.84")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-published=gh", "v0.5.85"}, deploy + ":"},
		{[]string{"-published=gh", "v0.5.85", "HEAD"}, " names the unpublished release 0.5.84"},
		{[]string{"-published=off", "v0.5.85"}, " names the previous release 0.5.84"},
	} {
		if rc, out := call(dir, tc.args...); rc != 1 || !strings.Contains(out, tc.want) || !strings.Contains(out, deploy+":") {
			t.Fatalf("%v: rc=%d, want %q:\n%s", tc.args, rc, tc.want, out)
		}
	}
	findings, err := Check(Dir(dir), nil, "v0.5.85")
	if err != nil {
		t.Fatal(err)
	}
	var fails []string
	for _, f := range findings {
		if !f.OK {
			fails = append(fails, f.Msg)
		}
	}
	if !strings.Contains(strings.Join(fails, "\n"), deploy+":") {
		t.Fatalf("go test (Check with no tags) must fail too, got %q", fails)
	}
}

// listPublished asks gh for published releases only: drafts and releases
// marked prerelease on GitHub (even with a plain vX.Y.Z tag) are excluded.
func TestListPublishedExcludesDraftsAndPrereleases(t *testing.T) {
	var got []string
	old := runGh
	runGh = func(dir string, args ...string) ([]byte, error) {
		got = args
		return []byte("v0.5.99\nv0.5.98\n"), nil
	}
	t.Cleanup(func() { runGh = old })
	tags, err := listPublished(".")
	if err != nil || strings.Join(tags, ",") != "v0.5.99,v0.5.98" {
		t.Fatalf("tags %q, err %v", tags, err)
	}
	want := "release list --limit 1000 --exclude-drafts --exclude-pre-releases --json tagName --jq .[].tagName"
	if strings.Join(got, " ") != want {
		t.Fatalf("gh %q, want gh %q", strings.Join(got, " "), want)
	}
	runGh = func(string, ...string) ([]byte, error) { return nil, fmt.Errorf("no auth") }
	if _, err := listPublished("."); err == nil || !strings.Contains(err.Error(), "gh release list: no auth") {
		t.Fatalf("err %v", err)
	}
}
