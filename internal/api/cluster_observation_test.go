// Package api provides module-level functionality for api.
// input: HTTP GET /api/cluster/overview, /api/workers, /metrics, /api/dashboard, and /api/sources/lookup
// output: cluster observation and source lookup from the store ownership copy; /metrics uses one snapshot per scrape
// pos: control-plane HTTP seam tests for cluster-wide ownership observation
// note: if this file changes, update this header and module README.md.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/tasks"
)

func seedClusterObservationStore(t *testing.T, items ...tasks.Task) *fakeAPIRunHistoryStore {
	t.Helper()
	store := newFakeAPIRunHistoryStore()
	for _, item := range items {
		store.tasks[item.ID] = item
	}
	return store
}

func getJSON(handler http.Handler, path string) *httptest.ResponseRecorder {
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, path, nil))
	return resp
}

func TestClusterObservation_FilteredDashboardIsNotClusterCount(t *testing.T) {
	store := seedClusterObservationStore(t,
		tasks.Task{
			ID:         "1",
			Name:       "src-a",
			ClusterKey: "src-a-key",
			State:      tasks.StateStopped,
			Source:     tasks.SourceConfig{Host: "db-a", Port: 3306, User: "repl"},
		},
		tasks.Task{
			ID:            "2",
			Name:          "src-b",
			ClusterKey:    "src-b-key",
			State:         tasks.StateRunning,
			OwnerWorkerID: "worker-a",
			Epoch:         3,
			Source:        tasks.SourceConfig{Host: "db-b", Port: 3307, User: "repl"},
		},
	)
	handler := NewServer(restoreSchedulerWithStore(t, store))
	store.tasks["3"] = tasks.Task{
		ID:            "3",
		Name:          "src-c",
		ClusterKey:    "src-c-key",
		State:         tasks.StateRunning,
		OwnerWorkerID: "worker-b",
		Epoch:         1,
		Source:        tasks.SourceConfig{Host: "db-c", Port: 3308, User: "repl"},
	}

	dashboardResp := getJSON(handler, "/api/dashboard?host=db-a")
	if dashboardResp.Code != http.StatusOK {
		t.Fatalf("dashboard returned %d body=%s", dashboardResp.Code, dashboardResp.Body.String())
	}
	var dashboard dashboardResponse
	if err := json.Unmarshal(dashboardResp.Body.Bytes(), &dashboard); err != nil {
		t.Fatalf("decode dashboard: %v", err)
	}
	if dashboard.Total != 1 || dashboard.Summary.Total != 1 {
		t.Fatalf("filtered dashboard total=%d summary.total=%d, want 1", dashboard.Total, dashboard.Summary.Total)
	}

	overviewResp := getJSON(handler, "/api/cluster/overview")
	if overviewResp.Code != http.StatusOK {
		t.Fatalf("overview returned %d body=%s", overviewResp.Code, overviewResp.Body.String())
	}
	var overview clusterOverview
	if err := json.Unmarshal(overviewResp.Body.Bytes(), &overview); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if overview.TaskCount == dashboard.Summary.Total {
		t.Fatalf("cluster task_count=%d matched filtered dashboard total=%d", overview.TaskCount, dashboard.Summary.Total)
	}
	if overview.TaskCount != 3 {
		t.Fatalf("cluster task_count=%d, want 3 from unfiltered store copy", overview.TaskCount)
	}
	if overview.RunningTaskCount != 2 {
		t.Fatalf("cluster running_task_count=%d, want 2", overview.RunningTaskCount)
	}
}

