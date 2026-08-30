// Package api provides module-level functionality for api.
// input: replication progress snapshots and task state for delay/status mapping
// output: regression coverage that at-tip RUNNING lag stays NORMAL while catch-up lag stays DELAYED
// pos: API-layer test boundary for operator-visible replication delay semantics
// note: if this file changes, update this header and module README.md.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"binlog_server/internal/tasks"
)

// TestBuildReplicationResponse_RunningAtTipWithOldEventTimeIsNormal 验证
// LATEST 已经跟在源库当前 file/pos 时，即使 last_event_at 是几天前的 event header，
// 值班看到的也必须是 delay 0 / NORMAL，而不是 DELAYED。
func TestBuildReplicationResponse_RunningAtTipWithOldEventTimeIsNormal(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	lastEventAt := now.Add(-72 * time.Hour)
	task := tasks.Task{ID: "1", State: tasks.StateRunning}
	progress := tasks.ReplicationProgress{
		TaskID:        "1",
		LastEventAt:   lastEventAt,
		LastEventFile: "mysql-bin.000003",
		LastEventPos:  56456,
		UpdatedAt:     now,
		AtTip:         true,
	}

	resp := buildReplicationResponse(task, progress, true, now, 30)
	if resp.Status != "NORMAL" {
		t.Fatalf("operator saw status=%s reason=%s, want NORMAL when dump is already at LATEST tip", resp.Status, resp.Reason)
	}
	if resp.DelaySeconds != 0 {
		t.Fatalf("delay_seconds=%d, want 0 at LATEST tip even if last event header is days old", resp.DelaySeconds)
	}
	if resp.LastEventAt == nil || !resp.LastEventAt.Equal(lastEventAt) {
		t.Fatalf("last_event_at should remain header time for diagnostics, got %v", resp.LastEventAt)
	}
	if resp.State != tasks.StateRunning {
		t.Fatalf("state=%s, want RUNNING", resp.State)
	}
}

// TestBuildReplicationResponse_RunningBehindTipKeepsCatchUpDelay 验证
// FILE_POS/GTID 还在追旧事件时，不能因为 UpdatedAt 新鲜就把 delay 抹成 0。
func TestBuildReplicationResponse_RunningBehindTipKeepsCatchUpDelay(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	lastEventAt := now.Add(-72 * time.Hour)
	task := tasks.Task{ID: "2", State: tasks.StateRunning}
	progress := tasks.ReplicationProgress{
		TaskID:        "2",
		LastEventAt:   lastEventAt,
		LastEventFile: "mysql-bin.000001",
		LastEventPos:  4,
		UpdatedAt:     now,
		AtTip:         false,
	}

	resp := buildReplicationResponse(task, progress, true, now, 30)
	if resp.Status != "DELAYED" {
		t.Fatalf("status=%s reason=%s, want DELAYED while catch-up is still behind source tip", resp.Status, resp.Reason)
	}
	if resp.DelaySeconds != 72*60*60 {
		t.Fatalf("delay_seconds=%d, want 259200 for 72h catch-up lag", resp.DelaySeconds)
	}
	if resp.Reason != "DELAY_EXCEEDS_THRESHOLD" {
		t.Fatalf("reason=%s, want DELAY_EXCEEDS_THRESHOLD", resp.Reason)
	}
}

func startRunningTask(t *testing.T) (*tasks.Scheduler, http.Handler, tasks.Task) {
	t.Helper()
	runner := &apiReadyGateRunner{readyIDs: map[string]bool{}}
	scheduler := tasks.NewScheduler(tasks.WithRunner(runner))
	task, err := scheduler.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	runner.readyIDs[task.ID] = true
	if err := scheduler.ConfigureSource(task.ID, tasks.SourceConfig{
		Host: "127.0.0.1",
		Port: 3306,
		User: "repl",
	}); err != nil {
		t.Fatalf("ConfigureSource returned error: %v", err)
	}
	if err := scheduler.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask returned error: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		cur, err := scheduler.GetTask(task.ID)
		if err != nil {
			t.Fatalf("GetTask returned error: %v", err)
		}
		if cur.State == tasks.StateRunning {
			t.Cleanup(func() { _ = scheduler.StopTask(task.ID) })
			return scheduler, NewServer(scheduler), cur
		}
		if time.Now().After(deadline) {
			t.Fatalf("task did not reach running state in time: %s", cur.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func getReplicationJSON(t *testing.T, handler http.Handler, taskID string) map[string]any {
	t.Helper()
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/"+taskID+"/replication", nil)
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return body
}

// TestTaskAPI_GetReplication_LatestAtTipWithOldEventTimeIsNormal 验证值班立刻 GET
// /replication 时，已经在 LATEST tip 的 RUNNING 任务不能显示 DELAYED。
func TestTaskAPI_GetReplication_LatestAtTipWithOldEventTimeIsNormal(t *testing.T) {
	scheduler, handler, task := startRunningTask(t)
	lastEventAt := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	scheduler.ReportReplicationProgress(task.ID, lastEventAt, "mysql-bin.000003", 56456, true)

	body := getReplicationJSON(t, handler, task.ID)
	if body["state"] != string(tasks.StateRunning) {
		t.Fatalf("state=%v, want RUNNING", body["state"])
	}
	if body["status"] != "NORMAL" {
		t.Fatalf("operator saw status=%v reason=%v, want NORMAL at LATEST tip", body["status"], body["reason"])
	}
	if delay, _ := body["delay_seconds"].(float64); delay != 0 {
		t.Fatalf("delay_seconds=%v, want 0 at LATEST tip", body["delay_seconds"])
	}
	gotAt, _ := body["last_event_at"].(string)
	parsed, err := time.Parse(time.RFC3339, gotAt)
	if err != nil || !parsed.Equal(lastEventAt) {
		t.Fatalf("last_event_at=%v, want header time %s", body["last_event_at"], lastEventAt.Format(time.RFC3339))
	}
}

// TestTaskAPI_GetReplication_CatchUpBehindTipIsDelayed 验证 GET JSON 对仍在追位点的任务保留真实 lag。
func TestTaskAPI_GetReplication_CatchUpBehindTipIsDelayed(t *testing.T) {
	scheduler, handler, task := startRunningTask(t)
	lastEventAt := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	scheduler.ReportReplicationProgress(task.ID, lastEventAt, "mysql-bin.000001", 4, false)

	body := getReplicationJSON(t, handler, task.ID)
	if body["status"] != "DELAYED" {
		t.Fatalf("status=%v reason=%v, want DELAYED while catch-up is behind tip", body["status"], body["reason"])
	}
	if delay, _ := body["delay_seconds"].(float64); delay < 30 {
		t.Fatalf("delay_seconds=%v, want catch-up lag above threshold", body["delay_seconds"])
	}
}
