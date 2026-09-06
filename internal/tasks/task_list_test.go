// Package tasks provides module-level functionality for tasks.
// input: in-memory task snapshots used by PageTasks and StartingUnownedTasks, and file snapshots used by FailedUploadFiles
// output: numeric-id page order, STARTING-unowned subset coverage, and UPLOAD_FAILED file subset coverage
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

func TestFailedUploadFiles_FiltersAndCaps(t *testing.T) {
	items := []BinlogFile{
		{FileName: "a", UploadState: "UPLOADED"},
		{FileName: "b", UploadState: "UPLOAD_FAILED"},
		{FileName: "c", UploadState: "upload_failed"},
		{FileName: "d", UploadState: "UPLOAD_FAILED"},
	}
	got := FailedUploadFiles(items, 2)
	if len(got) != 2 || got[0].FileName != "b" || got[1].FileName != "c" {
		t.Fatalf("FailedUploadFiles = %+v, want b then c", got)
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
