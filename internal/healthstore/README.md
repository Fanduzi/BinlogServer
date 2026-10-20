# internal/healthstore Module
- `Transition.Detail`：转换时实例的 detail（CONFIG 关闭的审计说明在这里，设计 0009 §21.5），文件存储与 meta `ListTransitions` 都返回。

健康评估的持久化接口（ADR 0009 阶段 2 P2-2，设计见 `docs/design/0009-phase2-health-evaluator.md` §5、§9）。尚未接线：P2-3 之后由评估器调用。

## Files

- `store.go`: （P2-6c：已释放 lease 上的代写 / 委派写同 epoch 不按 `fence_node` 比较）`Store` / `TransitionLister` 接口、`Applied`、`Transition`；错误 `ErrFenced`、`ErrLeaseLagging`、`ErrSchemaAbsent`、`ErrModeUnsupported`；`CheckFence(fence, lease, stored)` 是 cluster / standalone / never-leased 三种 fence 的唯一判定，MySQL 与文件实现共用。cluster 持有者要求 owner 与 epoch 都等于租约行（过期仍是自己的 → `ErrLeaseLagging`）；Proxy / Delegated 只在同 epoch 的租约已过期或已释放时可写；库里已有更高 epoch，或同 epoch 不同节点，一律 `ErrFenced`。§30：`Delegated` 还要求 `Stored.ChainOwner` 非空且等于 `Fence.Node`，否则 `ErrFenced`（`fence_node` 会被代写改掉，不是最后 owner）。§31：`Fence.PreserveChainOwner` 不参与 `CheckFence`；持有者写入是否改 `ChainOwner` 由 meta 和测试里的内存存储决定，该标志为真时不改。`ChainOwnerStore` 读这一列；文件存储不实现。standalone 不能写 cluster 写过的任务。`StoreClock.StoreNow`（§28）是可选的库时钟；文件存储不实现，HEALTH_STALE 那一轮不在 standalone 跑。
- `file.go`: `FileStore`，无 meta DSN 时使用，`{data_dir}/.health.json`，临时文件 + fsync + rename 原子写；只接受 standalone fence，其他返回 `ErrModeUnsupported`；`SavePending` / `LoadPending` 让重启后补写未落盘的 transition；transition 日志最多保留 20000 条。
- `file_test.go`: 文件实现跑 contract、重启与 pending 持久化、`CheckFence` 表驱动用例。
- `storetest/storetest.go`: 两种实现共用的 contract：generation 由 store 分配、按内容（digest）识别重放、5000 条 open finding 分页读全、standalone fence、1000 路并发写 generation 恰好 1..n 不重复、cluster fence（含 100 轮新旧 epoch 交替、委托写在新租约后失效）。
