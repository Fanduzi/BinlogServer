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
	On   map[string]any `yaml:"on"`
	Jobs map[string]struct {
		If    string `yaml:"if"`
		Uses  string `yaml:"uses"`
		Needs any    `yaml:"needs"`
	} `yaml:"jobs"`
}

func readWorkflow(t *testing.T, name string) workflowFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
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
// and publishes only after the whole e2e.yml workflow passed on that commit,
// and no e2e.yml job condition depends on the event or ref, which would make
// it skip when release.yml calls the workflow.
func TestReleaseWaitsForEveryE2EJob(t *testing.T) {
	release := readWorkflow(t, "release.yml")
	ci, ok := release.Jobs["ci"]
	if !ok || ci.Uses != "./.github/workflows/e2e.yml" {
		t.Fatalf("release.yml needs a job ci that uses ./.github/workflows/e2e.yml, got %+v", ci)
	}
	for name, job := range release.Jobs {
		if name == "ci" {
			continue
		}
		if !slices.Contains(needsList(job.Needs), "ci") {
			t.Errorf("release.yml job %s does not need ci", name)
		}
	}

	e2e := readWorkflow(t, "e2e.yml")
	if _, ok := e2e.On["workflow_call"]; !ok {
		t.Fatal("e2e.yml must be callable (on: workflow_call) so release.yml can run it")
	}
	for name, job := range e2e.Jobs {
		// A called workflow sees the caller's event (push for a tag,
		// workflow_dispatch for a manual release run), so a condition on the
		// event name or ref can skip the job before a release.
		if strings.Contains(job.If, "event_name") || strings.Contains(job.If, "github.ref") {
			t.Errorf("e2e.yml job %s keys on the event or ref (if: %s); key on github.event.inputs.e2e_profile so it also runs when release.yml calls it", name, job.If)
		}
	}
}
