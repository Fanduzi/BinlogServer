# internal/binlog Module

## Files
- `writer.go`: binlog 文件写入与旋转。
- `checkpoint.go`: checkpoint 数据结构。
- `durable.go`: 最高 open 分段里最后一个完整事件的源文件名和 end log_pos。封存名和没有完整事件的分段不是续传点。`DurableResumeDir` 直接扫 catalog `file_path` 所在目录。
- `event_time.go`: 读一个分段的事件头，给出复制流里第一个和最后一个非 0 时间戳。时间戳 0 跳过。格式描述（format description）和 previous-GTIDs 的时间是源文件创建时间，不计入覆盖。没有 magic 或没有剩余计时事件时不算覆盖。尾部撕掉的字节保留已经读完的事件时间。

## Exports
- 文件写入、rotate 与 checkpoint 推进基础能力。
- `DurableResume` / `DurableResumeDir` / `DurableCursor`：runner 下次 Start 和 `GET /api/tasks/{id}/checkpoint` 共用的本地续传位点。不使用文件大小。`DurableResumeDir` 扫接管时 catalog 记下的分段目录。
- `EventTimeSpan`：定点恢复用来判断一个分段的复制事件时间是否盖住窗口。格式描述和 previous-GTIDs 不参与。调用方持有 reader。

## Dependencies
- Upstream: `internal/replication`。
- Downstream: 本地文件系统。

## Update Rule
- 文件写入语义、checkpoint 语义变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
