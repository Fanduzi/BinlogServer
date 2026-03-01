# Cluster HA Task 19-20 Extension（2026-02-19）

本文承接：

- `docs/plans/2026-02-16-cluster-ha-implementation-plan.md`（主计划，Task 1-12）
- `docs/plans/2026-02-18-cluster-ha-task-13-18-extension.md`（续篇一，Task 13-18）

用于补齐冻结版后段收尾任务：`Task 19-20`。

## 背景

在 Task 18（control-plane failover resilience e2e）完成后，进入发版收尾阶段。

目标从“功能实现”转为“可发布证据与可观测基础能力”：

1. 收尾验收与 release 文档。
2. Swagger 契约一致性复核。
3. `/metrics` 与告警示例文档（不内置告警引擎）。

## 任务清单与状态

1. `Task 19`：冻结版全量验收与 release notes 收尾
   - 状态：已完成
2. `Task 19.1`：Swagger 注解与产物一致性复核
   - 状态：已完成
3. `Task 20`：暴露 Prometheus `/metrics` + 文档示例（Prometheus rule + runbook）
   - 状态：已完成
   - 验收证据：
     - 指标实现：`internal/api/server.go`
     - 指标测试：`internal/api/server_test.go`
     - upload failures total 语义与采集成本修复：`internal/tasks/scheduler.go`、`internal/meta/mysql_store.go`
     - 观测文档：`docs/observability.md`
     - 相关提交：`c8887fc0d9b2d468316344085bee4151a577c155`、`01dafbd849b242b5976e3b71f3d5975b5d79a56a`

## Task 20 范围约束（冻结版）

1. 仅实现 `/metrics` 暴露与基础指标。
2. 告警规则只提供文档示例，不在代码中内置 rule engine。
3. 不引入 Prometheus/Alertmanager/Grafana 运行依赖。
4. 不改前端，不扩展新业务语义。

## 参考

- Gate 流程：`docs/process/task-gate.md`
- Release 文档：`docs/releases/v0.1.0-alpha.2.md`

## 后续续篇说明（2026-02-20）

`Task 21-22` 已迁移到：

- `docs/plans/2026-02-20-cluster-ha-task-21-22-extension.md`
