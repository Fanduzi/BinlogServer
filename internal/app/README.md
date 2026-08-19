# internal/app Module

## Files
- `app.go`: 应用主流程与运行时装配。
- `tracing.go`: tracing provider 初始化与生命周期管理。
- `smoke_test.go`: 应用层烟测。

## Exports
- `New(cfg)` / `Run(ctx)`: 应用生命周期入口。
- role/mode 装配逻辑：control-plane/worker/all-in-one。
- 未配置 `meta_dsn` 时注入 `meta.FileTaskStore`，standalone 任务/checkpoint 落在 `data_dir`，重启后按 checkpoint resume。
- control-plane 与 worker-health HTTP server 均应用可配置超时（ReadHeader/Read/Write/Idle）。
- 通过 `config.meta.timeout.*` 注入 tasks/meta 的内部依赖调用超时（读/写/lease/上传）。
- API server 支持从 `config.API.Auth` 注入鉴权策略。
- tracing：默认关闭；启用时装配 HTTP 入站 span 与元数据存储调用 span。

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
