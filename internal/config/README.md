# internal/config Module

## Files
| File | Responsibility |
|------|---------------|
| `config.go` | 配置模型、加载与默认值、校验（`health.*` 是 ADR 0009 P2-3 评估器开关；`meta_dsn` 为空时 standalone 控制面仅内存；受保护 auth 不接受未解析 `${ENV}` 密钥；`enabled=true` 时未设置的 protect 标志视为 true；`EncryptionKey` 仅来自 `--encryption-key`） |
| `config.go`（`StandaloneConfig`） | ADR 0010：`standalone.task_records`（on/off，默认 on）、`standalone.task_key_file`、`standalone.task_key_previous_file`；密钥本身走 `BINLOG_SERVER_TASK_KEY` 或密钥文件，不进配置 |
| `config_test.go` | 配置加载、覆盖规则、protect 默认值和受保护 auth 未解析密钥测试 |
| `config_examples_test.go` | 用 `LoadConfig` 加载仓库根目录和发版归档里的每一份示例配置 |
| `config_health_test.go` | `health.evaluator`（off/shadow/on，默认 off）、`health.sweep_interval_sec`（默认 60）、`health.rehash_bytes_per_sec`（默认 4 MiB/s）、`health.stale_after_sec`（默认 0 = 5×sweep，负数拒绝，§28）、`health.node_gone_after_sec`（默认 0 = 3×sweep，负数拒绝，§30）的默认值、覆盖与非法值 |
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
- `tracing.*` - OTel tracing 配置
- 加密值格式：`enc:aes256:<base64-encoded-ciphertext>`

## Dependencies
- Upstream: `cmd/binlog-server`, `internal/app`, `internal/meta`
- Downstream: `viper`, process env, `crypto/aes`, `crypto/cipher`

## Update Rule
- 配置项、默认值、覆盖规则、加密格式变化时，更新本文件。
