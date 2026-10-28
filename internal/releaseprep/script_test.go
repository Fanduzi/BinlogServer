package releaseprep

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// releaseScript is what release.yml's release-prep job runs (prepStepRun). The tag gate is
// only its exit code, so the chain script -> cmd/release-check -> Main is
// tested end to end: the script builds the real binary from the copied tree
// and its exit code must be 0, 1 or 2 as documented.
const releaseScript = "scripts/check-release-prep.sh"

func TestReleaseScriptExitCodes(t *testing.T) {
	if testing.Short() {
		t.Skip("copies the repo and builds cmd/release-check through the script")
	}
	files, cur := realTree(t)
	next := nextVersions(cur)["patch"]
	edits := bumpEdits(t, files, cur.String(), next)
	const left = "docs/guide/admin/deployment.md"
	if _, ok := edits[left]; !ok {
		t.Fatalf("%s is not a manifest surface", left)
	}
	cases := []struct {
		name  string
		rc    int
		want  string
		setup func(t *testing.T, dir string)
	}{
		{"complete bump", 0, "all release surfaces ready for v" + next, func(*testing.T, string) {}},
		{"incomplete bump", 1, "FAIL " + left, func(t *testing.T, dir string) {
			writeFiles(t, dir, map[string]string{left: files[left]})
		}},
		// A directory where a tracked file belongs cannot be read, even as root.
		{"unreadable tracked file", 2, "[release-check] error: read " + left, func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, left)); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(dir, left), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"tool does not build", 2, "cmd/release-check", func(t *testing.T, dir string) {
			writeFiles(t, dir, map[string]string{"cmd/release-check/main.go": files["cmd/release-check/main.go"] + "\nfunc {\n"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := releasedCopy(t, files, cur)
			writeFiles(t, dir, edits)
			if err := os.Chmod(filepath.Join(dir, releaseScript), 0o755); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, dir)
			tmp := t.TempDir()
			cmd := exec.Command("./"+releaseScript, "-published=off", "v"+next)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "TMPDIR="+tmp, "GOTMPDIR=")
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			rc := 0
			if err := cmd.Run(); err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatal(err)
				}
				rc = exit.ExitCode()
			}
			if rc != tc.rc || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("%s exited %d (want %d), output lacks %q:\n%s", releaseScript, rc, tc.rc, tc.want, out.String())
			}
			// The script builds into a temp dir under TMPDIR; it must be gone.
			if left, err := os.ReadDir(tmp); err != nil || len(left) != 0 {
				t.Fatalf("%s left %v in TMPDIR (err %v)", releaseScript, left, err)
			}
		})
	}
}

// prepStepName and prepStepRun are release.yml's release-prep gate step, pinned
// exactly: a changed condition (an early exit on a tag), a different command
// or a swallowed exit code is a change to the gate and must change this test.
const (
	prepStepName = "Check release surfaces for the tag"
	prepStepRun  = `if [ "${GITHUB_REF_TYPE}" != "tag" ]; then
  echo "not a tag run; skipping release-prep check (run make release-check VERSION=vX.Y.Z locally)"
  exit 0
fi
./scripts/check-release-prep.sh -published=gh "${GITHUB_REF_NAME}" "${GITHUB_SHA}"
`
)

// TestReleasePrepStepOnATag runs the release-prep step from release.yml the
// way the runner does (bash -e, the job's env, a fake gh listing the current
// release as published) on a tagged copy of the repo: a tag missing a
// surface fails, a complete tag passes and a branch run skips.
func TestReleasePrepStepOnATag(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string            `yaml:"name"`
				Run  string            `yaml:"run"`
				Env  map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	var run string
	n := 0
	for _, st := range wf.Jobs["release-prep"].Steps {
		if st.Name == prepStepName {
			n++
			run = st.Run
			if len(st.Env) != 1 || st.Env["GH_TOKEN"] != "${{ github.token }}" {
				t.Errorf("release-prep step %q env must be exactly GH_TOKEN: ${{ github.token }}, got %v; if the change is intended, update this check in internal/releaseprep/script_test.go", prepStepName, st.Env)
			}
		}
	}
	if n != 1 || run != prepStepRun {
		t.Fatalf("release.yml release-prep must have one step %q running exactly\n%s\ngot %d steps, run:\n%s\nif the change is intended, update prepStepName and prepStepRun in internal/releaseprep/script_test.go with it", prepStepName, prepStepRun, n, run)
	}
	if testing.Short() {
		t.Skip("copies the repo and builds cmd/release-check through the step")
	}

	files, cur := realTree(t)
	next := nextVersions(cur)["patch"]
	edits := bumpEdits(t, files, cur.String(), next)
	const left = "docs/guide/admin/deployment.md"
	cases := []struct {
		name, refType string
		complete      bool
		rc            int
		want          string
	}{
		{"tag missing a surface", "tag", false, 1, "FAIL " + left},
		{"complete tag", "tag", true, 0, "all release surfaces ready for v" + next},
		{"branch run", "branch", false, 0, "not a tag run"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := releasedCopy(t, files, cur)
			writeFiles(t, dir, edits)
			if !tc.complete {
				writeFiles(t, dir, map[string]string{left: files[left]})
			}
			gitIn(t, dir, "add", "-A")
			gitIn(t, dir, "commit", "-qm", "v"+next)
			gitIn(t, dir, "tag", "v"+next)
			head, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
			if err != nil {
				t.Fatal(err)
			}
			sha := strings.TrimSpace(string(head))
			if err := os.Chmod(filepath.Join(dir, releaseScript), 0o755); err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			writeFiles(t, bin, map[string]string{"gh": "#!/bin/sh\necho v" + cur.String() + "\n"})
			if err := os.Chmod(filepath.Join(bin, "gh"), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-e", "-c", run)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+t.TempDir(),
				"GITHUB_REF_TYPE="+tc.refType, "GITHUB_REF_NAME=v"+next, "GITHUB_SHA="+sha, "GH_TOKEN=fake")
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			rc := 0
			if err := cmd.Run(); err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatal(err)
				}
				rc = exit.ExitCode()
			}
			if rc != tc.rc || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("release-prep step on a %s exited %d (want %d), output lacks %q:\n%s", tc.refType, rc, tc.rc, tc.want, out.String())
			}
		})
	}
}
