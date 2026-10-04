# internal/replication Module

## Files
- `mysql_runner.go`: 复制主执行流程（含 open segment 元数据及进度更新、LATEST 与已在 master file/pos 的 FILE_POS 立即 at-tip、dump preamble 不落盘也不计延迟、idle 仅在 dump 达到 master file/pos 时标 at-tip、heartbeat 跳过落盘）。停止后再启动时调用 `tasks.NextResumePosition`：从本地最高 open 分段最后一个完整事件的 `end_log_pos` 续传，不跳到当前 `SHOW MASTER STATUS`，也不把 epoch>1 回拨到位置 4；该分段改名到新 epoch 后继续追加，字节跨停止点连续。没有本地事件的接管仍从位置 4 重建。封文件后把已 seal 文件交给注入的 handler 上传。打开文件时按 `storage.retention_days` 清理过期封存分段：已上传的对象用目录里的 `object_key` 从桶里删除，并删掉该目录行；没有元数据时用与上传相同的 object key。还在保留期内的分段和任何 open 分段不删。对象删除失败时本地文件留下，任务错误以 `OBJECT_PURGE_FAILED` 开头，下次打开文件再试。`checksum` 不参与这个判断，失败时也不改写。`KeepLocalSegments` 为真时不改写 adopt 保存的 `FILE_POS`，也不删除其它 epoch 的 `.open.e*`。若某个 open 分段的最后一个完整事件已经结束在该 `FILE_POS`，仍把该分段改名到当前 epoch 后追加，避免新文件只剩 magic 和位点之后的事件。对不上的分段留在原地，新字节写到当前 epoch 的新 open 文件。
- `source_identity.go`: MySQL/MariaDB 源库身份，以及永久认证/配置错误与可重试网络错误分类。
- `resolver.go`: 起点解析，以及 dump 与 SHOW MASTER STATUS file/pos 的保守比较。
- 其余 `*_test.go`: 复制、恢复、上传等行为测试。
  其中 `mysql_runner_run_test.go` 重点覆盖 runner 级起点选择、LATEST at-tip vs FILE_POS catch-up/idle-behind 进度、checkpoint 推进/失败、错误传播与停止清理语义。

## Exports
- Runner 启停与进度上报。fresh LATEST，以及 FILE_POS 起点已经不落后于 SHOW MASTER STATUS（含 standalone adopt 续传，以及停止后从本地分段末尾续上且该位点已经不落后），在 StartSync 成功后立即按 at-tip 上报。请求位点之前的 dump preamble（format description，`log_pos` 为 0 或仍小于等于当前位点）不写入 open 段，也不参与 delay。idle dump wait 仅在 dump file/pos 已达到（或不落后于）源库 SHOW MASTER STATUS 时标 at-tip；仍落后的 FILE_POS/GTID 保持 event header lag。停止后再启动（无 meta 的 standalone，以及有 checkpoint 的 catalog）从该任务 open 分段最后一个完整事件继续，源在停止期间写入的事件追加在同一分段，不从文件头重拉。standalone 被 kill 后没有任务元数据时，adopt 不带 start 保存的 `FILE_POS` 是最高分段的字节大小；该大小等于最后一个完整事件的 `end_log_pos` 时，start 同样把这段 open 文件改名到新 epoch 再追加，不跳到 `SHOW MASTER STATUS`，也不从位置 4 重拉。
- 源网络超时、拒绝、主机不可达及复制流 EOF/UnexpectedEOF 统一暴露 `SOURCE_UNREACHABLE`，本地文件/metadata/lease 错误保持原分类。
- MariaDB 身份：`mariadb:<server_id>:<gtid_domain_id>`；MySQL 仍用 `server_uuid`。`flavor=mysql` 探测不到 `@@server_uuid`（空结果或 unknown system variable）时，永久错误 `SOURCE_IDENTITY_UNAVAILABLE` 提示这台源像 MariaDB，并要求 `flavor=mariadb`。`flavor=mariadb` 不读 `server_uuid`。
- 文件落盘、checkpoint 对接、随落盘进度更新的 OPEN/SEALED 生命周期元数据；上传与重试共用 `tasks.ApplySealedUpload`。保留清理在本地文件删除之前删除已上传对象；删除失败不把这次清理报成成功。

## Dependencies
- Upstream: `internal/tasks` 调度层。
- Downstream: `internal/binlog`, `internal/meta`, source MySQL replication stream。

## Update Rule
- 拉流逻辑、文件语义、失败恢复/上传策略变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
