# internal/replication Module

## Files
- `mysql_runner.go`: 复制主执行流程（含 open segment 元数据及进度更新、LATEST 立即 at-tip、idle 仅在 dump 达到 master file/pos 时标 at-tip、heartbeat 跳过落盘）。
- `source_identity.go`: MySQL/MariaDB 源库身份，以及永久认证/配置错误与可重试网络错误分类。
- `resolver.go`: 起点解析，以及 dump 与 SHOW MASTER STATUS file/pos 的保守比较。
- 其余 `*_test.go`: 复制、恢复、上传等行为测试。
  其中 `mysql_runner_run_test.go` 重点覆盖 runner 级起点选择、LATEST at-tip vs FILE_POS catch-up/idle-behind 进度、checkpoint 推进/失败、错误传播与停止清理语义。

## Exports
- Runner 启停与进度上报。fresh LATEST 在 StartSync 成功后立即按 at-tip 上报；idle dump wait 仅在 dump file/pos 已达到（或不落后于）源库 SHOW MASTER STATUS 时标 at-tip；仍落后的 FILE_POS/GTID 保持 event header lag。
- 源网络超时、拒绝、主机不可达及复制流 EOF/UnexpectedEOF 统一暴露 `SOURCE_UNREACHABLE`，本地文件/metadata/lease 错误保持原分类。
- MariaDB 身份：`mariadb:<server_id>:<gtid_domain_id>`；MySQL 仍用 `server_uuid`。
- 文件落盘、checkpoint 对接、随落盘进度更新的 OPEN/SEALED 生命周期元数据、上传触发。

## Dependencies
- Upstream: `internal/tasks` 调度层。
- Downstream: `internal/binlog`, `internal/meta`, source MySQL replication stream。

## Update Rule
- 拉流逻辑、文件语义、失败恢复/上传策略变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
