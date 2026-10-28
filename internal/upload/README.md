# internal/upload Module

## Files
- `s3_uploader.go`: 对象存储上传。`UseSSL` 为 true 时走 HTTPS（进程配置 `upload.use_ssl` 默认 true），传输从 minio-go 的默认传输建起（环境变量代理和 SDK 超时），`CAFile`（`upload.tls.ca_file`）设置后只替换 `RootCAs`，是这个客户端唯一的信任库，主机名仍检查。TLS 握手错误第一次就返回，不重试到调用方超时。原文保留，并按类型加一句提示：明文端点是 `upload.use_ssl: false`，主机名不匹配不提 `ca_file`，未知 CA 才提 `upload.tls.ca_file`，协议版本不匹配和 `remote error: tls:` 不加提示。每次调用把自己的 TLS 错误放在该次 context 上，并发失败不串文。上传后用对象 HEAD 的 ETag 与封存文件对比（`SealedObjectMatches`），按已有 object key 打开已上传对象（本进程磁盘上没有该封存分段时供下载），以及按同一 key 删除对象（`DeleteObject`，对象不存在视为成功）。
- `s3_uploader_test.go` 的 `TestUseSSLTrueSendsTLSClientHello`：`UseSSL` true 的第一字节是 TLS client hello，false 不是。`TestCAFileTrustsOnlyThatCA`：配了 CA 的探测不是 x509，错误 CA 和空 CA 是。测试里的假 TLS 监听在每个连接上做一次 `Handshake` 后关闭，accepter 出错即退出。
- `tls_live_test.go` 的 `TestLiveS3TLS`（`BINLOG_TEST_TLS=1`，CI job `e2e-tls`）：私有 CA 的 MinIO，`upload.tls.ca_file` 经 HTTPS 上传一个分段，不受信任的 CA 失败。明文 MinIO 配 `use_ssl: true` 时错误含 `upload.use_ssl: false`，不含 `ca_file`。
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
