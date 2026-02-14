# Binlog Server MVP Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build a runnable Go MVP for centralized MySQL binlog backup with reliable local persistence and resumable checkpoints.

**Architecture:** Implement a single-process service with modular packages: admin API, task scheduler, replication interface, local binlog writer, and metadata abstraction. Start with core reliability path (`fsync`-gated checkpoint advancement), then add task lifecycle and API surface. Keep storage/replication behind interfaces so real MySQL protocol pulling can be integrated incrementally without redesign.

**Tech Stack:** Go 1.22+, net/http, encoding/json, standard testing package.

---

### Task 1: Initialize Go Project Skeleton

**Files:**
- Create: `go.mod`
- Create: `cmd/binlog-server/main.go`
- Create: `internal/app/app.go`
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Step 1: Write the failing test**

```go
func TestLoadConfig_DefaultValues(t *testing.T) {
    cfg, err := LoadConfig("")
    if err != nil {
        t.Fatalf("LoadConfig returned error: %v", err)
    }
    if cfg.ListenAddr != ":8080" {
        t.Fatalf("expected :8080, got %s", cfg.ListenAddr)
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/config -run TestLoadConfig_DefaultValues -v`
Expected: FAIL because `LoadConfig` does not exist.

**Step 3: Write minimal implementation**

Implement config model and loader with defaults and optional env overrides.

**Step 4: Run test to verify it passes**

Run: `go test ./internal/config -run TestLoadConfig_DefaultValues -v`
Expected: PASS.

**Step 5: Commit**

```bash
git add go.mod cmd/binlog-server/main.go internal/app/app.go internal/config/config.go internal/config/config_test.go
git commit -m "feat: bootstrap go service skeleton"
```

### Task 2: Implement Reliable Binlog Writer (`fsync` before checkpoint)

**Files:**
- Create: `internal/binlog/writer.go`
- Create: `internal/binlog/checkpoint.go`
- Test: `internal/binlog/writer_test.go`

**Step 1: Write the failing tests**

```go
func TestWriter_AdvanceCheckpointAfterSync(t *testing.T)
func TestWriter_NoCheckpointAdvanceWhenSyncFails(t *testing.T)
```

Assertions:
- checkpoint advances only when `Sync()` succeeds.
- checkpoint remains unchanged when `Sync()` returns error.

**Step 2: Run tests to verify they fail**

Run: `go test ./internal/binlog -run TestWriter -v`
Expected: FAIL because writer/checkpoint types do not exist.

**Step 3: Write minimal implementation**

Implement:
- `Checkpoint` struct (`File`, `Pos`, `GTIDSet`, `UpdatedAt`)
- `Writer` with `Append(event []byte, next Checkpoint)` and `FlushAndCheckpoint()`
- dependency injection for file handle (`Write`, `Sync`) to unit-test sync failure path.

**Step 4: Run tests to verify they pass**

Run: `go test ./internal/binlog -run TestWriter -v`
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/binlog/writer.go internal/binlog/checkpoint.go internal/binlog/writer_test.go
git commit -m "feat: enforce fsync-gated checkpoint advancement"
```

### Task 3: Implement Task State Machine and In-Memory Scheduler Core

**Files:**
- Create: `internal/tasks/model.go`
- Create: `internal/tasks/scheduler.go`
- Test: `internal/tasks/scheduler_test.go`

**Step 1: Write the failing tests**

```go
func TestScheduler_StartTaskTransitionsToRunning(t *testing.T)
func TestScheduler_RetryableErrorTransitionsToBackoff(t *testing.T)
func TestScheduler_StopTaskTransitionsToStopped(t *testing.T)
```

Validate transitions:
- `CREATED -> STARTING -> RUNNING`
- retryable failure -> `RETRY_BACKOFF`
- stop request -> `STOPPING -> STOPPED`

**Step 2: Run tests to verify they fail**

Run: `go test ./internal/tasks -run TestScheduler -v`
Expected: FAIL because scheduler model does not exist.

**Step 3: Write minimal implementation**

Implement task state enum, transition guards, in-memory scheduler store, and methods:
- `CreateTask`
- `StartTask`
- `MarkRetryableError`
- `StopTask`
- `GetTask`

**Step 4: Run tests to verify they pass**

Run: `go test ./internal/tasks -run TestScheduler -v`
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/tasks/model.go internal/tasks/scheduler.go internal/tasks/scheduler_test.go
git commit -m "feat: add task lifecycle state machine"
```

### Task 4: Expose Admin API for Task CRUD + Start/Stop

**Files:**
- Create: `internal/api/server.go`
- Create: `internal/api/handlers_tasks.go`
- Modify: `internal/app/app.go`
- Test: `internal/api/server_test.go`

**Step 1: Write the failing tests**

Test endpoints:
- `POST /api/tasks`
- `GET /api/tasks`
- `POST /api/tasks/{id}/start`
- `POST /api/tasks/{id}/stop`

**Step 2: Run tests to verify they fail**

Run: `go test ./internal/api -run TestTaskAPI -v`
Expected: FAIL because API server does not exist.

**Step 3: Write minimal implementation**

Implement HTTP server with JSON handlers bound to scheduler interface.

**Step 4: Run tests to verify they pass**

Run: `go test ./internal/api -run TestTaskAPI -v`
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/api/server.go internal/api/handlers_tasks.go internal/api/server_test.go internal/app/app.go
git commit -m "feat: add admin task management api"
```

### Task 5: Wire Runnable Binary + End-to-End Smoke Test

**Files:**
- Modify: `cmd/binlog-server/main.go`
- Create: `internal/app/smoke_test.go`
- Modify: `README.md`

**Step 1: Write the failing smoke test**

```go
func TestApp_StartAndServeHealth(t *testing.T)
```

Start app on random port and assert `/healthz` returns `200`.

**Step 2: Run test to verify it fails**

Run: `go test ./internal/app -run TestApp_StartAndServeHealth -v`
Expected: FAIL.

**Step 3: Write minimal implementation**

Wire config + scheduler + API server + health endpoint in app bootstrap.

**Step 4: Run tests to verify it passes**

Run: `go test ./internal/app -run TestApp_StartAndServeHealth -v`
Expected: PASS.

**Step 5: Commit**

```bash
git add cmd/binlog-server/main.go internal/app/smoke_test.go README.md
git commit -m "feat: runnable binlog server mvp bootstrap"
```

### Task 6: Full Verification

**Files:**
- N/A

**Step 1: Run full test suite**

Run: `go test ./...`
Expected: all tests PASS.

**Step 2: Run formatting and vet checks**

Run:
- `gofmt -w ./cmd ./internal`
- `go test ./...`

Expected: no formatting diffs and tests PASS.

**Step 3: Commit final polish**

```bash
git add .
git commit -m "chore: finalize mvp baseline with tests"
```
