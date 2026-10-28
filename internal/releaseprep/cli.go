// Package releaseprep: the release-check command line, so the gate and its tests live in one package.
// input: release-check arguments ([-published=auto|gh|off] vX.Y.Z [git-ref]), a repo directory, and the repo's published GitHub releases (gh)
// output: one line per check; exit 1 when a release surface is missing or stale, 2 on a usage error
// pos: cmd/release-check's main calls Main; run by `make release-check` (scripts/check-release-prep.sh) and release.yml's release-prep job (-published=gh)
// note: if this file changes, update this header and README.md.
package releaseprep

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

const usage = `usage: release-check [-published=auto|gh|off] vX.Y.Z [git-ref]

Checks the release for vX.Y.Z: EN and zh release notes, the CHANGELOG section,
no copy of the previous release in any tracked file outside the release-history
allowlist (internal/releaseprep.History; failures name file:line), and every
file in the release-surface manifest (internal/releaseprep.Surfaces):
README/README_ZH, the deployment guide, the config examples, the landing page
and VERSION. Every PR runs the same checks against VERSION (internal/config
TestVersionPinsMatchVERSIONFile). With a git ref (release.yml passes the tag's
commit) files are read from that commit; without one, from the working tree.
Prerelease tags (vX.Y.Z-rc1) are rejected on purpose.

The previous release is the newest older published GitHub release (gh release
list, no drafts or prereleases) that is a tag in the release commit's history.
Every version tagged in that history or in the CHANGELOG between it and
vX.Y.Z (a release tagged but never published) is scanned too. -published=gh
requires gh (release.yml); auto (the default) falls back to the CHANGELOG when
gh is unavailable; off always takes the previous release from the CHANGELOG
sections whose tag is in the history. go test uses the same rule.
`

// publishedReleases lists the tags with a published (not draft, not
// prerelease) GitHub release; tests replace it.
var publishedReleases = listPublished

// ghReleaseList is the gh call behind listPublished: a release marked
// prerelease on GitHub is never the previous release, even when its tag is a
// plain vX.Y.Z.
var ghReleaseList = []string{"release", "list", "--limit", "1000", "--exclude-drafts", "--exclude-pre-releases", "--json", "tagName", "--jq", ".[].tagName"}

// runGh runs gh in dir; tests replace it.
var runGh = func(dir string, args ...string) (stdout []byte, err error) {
	cmd := exec.Command("gh", args...)
	cmd.Dir = dir
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}

func listPublished(dir string) ([]string, error) {
	out, err := runGh(dir, ghReleaseList...)
	if err != nil {
		return nil, fmt.Errorf("gh release list: %w", err)
	}
	return append([]string{}, strings.Fields(string(out))...), nil
}

// Main runs release-check with args in the repo at dir and returns the exit code.
func Main(args []string, dir string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("release-check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	published := fs.String("published", "auto", "")
	if err := fs.Parse(args); err != nil {
		_, _ = fmt.Fprintf(stderr, "[release-check] error: %v\n%s", err, usage)
		return 2
	}
	args = fs.Args()
	if len(args) < 1 || len(args) > 2 || (*published != "auto" && *published != "gh" && *published != "off") {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	tag := args[0]
	if _, err := ParseTag(tag); err != nil {
		_, _ = fmt.Fprintf(stderr, "[release-check] error: %v\n%s", err, usage)
		return 2
	}
	var src Source = Dir(dir)
	at, ref := "", "HEAD"
	if len(args) == 2 {
		if err := ResolveRef(dir, args[1]); err != nil {
			_, _ = fmt.Fprintf(stderr, "[release-check] error: %v\n", err)
			return 2
		}
		ref = args[1]
		src, at = GitRef{Dir: dir, Ref: ref}, " at "+ref
	}
	var rel Releases
	var err error
	if rel.Merged, err = MergedTags(dir, ref); err != nil {
		_, _ = fmt.Fprintf(stderr, "[release-check] error: %v\n", err)
		return 2
	}
	if *published != "off" {
		if rel.Published, err = publishedReleases(dir); err != nil {
			if *published == "gh" {
				_, _ = fmt.Fprintf(stderr, "[release-check] error: %v\n", err)
				return 2
			}
			_, _ = fmt.Fprintf(stdout, "[release-check] note published releases unknown (%v); using %s\n", err, "the CHANGELOG")
		}
	}
	findings, err := Check(src, &rel, tag)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "[release-check] error: %v\n", err)
		return 2
	}
	failed := 0
	for _, f := range findings {
		if f.OK {
			_, _ = fmt.Fprintf(stdout, "[release-check] ok   %s\n", f.Msg)
		} else {
			failed++
			_, _ = fmt.Fprintf(stderr, "[release-check] FAIL %s\n", f.Msg)
		}
	}
	if failed > 0 {
		_, _ = fmt.Fprintf(stderr, "[release-check] %d check(s) failed for %s%s\n", failed, tag, at)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "[release-check] all release surfaces ready for %s%s\n", tag, at)
	return 0
}
