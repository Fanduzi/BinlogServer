# cmd/binlog-server/cmd Module

binlog-server CLI 子命令定义目录。

## Files

| File | Responsibility |
|------|---------------|
| root.go | Cobra root command，负责参数绑定与应用启动调用 |

## Exports

- `NewRootCommand() *cobra.Command`

## Dependencies

- Upstream: `cmd/binlog-server/main.go`。
- Downstream: `internal/config`, `internal/app`。

## Update Rule

- CLI 参数、命令结构或启动流程变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
