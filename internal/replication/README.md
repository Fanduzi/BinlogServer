# internal/replication Module

## Files
- `mysql_runner.go`: 复制主执行流程。
- `resolver.go`: 起点解析。
- 其余 `*_test.go`: 复制、恢复、上传等行为测试。
  其中 `mysql_runner_run_test.go` 重点覆盖 runner 级起点选择、checkpoint 推进/失败、错误传播与停止清理语义。

## Exports
- Runner 启停与进度上报。
- 文件落盘、checkpoint 对接、上传触发。

## Dependencies
- Upstream: `internal/tasks` 调度层。
- Downstream: `internal/binlog`, `internal/meta`, source MySQL replication stream。

## Update Rule
- 拉流逻辑、文件语义、失败恢复/上传策略变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
