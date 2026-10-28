# internal Module

## Files
- `api`, `app`, `assurance`, `health`, `healthstore`, `binlog`, `config`, `logging`, `meta`, `ownership`, `replication`, `tasks`, `upload`, `ui`, `swaggerdocs`，以及发版门禁 `releaseprep`（`cmd/release-check` 调它的 `Main`；它的发布面清单 `Surfaces` 也被 `config` 的版本 pin 测试读取）。

## Exports
- 对外由 `cmd/*` 调用，不直接暴露给仓库外部导入。

## Dependencies
- Upstream: `cmd/binlog-server`, `cmd/migrate`。
- Downstream: 各子模块及外部基础设施（MySQL、对象存储、HTTP）。

## Update Rule
- 子模块边界/职责变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
