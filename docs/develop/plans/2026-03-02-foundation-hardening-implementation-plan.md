# Foundation Hardening Implementation Plan

> **For Claude/Codex:** Execute phase-by-phase with explicit review checkpoints. Do not modify `docs/guide` in this plan; `docs/guide` is owned by Claude-only workflow.

**Goal:** 在不引入行为回归的前提下，完成元数据层稳定性加固、SQL 访问层治理、API 参数校验统一和可观测性基础升级，使项目达到更可控的生产演进状态。

**Architecture:** 采用“先稳态后演进”的分阶段策略。先完成超时边界与重试策略标准化（低风险高收益），再推进 SQL 访问层重构（含 sqlc vs gorm 决策门），最后补齐 Prometheus/OTel 基础观测能力。每阶段独立可回滚。

**Tech Stack:** Go, Gin, MySQL, golang-migrate, go test/race/vet, Prometheus client, OpenTelemetry, (候选) sqlc/gorm.

---

## 0. Scope & Constraints

1. 不在本计划中改 `docs/guide/*`。
2. 任何阶段都不允许改变对外 API 行为语义（状态码、核心错误语义、状态机流转）。
3. 每阶段必须通过完整验证后才进入下一阶段。
4. 采用 worktree + 独立分支执行，避免污染 `main`。

## 1. Phase Overview

| Phase | 目标 | 风险等级 | 预期提交数 |
|---|---|---|---|
| P0 | 基线固化与回归保护 | 低 | 1-2 |
| P1 | context 超时边界治理 | 中 | 2-4 |
| P2 | 重试策略标准化（接入 cenkalti/backoff） | 中 | 2-3 |
| P3 | SQL 访问层治理决策与试点（sqlc vs gorm） | 中-高 | 3-6 |
| P4 | API 参数校验统一（validator + binding） | 中 | 2-4 |
| P5a | 可观测性升级（Prometheus client） | 中 | 2-4 |
| P5b | 可观测性升级（OTel 基础） | 中 | 2-4 |

---

## 2. Detailed Plan

### Phase P0: Baseline & Guardrails

**目标**
- 固化回归基线，确保后续每步变更可量化验证。

**任务分解**
1. 新增/整理验证脚本（可放 `scripts/`）：统一执行 `test/race/vet`。
2. 记录关键路径用例清单（task create/start/stop, lease renew, checkpoint, retry upload）。
3. 建立阶段验收模板（每阶段输出“变更点 + 验证摘要 + 回滚点”）。
4. 增加 E2E 基线：`make e2e-quick` 必须纳入阶段验收。
5. 增加轻量性能基线记录：
   - `go test ./...` 耗时
   - `go test -race ...` 耗时
   - `make e2e-quick` 耗时
   - 关键包 benchmark（示例：`go test -bench=. ./internal/tasks/...`）
6. 依赖盘点与版本策略（见第 6 节），形成“当前版本 + 目标策略”清单。

**验收点**
1. `go test ./...` 通过。
2. `go test -race ./internal/tasks ./internal/api ./internal/replication` 通过。
3. `go vet ./...` 通过。
4. `make e2e-quick` 通过。
5. 输出基线指标表（见第 7 节）。

**回滚点**
- 无行为改动，直接回退本阶段 commit。

---

### Phase P1: Context Timeout Hardening

**目标**
- 清理关键路径中无界 `context.Background()` 调用，为存储/调度调用增加可配置超时边界。

**任务分解**
1. 识别高风险路径：
   - `internal/tasks/*` 调用 `store/eventStore/fileStore/leaseManager/uploader` 的路径。
   - `internal/meta/*` 长耗时 DB 路径。
2. 先盘点现有 `context.Background()` 用法并按操作分类（读/写/lease/上传）。
   - 建议命令：`rg -n "context.Background\\(" internal/`
3. 明确与现有 `http.control_plane.*` / `http.worker_health.*` 的关系：
   - HTTP 超时仅控制入站连接层；
   - 本阶段新增的是内部依赖调用超时（存储/租约/上传），不得混淆。
4. 设计并落地内部调用超时配置（建议 `config.Meta.*` 按操作类型分层，而非单值）。
5. 在关键路径替换为 `context.WithTimeout`。
6. 增加超时场景测试（mock store 卡住/慢响应）。
7. 增加 `config.example.yaml` 兼容性验证（默认值可启动，旧配置不崩）。

**验收点**
1. 关键存储调用不再直接使用无界 `context.Background()`。
2. 超时配置有默认值、可覆盖、可测试。
3. `config.example.yaml` 在默认配置下可正常启动。
4. `test/race/vet` 全绿。
5. `make e2e-quick` 全绿。

**回滚点**
- 若出现误超时导致行为回归，可仅回退超时接入 commit，保留测试与配置骨架。

---

### Phase P2: Retry Policy Standardization

**目标**
- 以 `cenkalti/backoff/v4` 替换 `internal/meta/retry.go` 自研退避实现，同时保持现有错误语义。

