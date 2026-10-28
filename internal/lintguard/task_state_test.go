// Package lintguard checks that task state writes stay on the persist-first path.
// input: internal/tasks Go sources parsed as AST
// output: test failure when State is assigned outside the allowlist
// pos: AST guard for the task state machine
// note: if this file changes, update this header and module README.md.
package lintguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// operatorFuncs are the operator-driven transitions. Each must go through
// commitTransitionLocked / commitDeleteLocked (internal/tasks/transition_commit.go),
// which write the store first: they may not set State, assign s.tasks[...],
// delete from s.tasks, or call persistTaskLocked themselves.
var operatorFuncs = map[string]bool{
	"createTask": true, "ConfigureSource": true, "ConfigureStart": true, "ConfigureStorage": true,
	"ConfigureClusterKey": true, "ConfigureName": true, "AdoptDiskBackup": true, "UpdateTask": true,
	"DeleteTask": true, "StartTask": true, "writeStartIntent": true, "StopTask": true,
}

// stateWriters may set a task's State directly. Besides the commit helper,
// they are the runner and control-loop recipes (the run itself, the lease,
// the retry loop, store reads), which record what already happened to a
// dump rather than ask for something; the stalled-transition watchdog
// (stalled_transition.go) reconciles any of them that is left STARTING or
// STOPPING with nothing driving it. Adding a function here needs that
// argument; an operator action never belongs here.
var stateWriters = map[string]bool{
	"transition_commit.go:commitTransitionLocked": true,
	// runner / retry / lease recipes
	"scheduler_transitions.go:markStartingLocked":       true,
	"scheduler_transitions.go:markRetryBackoffLocked":   true,
	"scheduler_transitions.go:markRetryingLocked":       true,
	"scheduler_transitions.go:markRunnerReadyLocked":    true,
	"scheduler_transitions.go:markLeaseDegradedLocked":  true,
	"scheduler_transitions.go:markLeaseRenewedLocked":   true,
	"scheduler_transitions.go:failSafeStopLocked":       true,
	"scheduler_transitions.go:markFailedLocked":         true,
	"scheduler_transitions.go:writeStoppedLocked":       true,
	"scheduler_transitions.go:writeStopExpiredLocked":   true, // StopExpired already decided STOPPED or delete
	"scheduler_transitions.go:finishStoredStopLocked":   true,
	"scheduler_transitions.go:markRestoredFailedLocked": true, // a restore with no safe position stays FAILED; it is not a runner event
	"ownership_bridge.go:copyOwnership":                 true, // copies the phase Step already decided
	"scheduler_lifecycle.go:startTask":                  true,
	"scheduler_lifecycle.go:runTask":                    true,
	"scheduler_lifecycle.go:noteRemoteStopLocked":       true,
	"scheduler_lifecycle.go:settleForeignStop":          true,
	"scheduler_cluster_lease.go:yieldLostLeaseLocked":   true,
	// §29: this epoch's dump already stopped; the row stays desired RUN and is not persisted here
	"scheduler_cluster_lease.go:yieldForResumeLocked": true,
	// §29.10: a fenced append with no successor stays LEASE_DEGRADED, not STOPPED
	"scheduler_cluster_lease.go:yieldFencedEpochLocked": true,
	"control_loop.go:adoptNewerStopLocked":              true,
	"control_loop.go:intentLocked":                      true,
	// standalone startup read of the task record (ADR 0010), as Restore reads backup_tasks
	"task_records.go:restoredTask": true,
	// Task values built only to be read, never stored or persisted.
	"disk_files.go:diskBackupTask":               true, // read-only view of an on-disk backup with no metadata
	"scheduler_lifecycle.go:claimOwnedIdleTasks": true, // effectiveDesired probe
}

func TestTaskStateChangesGoThroughCommitHelper(t *testing.T) {
	dir := filepath.Join("..", "tasks")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var bad []string
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := name + ":" + fn.Name.Name
			seen[key] = true
			operator := operatorFuncs[fn.Name.Name] && fn.Recv != nil
			commits := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.AssignStmt:
					for _, lhs := range n.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "State" && !stateWriters[key] && !isNonTaskState(key) {
							bad = append(bad, key+": sets .State outside commitTransitionLocked")
						}
						if operator && isTasksIndex(lhs) {
							bad = append(bad, key+": operator action assigns s.tasks[...] directly")
						}
					}
				case *ast.CompositeLit:
					if id, ok := n.Type.(*ast.Ident); ok && id.Name == "Task" && !stateWriters[key] {
						for _, el := range n.Elts {
							if kv, ok := el.(*ast.KeyValueExpr); ok {
								if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "State" {
									bad = append(bad, key+": Task literal sets State outside commitTransitionLocked")
								}
							}
						}
					}
				case *ast.CallExpr:
					name := callName(n)
					if name == "commitTransitionLocked" || name == "commitDeleteLocked" || name == "writeStartIntent" {
						commits = true
					}
					if operator && name == "persistTaskLocked" {
						bad = append(bad, key+": operator action calls persistTaskLocked; use commitTransitionLocked")
					}
					if operator && name == "delete" && len(n.Args) > 0 && isTasksMap(n.Args[0]) {
						bad = append(bad, key+": operator action deletes from s.tasks; use commitDeleteLocked")
					}
				}
				return true
			})
			if operator && !commits {
				bad = append(bad, key+": operator action does not call commitTransitionLocked/commitDeleteLocked")
			}
		}
	}
	for key := range stateWriters {
		if !seen[key] {
			bad = append(bad, key+": allowlisted in stateWriters but no such function (remove the entry)")
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("task state changes outside the persist-first helper:\n  %s", strings.Join(bad, "\n  "))
	}
}

// isNonTaskState: State fields of other types (a catalog row, a filter).
func isNonTaskState(key string) bool {
	return key == "legacy_failback.go:renameLegacyRows" || strings.HasPrefix(key, "legacy_failback.go:")
}

func isTasksMap(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "tasks"
}

func isTasksIndex(e ast.Expr) bool {
	idx, ok := e.(*ast.IndexExpr)
	return ok && isTasksMap(idx.X)
}

func callName(c *ast.CallExpr) string {
	switch f := c.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}
