# Cluster HA (Strict Binlog File Semantics) Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 在不破坏现有 standalone 行为的前提下，引入 cluster 模式（control-plane + worker），实现多 worker 高可用抢占，并严格保证每个 `mysql-bin.xxxxxx` 为单一完整、字节一致文件。元数据 MySQL 在 orchestrator failover 期间应具备可恢复与 fail-safe 行为。

**Architecture:** 在现有 `tasks + replication + meta + api + app` 结构上，新增 cluster 抽象层（lease/heartbeat/run/session），由 worker 直接与元数据库交互执行 lease fencing。严格文件语义通过 `epoch + OPEN/SEALED 状态机 + rebuild_current_file` 达成。控制面仅承担管理与可观测，不在拉流数据路径。

**Tech Stack:** Go, Gin, MySQL (metadata), Viper, docker-compose e2e, orchestrator, go test.

---

### Task 1: 配置模型扩展（standalone/cluster 双模式）

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`
- Modify: `config.example.yaml`

**Step 1: Write the failing tests**

新增测试：

- `TestLoadConfig_ClusterDefaults`
- `TestLoadConfig_ClusterFromYAML`

断言字段：

- `mode` 默认 `standalone`
- `cluster.role` 默认 `all-in-one`
- `lease_ttl_sec/lease_renew_interval_sec/lease_grace_sec` 默认值正确

**Step 2: Run test to verify it fails**

Run: `go test ./internal/config -run Cluster -v`  
Expected: FAIL（字段不存在）。

**Step 3: Write minimal implementation**

扩展 `Config`：

- `Mode` (`standalone|cluster`)
- `Cluster.Role` (`control-plane|worker|all-in-one`)
- `Cluster.WorkerID`
- `Cluster.LeaseTTLSec`
- `Cluster.LeaseRenewIntervalSec`
- `Cluster.LeaseGraceSec`
- `Cluster.FailoverPolicy`（固定 `rebuild_current_file`）

补充加载规则与默认值。

**Step 4: Run test to verify it passes**

Run: `go test ./internal/config -run Cluster -v`  
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go config.example.yaml
git commit -m "feat(config): add cluster mode settings"
```

---

### Task 2: 元数据 schema 迁移（lease / run / file state）

**Files:**
- Modify: `internal/meta/mysql_store.go`
- Test: `internal/meta/mysql_store_test.go`
- Create: `internal/meta/lease_store.go`
- Test: `internal/meta/lease_store_test.go`

**Step 1: Write the failing tests**

新增用例：

- `TestMySQLTaskStore_InitSchemaIncludesLeaseTables`
- `TestLeaseStore_AcquireRenewRelease`
- `TestLeaseStore_FencingByEpoch`

**Step 2: Run test to verify it fails**

Run: `go test ./internal/meta -run Lease -v`  
Expected: FAIL（表/方法不存在）。

**Step 3: Write minimal implementation**

新增表：

- `task_leases(task_id PK, owner_worker_id, epoch, lease_expire_at, renewed_at)`
- `task_runs(run_id PK, task_id, worker_id, epoch, started_at, ended_at, end_reason)`

扩展 `binlog_files` 字段：

- `epoch`, `state`, `checksum`, `source_file`

实现 `LeaseStore`：

- `Acquire(taskID, workerID, now, ttl) -> (epoch, ok)`
- `Renew(taskID, workerID, epoch, now, ttl) -> ok`
- `Get(taskID)`
- `Release(taskID, workerID, epoch)`

要求：SQL 层 CAS 语义，防止并发双 owner。

**Step 4: Run test to verify it passes**

Run: `go test ./internal/meta -run Lease -v`  
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/meta/mysql_store.go internal/meta/mysql_store_test.go internal/meta/lease_store.go internal/meta/lease_store_test.go
git commit -m "feat(meta): add lease/run schema and lease store"
```

---

### Task 3: Retry + failover-safe DB 操作封装

**Files:**
- Create: `internal/meta/retry.go`
- Test: `internal/meta/retry_test.go`
- Modify: `internal/meta/mysql_store.go`

**Step 1: Write the failing tests**

新增：

- `TestWithRetry_RetryOnTransientErrors`
- `TestWithRetry_StopOnPermanentErrors`
- `TestWithRetry_DeadlineExceeded`

**Step 2: Run test to verify it fails**

Run: `go test ./internal/meta -run WithRetry -v`  
Expected: FAIL。

**Step 3: Write minimal implementation**

实现 `WithRetry(ctx, policy, fn)`：

- 指数退避 + jitter
- 可分类 transient/permanent 错误
- 上下文超时终止

把 lease/checkpoint/file state 写操作改为通过 `WithRetry`。

**Step 4: Run test to verify it passes**

Run: `go test ./internal/meta -run WithRetry -v`  
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/meta/retry.go internal/meta/retry_test.go internal/meta/mysql_store.go
git commit -m "feat(meta): add retry wrapper for mysql failover windows"
```

---

### Task 4: Scheduler 接入 cluster 运行会话与 lease fencing

