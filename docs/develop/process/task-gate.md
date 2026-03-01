# Task-by-Task Gate（强约束执行流程）

本文定义项目开发的强约束流程：每个 Task 必须逐关通过，未通过不得进入下一 Task。

适用范围：
- 架构级改造（如 standalone/cluster 双模式）
- 元数据一致性与高可用相关改动
- API 契约变更与可观测性改动
- e2e 回归场景新增/修改

## 1. 流程目标

1. 防止“单测通过但运行时未接线”。
2. 防止“代码改了但文档/Swagger/配置未同步”。
3. 防止“多个 Task 同时推进导致偏差累积”。
4. 让 review 有明确阻塞依据（可重复、可审计）。

## 2. 强约束规则（必须遵守）

1. 一次只推进一个 Task。
2. 每个 Task 单独 commit，commit message 必须包含 Task 编号。
3. 未完成当前 Task 的全部 Gate，不允许开始下一个 Task。
4. review 结论存在 `Critical/Major` finding 时，必须先修复再继续。
5. 禁止“只补测试不接运行时路径”的提交通过验收。
6. 每个 Task 必须归属到一个 Milestone（见 `docs/TODO.md`），不允许孤立新增 Task。
7. 凡是用户可控输入（前端表单、批量导入、API query/path/body/header、配置项）必须做“前端预校验 + 后端权威校验”，任一缺失均阻塞。

## 3. Gate 定义

### Gate 0：任务对齐（Task Contract）

输入：
- 计划文档中的 Task 条目（目标、文件、测试命令）。

动作：
- 写清楚本 Task 的 DoD（Definition of Done）：
  - 功能结果
  - 运行时接线路径
  - 测试范围
  - 文档/契约同步项
  - 输入校验矩阵（参数名、来源、约束、前端校验点、后端校验点）

通过标准：
- 有明确 DoD 与禁止项（Out of Scope）。

阻塞条件：
- DoD 模糊或无法验证。

### Gate 1：先失败（Fail First）

动作：
- 先写测试/断言，再执行并确认失败。

通过标准：
- 有失败证据（命令 + 关键输出摘要）。

阻塞条件：
- 测试“先天通过”或无法证明改动前失败。

### Gate 2：最小实现（Minimal Change）

动作：
- 只实现当前 Task 所需最小代码。
- 不跨 Task 偷加功能。

通过标准：
- 改动范围与 Task 文件清单一致。

阻塞条件：
- 出现与当前 Task 无关的大量改动。

### Gate 3：运行时接线检查（Runtime Wiring）

动作：
- 校验“能力是否真的接到启动路径/真实依赖”：
  - `app` 是否注入对应 option（如 lease manager / lease verifier）
  - role/mode 分支是否真的触发目标行为（而非空转）
  - metadata 字段是否持久化并可恢复

通过标准：
- 至少 1 个集成级测试或可执行检查证明接线生效。

阻塞条件：
- 仅在 fake/stub 单测可见，生产路径无效。

### Gate 4：契约一致性（Contract Sync）

动作：
- API 变更同步 OpenAPI/Swagger。
- 配置变更同步 `config.example.yaml` 与相关文档。
- 新状态/字段同步前端展示约定（若已暴露到 API）。

通过标准：
- 契约与实现一致，无“文档落后代码”。

阻塞条件：
- 新 endpoint 在 Swagger 缺失，或配置项未文档化。

### Gate 5：验证闭环（Verification）

动作：
- 跑受影响包测试：
  - `go test ./internal/<affected> -count=1`
- 跑关键场景测试（必要时 e2e）：
  - `./scripts/e2e/run-suite.sh --scenarios <scenario>`

通过标准：
- 命令全通过，结果可复现。

阻塞条件：
- 仅“本地口头通过”，无命令证据。

### Gate 6：Review 阻塞

动作：
- 独立 review（可同会话或他会话）。
- findings 按严重级别输出并关联文件行号。

通过标准：
- `Critical/Major` 为 0，或已修复并复检通过。

阻塞条件：
- 带着 Major/Critical 继续后续 Task。

### Gate 7：交付记录（Task Report）

动作：
- 输出该 Task 的交付记录：
  - 改动摘要
  - 测试证据
  - 风险与限制
  - 下一 Task 边界

通过标准：
- 任何人可仅凭记录复盘本 Task。

## 4. 严重级别定义（用于 Gate 6）

- `Critical`：会导致运行时错误语义、数据一致性风险、HA 失效。
- `Major`：计划目标不完整、关键链路未接线、契约不一致。
- `Minor`：可改进项，不阻塞继续开发。

规则：
- `Critical/Major` 阻塞合并与后续 Task。
- `Minor` 可带 issue 跟踪，不阻塞。

## 5. 本项目专用附加检查（Cluster HA）

涉及 cluster 任务时，额外必须检查：

