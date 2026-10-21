# internal/upload Module

## Files
- `s3_uploader.go`: 对象存储上传，上传后用对象 HEAD 的 ETag 与封存文件对比（`SealedObjectMatches`），按已有 object key 打开已上传对象（本进程磁盘上没有该封存分段时供下载），以及按同一 key 删除对象（`DeleteObject`，对象不存在视为成功）。
- `s3_uploader_test.go`: 上传、按 key 打开对象、以及删除对象的测试。
  重点覆盖配置校验、上传成功/失败、空对象 key、本地文件缺失、context cancel/deadline、HTTPS endpoint、打开已上传对象时的正文与大小，以及 DELETE 成功、对象不存在、存储错误。配置了 `BINLOG_E2E_MINIO_ENDPOINT` 时再对真实 MinIO 删除一次。

## Exports
- 上传接口：将 sealed binlog 文件上传到对象存储。
- `OpenObject`：用同一客户端和同一套上传配置按 object key 打开已上传对象，返回打开时的对象大小。不新增配置项。对象不存在时返回 `os.ErrNotExist`。
- `ProbeStorage`：启动时 HEAD 桶。桶在返回 nil；桶不在、鉴权失败和网络错误都带上桶名和端点，不带密钥。对象 HEAD 的空 404 会被 SDK 报成 `NoSuchKey`，分不清桶不在，所以探针不走 `HeadObject`。能 ListBucket 但不能 GetObject 的策略这里看不出来。
- `HeadObject`：按同一 key 做 HEAD，返回对象大小。对象不存在（`NoSuchKey` 或 404）时返回 `os.ErrNotExist`。响应里带 `NoSuchBucket` 时不是对象缺失，原样返回。巡检用它，不把它加进启动用的上传接口。
- `SealedObjectMatches`：HEAD 已上传对象，把 ETag 与封存文件比较。不超过 16MiB 用整文件 MD5；更大用与 `FPutObject` 相同的 16MiB 分片 ETag。ETag 为空或大小不一致返回 false。Stat 失败返回错误，调用方据此把 `checksum` 留空，不写成 `mismatch`。
- `ListObjects(ctx, prefix, limit)`：列出 prefix 下的 key 与大小（设计 0009 §21.3），超过 limit 返回 truncated=true；出错或截断时调用方不据此判定任何对象缺失。
- `DeleteObject`：按 object key 删除已上传对象。不新增配置项。对象不存在时返回 nil。桶不存在和其它错误返回给保留清理，本地分段留下。

## Dependencies
- Upstream: `internal/replication`, `internal/tasks`。
- Downstream: S3 兼容 SDK。

## Update Rule
- 上传协议、重试语义、配置契约变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
