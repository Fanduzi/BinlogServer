// Package tasks provides module-level functionality for tasks.
// input: HealthOf verdicts, chain rows, and a cluster task owned by another worker
// output: sample values and the rule that only a responsible task is exported
// pos: F2 snapshot tests
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"testing"
	"time"

	"binlog_server/internal/assurance"
	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
)

func TestSampleOfUsesHealthOfStatus(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	facts := &health.Facts{
		Now: now,
		Task: &health.TaskFacts{
			Desired:  "RUN",
			Liveness: &health.Liveness{LastRead: now.Add(-50 * time.Second)},
			Progress: &health.Progress{LastAdvance: now.Add(-10 * time.Second)},
		},
		Chain: &health.ChainFacts{UploadLag: 20 * time.Minute},
	}
	open := []health.Finding{{TaskID: "9", Kind: health.MetadataWriteBlocked, Severity: health.Critical}}
	h := health.TaskHealth{TaskID: "9", Status: health.StatusUnhealthy}
	s := sampleOf(h, facts, open, false)
	if s.Value != 0 || s.Reason != "critical" || s.OpenCritical != 1 {
		t.Fatalf("unhealthy sample %+v", s)
	}
	if s.ProgressAge != 10 || s.UploadLag != 1200 {
		t.Fatalf("slo ages progress=%v upload=%v", s.ProgressAge, s.UploadLag)
	}
	stoppedFacts := *facts
	stoppedTask := *facts.Task
	stoppedTask.Desired = "STOP"
	stoppedFacts.Task = &stoppedTask
	if stopped := sampleOf(h, &stoppedFacts, open, false); stopped.ProgressAge != 0 {
		t.Fatalf("stopped progress age %v", stopped.ProgressAge)
	}
	h.Status = health.StatusHealthy
	if ok := sampleOf(h, facts, nil, false); ok.Value != 1 || ok.Reason != "ok" {
		t.Fatalf("healthy sample %+v", ok)
	}
	if unread := sampleOf(h, facts, open, true); unread.Value != 0 || unread.Reason != "unread" || unread.OpenCritical != 1 {
		t.Fatalf("unread sample %+v", unread)
	}
}

func TestOldestUnuploadedSkipsMatched(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	rows := []assurance.Row{
		{Sealed: true, SealedAt: now.Add(-time.Hour), UploadState: "UPLOADED", Checksum: "match"},
		{Sealed: true, SealedAt: now.Add(-30 * time.Minute), UploadState: "UPLOAD_FAILED"},
		{Sealed: false, SealedAt: now.Add(-2 * time.Hour), UploadState: "LOCAL_ONLY"},
	}
	if got := oldestUnuploaded(rows, now); got != 30*time.Minute {
		t.Fatalf("lag %s", got)
	}
	if got := uploadLagOf(false, rows, chainCacheEntry{}, false, now); got != 0 {
		t.Fatalf("no object storage lag %s", got)
	}
}

func TestHealthSamplesOmitOtherOwners(t *testing.T) {
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, _ := evalTask(t, s, data)
	s.mu.Lock()
	stored := s.tasks[task.ID]
	stored.OwnerWorkerID = "other"
	stored.Epoch = 3
	s.tasks[task.ID] = stored
	s.mu.Unlock()

	store := healthstore.NewFileStore(data + "/.health.json")
	e := NewEvaluator(s, EvaluatorConfig{Mode: EvaluatorOn, Store: store, Cluster: true, NodeID: "me", Instance: "i", Sweep: time.Minute, Debounce: time.Millisecond})
	e.noteVerdict(health.TaskHealth{TaskID: task.ID, Status: health.StatusHealthy}, nil, nil, false)
	e.noteVerdict(health.TaskHealth{TaskID: "elsewhere", Status: health.StatusUnhealthy}, nil, nil, false)
	for _, sample := range e.HealthSamples() {
		if sample.TaskID == task.ID || sample.TaskID == "elsewhere" {
			t.Fatalf("non-owner exported %+v", sample)
		}
	}
}

func TestHealthSamplesIncludeUnevaluatedOwner(t *testing.T) {
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, _ := evalTask(t, s, data)
	store := healthstore.NewFileStore(data + "/.health.json")
	e := NewEvaluator(s, EvaluatorConfig{Mode: EvaluatorOn, Store: store, NodeID: "n", Instance: "i", Sweep: time.Minute, Debounce: time.Millisecond})
	var found bool
	for _, sample := range s.HealthSamples() {
		if sample.TaskID != task.ID {
			continue
		}
		found = true
		if sample.Value != 0 || sample.Reason != "unevaluated" {
			t.Fatalf("sample %+v", sample)
		}
	}
	if !found {
		t.Fatal("owned task missing before the first pass")
	}
	if _, err := e.EvaluateNow(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	found = false
	for _, sample := range s.HealthSamples() {
		if sample.TaskID == task.ID {
			found = true
			if sample.Value != 1 || sample.Reason != "ok" {
				t.Fatalf("after pass %+v", sample)
			}
		}
	}
	if !found {
		t.Fatal("owned task missing after the pass")
	}
}
