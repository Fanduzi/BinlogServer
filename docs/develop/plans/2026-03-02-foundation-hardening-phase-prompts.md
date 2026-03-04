# Foundation Hardening Phase Prompts

本文件提供可直接分发给执行者（“苦工”）的分阶段提示词。  
对应总计划：`docs/develop/plans/2026-03-02-foundation-hardening-implementation-plan.md`

---

## 通用调度头（每个阶段都要附带）

```text
你在仓库 /Users/fan/GolangProjects/BinlogServer 工作。
请仅执行本阶段任务，不跨阶段“顺手重构”。
Apply repository skill: task-delivery-guardrails.
Enforce all constraints, verification gates, and delivery artifacts defined in:
.agents/skills/task-delivery-guardrails/SKILL.md

补充约束（本计划特有）：
1) docs/guide/* 由 Claude 负责；除非本阶段明确要求，否则不要修改。
2) 除非本阶段明确声明，保持对外行为语义不变。
3) 默认使用独立分支 + 独立 worktree（除非 reviewer 明确豁免）。
```

---

## P0 Prompt（Baseline & Guardrails）

```text
Apply repository skill: task-delivery-guardrails.
严格按 docs/develop/plans/2026-03-02-foundation-hardening-phase-prompts.md 的 P0 Prompt 执行，只做 P0 范围。

在遵循“通用调度头”的前提下，执行 P0：基线固化与回归保护。

任务：
1) 整理统一验证入口（脚本或明确命令集合）覆盖 test/race/vet/e2e-quick。
2) 形成关键路径回归清单（task create/start/stop, lease renew, checkpoint, retry upload）。
3) 产出阶段验收模板（变更点 + 验证摘要 + 回滚点）。
4) 记录基线指标：
   - go test ./... 耗时
   - go test -race ... 耗时
   - make e2e-quick 耗时
   - go test -bench=. ./internal/tasks/... 结果
5) 依赖盘点：梳理 go.mod 中与本计划相关依赖（backoff/prometheus/otel/validator）的当前状态与策略。

注意：
- P0 不做业务逻辑改动。
- 验证层级：Full（若要降级，需 reviewer 明确批准）。
```

---

## P1 Prompt（Context Timeout Hardening）

```text
Apply repository skill: task-delivery-guardrails.
严格按 docs/develop/plans/2026-03-02-foundation-hardening-phase-prompts.md 的 P1 Prompt 执行，只做 P1 范围。

在遵循“通用调度头”的前提下，执行 P1：内部调用超时边界治理。

任务：
1) 盘点 internal/tasks 与 internal/meta 中无界 context.Background() 调用，按读/写/lease/上传分类。
   - 建议命令：`rg -n "context.Background\\(" internal/`
2) 设计并落地内部调用超时配置（建议 config.Meta.* 分层），不要与 http.control_plane/http.worker_health 混淆。
   - 说明要求：HTTP 超时仅管入站连接；本阶段治理的是内部依赖调用超时。
3) 将关键路径替换为 context.WithTimeout，并保留上下文取消语义。
4) 增加超时场景测试（慢存储/卡住依赖）。
5) 校验 config.example.yaml 兼容性（默认配置可启动，历史配置不崩）。

验收额外要求：
- 提交“调用点盘点表”（改前/改后）与配置项说明。
- 验证层级：Full（若要降级，需 reviewer 明确批准）。
```

---

## P2 Prompt（Retry Standardization）

````text
Apply repository skill: task-delivery-guardrails.
严格按 docs/develop/plans/2026-03-02-foundation-hardening-phase-prompts.md 的 P2 Prompt 执行，只做 P2 范围。

在遵循“通用调度头”的前提下，执行 P2：重试策略标准化。

任务：
1) 先补行为对齐测试：transient 重试、permanent 直返、context cancel/deadline 终止、max retries/jitter。
2) 引入 cenkalti/backoff/v4（v4.x）并通过适配层封装，禁止业务直接依赖第三方类型。
   - 依赖固化要求：提交 go.mod/go.sum 变更并说明版本锁定策略。
3) 替换 internal/meta/retry.go 自研实现，保持错误语义一致。
4) 保持对现有调用方的最小侵入改造。

适配层接口草图（可调整命名，但语义保持）：
```go
type RetryExecutor interface {
    Do(ctx context.Context, policy Policy, fn func() error) error
}

type Policy struct {
    BaseDelay  time.Duration
    MaxDelay   time.Duration
    MaxRetries int
    Jitter     float64
    IsTransient func(error) bool
}
```

验收额外要求：
- 提供“旧实现 vs 新实现”的行为对照摘要。
- 验证层级：Full（若要降级，需 reviewer 明确批准）。
````

