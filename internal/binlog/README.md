# internal/binlog Module

## Files
- `writer.go`: binlog 文件写入与旋转。
- `checkpoint.go`: checkpoint 数据结构。
- `durable.go`: 最高 open 分段里最后一个完整事件的源文件名和 end log_pos。封存名和没有完整事件的分段不是续传点。`DurableResumeDir` 直接扫 catalog `file_path` 所在目录。

## Exports
- 文件写入、rotate 与 checkpoint 推进基础能力。
- `DurableResume` / `DurableResumeDir` / `DurableCursor`：runner 下次 Start 和 `GET /api/tasks/{id}/checkpoint` 共用的本地续传位点。不使用文件大小。`DurableResumeDir` 扫接管时 catalog 记下的分段目录。

## Dependencies
- Upstream: `internal/replication`。
- Downstream: 本地文件系统。

## Update Rule
- 文件写入语义、checkpoint 语义变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
