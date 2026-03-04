# internal/config Module

## Files
- `config.go`: 配置模型、加载与默认值。
- `config_test.go`: 配置加载与覆盖规则测试。

## Exports
- `LoadConfig(path)`：加载配置。
- 环境变量覆盖规则与占位符展开。
- `api.auth.*`：API 鉴权开关、模式（`bearer`/`api_key`）与凭证配置。
- `api.auth.*` 默认不保护路由；开启 `protect_api/protect_metrics` 后需提供对应凭证。
- `http.control_plane.*` / `http.worker_health.*`：HTTP 超时配置（ReadHeader/Read/Write/Idle）。
- `meta.timeout.*`：内部依赖调用超时配置（read/write/lease/upload），用于存储/租约/上传边界，不作用于入站 HTTP 连接。
- 内置配置校验：鉴权凭证缺失或超时配置非法会返回错误。

## Dependencies
- Upstream: `cmd/binlog-server`, `internal/app`。
- Downstream: `viper`, process env。

## Update Rule
- 配置项、默认值、覆盖规则变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
