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
	id   string
	file string
	old  string
	new  string
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
			out[i] = result{m.id, status, test}
			fmt.Printf("%s\t%s\t%s\n", m.id, status, test)
		}(i, m)
	}
	wg.Wait()

	fmt.Println()
	fmt.Println("id\tresult\tkilled_by")
	killed, survived := 0, 0
	for _, r := range out {
		fmt.Printf("%s\t%s\t%s\n", r.id, r.status, r.test)
		switch r.status {
		case "killed":
			killed++
		default:
			survived++
		}
	}
	fmt.Printf("killed %d, total %d\n", killed, len(out))
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
	{id: "M28", file: "invariant.go", old: "func RefusalBlocks(s State, node string, now time.Time) bool {\n\tr, ok := s.Refusal[node]\n\tif !ok {\n\t\treturn false\n\t}\n\tif r.OpenEnded {\n\t\tn := s.node(node)\n\t\tif n != nil && n.CanRead {\n\t\t\treturn false\n\t\t}\n\t\treturn true\n\t}\n\treturn r.Until.After(now)\n}", new: "func RefusalBlocks(s State, node string, now time.Time) bool {\n\treturn false\n}"},
	{id: "M29", file: "step.go", old: "\tif s.Chain.Owner == \"\" || s.Phase != PhaseLeaseDegraded || s.TailSince.IsZero() {", new: "\tif s.Chain.Owner == \"\" {"},
	{id: "M30", file: "step.go", old: "func stopConfirmed(s State, e Event, now time.Time) partial {\n\tswitch fence(s, e) {\n\tcase fenceOK:\n\tdefault:\n\t\treturn dropped(s, fence(s, e))\n\t}\n", new: "func stopConfirmed(s State, e Event, now time.Time) partial {\n"},
	{id: "M31", file: "spec.go", old: "\tif shift > maxRefusalShift || claim > cap>>shift {\n\t\treturn cap\n\t}", new: "\tif shift > maxRefusalShift || claim > cap>>shift {\n\t\treturn cap + time.Hour\n\t}"},
	{id: "M32", file: "step.go", old: "\t\tDecision:       DecisionApplied,\n\t\tLocksTaskLease: false,\n\t\tIntents:        []Intent{{Kind: IntentClusterFinding, Finding: Finding{Name: FindingLockStalled, Subject: s.TaskID, OnCluster: true}}},", new: "\t\tDecision:       DecisionApplied,\n\t\tLocksTaskLease: true,\n\t\tIntents:        []Intent{{Kind: IntentClusterFinding, Finding: Finding{Name: FindingLockStalled, Subject: s.TaskID, OnCluster: true}}},"},
	{id: "M33", file: "invariant.go", old: "\tif ProgressOverdue(s, now) != hasFinding(s, FindingNoProgress, s.TaskID) {\n\t\tv = add(v, \"7\", \"TASK_NO_PROGRESS does not match the sweep and no_progress_after clocks\")\n\t}\n", new: ""},
	{id: "M34", file: "step.go", old: "\t\treturn !s.UnownedSince.IsZero() && !now.Before(s.UnownedSince.Add(s.Params.Sweep))", new: "\t\treturn !s.UnownedSince.IsZero() && !now.Before(s.UnownedSince.Add(10 * s.Params.Sweep))"},
	{id: "M35", file: "step.go", old: "\tif e.RowGeneration != s.RowGeneration {\n\t\treturn fenceReread\n\t}\n", new: ""},
	{id: "M36", file: "step.go", old: "\tif hb.IsZero() || now.Sub(hb) > EffectiveNodeGone(s.Params) {\n\t\treturn appliedPair(s, Result{Decision: DecisionRejected, Detail: \"heartbeat\"})\n\t}\n", new: "\tif false && (hb.IsZero() || now.Sub(hb) > EffectiveNodeGone(s.Params)) {\n\t\treturn appliedPair(s, Result{Decision: DecisionRejected, Detail: \"heartbeat\"})\n\t}\n"},
	{id: "M37", file: "spec.go", old: "\treturn claim << shift", new: "\treturn claim << (shift + 1)"},
	{id: "M38", file: "step.go", old: "\ts.Lease.ExpireAt = now.Add(s.Params.LeaseTTL)", new: "\ts.Lease.ExpireAt = now.Add(s.Params.LeaseTTL - time.Second)"},
	{id: "M39", file: "step.go", old: "\tnext.RunID = \"\"\n\tnext.FailedReason = \"\"\n\tnext.FailedAt = time.Time{}\n\tnext.FailedSpec = 0\n", new: "\tnext.RunID = \"\"\n\tnext.FailedReason = \"\"\n\tnext.FailedSpec = 0\n"},
	{id: "M40", file: "step.go", old: "\tnext.SpecRevision++\n\tnext.RetryAttempt = 0\n\tnext.RetryUntil = time.Time{}\n", new: "\tnext.SpecRevision++\n\tnext.RetryUntil = time.Time{}\n"},
	{id: "M41", file: "step.go", old: "\tnext.LastError = \"\"\n\tnext.RunID = \"\"\n\tnext.FailedReason = \"\"\n", new: "\tnext.RunID = \"\"\n\tnext.FailedReason = \"\"\n"},
	{id: "M42", file: "step.go", old: "\tnext.SpecRevision++\n\tnext.RetryAttempt = 0\n", new: "\tnext.RetryAttempt = 0\n"},
	{id: "M43", file: "step.go", old: "\tnext.RunID = \"\"\n\tnext.FailedReason = \"\"\n\tnext.FailedAt = time.Time{}\n", new: "\tnext.RunID = \"\"\n\tnext.FailedAt = time.Time{}\n"},
	{id: "M44", file: "invariant.go", old: "\t\tif !anchor.IsZero() && now.Sub(anchor) > p.NoProgressAfter {", new: "\t\tif !anchor.IsZero() && now.Sub(anchor) >= p.NoProgressAfter {"},
	{id: "M45", file: "step.go", old: "\t\twithin := next.LockRecoveries > 0 && next.Lock.HeldBy == e.Node && !now.After(next.Lock.Since.Add(next.Params.LockWait))", new: "\t\twithin := next.LockRecoveries > 0"},
	{id: "M46", file: "step.go", old: "\t\tif !anchor.IsZero() && now.Sub(anchor) > s.Params.NoProgressAfter {", new: "\t\tif !anchor.IsZero() && now.Sub(anchor) >= s.Params.NoProgressAfter {"},
	{id: "M47", file: "invariant.go", old: "\treturn s.Lease.Present && s.Lease.Owner != \"\" && s.Lease.ExpireAt.After(now)", new: "\treturn s.Lease.Present && s.Lease.Owner != \"\" && !s.Lease.ExpireAt.Before(now)"},
	{id: "M48", file: "bound.go", old: "\t\tfinish = 2 // acquire and DumpReady", new: "\t\tfinish = 1 // acquire and DumpReady"},
	{id: "M49", file: "invariant.go", old: "\treturn now.Before(s.RetryUntil)", new: "\treturn !now.After(s.RetryUntil)"},
	{id: "M50", file: "step.go", old: "\tgone := hb.IsZero() || now.Sub(hb) > EffectiveNodeGone(s.Params)", new: "\tgone := hb.IsZero() || now.Sub(hb) >= EffectiveNodeGone(s.Params)"},
	{id: "M51", file: "spec.go", old: "\tif shift > maxRefusalShift || claim > cap>>shift {\n\t\treturn cap\n\t}\n", new: ""},
	{id: "M52", file: "spec.go", old: "\tif attempts < 1 {\n\t\tattempts = 1\n\t}\n", new: ""},
	{id: "M53", file: "spec.go", old: "\tif claim <= 0 {\n\t\tclaim = 2 * time.Second\n\t}\n", new: ""},
	{id: "M54", file: "step.go", old: "\tif hb.IsZero() || now.Sub(hb) > EffectiveNodeGone(s.Params) {\n\t\treturn appliedPair(s, Result{Decision: DecisionRejected, Detail: \"heartbeat\"})\n\t}", new: "\tif hb.IsZero() || now.Sub(hb) >= EffectiveNodeGone(s.Params) {\n\t\treturn appliedPair(s, Result{Decision: DecisionRejected, Detail: \"heartbeat\"})\n\t}"},
	{id: "M55", file: "step.go", old: "!now.After(next.Lock.Since.Add(next.Params.LockWait))", new: "now.Before(next.Lock.Since.Add(next.Params.LockWait))"},
	{id: "M56", file: "step.go", old: "\tnext.ProgressAt = now\n", new: ""},
	{id: "M57", file: "step.go", old: "return Result{State: s, Decision: DecisionDiscarded, ZeroRow: true, Detail: \"deleted\"}", new: "return Result{State: s, Decision: DecisionRejected, HTTP: 409, Detail: \"deleted\"}"},
	{id: "M58", file: "step.go", old: "\t\tif s.UnownedSince.IsZero() {\n\t\t\ts.UnownedSince = now\n\t\t}\n", new: ""},
	{id: "M59", file: "step.go", old: "\ts.UnownedSince = time.Time{}\n", new: ""},
	{id: "M60", file: "step.go", old: "\tif s.Deleted || s.Desired != DesiredRun || s.Phase == PhaseFailed || s.Phase == PhaseCreated {", new: "\tif s.Deleted || s.Desired != DesiredRun || s.Phase == PhaseCreated {"},
	{id: "M61", file: "step.go", old: "\tnext.FailedSpec = s.SpecRevision\n", new: ""},
	{id: "M62", file: "spec.go", old: "\tif cap <= 0 {\n\t\tcap = 600 * time.Second\n\t}\n", new: ""},
	{id: "M63", file: "step.go", old: "\tif r.Attempts > maxRefusalAttempts {\n\t\tr.Attempts = maxRefusalAttempts\n\t}\n", new: ""},
	{id: "G01", file: "invariant.go", old: "live := snap.Present && snap.Owner != \"\" && snap.Expire.After(now)", new: "live := snap.Present && snap.Owner != \"\" && !snap.Expire.Before(now)"},
	{id: "J05", file: "bound.go", old: "\tcap := p.RefusalRetryCap", new: "\tcap := 600 * time.Second"},
	{id: "J06", file: "bound.go", old: "\tatt := r.Attempts", new: "\tatt := maxRefusalAttempts"},
	{id: "J11", file: "bound.go", old: "\tif p.Storage == StoragePerNode && !(s.Chain.Verified() && s.Chain.Owner == id) {", new: "\tif p.Storage == StoragePerNode && true {"},
	{id: "B01", file: "bound.go", old: "\t\textra = int(W/cap) + 1", new: "\t\textra = 0"},
	{id: "B02", file: "bound.go", old: "\tatt := r.Attempts", new: "\tatt := 0"},
	{id: "B03", file: "bound.go", old: "\t\treturn 1, pending", new: "\t\treturn 0, pending"},
	{id: "B04", file: "bound.go", old: "\treturn n + extra, pending", new: "\treturn n + extra - 1, pending"},
	{id: "U06", file: "bound.go", old: "\t\tjumps = wastes + pending + 1", new: "\t\tjumps = wastes + pending"},
	{id: "U08", file: "bound.go", old: "\tif ready > W {", new: "\tif ready >= W {"},
	{id: "U16", file: "bound.go", old: "\tif d > W {", new: "\tif W == 0 || d < W {"},
	{id: "U17", file: "bound.go", old: "\t\tadd(s.Lease.ExpireAt) // stopped owner", new: "\t\t_ = s.Lease.ExpireAt // stopped owner"},
	{id: "U18", file: "bound.go", old: "\t\tadd(s.RetryUntil) // retry window", new: "\t\t_ = s.RetryUntil // retry window"},
	{id: "V04", file: "bound.go", old: "\t\tidle := s.Params.IdleKill\n\t\tif idle <= 0 {\n\t\t\tidle = 5 * time.Second\n\t\t}\n\t\tadd(s.Lock.Since.Add(idle))", new: "\t\tadd(s.Lock.Since.Add(5 * time.Second))"},
	{id: "V17", file: "bound.go", old: "\tif live && owner != nil && owner.Stopped {\n\t\tadd(s.Lease.ExpireAt) // stopped owner\n\t}", new: "\tif live && owner != nil && owner.Stopped && owner.CanRead {\n\t\tadd(s.Lease.ExpireAt) // stopped owner\n\t}"},
	{id: "S03", file: "invariant.go", old: "\tcase ReasonChainOwnerGone:\n\t\tn := s.node(s.Chain.Owner)\n\t\treturn n == nil || n.Stopped || !n.CanRead", new: "\tcase ReasonChainOwnerGone:\n\t\treturn true"},
	{id: "W02", file: "observe_test.go", old: "\t// sweep step\n\tif gone := Step(s, Event{Kind: EventChainOwnerGone, Node: \"_health\"}, now); gone.Decision == DecisionApplied {\n\t\tgone.State.trace = nil\n\t\tout = append(out, obsMove{s: gone.State, at: now, kind: moveForced})\n\t}\n", new: ""},
	{id: "T08", file: "invariant.go", old: "\tif s.Lock.Held || s.Phase != PhaseRunning || s.Desired != DesiredRun {", new: "\tif s.Phase != PhaseRunning || s.Desired != DesiredRun {"},
	{id: "W04", file: "bound.go", old: "\t\tn++ // ChainOwnerGone", new: ""},
	{id: "P01", file: "invariant.go", old: "\t\tn := s.node(node)\n\t\tif n != nil && n.CanRead {\n\t\t\treturn false\n\t\t}\n\t\treturn true", new: "\t\treturn true"},
}
