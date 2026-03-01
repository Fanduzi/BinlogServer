# internal Module

## Files
- `api`, `app`, `binlog`, `config`, `logging`, `meta`, `replication`, `tasks`, `upload`, `ui`, `swaggerdocs`。

## Exports
- 对外由 `cmd/*` 调用，不直接暴露给仓库外部导入。

## Dependencies
- Upstream: `cmd/binlog-server`, `cmd/migrate`。
- Downstream: 各子模块及外部基础设施（MySQL、对象存储、HTTP）。

## Update Rule
- 子模块边界/职责变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
