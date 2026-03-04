# Foundation Hardening Phase Prompts

本文件提供可直接分发给执行者（“苦工”）的分阶段提示词。  
对应总计划：`docs/develop/plans/2026-03-02-foundation-hardening-implementation-plan.md`

---

## 通用要求（每个阶段都要附带）

```text
你在仓库 /Users/fan/GolangProjects/BinlogServer 工作。
请仅执行本阶段任务，不跨阶段“顺手重构”。

全局约束：
1) 不修改 docs/guide/*（该目录由 Claude 负责）。
2) 不改变对外行为语义（状态码、核心错误语义、状态机流程）。
3) 每次变更保持最小闭环，先测后改，最后回归。
4) 三层文档协议：至少同步受影响模块 README 与必要 L3 头注释。

阶段验收命令（必须全部通过）：
- go test ./...
- go test -race ./internal/tasks ./internal/api ./internal/replication
- go vet ./...
- make e2e-quick

交付物（必须包含）：
1) commit hash（按顺序）
2) git show --stat --name-only <hash范围> 摘要
3) 代码变更清单 + 测试变更清单
4) 配置变更（若有）与兼容性说明
5) 回滚命令（git revert 级别）
6) 未决事项
```

---

## P0 Prompt（Baseline & Guardrails）

```text
在遵循“通用要求”的前提下，执行 P0：基线固化与回归保护。

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
```

---

## P1 Prompt（Context Timeout Hardening）

```text
在遵循“通用要求”的前提下，执行 P1：内部调用超时边界治理。

任务：
1) 盘点 internal/tasks 与 internal/meta 中无界 context.Background() 调用，按读/写/lease/上传分类。
2) 设计并落地内部调用超时配置（建议 config.Meta.* 分层），不要与 http.control_plane/http.worker_health 混淆。
3) 将关键路径替换为 context.WithTimeout，并保留上下文取消语义。
4) 增加超时场景测试（慢存储/卡住依赖）。
5) 校验 config.example.yaml 兼容性（默认配置可启动，历史配置不崩）。

验收额外要求：
- 提交“调用点盘点表”（改前/改后）与配置项说明。
```

---

## P2 Prompt（Retry Standardization）

```text
在遵循“通用要求”的前提下，执行 P2：重试策略标准化。

任务：
1) 先补行为对齐测试：transient 重试、permanent 直返、context cancel/deadline 终止、max retries/jitter。
2) 引入 cenkalti/backoff/v4（v4.x）并通过适配层封装，禁止业务直接依赖第三方类型。
3) 替换 internal/meta/retry.go 自研实现，保持错误语义一致。
4) 保持对现有调用方的最小侵入改造。

验收额外要求：
- 提供“旧实现 vs 新实现”的行为对照摘要。
```

---

## P3 Prompt（SQL Access Governance Decision Gate）

```text
在遵循“通用要求”的前提下，执行 P3：SQL 访问层治理决策与试点。

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

若走 gorm 路线：
1) 仅做单一读接口 PoC，不进入 lease/关键写路径。
2) 给出性能、可控性、回滚评估后再申请扩大范围。

验收额外要求：
- 输出“决策报告 + 试点结果”，并给出是否继续扩展到 mysql_store 主体的建议。
- 若执行 sqlc 路线，附 make sqlc-generate / make sqlc-verify 的结果摘要。
```

---

## P4 Prompt（API Validation Unification）

```text
在遵循“通用要求”的前提下，执行 P4：API 参数校验统一。

任务：
1) 选 2-3 个高频接口试点，使用 Gin binding + validator 替代重复 parse/if。
2) 建立统一错误映射，保持既有状态码与错误语义兼容。
3) 对齐 Swagger 注解与 validator 约束，避免文档与运行时校验冲突。
4) 增加针对校验失败/边界参数的测试。

验收额外要求：
- 提供“旧错误响应 vs 新错误响应”兼容性对照。
```

---

## P5a Prompt（Prometheus Upgrade）

```text
在遵循“通用要求”的前提下，执行 P5a：Prometheus 指标升级。

任务：
1) 用 prometheus/client_golang 实现指标采集与输出。
2) 保持现有 /metrics 指标名与关键标签兼容，避免破坏现有监控。
3) 增加指标兼容性测试（关键 metric 快照或断言）。
4) 给出接入前后开销粗对比（请求耗时/CPU/内存）。

验收额外要求：
- 输出“兼容性清单”：保留指标、变更指标、新增指标。
```

---

## P5b Prompt（OTel Basic Tracing）

```text
在遵循“通用要求”的前提下，执行 P5b：OTel 基础 tracing。

任务：
1) 先确定导出策略（默认禁用；推荐 OTLP，Jaeger 可选）。
2) 接入 HTTP 入站 span 与元数据存储调用 span。
3) 增加采样率与开关配置，确保默认低风险。
4) 做开销对比（开关关闭/开启）。

验收额外要求：
- 明确“默认关闭时零影响”的证据。
- 给出最小启用配置示例（不写 docs/guide，只写模块说明或注释）。
```