**任务分解**
1. 先写“行为对齐测试”：
   - transient 错误会重试；
   - permanent 错误立即失败；
   - `context canceled/deadline` 立即终止；
   - 最大重试次数与 jitter 生效。
2. 明确依赖版本策略：`cenkalti/backoff/v4` 采用 `v4.x` 锁定，并通过 go.mod/go.sum 固化。
3. 封装 backoff 适配层（避免业务直接依赖第三方类型），先定义本地接口再接库。
4. 在 `internal/meta` 接入适配层并删除/瘦身旧实现。

**验收点**
1. 重试相关测试全过，关键边界行为与旧语义一致。
2. `internal/meta` 运行路径统一使用标准重试适配层。
3. `config.example.yaml` 兼容性不受影响。
4. `test/race/vet` 全绿。
5. `make e2e-quick` 全绿。

**回滚点**
- 可回退至旧 `retry.go` 实现，保留新测试用于后续再迁移。

---

### Phase P3: SQL Access Layer Governance (Decision Gate)

**目标**
- 对 `internal/meta` 的手写 SQL + Scan 进行工程化治理，降低运行时 SQL/类型错误。

**决策门：sqlc vs gorm**

**评估标准**
1. 对现有复杂 SQL（UPSERT、条件更新、lease fencing）保持可控性。
2. 编译期保障能力（SQL 与类型）。
3. 迁移成本与回滚难度。
4. 性能与可观测性（SQL 透明度）。

**推荐默认路线：sqlc（SQL-first）**
- 原因：项目当前为 SQL-first、复杂语义较多，sqlc 与现状匹配度更高。

**可选路线：gorm（受限使用）**
- 若选 gorm，建议仅用于非关键读路径，不覆盖 lease/事务关键写路径。

**任务分解（按 sqlc 路线）**
0. 先做复杂度评估输出（SQL 数量、手写 Scan 点位、事务/复杂语句分布）。
1. 试点子域：`lease_store`（最小闭环）。
2. 第二子域：`task_runs + worker_heartbeats`。
3. 明确 sqlc 与 golang-migrate 集成流程（schema 变更 -> migrate -> sqlc 生成 -> 编译校验）。
4. 补充 `Makefile` 生成与校验目标：
   - `make sqlc-generate`（执行 `sqlc generate`）
   - `make sqlc-verify`（执行生成后 `git diff --exit-code`，阻断未提交生成物）
   - 可选：`make generate` 聚合所有生成步骤。
5. 将 `make sqlc-verify` 接入 CI（至少在 SQL/schema 相关变更时执行）。
6. 评估是否扩展到 `mysql_store` 主体（按收益/风险决定，并给出优先级顺序）。
7. 明确 `internal/meta/mysql_store.go` 的迁移优先级建议：
   - P3 首先迁移 `lease_store`（高风险高收益）
   - 其次 `task_runs + worker_heartbeats`（中等复杂度）
   - 最后评估 `mysql_store` 主体（体量大，需拆子域逐步推进）

**任务分解（若选 gorm 路线）**
1. 建立 PoC，仅覆盖单一读接口。
2. 对比 SQL 可控性、性能和代码复杂度。
3. 通过决策评审后再决定是否扩大范围。

**验收点**
1. 至少一个子域完成治理并在编译期/测试中体现收益。
2. 不引入核心状态机行为变化。
3. `Makefile` 的 `sqlc-generate/sqlc-verify` 可执行，且 `sqlc-verify` 能发现未提交生成物。
4. CI 已接入 `sqlc-verify`（或明确阶段内临时策略与后续落地时间点）。
5. `config.example.yaml` 兼容性不受影响。
6. `test/race/vet` 全绿。
7. `make e2e-quick` 全绿。

**回滚点**
- 子域级回滚：每个子域独立 commit，不做跨域大爆改。

---

### Phase P4: API Validation Unification

**目标**
- 将 `internal/api` 的手写参数校验逐步迁移到 Gin binding + validator，统一错误处理。

**任务分解**
1. 选 2-3 个高频接口做试点（例如 task create/update、limit/port 参数）。
2. 引入 DTO + `binding`/`validate` 标签，替代重复 parse/if。
3. 建立统一错误映射（保持现有状态码和错误语义），避免直接暴露 validator 默认报错格式。
4. 对齐 Swagger 注解与 validator 约束，避免文档与运行时校验冲突。
5. 扩展到其他接口。

**验收点**
1. 试点接口手写校验显著减少，错误返回语义不变。
2. `internal/api` 新增针对 binding/validator 的单测覆盖。
3. Swagger 描述与实际校验一致（抽样核对关键接口）。
   - 对关键接口执行“注解参数约束 vs validator 标签”逐项核对并记录差异。
4. `config.example.yaml` 兼容性不受影响。
5. `test/race/vet` 全绿。
6. `make e2e-quick` 全绿。

**回滚点**
- 接口级回滚：每次只迁移小批接口，出问题可局部回退。

---

### Phase P5a: Observability Upgrade (Prometheus)

