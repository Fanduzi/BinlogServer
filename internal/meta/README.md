# internal/meta Module

## Files
- `mysql_store.go`: 元数据持久化实现与 schema 校验。
- `lease_store.go`: lease 读写逻辑。
- `retry.go`: MySQL 重试策略。

## Exports
- Task/Checkpoint/Event/File/Lease/Run/Worker metadata 存储接口。
- 启动期 schema 版本与结构校验。

## Dependencies
- Upstream: `internal/tasks`, `internal/app`, `internal/replication`。
- Downstream: MySQL (`database/sql`)。

## Update Rule
- 表结构契约、查询语义、重试策略变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
