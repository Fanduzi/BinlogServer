// Package health provides module-level functionality for health.
// input: task.state facts with and without a local run's progress book (run start, last advance, current segment file, whether that segment is under this worker's data_dir), desired/state/owner combinations, the source liveness clock and health.no_progress_after
// output: proof that TASK_NO_PROGRESS is CRITICAL and Managed on task.state, opens for a run that has not advanced under data_dir past the threshold (including a run whose progress is outside data_dir, #421 sc1) and for a desired-RUN task no worker runs, and stays closed for an advancing or caught-up run, FAILED, desired STOP, a task owned elsewhere, SOURCE_UNREACHABLE, a run with no progress book, and below the threshold
// pos: ADR 0009 P2-6d (docs/design/0009-phase2-health-evaluator.md §24.9)
// note: if this file changes, update this header and module README.md.
package health

import (
	"strings"
	"testing"
	"time"
)

func noProgressFacts(now time.Time, t *TaskFacts) *Facts {
	return &Facts{TaskID: "1", Now: now, Task: t, Complete: map[Scope]bool{ScopeTaskState: true, ScopeSignals: true}}
}

func TestTaskNoProgressRegistered(t *testing.T) {
	spec, ok := DefaultRegistry()[TaskNoProgress]
	if !ok || spec.Severity != Critical || !spec.Managed || spec.Derive == nil || spec.Clear.Holds == nil {
		t.Fatalf("TASK_NO_PROGRESS spec %+v, want CRITICAL, Managed, with Derive and Clear", spec)
	}
	if len(spec.Scopes) != 1 || spec.Scopes[0] != ScopeTaskState {
		t.Fatalf("scopes %v, want task.state only", spec.Scopes)
	}
	if !ManagedKinds()[TaskNoProgress] {
		t.Fatal("ManagedKinds does not list TASK_NO_PROGRESS")
	}
}

func TestTaskNoProgressDerive(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	running := func(p *Progress) *TaskFacts {
		return &TaskFacts{State: stateRunning, Desired: desiredRun, Owner: "n2", Epoch: 2, RunHere: true, StateSince: ago(time.Hour), Progress: p}
	}
	cases := []struct {
		name   string
		task   *TaskFacts
		after  time.Duration
		live   *Liveness
		open   bool
		detail string
	}{
		{name: "run here never advanced", task: running(&Progress{RunSince: ago(3 * time.Minute)}), open: true, detail: "has written nothing"},
		{name: "run here advanced recently", task: running(&Progress{RunSince: ago(time.Hour), LastAdvance: ago(10 * time.Second), File: "mysql-bin.000003", InDataDir: true})},
		{name: "run here advanced long ago", task: running(&Progress{RunSince: ago(time.Hour), LastAdvance: ago(5 * time.Minute), File: "mysql-bin.000003", InDataDir: true}), open: true},
		{name: "progress outside data_dir (#421 sc1)", task: running(&Progress{RunSince: ago(3 * time.Minute), LastAdvance: ago(time.Second), File: "mbin.000001", InDataDir: false}), open: true, detail: "not under this worker's data_dir"},
		{name: "fresh run below threshold", task: running(&Progress{RunSince: ago(30 * time.Second)})},
		{name: "custom threshold", task: running(&Progress{RunSince: ago(30 * time.Second)}), after: 20 * time.Second, open: true},
		{name: "retry inside one run does not reset", task: &TaskFacts{State: stateRunning, Desired: desiredRun, RunHere: true, StateSince: ago(time.Second), Progress: &Progress{RunSince: ago(10 * time.Minute)}}, open: true},
		{name: "run here without a progress book", task: &TaskFacts{State: stateRunning, Desired: desiredRun, RunHere: true, StateSince: ago(time.Hour)}},
		{name: "no run, desired RUN, STOPPED", task: &TaskFacts{State: stateStopped, Desired: desiredRun, StateSince: ago(5 * time.Minute)}, open: true, detail: "no worker is running"},
		{name: "no run, unclaimed STARTING", task: &TaskFacts{State: stateStarting, Desired: desiredRun, StateSince: ago(5 * time.Minute)}, open: true},
		{name: "no run, recent", task: &TaskFacts{State: stateStopped, Desired: desiredRun, StateSince: ago(time.Second)}},
		{name: "FAILED is TASK_FAILED's", task: &TaskFacts{State: stateFailed, Desired: desiredRun, StateSince: ago(time.Hour)}},
		{name: "deliberate stop", task: &TaskFacts{State: stateStopped, Desired: desiredStop, StateSince: ago(time.Hour)}},
		{name: "owned elsewhere", task: &TaskFacts{State: stateRunning, Desired: desiredRun, OwnedElsewhere: true, StateSince: ago(time.Hour)}},
		{name: "source unreachable speaks", task: running(&Progress{RunSince: ago(10 * time.Minute)}),
			live: &Liveness{LastRead: ago(10 * time.Minute), LastFailure: ago(time.Second)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.live != nil {
				c.task.Liveness = c.live
			}
			f := noProgressFacts(now, c.task)
			f.NoProgressAfter = c.after
			got := DefaultRegistry()[TaskNoProgress].Derive(f)
			if c.open != (len(got) == 1) || len(got) > 1 {
				t.Fatalf("derived %+v, want open=%v", got, c.open)
			}
			if c.open {
				if got[0].Key.Subject != "" {
					t.Fatalf("subject %q, want empty (one per task)", got[0].Key.Subject)
				}
				if c.detail != "" && !strings.Contains(got[0].Detail, c.detail) {
					t.Fatalf("detail %q, want it to contain %q", got[0].Detail, c.detail)
				}
			}
			if cleared := DefaultRegistry()[TaskNoProgress].Clear.Holds(f, Key{Kind: TaskNoProgress}); cleared == c.open {
				t.Fatalf("clear rule %v while open=%v", cleared, c.open)
			}
		})
	}
}
