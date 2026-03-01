# internal/binlog AGENTS

## Members
- `writer.go`: binlog 文件写入与旋转。
- `checkpoint.go`: checkpoint 数据结构。

## Interfaces
- 文件写入、rotate 与 checkpoint 推进基础能力。

## Dependencies
- Upstream: `internal/replication`。
- Downstream: 本地文件系统。

## Update Rule
- 文件写入语义、checkpoint 语义变化时，更新本文件。