1. `internal/app/app.go` 是否完成 role/mode 实际接线（不是只读配置）。
2. `internal/tasks/*` 新字段是否在 `internal/meta/mysql_store.go` 落库并可 `Restore`。
3. `internal/replication/*` 的 lease/epoch 校验是否在真实 runner 注入。
4. `/api` 新 endpoint 是否同步到 Swagger 产物。
5. worker 模式是否有实际工作循环，而非仅阻塞等待退出。

## 6. 输入校验硬门禁（全项目）

所有“用户可控输入”均适用本节；不区分 standalone/cluster，不区分前后端入口。

### 6.1 覆盖范围

1. 前端 UI：表单输入、筛选条件、批量创建文本/CSV。
2. HTTP API：`path`/`query`/`header`/`body` 全部参数。
3. 配置输入：YAML、环境变量、CLI flags。
4. 外部标识：`cluster_key`、host/port、对象存储 prefix、起点策略参数（file/pos/gtid）等。

### 6.2 最低校验要求（必须全部满足）

1. 前端预校验：必填、类型、长度、范围、枚举、格式（regex）与互斥关系。
2. 后端权威校验：即使绕过前端也必须拦截非法输入，返回 `400` 与清晰错误信息。
3. 存储兜底约束：关键字段使用 `NOT NULL`、`UNIQUE`、必要 `CHECK`（或应用层等价约束）。
4. 错误文案一致：前端提示与后端错误语义一致，避免“前端放行、后端拒绝但无解释”。

### 6.3 测试与验收要求（阻塞项）

1. 后端至少覆盖：合法样例、非法样例、边界样例（min/max/空值/格式错误）。
2. 前端至少覆盖：提交前拦截（不发请求）和错误提示渲染（若已有前端测试基础）。
3. e2e 至少覆盖 1 条非法输入场景（例如批量导入含坏行时可预览并拒绝提交）。
4. Gate 7 报告必须附“输入校验证据清单”（测试名 + 命令 + 关键输出）。

## 7. Task 执行模板（可直接复制）

```md
## Task <N> - <Title>

### Gate 0 - Task Contract
- Plan Ref: docs/plans/<plan>.md#Task-<N>
- DoD:
  1.
  2.
- Input Validation Matrix:
  - param: <name>, source: <ui/api/config>, constraints: <required/type/range/format>, frontend: <yes/no>, backend: <yes/no>
- Out of Scope:
  1.

### Gate 1 - Fail First
- Added tests:
  1.
- Fail command:
  - `go test ...`
- Fail evidence:
  - <关键报错摘要>

### Gate 2 - Minimal Change
- Changed files:
  1.
  2.
- Why minimal:
  - <说明>

### Gate 3 - Runtime Wiring
- Wiring checks:
  1.
  2.
- Evidence:
  - <测试名/命令/输出摘要>

### Gate 4 - Contract Sync
- Swagger updated: yes/no
- Config/doc updated: yes/no
- Validation rules synced to frontend/backend/docs: yes/no
- Evidence:
  - <文件路径>

### Gate 5 - Verification
- Unit/Integration:
  - `go test ...`
- E2E:
  - `./scripts/e2e/...`
- Result:
  - pass/fail

### Gate 6 - Review
- Findings:
  - Critical: <count>
  - Major: <count>
  - Minor: <count>
- Status:
  - pass/block

### Gate 7 - Task Report
- Summary:
  1.
- Risks:
  1.
- Input Validation Evidence:
  - <测试名/命令/关键输出>
- Next Task Boundary:
  - <只允许进入的下一步>
```

## 8. Cluster 计划快速清单（Task 4-24）

用于：

- `docs/plans/2026-02-16-cluster-ha-implementation-plan.md`（Task 1-12）
- `docs/plans/2026-02-18-cluster-ha-task-13-18-extension.md`（Task 13-18）
- `docs/plans/2026-02-19-cluster-ha-task-19-20-observability-extension.md`（Task 19-20）
- `docs/plans/2026-02-20-cluster-ha-task-21-22-extension.md`（Task 21-22）
- `docs/TODO.md`（Task 23+ backlog）

- Task 4（lease lifecycle）
  - [ ] scheduler acquire/renew/release 在真实启动路径可达
  - [ ] lease lost / grace exceeded 有 fail-safe stop 证据
- Task 5（OPEN/SEALED 语义）
  - [ ] seal 前 lease/epoch 校验在 runner 实际启用
  - [ ] 不会发布 `.open.e*` 文件
- Task 6（rebuild_current_file）
  - [ ] takeover 后从 `file:4` 重建且输出单一 sealed 文件
- Task 7（stale OPEN cleanup）
  - [ ] 启动清理旧 epoch OPEN 文件
  - [ ] 不误删当前 epoch 与 sealed 文件
- Task 8（app role wiring）
  - [ ] control-plane / worker / all-in-one 行为与计划一致
  - [ ] worker 不是空转
- Task 9（cluster APIs）
  - [ ] `/api/workers` `/api/cluster/overview` `/api/tasks/{id}/lease` `/api/tasks/{id}/runs` 可用
  - [ ] Swagger 同步
  - [ ] 响应字段与 task 持久化语义一致
