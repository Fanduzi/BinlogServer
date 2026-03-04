# internal/meta Module

## Files
- `mysql_store.go`: 元数据持久化实现与 schema 校验。
- `lease_store.go`: lease 读写逻辑。
- `retry.go`: 重试策略适配层与执行器封装（基于 backoff v4，屏蔽第三方类型）。

## Exports
- Task/Checkpoint/Event/File/Lease/Run/Worker metadata 存储接口。
- 启动期 schema 版本与结构校验（支持 schema 校验超时配置）。

## Dependencies
- Upstream: `internal/tasks`, `internal/app`, `internal/replication`。
- Downstream: MySQL (`database/sql`)。
- Retry adaptor: `github.com/cenkalti/backoff/v4`（仅 `retry.go` 内部使用）。

## Update Rule
- 表结构契约、查询语义、重试策略变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
