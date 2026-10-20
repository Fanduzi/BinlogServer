# internal/ownership/mutants

把评审用的源码变异重新打到 `internal/ownership` 上，确认测试会失败。表里是原先的 34 个，加上后来的 F01–F20。

## Files

| File | Responsibility |
|------|---------------|
| `kill.go` | `//go:build ignore`。复制本包，逐个替换，跑 `go test`，打印击杀表 |

## Exports

- `go run ./internal/ownership/mutants/kill.go`

退出码 0 表示每个变异都被测试打死，或被标成等价（行为不变）。退出码 1 表示有变异活下来。

## Dependencies

- Upstream: `internal/ownership` 的 Go 源文件。
- Downstream: 无。不参与 `go test ./...`。

## Update Rule

- 变异表或击杀方式变化时，改 `kill.go` 的文件头和本文件。
