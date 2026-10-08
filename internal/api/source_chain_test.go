// Package api provides module-level functionality for api.
// input: GET /api/tasks/{id}, GET /api/tasks/{id}/files, and GET /metrics for a task with a source chain
// output: assertions that source_chain and source_identity appear for a continued switch and a stopped switch, and that binlog_server_source_switchovers counts both outcomes
// pos: HTTP coverage for the VIP source-chain view and its gauge
// note: if this file changes, update this header and module README.md.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"binlog_server/internal/tasks"
)

type switchEventStore struct {
	mu     sync.Mutex
	events map[string][]tasks.TaskEvent
}

func newSwitchEventStore() *switchEventStore {
	return &switchEventStore{events: map[string][]tasks.TaskEvent{}}
}

func (s *switchEventStore) add(event tasks.TaskEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events[event.TaskID] = append(s.events[event.TaskID], event)
}

func (s *switchEventStore) AppendEvent(_ context.Context, event tasks.TaskEvent) error {
	s.add(event)
	return nil
}

func (s *switchEventStore) ListEvents(_ context.Context, taskID string, limit int) ([]tasks.TaskEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.events[taskID]
	if limit <= 0 || limit >= len(items) {
		out := make([]tasks.TaskEvent, len(items))
		copy(out, items)
		return out, nil
	}
	out := make([]tasks.TaskEvent, limit)
	copy(out, items[len(items)-limit:])
	return out, nil
}

func TestTaskAPI_SourceChainMetricAndFiles(t *testing.T) {
	dir := t.TempDir()
	events := newSwitchEventStore()
	scheduler := tasks.NewScheduler(tasks.WithDataDir(dir), tasks.WithEventStore(events))
	handler := NewServer(scheduler)
	create(`{"name":"continued","cluster_key":"continued-key","source":{"host":"127.0.0.1","port":3306,"user":"repl","password":"secret","flavor":"mysql"}}`, t, handler)
	create(`{"name":"stopped","cluster_key":"stopped-key","source":{"host":"127.0.0.1","port":3307,"user":"repl","password":"secret","flavor":"mysql"}}`, t, handler)
	bare := decodeTaskJSON(t, handler, "/api/tasks/1")
	if _, ok := bare["source_chain"]; ok {
		t.Fatalf("source_chain present before a switch: %#v", bare["source_chain"])
	}

	oldID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	newID := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	if err := os.MkdirAll(filepath.Join(dir, "1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1", ".source-chain"), []byte(oldID+"\n"+newID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1", "mysql-bin.000001"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1", newID+".mysql-bin.000001"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	events.add(tasks.TaskEvent{
		TaskID:  "1",
		Type:    "SOURCE_SWITCHOVER",
		Message: "source switched from " + oldID + " to " + newID + " at mysql-bin.000001:20 gtid_set=" + oldID + ":1-3; continuing from the executed GTID set",
		Detail:  "old=" + oldID + " new=" + newID + " gtid_set=" + oldID + ":1-3 file=mysql-bin.000001 pos=20",
	})
	events.add(tasks.TaskEvent{
		TaskID:  "2",
		Type:    "SOURCE_SWITCHOVER",
		Message: "source switched from " + oldID + " to " + newID + ". This backup has no GTID set, so the old source file and position cannot be applied to the new source. Start a new task against the new primary and keep this backup. This task will not mix the two servers.",
		Detail:  "old=" + oldID + " new=" + newID + " gtid_set= file=mysql-bin.000009 pos=4",
	})

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/api/tasks/9", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing task status=%d", missing.Code)
	}

	continued := decodeTaskJSON(t, handler, "/api/tasks/1")
	if _, ok := continued["source_chain"]; !ok {
		t.Fatalf("missing source_chain: %#v", continued)
	}
	chain := continued["source_chain"].(map[string]any)
	if chain["outcome"] != "continued" || chain["current"] != newID {
		t.Fatalf("chain %#v", chain)
	}
	servers := chain["servers"].([]any)
	if len(servers) != 2 {
		t.Fatalf("servers %#v", servers)
	}
	first := servers[0].(map[string]any)
	second := servers[1].(map[string]any)
	if first["identity"] != oldID || first["current"] != false || second["identity"] != newID || second["current"] != true {
		t.Fatalf("servers %#v", servers)
	}
	sw := chain["switches"].([]any)[0].(map[string]any)
	if sw["old"] != oldID || sw["new"] != newID || sw["continued"] != true || sw["file"] != "mysql-bin.000001" || sw["gtid_set"] != oldID+":1-3" {
		t.Fatalf("switch %#v", sw)
	}
	if sw["pos"] != float64(20) {
		t.Fatalf("pos %#v", sw["pos"])
	}

	filesResp := httptest.NewRecorder()
	handler.ServeHTTP(filesResp, httptest.NewRequest(http.MethodGet, "/api/tasks/1/files?limit=50", nil))
	if filesResp.Code != http.StatusOK {
		t.Fatalf("files status=%d body=%s", filesResp.Code, filesResp.Body.String())
	}
	var files []map[string]any
	if err := json.Unmarshal(filesResp.Body.Bytes(), &files); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, file := range files {
		id, _ := file["source_identity"].(string)
		seen[file["file_name"].(string)] = id
	}
	if seen["mysql-bin.000001"] != oldID || seen[newID+".mysql-bin.000001"] != newID {
		t.Fatalf("file identities %#v", seen)
	}

	stopped := decodeTaskJSON(t, handler, "/api/tasks/2")
	stoppedChain := stopped["source_chain"].(map[string]any)
	if stoppedChain["outcome"] != "stopped" || stoppedChain["current"] != oldID {
		t.Fatalf("stopped %#v", stoppedChain)
	}
	stoppedSwitch := stoppedChain["switches"].([]any)[0].(map[string]any)
	if stoppedSwitch["continued"] != false || stoppedSwitch["reason"] != "no_gtid" {
		t.Fatalf("stopped switch %#v", stoppedSwitch)
	}

	metrics := httptest.NewRecorder()
	handler.ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if metrics.Code != http.StatusOK {
		t.Fatalf("metrics status=%d body=%s", metrics.Code, metrics.Body.String())
	}
	body := metrics.Body.String()
	assertSwitchMetric(t, body, "1", "continued", 1)
	assertSwitchMetric(t, body, "1", "stopped", 0)
	assertSwitchMetric(t, body, "2", "continued", 0)
	assertSwitchMetric(t, body, "2", "stopped", 1)
}

func TestMetricsSourceSwitchoverEmpty(t *testing.T) {
	handler := NewServer(tasks.NewScheduler())
	metrics := httptest.NewRecorder()
	handler.ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if metrics.Code != http.StatusOK {
		t.Fatalf("status=%d", metrics.Code)
	}
	assertSwitchMetric(t, metrics.Body.String(), "", "continued", 0)
	assertSwitchMetric(t, metrics.Body.String(), "", "stopped", 0)
}

func assertSwitchMetric(t *testing.T, body, taskID, outcome string, want float64) {
	t.Helper()
	got, ok, err := readPromMetricValueWithLabels(body, "binlog_server_source_switchovers", map[string]string{
		"task_id": taskID,
		"outcome": outcome,
	})
	if err != nil || !ok || got != want {
		t.Fatalf("metric task=%q outcome=%s got=%v ok=%v err=%v\n%s", taskID, outcome, got, ok, err, body)
	}
}

func decodeTaskJSON(t *testing.T, handler http.Handler, path string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