**目标**
- 用 `prometheus/client_golang` 替代纯手写文本指标，实现可扩展观测面，并保持指标兼容。

**任务分解**
1. Prometheus 接入：
   - 复刻现有指标名与标签（避免破坏现有监控）。
   - 保留 `/metrics` 输出兼容。
2. 增加观测回归测试或快照验证（至少保证指标关键字段稳定）。
3. 记录 Prometheus 接入前后开销（请求耗时、CPU/内存粗对比）。

**验收点**
1. `/metrics` 行为兼容，现有关键指标仍可抓取。
2. `config.example.yaml` 兼容性不受影响。
3. `test/race/vet` 全绿。
4. `make e2e-quick` 全绿。

**回滚点**
- 观测逻辑通过 feature flag 控制，可快速禁用并回滚。

---

### Phase P5b: Observability Upgrade (OTel)

**目标**
- 接入 OTel 基础 tracing，优先做到“可开可关、可控开销”。

**任务分解**
1. 确定导出策略：
   - 默认禁用（开发手动开启）；
   - 首选 OTLP（http/grpc 二选一），Jaeger 作为兼容选项。
   - 在阶段评审中明确导出器决策：`otlp-http` / `otlp-grpc` / `jaeger` / `disabled`。
2. HTTP 入站与元数据存储调用接入基础 span。
3. 配置采样率和开关，避免默认高开销。
4. 做开销对比（至少给出请求路径级别的 before/after）。

**验收点**
1. 开关关闭时不改变现有行为和性能基线。
2. 开关开启时可观察到 HTTP + DB 基础链路。
3. `config.example.yaml` 与环境变量说明完整可用。
4. `test/race/vet` 全绿。
5. `make e2e-quick` 全绿。

**回滚点**
- 可通过配置立即关闭 tracing；必要时回滚 OTel 接入 commit。

---

## 3. Cross-Phase Acceptance Criteria

每阶段结束必须满足：
1. `go test ./...`
2. `go test -race ./internal/tasks ./internal/api ./internal/replication`
3. `go vet ./...`
4. `make e2e-quick`
5. 三层文档协议最低要求（L3 文件头、受影响模块 L2 README；不含 `docs/guide`）
6. 产出阶段总结：变更清单、风险、回滚命令、未决事项

---

## 4. Branching & Execution Model

1. 主分支：`main`（只接收验收通过后的合并）。
2. 建议分支：
   - `hardening/p1-context-timeout`
   - `hardening/p2-retry-standardization`
   - `hardening/p3-sql-access-governance`
   - `hardening/p4-api-validation`
   - `hardening/p5a-prometheus`
   - `hardening/p5b-otel`
3. 每阶段在独立 worktree 执行，阶段通过后再合并。

---

## 5. Open Questions (for review)

1. P3 最终路线是否锁定为 sqlc，还是先做 sqlc vs gorm 双 PoC 决策？
2. P1 超时配置是否明确采用“按操作类型分层”（读/写/lease/上传）？
3. P3 是否需要纳入读写分离评估（默认不做，除非明确业务需求）？
4. 是否追加独立阶段处理“错误处理标准化”（跨 tasks/meta/api 的错误码与文案一致性）？
5. P5b OTel 是否本轮必须上线，还是仅做可选基础设施接入？

---

## 6. Dependencies & Version Strategy

说明：本仓库部分库已在 `go.mod`（多为 indirect）中存在。本计划采用“先盘点现状，再决定升级”的策略，不预设不准确目标版本号。

| 依赖 | 当前状态（以 go.mod 为准） | 策略 | 目标阶段 |
|---|---|---|---|
| `github.com/cenkalti/backoff/v4` | 已存在（indirect） | P2 转为直接依赖并锁定 `v4.x` | P2 |
| `github.com/prometheus/client_golang` | 已存在（indirect） | P5a 转为直接依赖并锁定 `v1.x` | P5a |
| `go.opentelemetry.io/otel` 系列 | 已存在（indirect） | P5b 统一子模块版本并固定导出器方案 | P5b |
| `github.com/go-playground/validator/v10` | 已存在（indirect） | P4 按需转为直接依赖 | P4 |

---

## 7. Baseline Metrics (P0 Output)

P0 必须产出以下基线（初始为 TBD，执行后填实测值）：

| 指标 | 基线值 |
|---|---|
| `go test ./...` 耗时 | TBD |
| `go test -race ...` 耗时 | TBD |
| `make e2e-quick` 耗时 | TBD |
| `go test -bench=. ./internal/tasks/...` | TBD |

---

## 8. Stage Delivery Template

每阶段交付必须包含：

1. 代码变更清单（commit 列表 + 文件数）
2. 测试变更（新增/修改测试点）
3. 配置变更（若有，附迁移指南）
4. 性能对比（至少基线项 before/after）
5. 回滚命令（`git revert <hash>` 级别）
6. 未决事项（遗留问题与建议）
7. 若阶段涉及 sqlc：`make sqlc-generate` 与 `make sqlc-verify` 结果摘要
