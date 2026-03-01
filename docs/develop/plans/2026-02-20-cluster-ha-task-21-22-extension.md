# Cluster HA Task 21-22 Extension（2026-02-20）

本文承接：

- `docs/plans/2026-02-16-cluster-ha-implementation-plan.md`（主计划，Task 1-12）
- `docs/plans/2026-02-18-cluster-ha-task-13-18-extension.md`（续篇一，Task 13-18）
- `docs/plans/2026-02-19-cluster-ha-task-19-20-observability-extension.md`（续篇二，Task 19-20）

用于补齐 observability 回归闭环与下一阶段 HA 一致性验证任务：`Task 21-22`。

## 任务清单与状态

1. `Task 21`：observability e2e + CI 收口
   - 状态：已完成
   - 验收证据：
     - 场景脚本：`scripts/e2e/smoke-observability.sh`
     - 套件入口：`scripts/e2e/run-suite.sh`
     - CI 独立 job：`.github/workflows/e2e.yml`（`e2e-observability`）
     - 文档同步：`scripts/e2e/README.md`、`docs/process/task-gate.md`
     - 相关提交：`9f5858043fc299011636185c198dd486f444dd91`

2. `Task 22`：worker crash during OPEN + takeover strict consistency e2e
   - 状态：已完成
   - 验收证据：
     - 场景脚本：`scripts/e2e/smoke-worker-crash-recovery.sh`
     - 套件入口：`scripts/e2e/run-suite.sh`
     - 文档同步：`scripts/e2e/README.md`、`docs/TODO.md`
     - 相关提交：`fcbde5e6369f336cdad4c7d3cf9f0ea17239edc7`、`33b36615b178c52df4ce5db520fd69fb962ed548`、`7be2a0275ca3c52435d0ea7616fc311c8850c2e1`

## Task 22 范围约束（执行记录）

1. 本 task 只做 e2e 回归，不扩展业务功能。
2. 不修改现有复制语义（lease/fencing/file state machine）。
3. CI 先做 `workflow_dispatch` 可选入口，默认路径不加重。

## 建议验收命令

```bash
./scripts/e2e/run-suite.sh --scenarios smoke-worker-crash-recovery
go test ./internal/app ./internal/api ./internal/tasks ./internal/replication -count=1
```

## 参考

- Gate 流程：`docs/process/task-gate.md`
- 当前架构图：`docs/architecture-diagrams.md`

## 后续续篇说明（2026-02-20 晚间）

`Task 23-24` 已迁移到 `docs/TODO.md` 作为里程碑 backlog 管理：

- Task 23：上传失败补偿入口（已完成）
- Task 23.1：object key 命名升级（已完成）
- Task 24：上传可观测增强（已完成）