func TestClusterObservation_StoreOwnershipChangeUpdatesOverviewAndWorkers(t *testing.T) {
	store := seedClusterObservationStore(t, tasks.Task{
		ID:            "1",
		Name:          "owned",
		ClusterKey:    "owned-key",
		State:         tasks.StateStopped,
		OwnerWorkerID: "worker-stale",
		Epoch:         2,
		Source:        tasks.SourceConfig{Host: "db-a", Port: 3306, User: "repl"},
	})
	store.workers = []tasks.WorkerHeartbeat{
		{WorkerID: "worker-stale", Host: "host-stale", LastSeenAt: time.Now(), Status: "ONLINE"},
		{WorkerID: "worker-live", Host: "host-live", LastSeenAt: time.Now(), Status: "ONLINE"},
	}
	scheduler := restoreSchedulerWithStore(t, store)
	handler := NewServer(scheduler)

	current := store.tasks["1"]
	current.State = tasks.StateRunning
	current.OwnerWorkerID = "worker-live"
	current.Epoch = 9
	store.tasks["1"] = current

	memory := scheduler.ListTasks()
	if len(memory) != 1 || memory[0].OwnerWorkerID != "worker-stale" || memory[0].State != tasks.StateStopped {
		t.Fatalf("memory snapshot already refreshed: %+v", memory)
	}

	overviewResp := getJSON(handler, "/api/cluster/overview")
	if overviewResp.Code != http.StatusOK {
		t.Fatalf("overview returned %d body=%s", overviewResp.Code, overviewResp.Body.String())
	}
	var overview clusterOverview
	if err := json.Unmarshal(overviewResp.Body.Bytes(), &overview); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if overview.TaskCount != 1 || overview.RunningTaskCount != 1 || overview.LeasedTaskCount != 1 {
		t.Fatalf("overview counts=%+v, want task=1 running=1 leased=1", overview)
	}
	liveOverview := workerByID(overview.Workers, "worker-live")
	if liveOverview.TaskCount != 1 || liveOverview.Running != 1 || liveOverview.Leased != 1 {
		t.Fatalf("overview worker-live=%+v, want task=1 running=1 leased=1", liveOverview)
	}
	if stale := workerByID(overview.Workers, "worker-stale"); stale.WorkerID != "" {
		t.Fatalf("overview still lists stale owner: %+v", stale)
	}

	workersResp := getJSON(handler, "/api/workers")
	if workersResp.Code != http.StatusOK {
		t.Fatalf("workers returned %d body=%s", workersResp.Code, workersResp.Body.String())
	}
	var workers []workerItem
	if err := json.Unmarshal(workersResp.Body.Bytes(), &workers); err != nil {
		t.Fatalf("decode workers: %v", err)
	}
	liveWorkers := workerByID(workers, "worker-live")
	if liveWorkers.TaskCount != liveOverview.TaskCount || liveWorkers.Running != liveOverview.Running || liveWorkers.Leased != liveOverview.Leased {
		t.Fatalf("workers live=%+v overview live=%+v, want same task/running/leased", liveWorkers, liveOverview)
	}
	staleWorkers := workerByID(workers, "worker-stale")
	if staleWorkers.TaskCount != 0 || staleWorkers.Running != 0 || staleWorkers.Leased != 0 {
		t.Fatalf("workers stale owner still counted: %+v", staleWorkers)
	}
}

func workerByID(items []workerItem, id string) workerItem {
	for _, item := range items {
		if item.WorkerID == id {
			return item
		}
	}
	return workerItem{}
}

