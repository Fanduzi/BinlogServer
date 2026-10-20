//go:build ignore

// Package main replays the review mutants against the ownership tests.
// input: internal/ownership Go sources and the mutant table in this file
// output: a kill table on stdout; exit status 1 when a mutant survives
// pos: the reproducible mutation harness, excluded from go test
// note: if this file changes, update this header and module README.md.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

type mutant struct {
	id    string
	file  string
	old   string
	new   string
	note  string
	equiv bool
}

func main() {
	root, err := os.Getwd()
	if err != nil {
		fatal(err)
	}
	pkg := filepath.Join(root, "internal", "ownership")
	if _, err := os.Stat(filepath.Join(pkg, "step.go")); err != nil {
		fatal(fmt.Errorf("run from the module root: %w", err))
	}
	src, err := readSources(pkg)
	if err != nil {
		fatal(err)
	}
	for _, m := range mutants {
		body, ok := src[m.file]
		if !ok {
			fatal(fmt.Errorf("%s: missing %s", m.id, m.file))
		}
		if n := strings.Count(body, m.old); n != 1 {
			fatal(fmt.Errorf("%s: %s matched %d times", m.id, m.file, n))
		}
	}

	type result struct {
		id, status, test string
	}
	out := make([]result, len(mutants))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, m := range mutants {
		wg.Add(1)
		go func(i int, m mutant) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			status, test := runOne(pkg, src, m)
			if status == "survived" && m.equiv {
				status = "equivalent"
				test = m.note
			}
			out[i] = result{m.id, status, test}
			fmt.Printf("%s\t%s\t%s\n", m.id, status, test)
		}(i, m)
	}
	wg.Wait()

	fmt.Println()
	fmt.Println("id\tresult\tkilled_by")
	killed, equiv, survived := 0, 0, 0
	for _, r := range out {
		fmt.Printf("%s\t%s\t%s\n", r.id, r.status, r.test)
		switch r.status {
		case "killed":
			killed++
		case "equivalent":
			equiv++
		default:
			survived++
		}
	}
	fmt.Printf("killed %d, equivalent %d, total %d\n", killed, equiv, len(out))
	if survived > 0 {
		os.Exit(1)
	}
}

func runOne(pkg string, src map[string]string, m mutant) (status, test string) {
	dir, err := os.MkdirTemp("", "ownership-mutant-"+m.id+"-")
	if err != nil {
		return "error", err.Error()
	}
	defer os.RemoveAll(dir)
	for name, body := range src {
		if name == m.file {
			body = strings.Replace(body, m.old, m.new, 1)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			return "error", err.Error()
		}
	}
	mod := "module ownership\n\ngo 1.26.9\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644); err != nil {
		return "error", err.Error()
	}
	cmd := exec.Command("go", "test", "-count=1", "-failfast", "-timeout", "90s")
	cmd.Dir = dir
	raw, err := cmd.CombinedOutput()
	log := string(raw)
	if err == nil {
		return "survived", ""
	}
	name := failedTest(log)
	if name == "" {
		name = "build"
	}
	return "killed", name
}

func failedTest(log string) string {
	for _, line := range strings.Split(log, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "--- FAIL: ") {
			rest := strings.TrimPrefix(line, "--- FAIL: ")
			if i := strings.IndexByte(rest, ' '); i > 0 {
				return rest[:i]
			}
			return rest
		}
	}
	return ""
}

func readSources(pkg string) (map[string]string, error) {
	entries, err := os.ReadDir(pkg)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(pkg, e.Name()))
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(string(b), "//go:build ignore") {
			continue
		}
		out[e.Name()] = string(b)
	}
	return out, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}

