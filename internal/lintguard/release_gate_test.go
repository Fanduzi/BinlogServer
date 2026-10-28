package lintguard

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type workflowFile struct {
	On   map[string]any         `yaml:"on"`
	Jobs map[string]workflowJob `yaml:"jobs"`
}

type workflowJob struct {
	If              any            `yaml:"if"`
	Uses            string         `yaml:"uses"`
	Needs           any            `yaml:"needs"`
	ContinueOnError any            `yaml:"continue-on-error"`
	Steps           []workflowStep `yaml:"steps"`
}

type workflowStep struct {
	Name            string            `yaml:"name"`
	Uses            string            `yaml:"uses"`
	Run             string            `yaml:"run"`
	With            map[string]any    `yaml:"with"`
	Env             map[string]string `yaml:"env"`
	If              any               `yaml:"if"`
	ContinueOnError any               `yaml:"continue-on-error"`
}

func readWorkflow(t *testing.T, name string) workflowFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	return parseWorkflow(t, name, raw)
}

func parseWorkflow(t *testing.T, name string, raw []byte) workflowFile {
	t.Helper()
	var wf workflowFile
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return wf
}

func needsList(v any) []string {
	switch n := v.(type) {
	case string:
		return []string{n}
	case []any:
		var out []string
		for _, x := range n {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// TestReleaseWaitsForEveryE2EJob locks production-readiness J3: a tag builds
// and publishes only after the whole e2e.yml workflow passed on that commit.
// Conditions and continue-on-error on that path are checked by
// TestReleaseGatesCannotBeBypassed.
func TestReleaseWaitsForEveryE2EJob(t *testing.T) {
	release := readWorkflow(t, "release.yml")
	ci, ok := release.Jobs["ci"]
	if !ok || ci.Uses != "./.github/workflows/e2e.yml" {
		t.Fatalf("release.yml needs a job ci that uses ./.github/workflows/e2e.yml, got %+v", ci)
	}
	// release-prep only reads files, so it runs beside ci and fails fast;
	// release must still wait for both.
	for name, job := range release.Jobs {
		if name == "ci" || name == "release-prep" {
			continue
		}
		if !slices.Contains(needsList(job.Needs), "ci") {
			t.Errorf("release.yml job %s does not need ci", name)
		}
	}
	if !slices.Contains(needsList(release.Jobs["release"].Needs), "release-prep") {
		t.Error("release.yml job release must need release-prep, or a tag missing its notes, pins or landing page still publishes")
	}

	if _, ok := readWorkflow(t, "e2e.yml").On["workflow_call"]; !ok {
		t.Fatal("e2e.yml must be callable (on: workflow_call) so release.yml can run it")
	}
}

// TestReleasePrepGateIsWired locks how the release-prep gate runs, since a
// workflow or Makefile edit can silently stop it from checking anything:
//   - `make release-check` runs the gate and then the full internal/config
//     and internal/releaseprep go tests (no -run filter, not conditional),
//     so a hidden or missing site fails both;
//   - release.yml's release-prep job checks out the full history (the
//     previous release is a tag in the release commit's history); the gate
//     step's exact text and its behaviour on a tag are pinned in
//     internal/releaseprep (TestReleasePrepStepOnATag);
//   - e2e.yml's release-prep-selftest runs `make release-check` on every PR
//     from a full-history checkout, so the gate itself runs, not only its tests.
func TestReleasePrepGateIsWired(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	makefile := string(raw)
	recipe := func(target string) []string {
		var out []string
		in := false
		for _, l := range strings.Split(makefile, "\n") {
			switch {
			case strings.HasPrefix(l, target+":"):
				in = true
			case in && strings.HasPrefix(l, "\t"):
				out = append(out, expandMake(makefile, strings.TrimSpace(l)))
			case in:
				return out
			}
		}
		return out
	}
	fullTests := func(line string) bool {
		f := strings.Fields(line)
		return len(f) > 2 && f[0] == "go" && f[1] == "test" && !strings.Contains(line, "-run") && !strings.Contains(line, "$(if") &&
			slices.Contains(f, "./internal/config") && slices.Contains(f, "./internal/releaseprep")
	}
	check := recipe("release-check")
	if len(check) != 2 || !strings.HasPrefix(check[0], "./scripts/check-release-prep.sh ") || !fullTests(check[1]) {
		t.Errorf("make release-check must run ./scripts/check-release-prep.sh and then go test ./internal/config ./internal/releaseprep unconditionally, got %q", check)
	}
	if test := recipe("release-check-test"); len(test) != 1 || !fullTests(test[0]) {
		t.Errorf("make release-check-test must run the full gate tests, got %q", test)
	}

	fullCheckout := func(steps []workflowStep) bool {
		for _, s := range steps {
			if strings.HasPrefix(s.Uses, "actions/checkout@") {
				return s.With["fetch-depth"] == 0
			}
		}
		return false
	}
	runs := func(steps []workflowStep, cmd string) bool {
		for _, s := range steps {
			for _, l := range strings.Split(s.Run, "\n") {
				if strings.TrimSpace(l) == cmd && s.Env["GH_TOKEN"] != "" {
					return true
				}
			}
		}
		return false
	}
	prep := readWorkflow(t, "release.yml").Jobs["release-prep"]
	if !fullCheckout(prep.Steps) {
		t.Error("release.yml release-prep must check out with fetch-depth: 0 (the previous release is a tag in the release commit's history)")
	}
	self, ok := readWorkflow(t, "e2e.yml").Jobs["release-prep-selftest"]
	if !ok || !fullCheckout(self.Steps) || !runs(self.Steps, "make release-check") {
		t.Error("e2e.yml release-prep-selftest must check out with fetch-depth: 0 and run make release-check with GH_TOKEN set")
	}
}

// expandMake expands $(NAME) variables defined as NAME = value in makefile.
func expandMake(makefile, line string) string {
	for i := 0; i < 5 && strings.Contains(line, "$("); i++ {
		for _, l := range strings.Split(makefile, "\n") {
			name, value, ok := strings.Cut(l, " = ")
			if ok && !strings.ContainsAny(name, " \t:") {
				line = strings.ReplaceAll(line, "$("+name+")", strings.TrimSpace(value))
			}
		}
	}
	return line
}
