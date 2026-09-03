// Package tasks provides module-level functionality for tasks.
// input: in-memory task snapshots used by PageTasks and StartingUnownedTasks
// output: numeric-id page order and STARTING-unowned subset coverage
// pos: unit tests for shared task list helpers
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"testing"
)

func TestPageTasks_NumericIDOrderAndTotal(t *testing.T) {
	items := []Task{
		{ID: "100"},
		{ID: "task-b"},
		{ID: "2"},
		{ID: "10"},
		{ID: "task-a"},
		{ID: "1"},
	}
	page, total := PageTasks(items, TaskListFilter{Limit: 2, Offset: 2})
	if total != 6 {
		t.Fatalf("total = %d, want 6", total)
	}
	got := []string{page[0].ID, page[1].ID}
	want := []string{"10", "100"}
	if got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("page ids = %v, want %v", got, want)
	}
}

func TestStartingUnownedTasks_FiltersOwnedAndNonStarting(t *testing.T) {
	items := []Task{
		{ID: "1", State: StateStarting, OwnerWorkerID: ""},
		{ID: "2", State: StateStarting, OwnerWorkerID: "worker-a"},
		{ID: "3", State: StateRunning, OwnerWorkerID: ""},
	}
	got := StartingUnownedTasks(items)
	if len(got) != 1 || got[0].ID != "1" {
		t.Fatalf("StartingUnownedTasks = %+v, want task 1 only", got)
	}
}
