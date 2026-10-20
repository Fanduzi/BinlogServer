# internal/ownership/mutants

把评审用的源码变异重新打到 `internal/ownership` 上，确认测试会失败。表里的编号是 M01 起的一串，和评审评论里的 F/G 编号不是同一套。

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
