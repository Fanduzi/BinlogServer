# Cluster HA Task 13-18 Extension（2026-02-18）

本文用于承接 `docs/plans/2026-02-16-cluster-ha-implementation-plan.md` 的后续任务，补齐 `Task 13-18` 编号与状态。

## 背景

- 原主计划定义了 `Task 1-12`。
- 在 2026-02-17 到 2026-02-18 的迭代中，新增了若干交付项。
- 为保持任务编号连续并便于 review，统一归档为 `Task 13-18`。

## 任务清单与状态

1. `Task 13`：`/api/tasks/{id}/runs` 从单条升级为历史列表（limit/排序/字段补齐）
   - 状态：已完成
2. `Task 14`：`worker_heartbeats` 持久化，`/api/workers` 基于心跳表给出真实在线状态
   - 状态：已完成
3. `Task 15`：多进程角色分离 e2e（control-plane + worker），覆盖在线/离线/恢复链路
   - 状态：已完成
4. `Task 16`：CI 接入 `smoke-cluster-roles` 独立场景回归
   - 状态：已完成
5. `Task 17`：worker-only 健康探针（`/healthz`、`/readyz`），且不暴露 `/api/*`
   - 状态：已完成
6. `Task 18`：control-plane crash/restart resilience e2e（验证 control-plane 故障不影响 worker 拉流）
   - 状态：已完成

> 说明：Task 19-20 迁移至 `docs/plans/2026-02-19-cluster-ha-task-19-20-observability-extension.md`。

## 执行约束

1. 仍按 `docs/process/task-gate.md` 的 G0-G7 执行。
2. 一次只推进一个 Task。
3. 每个 Task 单独 commit。
4. `Critical/Major` finding 未清零前，不进入下一 Task。

## 参考

- 主计划：`docs/plans/2026-02-16-cluster-ha-implementation-plan.md`
- Gate 流程：`docs/process/task-gate.md`
