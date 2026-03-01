# cmd AGENTS

## Members
- `binlog-server/`: 主服务命令入口。
- `migrate/`: 迁移命令入口。

## Interfaces
- 提供可执行命令入口，不承载业务领域逻辑。

## Dependencies
- Upstream: 终端命令与进程管理。
- Downstream: `internal/*` 运行时模块。

## Update Rule
- 新增/调整命令入口时，更新本文件。
