# internal/tasks Module

## Files
- `scheduler.go`: 调度器核心类型、选项注入与通用辅助函数。
- `scheduler_task_ops.go`: 任务 CRUD 与配置更新（非运行时生命周期）。
- `scheduler_lifecycle.go`: 启停、运行协程与重试退避生命周期流程。
- `scheduler_cluster_lease.go`: cluster lease 续租与降级/失租处理。
- `scheduler_observability.go`: 复制进度、checkpoint、事件/文件/运行历史查询。
- `scheduler_retry_upload.go`: 上传失败补偿重试与失败原因聚合。
- `model.go`: 任务领域模型与状态定义。
- 各 `*_test.go`: 状态机、租约、上传重试、事件等测试。
- `event_store_test.go` 中 fake store 为并发安全实现，用于 `-race` 校验稳定性。

## Exports
- 任务 CRUD、启动停止、状态推进。
- 事件记录、文件元信息、上传补偿。
- `WithInternalCallTimeouts`：注入内部调用超时（read/write/lease/upload），用于 store/lease/uploader 依赖边界治理。
- Stop 路径 lease release 使用独立超时上下文（不复用已取消 runner ctx）。

## Dependencies
- Upstream: `internal/api`, `internal/app`。
- Downstream: `internal/replication` runner, `internal/meta` stores。

## Update Rule
- 状态机规则、调度策略、外部接口变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
