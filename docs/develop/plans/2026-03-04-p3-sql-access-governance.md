# P3 SQL 访问层治理：决策报告与试点结果

## 1) 决策评估

### 复杂度评估（基于当前 `internal/meta`）
- SQL 总量：
  - 既有手写 SQL 常量：31 条（`rg '^const .*SQL =`' internal/meta/*.go`）
  - 本阶段 sqlc 查询定义：11 条（`internal/meta/sql/*.sql`）
- 手写 Scan 点位：13 处（`rg '\\.Scan\\(' internal/meta/*.go`）
- 事务分布：核心事务 1 处（`MySQLTaskStore.UpsertTask`，含 task 基础信息 + run 状态联动）
- 复杂语句分布：主要集中在
  - `task_leases`（`ON DUPLICATE KEY UPDATE` + `IF` + `DATE_ADD`）
  - `worker_registrations`（同 session 续租/过期接管）
  - `task_runs`/`worker_heartbeats`（UPSERT + 列表查询）

### sqlc vs gorm 适配性对比（结合当前复杂 SQL）
| 维度 | sqlc | gorm |
|---|---|---|
| 复杂 SQL 可控性 | 保留原 SQL，语义透明 | 复杂 SQL 仍需 raw SQL，抽象收益有限 |
| 行为一致性风险 | 低，主要是调用层替换 | 中，ORM 映射/默认行为更易引入隐式变更 |
| 事务内渐进迁移 | 强，可 `sqlcgen.New(tx)` 按语句替换 | 可行但事务 + 原生 SQL 混用复杂度更高 |
| 类型安全/Scan 负担 | 自动生成类型，减少手写 Scan | 依赖 struct tag/模型，和现有结构耦合更深 |
| 回滚成本 | 低，保留 SQL 与 store 接口 | 中，模型层引入后回退成本更高 |

### 结论与本阶段试点路线
- 结论：采用 **sqlc**。
- 本阶段路线：
  1. 试点 `lease_store`。
  2. 试点 `task_runs + worker_heartbeats`。
  3. 保持对外接口与错误语义不变，仅替换内部 SQL 调用实现。

## 2) 试点实现结果

### 覆盖范围
- `internal/meta/lease_store.go`
  - `Acquire/Renew/Get/Release/currentDBTime` 切换到 `sqlcgen`。
- `internal/meta/mysql_store.go`
  - `UpsertTask` 事务内 run 状态读写：`LoadTaskRunState/UpsertTaskRun/FinishTaskRun`。
  - `ListTaskRuns/UpsertWorkerHeartbeat/ListWorkerHeartbeats` 切换到 `sqlcgen`。

### 兼容性结论
- 对外行为语义保持不变：
  - API 状态码无改动。
  - 任务状态机流转无改动。
  - 关键错误处理路径保持既有语义（仍由原调用栈返回 DB/上下文错误）。

## 3) sqlc 组织方案（签名与调用适配）

### 目录与生成包
- 配置文件：`sqlc.yaml`
- 查询目录：`internal/meta/sql/`
  - `lease.sql`
  - `task_runs_worker_heartbeats.sql`
- 生成包：`internal/meta/sqlcgen`

### lease 相关 queries
- `AcquireTaskLease(task_id, worker_id, ttl_micros) -> exec`
- `RenewTaskLease(ttl_micros, task_id, worker_id, epoch) -> execresult`
- `ReleaseTaskLease(task_id, worker_id, epoch) -> execresult`
- `GetTaskLease(task_id) -> one(TaskLease)`
- `GetCurrentDBTime() -> one(time.Time)`

### task_runs / worker_heartbeats 相关 queries
- `LoadTaskRunState(task_id) -> one(sql.NullString)`
- `UpsertTaskRun(run_id, task_id, worker_id, epoch, started_at) -> exec`
- `FinishTaskRun(ended_at, end_reason, run_id) -> exec`
- `ListTaskRuns(task_id, limit) -> many(TaskRun)`
- `UpsertWorkerHeartbeat(worker_id, host, version, last_seen_at, status) -> exec`
- `ListWorkerHeartbeats(limit) -> many(WorkerHeartbeat)`

### 调用侧适配方式
- 非事务调用：`q := sqlcgen.New(s.db)`
- 事务调用：`q := sqlcgen.New(tx)`
- 业务层接口保持原样，调用方无感知。

## 4) 与 golang-migrate 集成流程

统一流程：`schema -> migrate -> generate -> compile/test`
1. 修改 schema/migration（`migrations/*.sql`）。
2. 使用 `cmd/migrate` 或现有迁移流程应用数据库版本。
3. 执行 `make sqlc-generate` 生成类型化访问代码。
4. 执行编译/测试（`go test ./...` 等）验证行为与兼容性。

## 5) Makefile 与 CI 接入

### Makefile 目标
- `make sqlc-generate`
- `make sqlc-verify`（先 generate，再 `git diff --exit-code`）

### CI 接入
- 在 `.github/workflows/e2e.yml` 的 `unit-and-e2e` job 增加：
  - `make sqlc-verify`

说明：当前实现为全量执行，覆盖 SQL/schema 相关改动场景；后续可按路径条件化执行以缩短 CI 时长。

## 6) 旧实现 vs 新实现（行为对照摘要）
- SQL 语义：保持一致（沿用原 SQL 结构，仅迁移为 sqlc query 定义）。
- 事务语义：保持一致（`UpsertTask` 仍在单事务内执行 run 状态对齐）。
- 错误语义：保持一致（仍返回底层 DB/context 错误）。
- 类型安全：增强（手写 Scan 减少，映射由生成代码统一处理）。

## 7) 是否继续扩展到 mysql_store 主体
- 建议：**继续扩展，但按域分批**。
- 推荐顺序：
  1. `backup_checkpoints` 与 `task_events`（读写简单、风险低）
  2. `binlog_files` 列表与统计查询
  3. `worker_registrations`（保留接管语义回归测试后再迁移）

## 8) 模板引用
- 阶段验收模板：`docs/develop/plans/2026-03-02-phase-acceptance-template.md`
