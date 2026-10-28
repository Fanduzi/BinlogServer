package lintguard

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A gate is only its failure status, and a release must not get past one that
// failed or never ran. The walk below visits the release job, every job it
// needs (directly or through needs) and every job of a local workflow such a
// job calls (ci calls e2e.yml, so all of e2e.yml). On each of those jobs and
// each of their steps:
//   - continue-on-error must be absent or false. On a job it makes dependents
//     see success; on a step it lets the job pass anyway.
//   - if must be absent or exactly a row of allowedIfs. A skipped job counts
//     as success inside a called workflow, a skipped step leaves its job
//     green, and always() or !cancelled() on the release job runs it after a
//     gate failed. So any other condition, even one that looks harmless,
//     fails until it is added to the table with its reason.

// allowedIf is one condition that may appear on the walk.
type allowedIf struct {
	workflow string // workflow file name
	job      string // job id, or "" for any job the walk reaches in workflow
	onStep   bool   // false: the job's own if; true: a step's if
	step     string // with onStep, the step name, or "" for any step
	// expr is the exact text. {profile} stands for the job id without its
	// "e2e-" prefix, so a profile job may only select its own profile.
	expr   string
	reason string
}

// allowedIfs is the single place a new condition on the release path is
// added, with a one-line reason why it cannot skip a gate or bypass one.
var allowedIfs = []allowedIf{
	{"e2e.yml", "", false, "", "${{ !github.event.inputs.e2e_profile }}",
		"runs unless a manual e2e run picks a profile; release.yml never sets e2e_profile, so it always runs there"},
	{"e2e.yml", "unit-and-e2e", false, "", "${{ !github.event.inputs.e2e_profile || (github.event.inputs.e2e_profile != 'cluster-roles' && github.event.inputs.e2e_profile != 'observability' && github.event.inputs.e2e_profile != 'worker-crash-recovery' && github.event.inputs.e2e_profile != 'sigstop-takeover' && github.event.inputs.e2e_profile != 'gtid-purge' && github.event.inputs.e2e_profile != 'gtid-pitr' && github.event.inputs.e2e_profile != 'coverage' && github.event.inputs.e2e_profile != 'source-switchover' && github.event.inputs.e2e_profile != 'verify' && github.event.inputs.e2e_profile != 'upgrade-failback' && github.event.inputs.e2e_profile != 'tls') }}",
		"skipped only for a manual run of another profile; with no e2e_profile (every release run) it runs"},
	{"e2e.yml", "", false, "", "${{ !github.event.inputs.e2e_profile || github.event.inputs.e2e_profile == '{profile}' }}",
		"a profile job runs with no profile (every release run) or its own"},
	{"e2e.yml", "", true, "", "failure()",
		"a step that runs only after another step failed (log dumps); the job stays failed"},
	{"e2e.yml", "", true, "", "always()",
		"a step that runs either way (teardown); it cannot turn a failed job green"},
	{"release.yml", "release", true, "Publish release assets", "${{ github.ref_type == 'tag' }}",
		"a manual branch run builds and checks snapshots but publishes nothing"},
	{"release.yml", "release", true, "Verify published Linux archive, version and commit", "${{ github.ref_type == 'tag' }}",
		"checks the uploaded archive, which exists only when the step above published"},
}

// ifAllowed returns the index of the allowedIfs row that permits cond on job
// in workflow, or -1: on the job itself, or with onStep on its step named step.
func ifAllowed(workflow, job string, onStep bool, step string, cond any) int {
	s, ok := cond.(string)
	if !ok {
		return -1
	}
	for i, a := range allowedIfs {
		if a.workflow == workflow && (a.job == "" || a.job == job) && a.onStep == onStep && (a.step == "" || a.step == step) &&
			strings.ReplaceAll(a.expr, "{profile}", strings.TrimPrefix(job, "e2e-")) == s {
			return i
		}
	}
	return -1
}

