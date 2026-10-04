# internal/upload Module

## Files
- `s3_uploader.go`: 对象存储上传，以及按已有 object key 打开已上传对象（本进程磁盘上没有该封存分段时供下载）。
- `s3_uploader_test.go`: 上传与按 key 打开对象的测试。
  重点覆盖配置校验、上传成功/失败、空对象 key、本地文件缺失、context cancel/deadline、HTTPS endpoint，以及打开已上传对象时的正文与大小。

## Exports
- 上传接口：将 sealed binlog 文件上传到对象存储。
- `OpenObject`：用同一客户端和同一套上传配置按 object key 打开已上传对象，返回打开时的对象大小。不新增配置项。对象不存在时返回 `os.ErrNotExist`。

## Dependencies
- Upstream: `internal/replication`, `internal/tasks`。
- Downstream: S3 兼容 SDK。

## Update Rule
- 上传协议、重试语义、配置契约变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