---

## P3 Prompt（SQL Access Governance Decision Gate）

```text
Apply repository skill: task-delivery-guardrails.
严格按 docs/develop/plans/2026-03-02-foundation-hardening-phase-prompts.md 的 P3 Prompt 执行，只做 P3 范围。

在遵循“通用调度头”的前提下，执行 P3：SQL 访问层治理决策与试点。

先做决策评估（必须）：
1) 输出复杂度评估：SQL 数量、手写 Scan 点位、事务/复杂语句分布。
2) 说明 sqlc 与 gorm 的适配性对比（以当前 internal/meta 复杂 SQL 为准）。
3) 给出结论并明确“本阶段试点路线”。

若走 sqlc 路线（默认推荐）：
1) 先试点 lease_store。
2) 再试点 task_runs + worker_heartbeats。
3) 明确 sqlc 与 golang-migrate 的集成流程（schema -> migrate -> generate -> compile/test）。
4) 在 Makefile 增加并使用：
   - make sqlc-generate
   - make sqlc-verify（生成后 git diff --exit-code）
5) CI 接入 make sqlc-verify（至少 SQL/schema 相关改动必须执行）。
6) 输出 sqlc 所需 SQL 签名/queries 组织方案（用于 sqlc.yaml/sqlc.json），至少包含：
   - lease 相关 query 名称与参数/返回结构
   - task_runs/worker_heartbeats 相关 query 名称与参数/返回结构
   - 生成包路径与调用侧适配方式

若走 gorm 路线：
1) 仅做单一读接口 PoC，不进入 lease/关键写路径。
2) 给出性能、可控性、回滚评估后再申请扩大范围。

验收额外要求：
- 输出“决策报告 + 试点结果”，并给出是否继续扩展到 mysql_store 主体的建议。
- 若执行 sqlc 路线，附 make sqlc-generate / make sqlc-verify 的结果摘要。
- 验证层级：Full（若要降级，需 reviewer 明确批准）。
```

---

## P4 Prompt（API Validation Unification）

```text
Apply repository skill: task-delivery-guardrails.
严格按 docs/develop/plans/2026-03-02-foundation-hardening-phase-prompts.md 的 P4 Prompt 执行，只做 P4 范围。

在遵循“通用调度头”的前提下，执行 P4：API 参数校验统一。

任务：
1) 选 2-3 个高频接口试点，使用 Gin binding + validator 替代重复 parse/if。
2) 建立统一错误映射，保持既有状态码与错误语义兼容。
3) 对齐 Swagger 注解与 validator 约束，避免文档与运行时校验冲突。
4) 增加针对校验失败/边界参数的测试。

验收额外要求：
- 提供“旧错误响应 vs 新错误响应”兼容性对照。
- 验证层级：Full（若要降级，需 reviewer 明确批准）。
```

---

## P5a Prompt（Prometheus Upgrade）

```text
Apply repository skill: task-delivery-guardrails.
严格按 docs/develop/plans/2026-03-02-foundation-hardening-phase-prompts.md 的 P5a Prompt 执行，只做 P5a 范围。

在遵循“通用调度头”的前提下，执行 P5a：Prometheus 指标升级。

任务：
1) 用 prometheus/client_golang 实现指标采集与输出。
2) 保持现有 /metrics 指标名与关键标签兼容，避免破坏现有监控。
3) 增加指标兼容性测试（关键 metric 快照或断言）。
4) 给出接入前后开销粗对比（请求耗时/CPU/内存）。

验收额外要求：
- 输出“兼容性清单”：保留指标、变更指标、新增指标。
- 验证层级：Full（若要降级，需 reviewer 明确批准）。
```

---

## P5b Prompt（OTel Basic Tracing）

```text
Apply repository skill: task-delivery-guardrails.
严格按 docs/develop/plans/2026-03-02-foundation-hardening-phase-prompts.md 的 P5b Prompt 执行，只做 P5b 范围。

在遵循“通用调度头”的前提下，执行 P5b：OTel 基础 tracing。

任务：
1) 先确定导出策略（默认禁用；推荐 OTLP，Jaeger 可选）。
   - 必须在阶段评审中明确四选一决策：`otlp-http` / `otlp-grpc` / `jaeger` / `disabled`。
2) 接入 HTTP 入站 span 与元数据存储调用 span。
3) 增加采样率与开关配置，确保默认低风险。
4) 做开销对比（开关关闭/开启）。

验收额外要求：
- 明确“默认关闭时零影响”的证据。
- 给出最小启用配置示例（不写 docs/guide，只写模块说明或注释）。
- 验证层级：Full（若要降级，需 reviewer 明确批准）。
```
