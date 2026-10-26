# internal/config Module

## Files
| File | Responsibility |
|------|---------------|
| `config.go` | 配置模型、加载与默认值、校验（`meta.tls` 默认 disabled，非 disabled 且 `meta_dsn` 为空时拒绝启动；`upload.use_ssl` 默认 true，`upload.endpoint` 带 `://` 时拒绝启动；`cluster.side_kill` 只接受 off 或 auto，缺省 off；`verify.*` 是阶段 3 巡检：`verify.recent_share_percent` 拒绝 0 和 1–100 以外；`verify.drill_dsn` 按密钥保护；`health.*` 是 ADR 0009 P2-3 评估器开关；`meta_dsn` 为空时 standalone 任务写在 `.tasks.enc`；`meta_dsn` 里未设置的 `${ENV}` 报出变量名；布尔只接受 `true`/`false`，其他字面量记 `invalid boolean`；受保护 auth 不接受未解析 `${ENV}` 密钥；`enabled=true` 时未设置的 protect 标志视为 true；`EncryptionKey` 仅来自 `--encryption-key`；非整型记 `invalid integer`、非数字记 `invalid number`，不再变成 0；明文告警只看文件原文，`${ENV}` 不告警） |
| `config.go`（`StandaloneConfig`） | ADR 0010：`standalone.task_records`（on/off，默认 on）、`standalone.task_key_file`、`standalone.task_key_previous_file`；密钥本身走 `BINLOG_SERVER_TASK_KEY` 或密钥文件，不进配置 |
| `config_test.go` | 配置加载、覆盖规则、protect 默认值、受保护 auth 未解析密钥、未设置的 `meta_dsn` 占位符、非数字拒绝、非 true/false 布尔拒绝，以及 `${ENV}` 不触发明文告警 |
| `config_examples_test.go` | 用 `LoadConfig` 加载仓库根目录和发版归档里的每一份示例配置，包括 `BINLOG_SERVER_META_DSN` 未设置时 |
| `config_doc_test.go` | `configuration.md` 必须写到 `config.go` 里的每一个 YAML 键；`VERSION` 必须和 README、部署指南、示例头、落地页当前版本一致 |
| `config_health_test.go` | `health.evaluator`（off/shadow/on，默认 off）、`health.sweep_interval_sec`（默认 60）、`health.rehash_bytes_per_sec`（默认 4 MiB/s）、`health.stale_after_sec`（默认 0 = 5×sweep，负数拒绝，§28）、`health.node_gone_after_sec`（默认 0 = 3×sweep，负数拒绝，§30）的默认值、覆盖与非法值 |
| `config_verify_test.go` | `verify.every_sec`（默认 86400）、预算、轮换、`verify.recent_share_percent`（默认 25，拒绝 0 和 1–100 以外）、截止时间、`verify.pass_max_bytes`、最近窗口和演练周期 |
| `encryption.go` | AES-256-GCM `enc:aes256:` 加解密工具（配置值与 meta 源库密码共用） |
| `encryption_test.go` | 加解密往返测试 |

## Exports
- `LoadConfig(path)` - 加载配置（无加密）
- `LoadConfigWithEncryption(path, key)` - 加载配置（支持解密，并把 key 保留到 `Config.EncryptionKey`）
- `ValidateAPIAuthConfig(auth)` - 校验鉴权模式、路由保护与已解析凭证
- `NewDecryptor(key)` - 创建加解密器（32 字节 AES-256 密钥）
- `Decryptor.Encrypt(value)` / `Decryptor.DecryptIfEncrypted(value)` - 生成或解密 `enc:aes256:` 值

## Configuration Sections
- `api.auth.*` - API 鉴权开关、模式（`bearer`/`api_key`）与凭证配置；`enabled=true` 且未显式设置时 `protect_api`/`protect_metrics` 为 true
- `config.production.example.yaml`（仓库根目录）- 无密钥且可直接加载的生产模板；仅需环境变量注入 bearer token，并同时保护 `/api/*` 与 `/metrics`
- `api.rate_limit.*` - API 限流配置（enabled/requests_per_second/burst），默认 100 req/s、burst 200
- `http.control_plane.*` / `http.worker_health.*` - HTTP 超时配置
- `meta.timeout.*` - 内部依赖调用超时配置
- `meta.tls.*` - meta 连接 TLS（ADR 0012）。默认 disabled。`ca`/`cert`/`key` 是路径，启动时读取，错误不包含 PEM
- `tracing.*` - OTel tracing 配置
- 加密值格式：`enc:aes256:<base64-encoded-ciphertext>`

## Dependencies
- Upstream: `cmd/binlog-server`, `internal/app`, `internal/meta`
- Downstream: `viper`, process env, `crypto/aes`, `crypto/cipher`

## Update Rule
- 配置项、默认值、覆盖规则、加密格式变化时，更新本文件。
