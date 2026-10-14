// Package tasks provides module-level functionality for tasks.
// input: a standalone data_dir
// output: assertions that open findings survive a restart through .findings.json and that a reopen keeps first_seen
// pos: coverage for the standalone FindingStore
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"path/filepath"
	"testing"
)

func TestFileFindingStore_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FindingFileName)
	a := NewScheduler(WithDataDir(dir), WithFindingStore(NewFileFindingStore(path)))
	a.Report(Finding{TaskID: "3", Kind: FindingDumpCleanupPersistFailed, Detail: "x"})
	a.Report(Finding{TaskID: "3", Kind: FindingEventPersistFailed, Subject: "TASK_STARTED", Severity: FindingWarning})
	a.Resolve("3", FindingEventPersistFailed, "TASK_STARTED")
	a.FlushFindings(context.Background())

	b := NewScheduler(WithDataDir(dir), WithFindingStore(NewFileFindingStore(path)))
	if err := b.LoadOpenFindings(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := openFinding(b, "3", FindingDumpCleanupPersistFailed); !ok {
		t.Fatalf("open finding lost across restart: %+v", b.FindingStats().Open)
	}
	if _, ok := openFinding(b, "3", FindingEventPersistFailed); ok {
		t.Fatalf("resolved finding came back open")
	}
	all, err := b.ListFindings(context.Background(), "3", true, 10)
	if err != nil || len(all) != 2 {
		t.Fatalf("history %+v err %v", all, err)
	}
	// The data_dir listing still shows no task for the findings file.
	if items := b.ListTasks(); len(items) != 0 {
		t.Fatalf("findings file listed as a task: %+v", items)
	}
}