**Files:**
- Modify: `internal/tasks/model.go`
- Modify: `internal/tasks/scheduler.go`
- Test: `internal/tasks/scheduler_test.go`
- Create: `internal/tasks/lease_test.go`

**Step 1: Write the failing tests**

新增：

- `TestScheduler_ClusterStartRequiresLease`
- `TestScheduler_LeaseLostTransitionsToStoppingStopped`
- `TestScheduler_LeaseRenewFailureEntersDegradedThenStop`

**Step 2: Run test to verify it fails**

Run: `go test ./internal/tasks -run Lease -v`  
Expected: FAIL。

**Step 3: Write minimal implementation**

扩展状态与字段：

- `LEASE_DEGRADED`, `REBUILDING_FILE`
- `OwnerWorkerID`, `Epoch`, `RunID`

在 cluster 模式启动路径：

- 先 acquire lease
- 启动 renew goroutine
- renew 超时触发 fail-safe stop

**Step 4: Run test to verify it passes**

Run: `go test ./internal/tasks -run Lease -v`  
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/tasks/model.go internal/tasks/scheduler.go internal/tasks/scheduler_test.go internal/tasks/lease_test.go
git commit -m "feat(tasks): add lease-driven cluster run lifecycle"
```

---

### Task 5: Runner strict 文件状态机（OPEN/SEALED + epoch 文件名）

**Files:**
- Modify: `internal/replication/mysql_runner.go`
- Test: `internal/replication/mysql_runner_test.go`
- Create: `internal/replication/file_state_test.go`

**Step 1: Write the failing tests**

新增：

- `TestRunner_OpenFileUsesEpochSuffix`
- `TestRunner_SealRequiresLeaseAndEpochMatch`
- `TestRunner_NeverPublishOpenFile`

**Step 2: Run test to verify it fails**

Run: `go test ./internal/replication -run FileState -v`  
Expected: FAIL。

**Step 3: Write minimal implementation**

实现规则：

- OPEN 文件：`<file>.open.e<epoch>`
- rotate 后 seal：重命名为 `<file>`，写 `SEALED`
- 任何 lease/epoch 不匹配时，禁止 seal/upload

**Step 4: Run test to verify it passes**

Run: `go test ./internal/replication -run FileState -v`  
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/replication/mysql_runner.go internal/replication/mysql_runner_test.go internal/replication/file_state_test.go
git commit -m "feat(replication): enforce strict open/sealed file semantics"
```

---

### Task 6: 接管恢复 `rebuild_current_file`（严格单文件一致）

**Files:**
- Modify: `internal/replication/mysql_runner.go`
- Test: `internal/replication/checkpoint_resume_test.go`
- Create: `internal/replication/rebuild_current_file_test.go`

**Step 1: Write the failing tests**

新增：

- `TestRunner_RebuildCurrentFileAfterTakeover`
- `TestRunner_TakeoverProducesSingleSealedFile`

**Step 2: Run test to verify it fails**

Run: `go test ./internal/replication -run RebuildCurrentFile -v`  
Expected: FAIL。

**Step 3: Write minimal implementation**

接管逻辑：

- 新 owner（epoch+1）读取 source 当前 file
- 从 `pos=4` 重建该 file 完整内容
- 追平后切回实时拉流

**Step 4: Run test to verify it passes**

Run: `go test ./internal/replication -run RebuildCurrentFile -v`  
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/replication/mysql_runner.go internal/replication/checkpoint_resume_test.go internal/replication/rebuild_current_file_test.go
git commit -m "feat(replication): add rebuild-current-file takeover flow"
```

---

### Task 7: 老 worker 恢复后的 OPEN 文件清理

**Files:**
- Modify: `internal/replication/mysql_runner.go`
- Create: `internal/replication/stale_open_cleanup_test.go`

**Step 1: Write the failing test**

新增：

- `TestRunner_CleanupStaleOpenFilesOnStartup`

断言：非当前 epoch 的 `.open.e*` 启动后被清理；不会影响已 seal 文件。

**Step 2: Run test to verify it fails**

Run: `go test ./internal/replication -run CleanupStaleOpen -v`  
Expected: FAIL。

**Step 3: Write minimal implementation**

worker 启动时：

- 读取本地任务目录
- 清理非当前 epoch OPEN 文件（可先改名 quarantine 再删）

**Step 4: Run test to verify it passes**

Run: `go test ./internal/replication -run CleanupStaleOpen -v`  
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/replication/mysql_runner.go internal/replication/stale_open_cleanup_test.go
git commit -m "feat(replication): cleanup stale open files after worker recovery"
```

---

### Task 8: App 组装 role（control-plane / worker / all-in-one）

**Files:**
- Modify: `internal/app/app.go`
- Test: `internal/app/smoke_test.go`
- Modify: `cmd/binlog-server/main.go`

**Step 1: Write the failing tests**

新增：

- `TestApp_ClusterControlPlaneRole`
- `TestApp_ClusterWorkerRole`
- `TestApp_ClusterAllInOneRole`

**Step 2: Run test to verify it fails**

Run: `go test ./internal/app -run Cluster -v`  
Expected: FAIL。

