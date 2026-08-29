# internal/config Module

## Files
| File | Responsibility |
|------|---------------|
| `config.go` | 配置模型、加载与默认值、校验（`meta_dsn` 为空时 standalone 控制面仅内存；受保护 auth 不接受未解析 `${ENV}` 密钥） |
| `config_test.go` | 配置加载、覆盖规则和受保护 auth 未解析密钥测试 |
| `encryption.go` | AES-256-GCM 配置值解密工具 |

## Exports
- `LoadConfig(path)` - 加载配置（无加密）
- `LoadConfigWithEncryption(path, key)` - 加载配置（支持解密）
- `ValidateAPIAuthConfig(auth)` - 校验鉴权模式、路由保护与已解析凭证
- `NewDecryptor(key)` - 创建解密器（32 字节 AES-256 密钥）
- `Decryptor.DecryptIfEncrypted(value)` - 解密带 `enc:aes256:` 前缀的值

## Configuration Sections
- `api.auth.*` - API 鉴权开关、模式（`bearer`/`api_key`）与凭证配置
- `config.production.example.yaml`（仓库根目录）- 无密钥且可直接加载的生产模板；仅需环境变量注入 bearer token，并同时保护 `/api/*` 与 `/metrics`
- `api.rate_limit.*` - API 限流配置（enabled/requests_per_second/burst），默认 100 req/s、burst 200
- `http.control_plane.*` / `http.worker_health.*` - HTTP 超时配置
- `meta.timeout.*` - 内部依赖调用超时配置
- `tracing.*` - OTel tracing 配置
- 加密值格式：`enc:aes256:<base64-encoded-ciphertext>`

## Dependencies
- Upstream: `cmd/binlog-server`, `internal/app`
- Downstream: `viper`, process env, `crypto/aes`, `crypto/cipher`

## Update Rule
- 配置项、默认值、覆盖规则、加密格式变化时，更新本文件。
