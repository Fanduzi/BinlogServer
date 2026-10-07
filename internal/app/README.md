# internal/app Module

## Files
- `app.go`: 应用主流程与运行时装配。Restore 之前先对齐遗留 `desired_run`。worker 启动、认领任务之前，删掉 data_dir 下每个任务目录里崩溃留下的 `.takeover-*`；本进程正在写的那份不删。同一个上传客户端既删对象，也在接管时把封存 `UPLOADED` 对象读回来。配置了上传且本进程是 worker 时，启动已封存 `UPLOAD_FAILED` 的后台补传。worker（含单机和 all-in-one）同时启动残留 Binlog Dump 的 KILL 重试，任何任务状态都会试。集群模式打开 dump fence；schema 还没有 `pending_dump_cleanup` 列时启动日志写明要先跑迁移 `000004`。
- `uploaded_takeover_test.go`: 走 `Run` 的启动接线。checkpoint 只在已上传对象里、本地文件不在时，不报 `SEGMENT_NOT_ON_WORKER`。worker 启动会删掉崩溃留下的 `.takeover-*`，其它名字留下。
- `tracing.go`: tracing provider 初始化与生命周期管理。
- `tracing_test.go`: OTLP HTTP 默认 traces 路径兼容性回归测试。
- `http_server_test.go`: HTTP 超时、PRODUCTION 以及非 loopback listen 的 auth fail-closed 校验测试。
- `smoke_test.go`: 应用层烟测（HTTP 创建任务需带 `source.password`），包括 auth 到真实路由、未带凭证可打开 Console、ClaimRunnableTasks 恢复、以及认领循环回归。
- `restart_recovery_test.go`: standalone 重启后安全的持久化 active task 自动恢复、metadata/source 冲突任务保持停止的回归测试。
- `source_guard_test.go`: 从 `meta_dsn` 到任务 API 的 metadata/source 隔离装配回归测试。

## Exports
- `New(cfg)` / `Run(ctx)`: 应用生命周期入口。
- role/mode 装配逻辑：control-plane/worker/all-in-one。
- control-plane 与 worker-health HTTP server 均应用可配置超时（ReadHeader/Read/Write/Idle）。
- 通过 `config.meta.timeout.*` 注入 tasks/meta 的内部依赖调用超时（读/写/lease/上传）。
- 对 TCP `meta_dsn` 提取 host/port 并注入 tasks，同端点 source 在 create/update/start 边界被拒绝。
- API server 支持从 `config.API.Auth` 注入鉴权策略。`/ui/*` 不走这套中间件；`/api/*`、`/metrics`、`/swagger/*` 仍按配置保护。
- 非空 `PRODUCTION` 用标准布尔值解析；true 时 `EncryptionKey`（`--encryption-key`）为空则拒绝启动。control-plane 在 true 时仍强制 auth 已启用、同时保护 `/api/*` 和 `/metrics`，并复用 `config.ValidateAPIAuthConfig` 校验模式/已解析凭证；worker-only 不暴露该 API 且不套用 auth 约束，但仍拒绝空的 encryption key。
- control-plane `listen_addr` 非 loopback（含 `:8080`、`0.0.0.0:8080`）时同样强制 `api.auth.enabled` + `protect_api` + `protect_metrics`；`127.0.0.1`/`localhost`/`::1` 可保持未鉴权本地演示。`/healthz` 仍匿名。
- 创建 meta store 时把 `config.EncryptionKey` 注入，用于 `source_json` 源库密码加解密。
- tracing：默认关闭；启用时装配 HTTP 入站 span 与元数据存储调用 span；无路径的 OTLP HTTP endpoint 沿用 `/v1/traces` 默认路径。
- worker 启动时先删掉 data_dir 下每个任务目录里崩溃留下的 `.takeover-*`（日志带任务、文件名和大小；本进程正在写的不删），再与认领循环都走 `ClaimRunnableTasks`：先对齐遗留 `desired_run`，再没人要的 STARTING、过期租约、自己名下空闲的 active 任务。本进程正在跑的任务如果期望是 STOP，或共享行已经是 STOPPING 或 STOPPED，这一轮会取消拉流；STOPPED 要等 Close 返回后才写。租约已过期的 STOPPING 会收成 STOPPED。不对别人仍持有未过期租约的 RUNNING 做 Stop。
- 封文件前验租把 `LeaseManager` 直接交给 runner（`Verify`），不再经 App 适配。
- standalone worker 注入进程内 `MemoryLease`（worker_id=`standalone`），与集群走同一扇所有权门。
- 封文件后上传由 App 注入 `ApplySealedUpload`，执行器只 seal。同一个上传客户端也作为保留清理的对象删除器注入。该客户端能读对象时，接管用它把 checkpoint 已经落在其中的封存 `UPLOADED` 对象读回来。封存后的这次 put 使用 `meta.timeout.upload_sec`，与后台补传的 `withUploadTimeout` 是同一个秒数。不新增配置项。
- 同一个上传配置下，worker（含单机和 all-in-one）启动 `RunBackgroundUploadRetry`。桶恢复后，已封存的 `UPLOAD_FAILED` 不必调用补传 API 就会再传。control-plane-only 不跑这个循环。手动 `POST /api/tasks/{id}/files/retry-upload` 仍可用。不新增配置项。
- `config.DataDir` 注入 scheduler。`binlog_files` 没有该任务的行时，`GET /api/tasks/{id}/files` 扫描这个目录下的本地分段。

### Minimal Tracing Config Example
```yaml
tracing:
  enabled: true
  exporter: "otlp-http"
  endpoint: "http://127.0.0.1:4318/v1/traces"
  sample_ratio: 0.1
  service_name: "binlog-server"
```

## Dependencies
- Upstream: `cmd/binlog-server`。
- Downstream: `internal/config`, `internal/tasks`, `internal/replication`, `internal/meta`, `internal/api`。

## Update Rule
- 装配流程、运行模式、生命周期行为变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