// gateFindings walks root and the jobs it needs (transitively) in workflow
// name, and every job of a local workflow those jobs call, and lists every
// continue-on-error set to anything but false and every if that no
// allowedIfs row permits. used reports which rows matched. load reads a
// workflow by file name.
func gateFindings(load func(name string) workflowFile, name, root string) (out []string, used map[int]bool) {
	used = map[int]bool{}
	checkIf := func(workflow, job string, onStep bool, step, where string, cond any) {
		if cond == nil {
			return
		}
		if i := ifAllowed(workflow, job, onStep, step, cond); i >= 0 {
			used[i] = true
			return
		}
		out = append(out, notAllowedIf(where, cond))
	}
	seenWF := map[string]bool{}
	var checkWorkflow func(name string, jobs []string)
	checkWorkflow = func(name string, jobs []string) {
		wf := load(name)
		seen := map[string]bool{}
		for len(jobs) > 0 {
			id := jobs[0]
			jobs = jobs[1:]
			if seen[id] {
				continue
			}
			seen[id] = true
			job, ok := wf.Jobs[id]
			if !ok {
				out = append(out, fmt.Sprintf("%s: job %s is needed but missing", name, id))
				continue
			}
			where := fmt.Sprintf("%s job %s", name, id)
			if softFail(job.ContinueOnError) {
				out = append(out, fmt.Sprintf("%s: continue-on-error: %v", where, job.ContinueOnError))
			}
			checkIf(name, id, false, "", where, job.If)
			for i, st := range job.Steps {
				at := fmt.Sprintf("%s step %d (%s)", where, i+1, st.Name)
				if softFail(st.ContinueOnError) {
					out = append(out, fmt.Sprintf("%s: continue-on-error: %v", at, st.ContinueOnError))
				}
				checkIf(name, id, true, st.Name, at, st.If)
			}
			jobs = append(jobs, needsList(job.Needs)...)
			if called, ok := strings.CutPrefix(job.Uses, "./.github/workflows/"); ok && !seenWF[called] {
				seenWF[called] = true
				cw := load(called)
				all := make([]string, 0, len(cw.Jobs))
				for j := range cw.Jobs {
					all = append(all, j)
				}
				slices.Sort(all)
				checkWorkflow(called, all)
			}
		}
	}
	checkWorkflow(name, []string{root})
	return out, used
}

// notAllowedIf is the finding for a condition no allowedIfs row permits.
func notAllowedIf(where string, cond any) string {
	return fmt.Sprintf("%s: if: %v is not allowed on the release path; if it cannot skip or bypass a gate, add or update a row with its reason in allowedIfs (internal/lintguard/release_walk_test.go)", where, cond)
}

// staleRow is the finding for an allowedIfs row that matches nothing.
func staleRow(row string) string {
	return fmt.Sprintf("allowedIfs row %s matches nothing; remove or update it", row)
}

// softFail is true for any continue-on-error but absent or false, so an
// expression that may evaluate to true counts too.
func softFail(v any) bool { return v != nil && v != false }

// releaseGateFindings checks the release job and everything it waits for.
// The synthetic test goes through it too, so the root it starts from is tested.
func releaseGateFindings(load func(name string) workflowFile) ([]string, map[int]bool) {
	return gateFindings(load, "release.yml", "release")
}

func TestReleaseGatesCannotBeBypassed(t *testing.T) {
	load := func(name string) workflowFile { return readWorkflow(t, name) }
	if needs := needsList(readWorkflow(t, "release.yml").Jobs["release"].Needs); !slices.Contains(needs, "release-prep") || !slices.Contains(needs, "ci") {
		t.Fatalf("release.yml job release must need ci and release-prep, got %v", needs)
	}
	out, used := releaseGateFindings(load)
	for _, f := range out {
		t.Error(f)
	}
	// Every row is a condition that exists today; a stale row would let it
	// come back unreviewed.
	for _, r := range staleAllowedIfs(used) {
		t.Error(staleRow(r))
	}
}

// staleAllowedIfs names the allowedIfs rows not in used.
func staleAllowedIfs(used map[int]bool) []string {
	var out []string
	for i, a := range allowedIfs {
		if !used[i] {
			out = append(out, fmt.Sprintf("%d (%s job %q step %q: %q)", i, a.workflow, a.job, a.step, a.expr))
		}
	}
	return out
}

