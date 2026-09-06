# internal/app Module

## Files
- `app.go`: 应用主流程与运行时装配。
- `tracing.go`: tracing provider 初始化与生命周期管理。
- `tracing_test.go`: OTLP HTTP 默认 traces 路径兼容性回归测试。
- `http_server_test.go`: HTTP 超时、PRODUCTION 以及非 loopback listen 的 auth fail-closed 校验测试。
- `smoke_test.go`: 应用层烟测（HTTP 创建任务需带 `source.password`），包括 auth 到真实路由、cluster 启动只恢复本 worker 任务、以及 worker claim 循环同时认领 STARTING/过期租约的回归。
- `restart_recovery_test.go`: standalone 重启后安全的持久化 active task 自动恢复、metadata/source 冲突任务保持停止的回归测试。
- `source_guard_test.go`: 从 `meta_dsn` 到任务 API 的 metadata/source 隔离装配回归测试。

## Exports
- `New(cfg)` / `Run(ctx)`: 应用生命周期入口。
- role/mode 装配逻辑：control-plane/worker/all-in-one。
- control-plane 与 worker-health HTTP server 均应用可配置超时（ReadHeader/Read/Write/Idle）。
- 通过 `config.meta.timeout.*` 注入 tasks/meta 的内部依赖调用超时（读/写/lease/上传）。
- 对 TCP `meta_dsn` 提取 host/port 并注入 tasks，同端点 source 在 create/update/start 边界被拒绝。
- API server 支持从 `config.API.Auth` 注入鉴权策略。
- 非空 `PRODUCTION` 用标准布尔值解析；control-plane 在 true 时强制 auth 已启用、同时保护 `/api/*` 和 `/metrics`，并复用 `config.ValidateAPIAuthConfig` 校验模式/已解析凭证；worker-only 不暴露该 API 且不套用此约束。
- control-plane `listen_addr` 非 loopback（含 `:8080`、`0.0.0.0:8080`）时同样强制 `api.auth.enabled` + `protect_api` + `protect_metrics`；`127.0.0.1`/`localhost`/`::1` 可保持未鉴权本地演示。`/healthz` 仍匿名。
- 创建 meta store 时把 `config.EncryptionKey` 注入，用于 `source_json` 源库密码加解密。
- tracing：默认关闭；启用时装配 HTTP 入站 span 与元数据存储调用 span；无路径的 OTLP HTTP endpoint 沿用 `/v1/traces` 默认路径。
- worker 启动与认领循环都走 `ClaimRunnableTasks`：没人要的 STARTING、过期租约、自己名下空闲的 active 任务。不对别人仍持有未过期租约的 RUNNING 做 Stop。
- 封文件前验租走 `LeaseManager.Verify`，不再把 MySQL store 转一层。

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
