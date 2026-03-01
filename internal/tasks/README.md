# internal/tasks Module

## Files
- `scheduler.go`: 任务状态机与调度核心。
- `model.go`: 任务领域模型与状态定义。
- 各 `*_test.go`: 状态机、租约、上传重试、事件等测试。

## Exports
- 任务 CRUD、启动停止、状态推进。
- 事件记录、文件元信息、上传补偿。

## Dependencies
- Upstream: `internal/api`, `internal/app`。
- Downstream: `internal/replication` runner, `internal/meta` stores。

## Update Rule
- 状态机规则、调度策略、外部接口变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
