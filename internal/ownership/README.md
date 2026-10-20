# internal/ownership Module

ADR 0011 的任务所有权过渡。一个纯函数：当前任务状态加一个事件，得到下一状态和副作用意图。不做 I/O，不读时钟，时钟由调用方传入。调度器还没接上，现有运行路径不变。

词汇用仓库 `CONTEXT.md`：任务所有权、租约、认领。执行器事件只在 epoch、owner、`row_generation` 都对得上时写入。`LeaseUnreadable` 走 EpochLost，不进 FAILED，也不进 RUNNING。`MetaLockTimeout` 只记下 `META_LOCK_STALLED`，不把行锁清掉；空闲事务由 `idle_kill` 在下一步开头丢掉。

## Files

| File | Responsibility |
|------|---------------|
| `state.go` | 任务状态：期望、观察态、租约、备份链、拒绝、结论、节点 |
| `event.go` | ADR 0011 §3.2 的事件，以及续租、sweep、行锁、旧二进制更新 |
| `step.go` | `Step`：一次事件到下一状态和意图 |
| `invariant.go` | 不变量 1–10 的可检查谓词，认领集、停止集、`/api/health`。不变量 7 用 `ProgressOverdue` 从状态和时钟重算 |
| `finding.go` | 结论从事实派生。事实没变时，同一键不会先关后开 |
| `spec.go` | RTO、stale、`node_gone`、拒绝退避、迁移预检、旁路杀计划 |
| `invariant_test.go` | 安全性：固定种子的随机前缀，加深度 3 的有界穷举。字母表含旧 epoch、`StopConfirmed`、`StopExpired`、`Lock`，时钟会跳过租约 TTL |
| `liveness_test.go` | 活性 (a)(b)(c)。有界搜索每一条公平调度，起点是多条带偏随机游走里的前提状态。检查方法写在文件头。K 含 N 次无用认领 |
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
- `MigrationAllowed`、`PlanSideKill`、`BackfillChain`、`GenerationBumped`、`RefusalDelay`
- `New`、`DefaultParams`、`Held`

`PlanSideKill` 在缺省 `off` 下不发 `KILL`。`auto` 只在直连、过程存在、演练通过、并且有 `PROCESS`（MySQL 还要 `data_locks`）时调用过程。

## Dependencies

- Upstream: 无。不导入调度器，不打开数据库。
- Downstream: 无。PR-2 接会话超时和旁路杀，PR-4 把 `Step` 接到调度器。本包不实现观察者死后由 `_health` 持有者关闭 `META_LOCK_STALLED`（N6），也不实现 catalog 一致性（F7，PR-5）。

## Update Rule

- 成员、接口或依赖变化时，同时改本文件和根 `README.md` 的 Modules。
- 事件或意图变化时，改对应 Go 文件头的 input/output/pos。
