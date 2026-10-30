# internal/config Module

## Files
| File | Responsibility |
|------|---------------|
| `config.go` | 配置模型、加载与默认值、校验（`meta.tls` 默认 disabled，非 disabled 且 `meta_dsn` 为空时拒绝启动；`upload.use_ssl` 默认 true，`upload.endpoint` 带 `://` 时拒绝启动；`cluster.side_kill` 只接受 off 或 auto，缺省 off；`cluster.storage` 只接受 per_node 或 shared，缺省 per_node；`health.refusal_retry_cap_sec` 缺省 600，负数拒绝；`verify.*` 是阶段 3 巡检：`verify.recent_share_percent` 拒绝 0 和 1–100 以外；`verify.drill_dsn` 按密钥保护，空则还原演练完全不拨号，非空时每 `drill_every_sec`（默认 7 天）演练一次；`drill_check_sql` 是可选回放后校验；`health.*` 是 ADR 0009 P2-3 评估器开关；`meta_dsn` 为空时 standalone 任务写在 `.tasks.enc`；`meta_dsn` 里未设置的 `${ENV}` 报出变量名；布尔只接受 `true`/`false`，其他字面量记 `invalid boolean`；受保护 auth 不接受未解析 `${ENV}` 密钥；`enabled=true` 时未设置的 protect 标志视为 true；`EncryptionKey` 仅来自 `--encryption-key`；非整型记 `invalid integer`、非数字记 `invalid number`，不再变成 0；明文告警只看文件原文，`${ENV}` 不告警；`notify.webhook_url` 空则关闭 webhook，`notify.webhook_token`、`notify.timeout_sec` 默认 5、`notify.attempts` 默认 3、`notify.backoff_ms` 默认 200） |
| `cluster_claim.go` | ADR 0011 的启动校验（从 `validateConfig` 分出）：`validateClusterStorage` 拒绝 per_node/shared 以外的 `cluster.storage`；`validateOwnershipBounds` 按 `ownership.Params` 拒绝会让 `NodeGoneRejectsStart` 或 `RefusalCapRejectsStart` 成立的 `health.lease_ttl`/`sweep`/`node_gone_after`/`refusal_retry_cap` 组合 |
| `config.go`（`StandaloneConfig`） | ADR 0010：`standalone.task_records`（on/off，默认 on）、`standalone.task_key_file`、`standalone.task_key_previous_file`；密钥本身走 `BINLOG_SERVER_TASK_KEY` 或密钥文件，不进配置 |
| `config_test.go` | 配置加载、覆盖规则、protect 默认值、受保护 auth 未解析密钥、未设置的 `meta_dsn` 占位符、非数字拒绝、非 true/false 布尔拒绝，以及 `${ENV}` 不触发明文告警 |
| `config_examples_test.go` | 用 `LoadConfig` 加载仓库根目录和发版归档里的每一份示例配置，包括 `BINLOG_SERVER_META_DSN` 未设置时 |
| `config_doc_test.go` | `configuration.md` 必须写到 `config.go` 里的每一个 YAML 键；`VERSION` 必须通过不带 tag 的 `internal/releaseprep.Check`（`make release-check` 在打 tag 时用同一套）：有中英文发布说明和 CHANGELOG 小节，除发版历史白名单外没有上一个版本（和 tag 门禁同一个 `scanVersions`；没有 tag 时是 CHANGELOG 里比它旧的最新一节），并满足发布面清单 `Surfaces`：README、部署指南、升级指南、示例头、落地页、VERSION。新增发布面只改清单 |
| `landing_doc_test.go` | 落地页白名单：`docs/landing` 的文件集合、section 数、标题和元素 id 固定；版本号只能是 `VERSION`，且只出现在 `data-version`、徽章、`VER=`、CTA 四处；禁止日期、40 位以上 hex、运行时加载、超出体积上限；表驱动反例覆盖 #565 r1 QA 的 24 种回流写法 |
| `config_health_test.go` | `health.sweep_interval_sec`（默认 60）、`health.rehash_bytes_per_sec`（默认 4 MiB/s）、残留的 `health.evaluator` 键被忽略并告警、`health.stale_after_sec`（默认 0 = 5×sweep，负数拒绝，§28）、`health.node_gone_after_sec`（默认 0 = max(3×sweep, lease_ttl+2×sweep)，负数拒绝，§30）、`health.refusal_retry_cap_sec`（默认 600）的默认值、覆盖与非法值 |
| `config_notify_test.go` | `notify.webhook_url` 为空时不发；超时默认 5、次数默认 3、退避默认 200；非法 URL 和越界次数拒绝启动 |
| `config_verify_test.go` | `verify.every_sec`（默认 86400）、预算、轮换、`verify.recent_share_percent`（默认 25，拒绝 0 和 1–100 以外）、截止时间、`verify.pass_max_bytes`、最近窗口和演练周期 |
| `encryption.go` | AES-256-GCM `enc:aes256:` 加解密工具（配置值与 meta 源库密码共用） |
| `encryption_test.go` | 加解密往返测试 |
| `readme_doc_test.go` | README.md / README_ZH.md 白名单守卫，只防日常修改重新长出版本历史，不防刻意绕过：不得出现 `VERSION` 以外、以 `VERSION` 主.次版本号开头的三段版本号（带不带 `v`，含 `-rc` 后缀；0.99、0.5、`@` 后的固定版本、四段数字、其他软件版本和日期放行；minor/major 升级后旧版本线的漏改归发版检查的上一版本扫描）；一级/二级标题（ATX、setext、HTML，代码块 ``` 和 ~~~ 内不算）必须与 `readmeContracts` 逐条一致；每份 README 不得超过字节上限（设为大小 + 2048 + 最大的一条日常修改用例，所以 README 长 2 KB 后日常修改用例仍能逐条通过），上限也不得比大小多出 4 KB 以上；`Upgrading` / `升级` 一节到下一个一级/二级标题为止，只能是 `Latest release` / `最新版本` 一行加固定的升级指南链接，不得有三级及以下标题。每条规则都有只违反该规则的负例，日常修改有逐条单独检查的正例；全部用例还会在模拟的下一个 minor 和 major 版本下再跑一遍 |
| `readme_accepts_test.go` | README 守卫的正例：日常修改（三级标题、安装片段、FAQ 段落、检查清单条目、其他软件版本、`@` 固定版本等）逐条单独加到 README 上都必须通过；headroom check：上限下剩余空间必须放得下最大的一条日常修改，放不下时只报一条 headroom check（给出该设的上限），不报某条用例；按“大小 + 2048 + 最大用例”设的上限下，README 长满 2048 字节仍全部通过，多 1 字节由 headroom check 报出 |
| `upgrade_guide_doc_test.go` | `docs/guide/admin/upgrade*.md` 以版本号开头的顶层列表项、标题、引用列表项（可带强调、方括号、代码片段或 `Release`/`Version`/`版本` 前缀；嵌套子条目和表格行不算）必须严格从新到旧且最上面是 `VERSION`，格式不认识时报出那一行和可用格式。由 `readme_doc_test.go` 的升级用例在下一个 minor 和 major 版本下一并检查 |

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
- `upload.tls.ca_file` - 对象存储 PEM CA。空用系统信任库。设置后只信任该文件，主机名仍检查。`upload.use_ssl: false` 时拒绝
- `notify.*` - 通用 webhook（ADR 0009 决定 1）。`notify.webhook_url` 为空不发送；`notify.webhook_token` 放在 `X-BinlogServer-Token`；`notify.timeout_sec`、`notify.attempts`、`notify.backoff_ms` 见 configuration.md
- `tracing.*` - OTel tracing 配置
- 加密值格式：`enc:aes256:<base64-encoded-ciphertext>`

## Dependencies
- Upstream: `cmd/binlog-server`, `internal/app`, `internal/meta`
- Downstream: `viper`, process env, `crypto/aes`, `crypto/cipher`

## Update Rule
- 配置项、默认值、覆盖规则、加密格式变化时，更新本文件。