- Task 10（meta-failover e2e）
  - [ ] `setup-meta-replication.sh` 初始化主从与 ProxySQL 路径可用
  - [ ] `smoke-meta-failover.sh` 在切主后 checkpoint 持续推进
  - [ ] `meta-failover-override` 分支（`E2E_API`/`E2E_ORC_API`）可执行
- Task 11（frontend cluster 视图）
  - [ ] worker 列表在线/离线可见
  - [ ] 任务归属 worker、lease 风险标记、run history 可见
  - [ ] `cd frontend && npm run build` 通过
- Task 12（全量验证与文档收尾）
  - [ ] `go test ./...`、前端 build、关键 e2e 场景通过
  - [ ] `README.md` 与 `docs/deployment-modes.md` 完成同步
  - [ ] 设计文档与实现差异已注明（如有）
- Task 13（runs 历史列表）
  - [ ] `/api/tasks/{id}/runs` 返回历史列表（非单条）
  - [ ] 支持 `limit`（默认 10，最大 200）并按 `started_at DESC`
  - [ ] 前端文案与能力一致（Run History）
- Task 14（worker heartbeat 持久化）
  - [ ] `worker_heartbeats` 落库并持续上报
  - [ ] worker 停止后在线状态可降为 `offline`
  - [ ] `/api/workers` 不再依赖任务更新时间推导在线
- Task 15（多进程角色分离 e2e）
  - [ ] control-plane + worker 双进程启动并可完成任务启动
  - [ ] worker offline 检测与恢复链路可回归
  - [ ] checkpoint 推进可证明实际由 worker 执行
- Task 16（CI 覆盖 cluster roles）
  - [ ] workflow 增加 `smoke-cluster-roles` 独立 job
  - [ ] 保留原 quick/full 流程
  - [ ] `workflow_dispatch` 支持选择 `cluster-roles`
- Task 17（worker health probe）
  - [ ] worker-only 角色可启动独立 health endpoint
  - [ ] `/healthz`、`/readyz` 返回 200
  - [ ] worker health 服务不暴露 `/api/*`
- Task 18（control-plane failover resilience e2e）
  - [ ] control-plane 挂掉期间 worker 仍可推进 checkpoint
  - [ ] control-plane 恢复后 API 可访问且状态一致
  - [ ] 场景可加入 CI 作为可选回归
- Task 19（冻结版收尾验收）
  - [ ] `go test ./...`、前端 build、关键 e2e 场景全通过
  - [ ] release notes 完成并可追溯命令证据
  - [ ] 发布建议（tag + 灰度策略）明确
- Task 19.1（Swagger 一致性复核）
  - [ ] `go test ./internal/api -run Swagger` 通过
  - [ ] 重新生成 swagger 产物后无 diff
  - [ ] 契约与实现字段一致
- Task 20（observability 基础能力）
  - [ ] 暴露 `/metrics`（Prometheus exposition）
  - [ ] 提供核心指标：任务状态、延迟、checkpoint age、worker online、upload 失败计数
  - [ ] 文档提供 Prometheus rule 示例与 runbook
- Task 21（observability e2e + CI 收口）
  - [ ] 新增 `smoke-observability` 场景并可独立执行
  - [ ] 场景验证 5 个核心指标存在，且至少 2 个指标值发生预期变化
  - [ ] CI 新增独立 observability job，`workflow_dispatch` 可单独触发
- Task 22（worker crash 接管一致性 e2e）
  - [ ] 模拟 worker 在 OPEN 阶段异常退出并由新 worker 接管
  - [ ] checkpoint 在接管前后持续推进（无回退）
  - [ ] stale `.open.e*` 可清理且不影响最终 seal 结果
  - [ ] 抽样校验主库 binlog 与备份文件 md5 一致
- Task 23（上传失败补偿最小闭环）
  - [ ] 提供手动补传入口（仅处理 `UPLOAD_FAILED` + sealed 文件）
  - [ ] 返回补传统计（scanned/succeeded/failed/skipped）
  - [ ] 并发触发同任务补传时有冲突保护
- Task 24（上传可观测增强）
  - [ ] `/metrics` 暴露补传指标（`upload_retry_total`、`upload_retry_last_ts`）
  - [ ] 提供上传失败原因聚合 API（`/upload-failures/reasons`）
  - [ ] runbook 覆盖“先看指标 -> 再看聚合原因 -> 再补传验证”路径

---

执行口令（建议固定）：

```bash
# 受影响包
go test ./internal/tasks ./internal/replication ./internal/app ./internal/api -count=1

# 必要场景（按任务增减）
./scripts/e2e/run-suite.sh --scenarios meta-failover

# 覆盖 override 分支（Task 10）
./scripts/e2e/run-suite.sh --scenarios meta-failover-override

# 收尾全量检查（Task 12）
go test ./...
cd frontend && npm run build
./scripts/e2e/run-suite.sh --scenarios smoke,compression
```
