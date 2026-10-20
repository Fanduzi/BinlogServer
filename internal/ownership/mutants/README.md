# internal/ownership/mutants

把评审用的源码变异重新打到 `internal/ownership` 上，确认测试会失败。表里有 M01 起的一串，也有 G01，以及打在 `FairStepBound` 上的 J05、J06、J11、B01–B04、U06、U08、U16、U17、U18、V04、V17，和把任何 `CHAIN_OWNER_GONE` 都当成终态的 S03，删掉 sweep 步的 W02，`fairRunning` 不看行锁的 T08，上界丢掉 `ChainOwnerGone` 的 W04，把 NULL 拒绝重新写成永久的 P01，把 `DumpReady` 换成 `EpochLost` 的 L01，把进度步换成纯退避跳的 L02，以及把饱和比较从 `<` 改回 `<=` 的 K01。L01 和 L02 要被底强连通分量检查打死。K01 要被时间键的边界检查打死。没有单独标成等价的一档。

## Files

| File | Responsibility |
|------|---------------|
| `kill.go` | `//go:build ignore`。复制本包，逐个替换，跑 `go test`，打印击杀表 |

## Exports

- `go run ./internal/ownership/mutants/kill.go`

退出码 0 表示每个变异都被测试打死。退出码 1 表示有变异活下来。没有「等价、可以放过」这一档。

## Dependencies

- Upstream: `internal/ownership` 的 Go 源文件。
- Downstream: 无。不参与 `go test ./...`。

## Update Rule

- 变异表或击杀方式变化时，改 `kill.go` 的文件头和本文件。
