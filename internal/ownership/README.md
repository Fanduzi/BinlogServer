# internal/ownership Module

ADR 0011 的任务所有权过渡。一个纯函数：当前任务状态加一个事件，得到下一状态和副作用意图。不做 I/O，不读时钟，时钟由调用方传入。调度器还没接上，现有运行路径不变。

词汇用仓库 `CONTEXT.md`：任务所有权、租约、认领。执行器事件只在 epoch、owner、`row_generation` 都对得上时写入。`LeaseUnreadable` 走 EpochLost，不进 FAILED，也不进 RUNNING。`MetaLockTimeout` 只记下 `META_LOCK_STALLED`，不把行锁清掉；空闲事务由 `idle_kill` 在下一步开头丢掉。

## Files

| File | Responsibility |
|------|---------------|
| `state.go` | 任务状态：期望、观察态、租约、备份链、拒绝、结论、节点。已应用事件轨迹不导出；`Step` 仍往里追加，调用方留着的 State 会随轨迹变长 |
| `event.go` | ADR 0011 §3.2 的事件，以及续租、sweep、行锁、旧二进制更新 |
| `step.go` | `Step`：一次事件到下一状态和意图。`per_node` 的 NULL 拒绝在 `CanRead` 恢复后不再挡住 `Acquire`。链主人目录没了时 `ChainOwnerGone` 立刻成立 |
| `invariant.go` | 不变量 1–10 的可检查谓词。不变量 7 的 `ProgressOverdue` 只重放 `Step` 追加的未导出事件轨迹，不读 `ProgressAt` / `UnownedSince` / `NoProgress`，也不调用 `LeaseLive` / `LegalRetry`。终态 `FAILED` 只有 `spec_revision > failed_spec_revision` 才进认领集 |
| `finding.go` | 结论从事实派生。事实没变时，同一键不会先关后开 |
| `spec.go` | RTO、stale、`node_gone`、拒绝退避、迁移预检、旁路杀计划。`RefusalDelay` 在指数大于 20 或 `claim > cap>>shift` 时取 cap，移位回绕不能把期限算短 |
| `invariant_test.go` | 安全性：固定种子的随机前缀，加深度 3 的有界穷举。字母表含旧 epoch、`StopConfirmed`、`StopExpired`、`Lock`，时钟会跳过租约 TTL |
| `bottom_scc_test.go` | 底强连通分量检查。起点在 `DumpReady` 之前，所以把这一步换成退避环时，这个测试先失败 |
| `bound.go` | `FairStepBound`：公平步数的上界。窗口 W 是 `fairDeadlines` 的最大值，`NextDeadline` 是同一组的最小值。读者期限之后、认领集已经开着、又严格早于已有终点的非读者拒绝也在最小值里，不把 W 拉长。目录已经没了、行还没写成链消失时，K 仍算随后的 `ChainOwnerGone`。估大可以，估小是错 |
| `liveness_test.go` | 活性 (a)(b)(c) 的起点。60 次偏向游走加上 `FAILED` / `CHAIN_OWNER_GONE`。能搜完的起点要求观察步数 ≤ `FairStepBound`；两者相等时，界再减一就装不下。检查方法写在文件头 |
| `time_region.go` | 公平搜索的时间区域。一张表列出 `Step` 的每条时间比较。边界 c 和 c±1ns 保持精确，只有严格更早的差值饱和 |
| `observe_test.go` | 公平步图。状态键用上面的边界，不存绝对 now。底强连通分量要么含 `RUNNING`，要么是 ADR 字面量里的单个操作员 `FAILED`。`FairTerminal` 只用来对账。唯一的时间步是 `NextDeadline`。超过状态上限或单种子时间预算是失败 |
| `bound_rand_test.go` | 至少 300 个带种子的随机配置，含 `CanRead` 恢复这一环境步。穷尽比例必须是 1，超过状态上限的种子是失败。底强连通分量按种类计数。更多种子用 `FAIR_BOUND_SEEDS` |
| `trajectory_test.go` | §4.1 轨迹 |
| `fault_matrix_test.go` | §4.2 里纯模型能表达的每一行 |
| `scenario_walk_test.go` | §4.3 里纯模型能表达的走查 |
| `mutant_test.go` | 评审里活下来的每个变异一条具名断言 |
| `fixture_test.go` | 测试用的固定时钟和已运行任务 |
| `mutants/kill.go` | 变异工具。`go run` 逐个打补丁再跑本包测试，期望失败。不进 `go test` |

## Exports

- `Step(State, Event, time.Time) Result`
- `Check`、`CheckStep`、`ProgressOverdue`
- `InClaimSet`、`InStopSet`、`Covered`、`LeaseLive`、`HealthCode`
- `RTOBound`、`RTOStart`、`EffectiveStaleAfter`、`EffectiveNodeGone`、`NodeGoneRejectsStart`
- `MigrationAllowed`、`PlanSideKill`、`BackfillChain`、`GenerationBumped`、`RefusalDelay`、`FairStepBound`
- `New`、`DefaultParams`、`Held`

`PlanSideKill` 在缺省 `off` 下不发 `KILL`。`auto` 只在直连、过程存在、演练通过、并且有 `PROCESS`（MySQL 还要 `data_locks`）时调用过程。

## Dependencies

- Upstream: 无。不导入调度器，不打开数据库。
- Downstream: 无。PR-2 接会话超时和旁路杀，PR-4 把 `Step` 接到调度器。本包不实现观察者死后由 `_health` 持有者关闭 `META_LOCK_STALLED`（N6），也不实现 catalog 一致性（F7，PR-5）。

## Update Rule

- 成员、接口或依赖变化时，同时改本文件和根 `README.md` 的 Modules。
- 事件或意图变化时，改对应 Go 文件头的 input/output/pos。
