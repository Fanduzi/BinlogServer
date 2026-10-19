# internal/healthstore/storetest

健康存储的共用 contract（ADR 0009 阶段 2 P2-2）。文件存储和 MySQL 存储都跑 `Run`。

## Files

- `storetest.go`: generation 由 store 分配、按内容（digest）识别重放、open finding 分页读全、standalone fence、并发写 generation 恰好 1..n。有 lease 表时还有 cluster fence。§30：并发的 proxy / delegated 风暴之前先做一次持有者写入（转移为空，generation 仍是 0），记下 `chain_owner`，否则委派写会被空的 `chain_owner` 拒绝。

## Exports

- `Harness`、`Run`。`SetLease` 为空时跳过 cluster 用例（文件存储）。

## Dependencies

- Upstream: `internal/healthstore` 的文件测试与 `internal/meta` 的 MySQL contract。
- Downstream: `internal/health`、`internal/healthstore`。