**Step 3: Write minimal implementation**

按 role 装配：

- control-plane：启动 API + scheduler 管理能力，不起拉流 worker loops
- worker：起 worker loops，可选健康接口
- all-in-one：两者都起

**Step 4: Run test to verify it passes**

Run: `go test ./internal/app -run Cluster -v`  
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/app/app.go internal/app/smoke_test.go cmd/binlog-server/main.go
git commit -m "feat(app): wire cluster roles"
```

---

### Task 9: 控制面 API（workers / lease / runs / cluster overview）

**Files:**
- Modify: `internal/api/server.go`
- Modify: `internal/api/handlers_tasks.go`
- Create: `internal/api/handlers_cluster.go`
- Test: `internal/api/server_test.go`
- Modify: `internal/api/swagger_docs_only.go`
- Modify: `internal/swaggerdocs/*`

**Step 1: Write the failing tests**

新增接口测试：

- `GET /api/workers`
- `GET /api/tasks/{id}/lease`
- `GET /api/tasks/{id}/runs`
- `GET /api/cluster/overview`

**Step 2: Run test to verify it fails**

Run: `go test ./internal/api -run Cluster -v`  
Expected: FAIL。

**Step 3: Write minimal implementation**

返回最小可用字段，满足 UI 聚合视图。

**Step 4: Run test to verify it passes**

Run: `go test ./internal/api -run Cluster -v`  
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/api/server.go internal/api/handlers_tasks.go internal/api/handlers_cluster.go internal/api/server_test.go internal/api/swagger_docs_only.go internal/swaggerdocs
git commit -m "feat(api): add cluster worker/lease/run endpoints"
```

---

### Task 10: e2e - orchestrator failover 窗口回归

**Files:**
- Modify: `deploy/e2e/docker-compose.yml`
- Modify: `scripts/e2e/run-suite.sh`
- Create: `scripts/e2e/smoke-meta-failover.sh`
- Modify: `scripts/e2e/README.md`

**Step 1: Write failing scenario script (RED)**

场景：

1. 启动 cluster(all-in-one + extra worker)
2. 创建并启动复制任务
3. 触发元数据库主从切换（orchestrator recover）
4. 观察：任务短暂 degraded 后恢复，最终 `SEALED` 文件唯一且 checksum 正确

**Step 2: Run to verify it fails**

Run: `./scripts/e2e/smoke-meta-failover.sh`  
Expected: FAIL（脚本/实现未完成）。

**Step 3: Write minimal implementation**

补全脚本编排与断言。

**Step 4: Run to verify it passes**

Run: `./scripts/e2e/smoke-meta-failover.sh`  
Expected: PASS.

**Step 5: Commit**

```bash
git add deploy/e2e/docker-compose.yml scripts/e2e/run-suite.sh scripts/e2e/smoke-meta-failover.sh scripts/e2e/README.md
git commit -m "test(e2e): add metadata failover recovery scenario"
```

---

### Task 11: 前端集群视图（统一管理页）

**Files:**
- Modify: `frontend/src/App.vue`
- Modify: `frontend/src/api.js`
- Test: `frontend` (build-level)

**Step 1: Write visual acceptance checklist**

- worker 列表（在线/离线）
- 任务归属 worker
- lease 过期风险标记
- run history 最近 N 条

**Step 2: Run current build to establish baseline**

Run: `cd frontend && npm run build`  
Expected: PASS。

**Step 3: Implement minimal UI integration**

接入新 API，提供 cluster overview 卡片与任务详情扩展区。

**Step 4: Run build and smoke check**

Run: `cd frontend && npm run build`  
Expected: PASS.

**Step 5: Commit**

```bash
git add frontend/src/App.vue frontend/src/api.js
git commit -m "feat(frontend): add cluster visibility panels"
```

---

### Task 12: 全量验证与文档收尾

**Files:**
- Modify: `README.md`
- Modify: `docs/deployment-modes.md`
- Modify: `docs/plans/2026-02-16-cluster-ha-design.md`（如需同步实现差异）

**Step 1: Run verification suite**

Run:

- `go test ./...`
- `cd frontend && npm run build`
- `./scripts/e2e/run-suite.sh --scenarios smoke,compression`
- `./scripts/e2e/run-suite.sh --scenarios meta-failover`

Expected: 全通过。

**Step 2: Update docs**

补充：

- cluster 启动示例
- role 部署建议
- failover 行为说明与告警解释

**Step 3: Commit**

```bash
git add README.md docs/deployment-modes.md docs/plans/2026-02-16-cluster-ha-design.md
git commit -m "docs: finalize cluster ha operational guide"
```

---

## 风险与回滚策略

1. 引入 cluster 后默认仍为 `standalone`，保证老用户无感升级。
2. strict 语义导致 failover 窗口可能短暂停拉，这是设计权衡（优先文件一致性）。
3. 任何 lease 不确定状态必须 fail-safe 停止，禁止继续写 OPEN 文件。
4. 回滚路径：
   - 配置切回 `mode: standalone`
   - 保留已有任务元数据结构，不删除旧表，仅停用 cluster 功能。