func TestGateFindingsFindsEveryLevel(t *testing.T) {
	files := map[string]string{
		"release.yml": `jobs:
  ci:
    uses: ./.github/workflows/e2e.yml
    continue-on-error: true
  release-prep:
    needs: [lint, gone]
    steps:
      - name: gate
        run: ./gate
        continue-on-error: ${{ inputs.soft }}
      - name: fine
        run: x
        continue-on-error: false
      - name: skipped
        if: false
  lint:
    continue-on-error: "true"
    if: ${{ !github.event.inputs.e2e_profile }}
  unrelated:
    continue-on-error: true
    if: always()
  release:
    needs: [ci, release-prep]
    if: ${{ !cancelled() }}
    steps:
      - name: Publish release assets
        if: ${{ github.ref_type == 'tag' }}
      - name: Publish elsewhere
        if: ${{ github.ref_type == 'tag' }}
`,
		"e2e.yml": `jobs:
  a:
    steps:
      - name: s
        continue-on-error: true
      - name: Cleanup
        if: always()
      - name: Dump Logs On Failure
        if: always()
      - name: Clean up, renamed
        if: always()
      - name: Dump logs, renamed
        if: failure()
      - name: prefix
        if: always() && false
      - name: job text on a step
        if: ${{ !github.event.inputs.e2e_profile }}
  b:
    continue-on-error: false
    if: ${{ !github.event.inputs.e2e_profile }}
  e2e-tls:
    if: ${{ !github.event.inputs.e2e_profile || github.event.inputs.e2e_profile == 'tls' }}
  e2e-verify:
    if: ${{ !github.event.inputs.e2e_profile || github.event.inputs.e2e_profile == 'tls' }}
  c:
    if: ${{ always() && !github.event.inputs.e2e_profile }}
  d:
    if: ` + allowedIfs[1].expr + `
    steps:
      - name: Cleanup
        if: false || always()
  f:
    if: always()
`,
	}
	load := func(name string) workflowFile { return parseWorkflow(t, name, []byte(files[name])) }
	got, used := releaseGateFindings(load)
	slices.Sort(got)
	want := []string{
		"e2e.yml job a step 1 (s): continue-on-error: true",
		notAllowedIf("e2e.yml job a step 6 (prefix)", `always() && false`),
		notAllowedIf("e2e.yml job a step 7 (job text on a step)", `${{ !github.event.inputs.e2e_profile }}`),
		notAllowedIf("e2e.yml job c", `${{ always() && !github.event.inputs.e2e_profile }}`),
		notAllowedIf("e2e.yml job d step 1 (Cleanup)", `false || always()`),
		notAllowedIf("e2e.yml job d", allowedIfs[1].expr),
		notAllowedIf("e2e.yml job e2e-verify", `${{ !github.event.inputs.e2e_profile || github.event.inputs.e2e_profile == 'tls' }}`),
		notAllowedIf("e2e.yml job f", `always()`),
		"release.yml job ci: continue-on-error: true",
		"release.yml job lint: continue-on-error: true",
		notAllowedIf("release.yml job lint", `${{ !github.event.inputs.e2e_profile }}`),
		notAllowedIf("release.yml job release step 2 (Publish elsewhere)", `${{ github.ref_type == 'tag' }}`),
		"release.yml job release-prep step 1 (gate): continue-on-error: ${{ inputs.soft }}",
		notAllowedIf("release.yml job release-prep step 3 (skipped)", `false`),
		notAllowedIf("release.yml job release", `${{ !cancelled() }}`),
		"release.yml: job gone is needed but missing",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	// e2e.yml b, e2e-tls, Cleanup and Dump steps under any name and the
	// release publish step are permitted;
	// the other rows are not used here, so they are reported as stale.
	if stale := staleAllowedIfs(used); len(stale) != 2 || !strings.HasPrefix(stale[0], "1 ") || !strings.HasPrefix(stale[1], "6 ") {
		t.Errorf("stale rows %q, want rows 1 and 6", stale)
	}
}

// TestAllowedIfsMessagesSayWhatToDo: adding a profile to unit-and-e2e's
// exclusion list is a legitimate edit, so the findings must point at the
// table and say to update the row, not that a gate was skipped or that the
// row should go.
func TestAllowedIfsMessagesSayWhatToDo(t *testing.T) {
	cond := strings.Replace(allowedIfs[1].expr, ") }}", " && github.event.inputs.e2e_profile != 'new-profile') }}", 1)
	files := map[string]string{
		"release.yml": "jobs:\n  ci:\n    uses: ./.github/workflows/e2e.yml\n  release:\n    needs: [ci]\n",
		"e2e.yml":     "jobs:\n  unit-and-e2e:\n    if: " + cond + "\n",
	}
	load := func(name string) workflowFile { return parseWorkflow(t, name, []byte(files[name])) }
	got, used := releaseGateFindings(load)
	if len(got) != 1 || !strings.HasPrefix(got[0], "e2e.yml job unit-and-e2e: if: "+cond+" ") {
		t.Fatalf("got %q, want one finding for unit-and-e2e", got)
	}
	for _, s := range []string{"internal/lintguard/release_walk_test.go", "allowedIfs", "add or update a row with its reason"} {
		if !strings.Contains(got[0], s) {
			t.Errorf("finding %q does not say %q", got[0], s)
		}
	}
	if strings.Contains(got[0], "gate the release needs") {
		t.Errorf("finding %q calls the condition a gate", got[0])
	}
	stale := staleAllowedIfs(used)
	if !slices.ContainsFunc(stale, func(r string) bool { return strings.HasPrefix(r, "1 ") }) {
		t.Fatalf("row 1 not reported stale: %q", stale)
	}
	if msg := staleRow(stale[0]); !strings.HasSuffix(msg, " matches nothing; remove or update it") {
		t.Errorf("stale message %q must say to remove or update the row", msg)
	}
}

// TestRenamingConditionalStepsPasses: the always() and failure() rows hold
// for any step, so renaming the e2e.yml Cleanup and Dump Logs On Failure
// steps in the real workflows reports nothing.
func TestRenamingConditionalStepsPasses(t *testing.T) {
	rename := strings.NewReplacer("- name: Cleanup\n", "- name: Clean up\n", "- name: Dump Logs On Failure\n", "- name: Dump logs\n")
	load := func(name string) workflowFile {
		raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", name))
		if err != nil {
			t.Fatal(err)
		}
		renamed := rename.Replace(string(raw))
		if name == "e2e.yml" && renamed == string(raw) {
			t.Fatal("e2e.yml has no Cleanup or Dump Logs On Failure step to rename")
		}
		return parseWorkflow(t, name, []byte(renamed))
	}
	if out, _ := releaseGateFindings(load); len(out) != 0 {
		t.Errorf("renamed steps reported: %q", out)
	}
}
