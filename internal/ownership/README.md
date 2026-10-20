# internal/ownership Module

ADR 0011 的任务所有权过渡。一个纯函数：当前任务状态加一个事件，得到下一状态和副作用意图。不做 I/O，不读时钟，时钟由调用方传入。调度器还没接上，现有运行路径不变。

词汇用仓库 `CONTEXT.md`：任务所有权、租约、认领。执行器事件只在 epoch、owner、`row_generation` 都对得上时写入。`LeaseUnreadable` 走 EpochLost，不进 FAILED，也不进 RUNNING。`MetaLockTimeout` 只记下 `META_LOCK_STALLED`，不把行锁清掉；空闲事务由 `idle_kill` 在下一步开头丢掉。

## Files

| File | Responsibility |
|------|---------------|
| `state.go` | 任务状态：期望、观察态、租约、备份链、拒绝、结论、节点。已应用事件轨迹不导出；`Step` 仍往里追加，调用方留着的 State 会随轨迹变长 |
| `event.go` | ADR 0011 §3.2 的事件，以及续租、sweep、行锁、旧二进制更新 |
| `step.go` | `Step`：一次事件到下一状态和意图 |
| `invariant.go` | 不变量 1–10 的可检查谓词。不变量 7 的 `ProgressOverdue` 只重放 `Step` 追加的未导出事件轨迹，不读 `ProgressAt` / `UnownedSince` / `NoProgress`，也不调用 `LeaseLive` / `LegalRetry`。终态 `FAILED` 只有 `spec_revision > failed_spec_revision` 才进认领集 |
| `finding.go` | 结论从事实派生。事实没变时，同一键不会先关后开 |
| `spec.go` | RTO、stale、`node_gone`、拒绝退避、迁移预检、旁路杀计划。`RefusalDelay` 在指数大于 20 或 `claim > cap>>shift` 时取 cap，移位回绕不能把期限算短 |
| `invariant_test.go` | 安全性：固定种子的随机前缀，加深度 3 的有界穷举。字母表含旧 epoch、`StopConfirmed`、`StopExpired`、`Lock`，时钟会跳过租约 TTL |
| `bound.go` | `FairStepBound`：公平步数的上界，直到 `FairTerminal`。允许名单上的 FAILED 只有不可认领时才是终态。`CHAIN_OWNER_GONE` 在链主人停住、缺席或活着但读不到目录时是终态。窗口 W 含行锁 idle_kill、停住 owner 的 TTL、重试窗口，以及停住链主人的 node_gone。估大可以，估小是错 |
| `liveness_test.go` | 活性 (a)(b)(c) 的起点。60 次偏向游走加上 `FAILED` / `CHAIN_OWNER_GONE`。能搜完的起点要求观察步数 ≤ `FairStepBound`；两者相等时，界再减一就装不下。检查方法写在文件头 |
| `observe_test.go` | 只调用 `Step` 和 `Check` 的公平搜索，终态用 `FairTerminal`。`ChainVerified` 的 `ReadOK` 取节点的 `CanRead`。sweep 到期后可以先走其它公平步。20 万状态以内穷尽；3/4 个非读者用带种子的公平游走。已知长调度按 `Step` 重放，只要求不超过上界 |
| `bound_rand_test.go` | 至少 300 个带种子的随机配置：1–4 个节点，shared 与 per_node，非缺省 idle_kill、TTL 和 cap，行锁、停住的非读者 owner、重试和链主人可以同时在。穷尽搜索要求观察步数 ≤ `FairStepBound`。状态数超过上限的不记成通过，穷尽比例必须高于 95%，超过上限的种子数打在输出里。中位状态数要明显高于个位数。更多种子用 `FAIR_BOUND_SEEDS` |
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
