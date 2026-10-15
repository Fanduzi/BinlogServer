# cmd/binlog-server/cmd Module

binlog-server CLI 子命令定义目录。

## Files

| File | Responsibility |
|------|---------------|
| root.go | Cobra root command，负责参数绑定、根命令参数校验与应用启动调用；`--encryption-key` 同时用于配置值解密和 meta 源库密码加密；bind 失败不刷 Usage |
| version.go | `version` 子命令、`--version` 输出与 ASCII banner 版本信息渲染；启动失败在日志重定向撤掉之前写入配置的日志文件 |
| task_key.go | `task-key generate --out <path>`：生成 32 字节随机 standalone 任务密钥（base64）写入新的 0600 文件，已存在则拒绝；只打印路径和 key id，从不打印密钥（ADR 0010） |
| root_test.go | CLI 回归测试，覆盖 `version`、`--version` 与位置参数校验 |

## Exports

- `NewRootCommand() *cobra.Command`
- `binlog-server version`
- `binlog-server --version`

## Dependencies

- Upstream: `cmd/binlog-server/main.go`。
- Downstream: `internal/config`, `internal/app`, `internal/logging`。

## Update Rule

- CLI 参数、命令结构或启动流程变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
