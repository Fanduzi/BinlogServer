# P5b OTel 基础 Tracing 报告

## 1) 导出策略决策（四选一）
- 决策：**`otlp-http`**
- 理由：
  - 与现有部署面最小耦合，接入成本低。
  - 支持通过统一 OTLP collector 转发，不绑定 Jaeger 专有协议。
  - 便于后续扩展 metrics/logs 的同通道治理。
- 兼容策略：默认 `enabled=false`，`exporter=disabled`，显式启用才装配 provider/middleware。

## 2) 实现范围
- HTTP 入站 span：`internal/api/tracing.go` middleware。
- 元数据存储调用 span：`internal/meta` 的 `MySQLTaskStore` 与 `LeaseStore` 关键公开方法。
- 启动装配：`internal/app/tracing.go` + `app.go`。
- 配置：`internal/config` + `config.example.yaml` 新增 `tracing.*`。

## 3) 默认关闭“零影响”证据
1. 代码路径证据：
   - `api` 仅在 `WithTracing(Enabled=true)` 时注入 tracing middleware。
   - `meta` 仅在 app 调用 `meta.ConfigureTracing(true, ...)` 后才启动 span；默认关闭不产出 span。
2. 测试证据：
   - `TestAPI_TracingDisabledHasZeroIngressSpans`：默认配置下请求 `/healthz`，span 数量为 0。

## 4) 最小启用配置示例
```yaml
tracing:
  enabled: true
  exporter: "otlp-http"
  endpoint: "http://127.0.0.1:4318/v1/traces"
  sample_ratio: 0.1
  service_name: "binlog-server"
```

## 5) 开销粗对比（关闭/开启）
命令：
`go test ./internal/api -run '^$' -bench 'BenchmarkAPIHealthzTracing' -benchmem -count=1`

结果：
- Disabled: `684.5 ns/op`, `5388 B/op`, `15 allocs/op`
- Enabled:  `1247 ns/op`, `7371 B/op`, `25 allocs/op`

结论：开启 tracing 后请求路径有可预期开销上升；默认关闭时不引入 span 路径开销。

## 6) 验收说明
- 本阶段未修改 `docs/guide/*`。
- 未改变 API 状态码与任务状态机语义。