func TestClusterObservation_MetricsFollowsStoreNotMemory(t *testing.T) {
	store := seedClusterObservationStore(t, tasks.Task{
		ID:            "1",
		Name:          "owned",
		ClusterKey:    "owned-key",
		State:         tasks.StateStopped,
		OwnerWorkerID: "worker-stale",
		Epoch:         2,
		Source:        tasks.SourceConfig{Host: "db-a", Port: 3306, User: "repl"},
	})
	scheduler := restoreSchedulerWithStore(t, store)
	handler := NewServer(scheduler)

	current := store.tasks["1"]
	current.State = tasks.StateRunning
	current.OwnerWorkerID = "worker-live"
	current.Epoch = 9
	store.tasks["1"] = current

	memory := scheduler.ListTasks()
	if len(memory) != 1 || memory[0].State != tasks.StateStopped {
		t.Fatalf("memory snapshot already refreshed: %+v", memory)
	}

	resp := getJSON(handler, "/metrics")
	if resp.Code != http.StatusOK {
		t.Fatalf("metrics returned %d body=%s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if !strings.Contains(body, `binlog_server_task_state_count{state="RUNNING"} 1`) {
		t.Fatalf("metrics missing store RUNNING count, body=%s", body)
	}
	if strings.Contains(body, `binlog_server_task_state_count{state="STOPPED"}`) {
		t.Fatalf("metrics still used boot-time STOPPED copy, body=%s", body)
	}
}

func TestClusterObservation_NoStoreUsesMemoryList(t *testing.T) {
	scheduler := tasks.NewScheduler()
	handler := NewServer(scheduler)
	if _, err := scheduler.CreateTask("memory-a", "memory-a-key"); err != nil {
		t.Fatalf("CreateTask a: %v", err)
	}
	if _, err := scheduler.CreateTask("memory-b", "memory-b-key"); err != nil {
		t.Fatalf("CreateTask b: %v", err)
	}

	overviewResp := getJSON(handler, "/api/cluster/overview")
	if overviewResp.Code != http.StatusOK {
		t.Fatalf("overview returned %d body=%s", overviewResp.Code, overviewResp.Body.String())
	}
	var overview clusterOverview
	if err := json.Unmarshal(overviewResp.Body.Bytes(), &overview); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if overview.TaskCount != 2 || overview.RunningTaskCount != 0 {
		t.Fatalf("no-store overview=%+v, want task_count=2 from memory list", overview)
	}

	metricsResp := getJSON(handler, "/metrics")
	if metricsResp.Code != http.StatusOK {
		t.Fatalf("metrics returned %d body=%s", metricsResp.Code, metricsResp.Body.String())
	}
	if !strings.Contains(metricsResp.Body.String(), `binlog_server_task_state_count{state="CREATED"} 2`) {
		t.Fatalf("no-store metrics missing memory CREATED count, body=%s", metricsResp.Body.String())
	}
}

func TestClusterObservation_MetricsUsesOneStoreSnapshot(t *testing.T) {
	inner := seedClusterObservationStore(t, tasks.Task{
		ID:         "1",
		Name:       "owned",
		ClusterKey: "owned-key",
		State:      tasks.StateRunning,
		Source:     tasks.SourceConfig{Host: "db-a", Port: 3306, User: "repl"},
	})
	store := &countingClusterListStore{
		TaskStore: inner,
		failAt:    2,
		err:       errors.New("second snapshot failed"),
	}
	scheduler := restoreSchedulerWithStore(t, store)
	store.calls = 0
	handler := NewServer(scheduler)

	resp := getJSON(handler, "/metrics")
	if resp.Code != http.StatusOK {
		t.Fatalf("metrics returned %d body=%s", resp.Code, resp.Body.String())
	}
	if store.calls != 1 {
		t.Fatalf("ListTasks calls=%d, want 1 per scrape", store.calls)
	}
	if !strings.Contains(resp.Body.String(), `binlog_server_task_state_count{state="RUNNING"} 1`) {
		t.Fatalf("metrics missing store RUNNING count, body=%s", resp.Body.String())
	}
}

func TestSourceLookup_UsesStoreCopyNotMemory(t *testing.T) {
	store := seedClusterObservationStore(t, tasks.Task{
		ID:         "1",
		Name:       "src-a",
		ClusterKey: "src-a-key",
		State:      tasks.StateStopped,
		Source:     tasks.SourceConfig{Host: "db-stale", Port: 3306, User: "repl"},
	})
	scheduler := restoreSchedulerWithStore(t, store)
	handler := NewServer(scheduler)

	current := store.tasks["1"]
	current.Source.Host = "db-live"
	store.tasks["1"] = current
	store.tasks["2"] = tasks.Task{
		ID:         "2",
		Name:       "src-b",
		ClusterKey: "src-b-key",
		State:      tasks.StateCreated,
		Source:     tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"},
	}

	memory := scheduler.ListTasks()
	if len(memory) != 1 || memory[0].Source.Host != "db-stale" {
		t.Fatalf("memory snapshot already refreshed: %+v", memory)
	}

	staleResp := getJSON(handler, "/api/sources/lookup?host=db-stale&port=3306")
	if staleResp.Code != http.StatusOK {
		t.Fatalf("lookup db-stale returned %d body=%s", staleResp.Code, staleResp.Body.String())
	}
	var stale sourceLookupResponse
	if err := json.Unmarshal(staleResp.Body.Bytes(), &stale); err != nil {
		t.Fatalf("decode lookup db-stale: %v", err)
	}
	if stale.Exists || stale.Count != 0 || len(stale.TaskIDs) != 0 {
		t.Fatalf("lookup used boot-time memory host db-stale: %+v", stale)
	}

	liveResp := getJSON(handler, "/api/sources/lookup?host=db-live&port=3306")
	if liveResp.Code != http.StatusOK {
		t.Fatalf("lookup db-live returned %d body=%s", liveResp.Code, liveResp.Body.String())
	}
	var live sourceLookupResponse
	if err := json.Unmarshal(liveResp.Body.Bytes(), &live); err != nil {
		t.Fatalf("decode lookup db-live: %v", err)
	}
	if !live.Exists || live.Count != 1 || strings.Join(live.TaskIDs, ",") != "1" {
		t.Fatalf("lookup db-live=%+v, want task 1 from store copy", live)
	}

	loopbackResp := getJSON(handler, "/api/sources/lookup?host=localhost&port=3306")
	if loopbackResp.Code != http.StatusOK {
		t.Fatalf("lookup localhost returned %d body=%s", loopbackResp.Code, loopbackResp.Body.String())
	}
	var loopback sourceLookupResponse
	if err := json.Unmarshal(loopbackResp.Body.Bytes(), &loopback); err != nil {
		t.Fatalf("decode lookup localhost: %v", err)
	}
	if !loopback.Exists || loopback.Count != 1 || strings.Join(loopback.TaskIDs, ",") != "2" {
		t.Fatalf("lookup localhost=%+v, want task 2 via SameSourceHost on store copy", loopback)
	}
}

func TestClusterObservation_StoreErrorDoesNotReturnMemoryCopy(t *testing.T) {
	inner := newGetTaskFailLoudStore(staleOwnershipTask())
	store := &failingClusterListStore{TaskStore: inner}
	handler := NewServer(restoreSchedulerWithStore(t, store))
	store.err = errors.New("store unavailable")

	for _, path := range []string{"/api/cluster/overview", "/api/workers", "/metrics", "/api/sources/lookup?host=127.0.0.1&port=3306"} {
		resp := getJSON(handler, path)
		if resp.Code == http.StatusOK {
			t.Fatalf("%s returned 200 after store list error; body=%s", path, resp.Body.String())
		}
		if resp.Code < 500 {
			t.Fatalf("%s returned %d body=%s, want 5xx", path, resp.Code, resp.Body.String())
		}
	}
}

type failingClusterListStore struct {
	tasks.TaskStore
	err error
}

func (s *failingClusterListStore) ListTasks(ctx context.Context) ([]tasks.Task, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.TaskStore.ListTasks(ctx)
}

type countingClusterListStore struct {
	tasks.TaskStore
	calls  int
	failAt int
	err    error
}

func (s *countingClusterListStore) ListTasks(ctx context.Context) ([]tasks.Task, error) {
	s.calls++
	if s.failAt > 0 && s.calls >= s.failAt {
		return nil, s.err
	}
	return s.TaskStore.ListTasks(ctx)
}