var mutants = []mutant{
	{id: "M01", file: "step.go", old: "func operatorStart(s State, e Event, now time.Time) partial {\n\tif e.MetaDown {\n\t\treturn appliedPair(s, Result{Decision: DecisionRejected, HTTP: 503, Detail: \"meta down\"})\n\t}\n", new: "func operatorStart(s State, e Event, now time.Time) partial {\n"},
	{id: "M02", file: "step.go", old: "\tif s.PendingDelete {\n\t\treturn appliedPair(s, Result{Decision: DecisionRejected, HTTP: 409, Detail: string(s.Phase) + \" pending_delete\"})\n\t}\n", new: ""},
	{id: "M03", file: "step.go", old: "\tif (s.Phase == PhaseStarting || s.Phase == PhaseRunning) && !e.ChangesSpec {\n\t\treturn appliedPair(s, Result{Decision: DecisionIdempotent, HTTP: 204, Detail: string(s.Phase)})\n\t}\n", new: ""},
	{id: "M04", file: "step.go", old: "\t\tr.Until = now.Add(RefusalDelay(r.Attempts, next.Params.ClaimInterval, next.Params.RefusalRetryCap))", new: "\t\tr.Until = now.Add(next.Params.ClaimInterval)"},
	{id: "M05", file: "step.go", old: "\tif !active {", new: "\tif !active && false {"},
	{id: "M06", file: "step.go", old: "\tif LeaseLive(s, now) {\n\t\treturn partial{state: next, result: Result{Decision: DecisionApplied, HTTP: 204, Detail: \"STOPPING pending_delete\", LocksTaskLease: true}}\n\t}\n", new: ""},
	{id: "M07", file: "step.go", old: "\tif s.Lease.Epoch != e.Epoch {\n\t\treturn fenceDiscard\n\t}\n", new: ""},
	{id: "M08", file: "step.go", old: "\tif s.Desired != DesiredRun {\n\t\treturn appliedPair(s, Result{Decision: DecisionDiscarded, Detail: \"desired STOP\", Intents: []Intent{{Kind: IntentStopLocal}}})\n\t}\n", new: ""},
	{id: "M09", file: "step.go", old: "\tif RefusalBlocks(s, e.Node, now) || !InClaimSet(s, now) {", new: "\tif RefusalBlocks(s, e.Node, now) || false {"},
	{id: "M10", file: "step.go", old: "\tif !InStopSet(s, now) {\n\t\treturn appliedPair(s, Result{Decision: DecisionRejected, Detail: \"not in stop set\"})\n\t}\n", new: ""},
	{id: "M11", file: "step.go", old: "\tdefault:\n\t\treturn appliedPair(s, Result{Decision: DecisionRejected, Detail: \"not on the terminal allowlist\"})\n", new: "\tdefault:\n\t\treason = string(e.Cause)\n"},
	{id: "M12", file: "step.go", old: "\tcase fenceOK:\n\tcase fenceDiscard:\n\t\treturn dropped(s, fenceDiscard)\n", new: "\tcase fenceOK, fenceDiscard:\n"},
	{id: "M13", file: "step.go", old: "\tif e.LeaseUnreadable || !LeaseLive(s, now) {", new: "\tif e.LeaseUnreadable {"},
	{id: "M14", file: "step.go", old: "\topenEnded := s.Params.Storage == StoragePerNode", new: "\topenEnded := false"},
	{id: "M15", file: "step.go", old: "\tif s.Chain.Verified() && s.Chain.Owner == e.Node {\n\t\treturn appliedPair(s, Result{Decision: DecisionRejected, Detail: \"verified chain owner\"})\n\t}\n", new: ""},
	{id: "M16", file: "step.go", old: "\t\tif next.LockRecoveries >= 2 {", new: "\t\tif next.LockRecoveries >= 1 {"},
	{id: "M17", file: "step.go", old: "\tnext := yield(s, e.Node, now, false, RefusalEpochLost)", new: "\tnext := yield(s, e.Node, now, true, RefusalEpochLost)"},
	{id: "M18", file: "step.go", old: "\tnext.bump()\n\tnext.Desired = DesiredStop\n\tnext.StoppedByOperator = true\n\tnext.Phase = PhaseStopping\n\tnext.SpecRevision++\n", new: "\tnext.bump()\n\tnext.StoppedByOperator = true\n\tnext.Phase = PhaseStopping\n\tnext.SpecRevision++\n"},
	{id: "M19", file: "step.go", old: "\tif LeaseLive(s, now) && s.Lease.Owner != e.Node {\n\t\treturn appliedPair(s, Result{Decision: DecisionRejected, HTTP: 409, Detail: string(s.Phase) + \" lease_held\"})\n\t}\n", new: ""},
	{id: "M20", file: "step.go", old: "\tnext.TailSince = time.Time{}\n", new: ""},
	{id: "M21", file: "step.go", old: "\tif e.Node == \"\" || e.Incomplete || !e.ReadOK {", new: "\tif e.Node == \"\" || e.Incomplete {"},
	{id: "M22", file: "step.go", old: "\tif reason == RefusalTail && next.TailSince.IsZero() {", new: "\tif reason == RefusalTail {"},
	{id: "M23", file: "step.go", old: "\tif e.Node != s.Chain.Owner || !s.Chain.Verified() || !s.Chain.VerifiedAt.After(s.FailedAt) {", new: "\tif e.Node != s.Chain.Owner || !s.Chain.Verified() {"},
	{id: "M24", file: "step.go", old: "\tsame := s.Lease.Owner == node && node != \"\"", new: "\tsame := false"},
	{id: "M25", file: "step.go", old: "\tif !s.Lease.Present || s.Lease.Owner != e.Node || s.Lease.Epoch != e.Epoch {", new: "\tif !s.Lease.Present || s.Lease.Epoch != e.Epoch {"},
	{id: "M26", file: "step.go", old: "\tif !s.Lease.Present || s.Lease.Owner != e.Node || s.Lease.Epoch != e.Epoch {", new: "\tif !s.Lease.Present || s.Lease.Owner != e.Node {"},
	{id: "M27", file: "step.go", old: "\tif s.RetryAttempt >= RetryBudget {\n\t\treturn epochLost(s, e, now)\n\t}\n", new: ""},
	{id: "M28", file: "invariant.go", old: "func RefusalBlocks(s State, node string, now time.Time) bool {\n\tr, ok := s.Refusal[node]\n\tif !ok {\n\t\treturn false\n\t}\n\tif r.OpenEnded {\n\t\treturn true\n\t}\n\treturn r.Until.After(now)\n}", new: "func RefusalBlocks(s State, node string, now time.Time) bool {\n\treturn false\n}"},
	{id: "M29", file: "step.go", old: "\tif s.Chain.Owner == \"\" || s.Phase != PhaseLeaseDegraded || s.TailSince.IsZero() {", new: "\tif s.Chain.Owner == \"\" {"},
	{id: "M30", file: "step.go", old: "func stopConfirmed(s State, e Event, now time.Time) partial {\n\tswitch fence(s, e) {\n\tcase fenceOK:\n\tdefault:\n\t\treturn dropped(s, fence(s, e))\n\t}\n", new: "func stopConfirmed(s State, e Event, now time.Time) partial {\n"},
	{id: "M31", file: "spec.go", old: "\tif d <= 0 || d > cap {\n\t\treturn cap\n\t}", new: "\tif d <= 0 || d > cap {\n\t\treturn cap + time.Hour\n\t}"},
	{id: "M32", file: "step.go", old: "\t\tDecision:       DecisionApplied,\n\t\tLocksTaskLease: false,\n\t\tIntents:        []Intent{{Kind: IntentClusterFinding, Finding: Finding{Name: FindingLockStalled, Subject: s.TaskID, OnCluster: true}}},", new: "\t\tDecision:       DecisionApplied,\n\t\tLocksTaskLease: true,\n\t\tIntents:        []Intent{{Kind: IntentClusterFinding, Finding: Finding{Name: FindingLockStalled, Subject: s.TaskID, OnCluster: true}}},"},
	{id: "M33", file: "invariant.go", old: "\tif ProgressOverdue(s, now) != hasFinding(s, FindingNoProgress, s.TaskID) {\n\t\tv = add(v, \"7\", \"TASK_NO_PROGRESS does not match the sweep and no_progress_after clocks\")\n\t}\n", new: ""},
	{id: "M34", file: "step.go", old: "\t\treturn !s.UnownedSince.IsZero() && !now.Before(s.UnownedSince.Add(s.Params.Sweep))", new: "\t\treturn !s.UnownedSince.IsZero() && !now.Before(s.UnownedSince.Add(10 * s.Params.Sweep))"},
	{id: "F01", file: "step.go", old: "\tif e.RowGeneration != s.RowGeneration {\n\t\treturn fenceReread\n\t}\n", new: ""},
	{id: "F02", file: "step.go", old: "\tif hb.IsZero() || now.Sub(hb) > EffectiveNodeGone(s.Params) {\n\t\treturn appliedPair(s, Result{Decision: DecisionRejected, Detail: \"heartbeat\"})\n\t}\n", new: "\tif false && (hb.IsZero() || now.Sub(hb) > EffectiveNodeGone(s.Params)) {\n\t\treturn appliedPair(s, Result{Decision: DecisionRejected, Detail: \"heartbeat\"})\n\t}\n"},
	{id: "F03", file: "spec.go", old: "\td := claim << shift", new: "\td := claim << (shift + 1)"},
	{id: "F04", file: "step.go", old: "\ts.Lease.ExpireAt = now.Add(s.Params.LeaseTTL)", new: "\ts.Lease.ExpireAt = now.Add(s.Params.LeaseTTL - time.Second)"},
	{id: "F05", file: "step.go", old: "\tnext.RunID = \"\"\n\tnext.FailedReason = \"\"\n\tnext.FailedAt = time.Time{}\n\tnext.FailedSpec = 0\n", new: "\tnext.RunID = \"\"\n\tnext.FailedReason = \"\"\n\tnext.FailedSpec = 0\n"},
	{id: "F06", file: "step.go", old: "\tnext.SpecRevision++\n\tnext.RetryAttempt = 0\n\tnext.RetryUntil = time.Time{}\n", new: "\tnext.SpecRevision++\n\tnext.RetryUntil = time.Time{}\n"},
	{id: "F07", file: "step.go", old: "\tnext.LastError = \"\"\n\tnext.RunID = \"\"\n\tnext.FailedReason = \"\"\n", new: "\tnext.RunID = \"\"\n\tnext.FailedReason = \"\"\n"},
	{id: "F08", file: "step.go", old: "\tnext.SpecRevision++\n\tnext.RetryAttempt = 0\n", new: "\tnext.RetryAttempt = 0\n"},
	{id: "F09", file: "step.go", old: "\tnext.RunID = \"\"\n\tnext.FailedReason = \"\"\n\tnext.FailedAt = time.Time{}\n", new: "\tnext.RunID = \"\"\n\tnext.FailedAt = time.Time{}\n"},
	{id: "F10", file: "invariant.go", old: "\t\tif !anchor.IsZero() && now.Sub(anchor) > p.NoProgressAfter {", new: "\t\tif !anchor.IsZero() && now.Sub(anchor) >= p.NoProgressAfter {"},
	{id: "F11", file: "step.go", old: "\t\twithin := next.LockRecoveries > 0 && next.Lock.HeldBy == e.Node && !now.After(next.Lock.Since.Add(next.Params.LockWait))", new: "\t\twithin := next.LockRecoveries > 0"},
	{id: "F12", file: "step.go", old: "\t\tif !anchor.IsZero() && now.Sub(anchor) > s.Params.NoProgressAfter {", new: "\t\tif !anchor.IsZero() && now.Sub(anchor) >= s.Params.NoProgressAfter {"},
	{id: "F13", file: "invariant.go", old: "\treturn s.Lease.Present && s.Lease.Owner != \"\" && s.Lease.ExpireAt.After(now)", new: "\treturn s.Lease.Present && s.Lease.Owner != \"\" && !s.Lease.ExpireAt.Before(now)"},
	{id: "F14", file: "liveness_test.go", old: "\tk := 2 + wastedPossible(s, now) + extraWastes(s, now) // claim + DumpReady + N + re-claims", new: "\tk := 3 + wastedPossible(s, now) + extraWastes(s, now) // claim + DumpReady + N + re-claims"},
	{id: "F15", file: "invariant.go", old: "\treturn now.Before(s.RetryUntil)", new: "\treturn !now.After(s.RetryUntil)"},
	{id: "F16", file: "step.go", old: "\tgone := hb.IsZero() || now.Sub(hb) > EffectiveNodeGone(s.Params)", new: "\tgone := hb.IsZero() || now.Sub(hb) >= EffectiveNodeGone(s.Params)"},
	{id: "F17", file: "spec.go", old: "\tif shift > 20 {\n\t\treturn cap\n\t}\n", new: "", equiv: true, note: "equivalent: attempts that overflow the shift already return cap via d<=0 or d>cap"},
	{id: "F18", file: "spec.go", old: "\tif attempts < 1 {\n\t\tattempts = 1\n\t}\n", new: "", equiv: true, note: "equivalent: Step increments attempts before RefusalDelay"},
	{id: "F19", file: "spec.go", old: "\tif claim <= 0 {\n\t\tclaim = 2 * time.Second\n\t}\n", new: "", equiv: true, note: "equivalent: Step fills ClaimInterval before RefusalDelay"},
	{id: "F20", file: "step.go", old: "\tif hb.IsZero() || now.Sub(hb) > EffectiveNodeGone(s.Params) {\n\t\treturn appliedPair(s, Result{Decision: DecisionRejected, Detail: \"heartbeat\"})\n\t}", new: "\tif hb.IsZero() || now.Sub(hb) >= EffectiveNodeGone(s.Params) {\n\t\treturn appliedPair(s, Result{Decision: DecisionRejected, Detail: \"heartbeat\"})\n\t}"},
}
