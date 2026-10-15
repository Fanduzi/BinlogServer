# internal/health Module

健康评估的纯类型与生命周期函数（ADR 0009 阶段 2 P2-1，设计见 `docs/design/0009-phase2-health-evaluator.md`）。只依赖 `internal/assurance`，不做 I/O，尚未接线。

## Files
- `types.go`: `Kind`（封闭集合，16 种 v0.5.66 kind + 阶段 2 新增）、`Severity`、`Scope`、`Key`、`Instance`、`ClearRule`、`KindSpec`（含 `Scopes`、`Managed`、`Version`、`Waivable`）、`Produced`（本阶段有评估器产生的 scope）、`Registry`、`Facts`（`TaskFacts`、`ChainFacts`、`SignalState`、`StoreFacts`、`Waiver`）、`Mute`、`Finding`、`Transition`、`Desired`、`TaskHealth`。
- `registry.go`: `TASK_RECORDS_FAILED`（ADR 0010，信号 kind S16，CRITICAL，Managed）。`DefaultRegistry()`（设计 §6 的 kind 表；P2-3 起 `SEGMENT_UNREADABLE`、`SEGMENT_OVERLAP`、`SEGMENT_EMPTY_LEFTOVER`（INFO，来自 `I4_EMPTY_LEFTOVER`）是 Managed；P2-4 起除 SEGMENT_* 外的信号与任务状态 kind 以及新增的 SOURCE_UNREACHABLE、TASK_FAILED 也是 Managed，#385 起 TASK_UNADOPTED（CRITICAL，standalone 残留目录）也是 Managed，`ManagedKinds()` 列出全部）、`FromAssurance`（每个 `assurance.Kind` 映射到一个 finding kind）、`LegacyKinds`、`CanonicalGTIDRanges`（waiver evidence 规范格式：MySQL `uuid:a-b`（十六进制 uuid），接受 `uuid:1-5:7-9` 与 8.4 tag `uuid:tag:a-b`；MariaDB `domain-server:a-b`，接受 `d-s-n`）。
- `signals.go`: P2-4 信号与任务状态 kind 的派生/关闭规则：信号在失败后打开、首次成功或探针证明时关闭、过期显示为 INFO；DUMP_STALLED 与 SOURCE_UNREACHABLE 都由 `TaskFacts.Liveness`（源存活时钟）派生：静默 ≥45s 为 DUMP_STALLED（任何状态），最近一次失败距最后读到字节 ≥90s 为 SOURCE_UNREACHABLE（退避不计入），同一轮以 HANDED_OFF 交接、TASK_FAILED（按 `ClassifyFailure` 的类别）、TASK_TRANSITION_STALLED（任务行派生）、TASK_UNADOPTED（详情文本 `TaskUnadoptedDetail` 与旧 sink 共用；`TaskFacts.Unadopted`：无任务库的 standalone 下，任务只是 data_dir 里有分段的目录，本进程没有它的记录，复制停着直到 adopt；#385）。
- `reconcile.go`: `Registry.Desire`、`Registry.Reconcile`（只对 Managed kind 产生转移；所需 scope 不全完整（也不 UPDATE/DOWNGRADE，ESCALATE 照常）、`ClearRule.Holds` 不成立或在 HoldDown 内（从 `Facts.AbsentSince` 起算）不关闭；降级是显式 DOWNGRADE；同 key 重复行只产生一条转移；转移带 `FactAt`；精确 waiver 以 WAIVED 关闭；generation 留给存储分配）、`Registry.HealthOf`（CRITICAL→UNHEALTHY、WARNING→DEGRADED、INFO 不计；Managed kind 的非信号、且在 `Produced` 中的 scope 不完整→UNKNOWN；信号的内部 unknown 不进健康）。

## Exports
- 见上；调用方（P2-3 起的 `tasks.Evaluator`）只用 `DefaultRegistry().Reconcile/HealthOf`。

## Dependencies
- Upstream: 尚无（P2-3 起 `internal/tasks`）。
- Downstream: `internal/assurance`。

## Update Rule
- 新增 kind 必须在 `DefaultRegistry` 有完整 spec（severity、scope、invariant、clears_when 文本、Version ≥ 1）；`assurance` 新增 kind 必须在 `fromAssurance` 有映射，否则 `TestRegistryComplete` 失败。
- 把 kind 翻成 Managed 的 PR 必须同时把该 kind 的 `Report/Resolve` 调用改为 `Signal`。
