# cmd Module

## Files
- `binlog-server/`: 主服务命令入口。
- `migrate/`: 迁移命令入口。

## Exports
- 提供可执行命令入口，不承载业务领域逻辑。

## Dependencies
- Upstream: 终端命令与进程管理。
- Downstream: `internal/*` 运行时模块。

## Update Rule
- 新增/调整命令入口时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
