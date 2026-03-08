# 开源准备改进计划

**创建日期**: 2026-03-08
**目标**: 达到开源发布标准，总分从 82 提升到 90+
**预计工期**: 1-2 周

---

## 评分现状

| 维度 | 当前分数 | 目标分数 |
|------|----------|----------|
| 开源准备 | 60 | 90 |
| 安全性 | 75 | 85 |
| **相关总分** | **~70** | **~88** |

> 注：代码质量、测试覆盖等属于独立改进计划，不在此范围内。

---

## Phase 0: 开源治理文件（P0 - 阻塞开源）

**优先级**: 🔴 必须完成才能开源
**工期**: 1 天

### 任务清单

- [ ] 添加 `LICENSE` 文件（推荐 Apache 2.0 或 MIT）
- [ ] 添加 `CONTRIBUTING.md` 贡献指南
- [ ] 添加 `CODE_OF_CONDUCT.md` 行为准则
- [ ] 添加 `.github/ISSUE_TEMPLATE/` Issue 模板（bug_report.yml, feature_request.yml）
- [ ] 添加 `.github/PULL_REQUEST_TEMPLATE.md` PR 模板
- [ ] 在 README 中添加开源徽章（License、Go Report Card 等）

### 验收标准
- [ ] GitHub 能正确识别 License
- [ ] 新用户能通过 CONTRIBUTING.md 了解如何贡献代码
- [ ] Issue/PR 页面显示结构化模板选项

---

## Phase 1: 安全性加固（P0 - 安全风险）

**优先级**: 🔴 存在安全隐患
**工期**: 3-4 天

### 1.1 认证默认关闭风险

**问题**: `api.auth.enabled` 默认为 `false`，生产环境可能忘记开启。

**解决方案**:
- [ ] 启动时检测认证关闭，打印醒目警告（橙色/红色日志）
- [ ] 支持 `PRODUCTION=true` 环境变量，强制要求开启认证
- [ ] README 中添加醒目的安全警告框

```go
// 建议实现
func validateAuthConfig(cfg *config.Config) error {
    if !cfg.API.Auth.Enabled {
        if os.Getenv("PRODUCTION") == "true" {
            return errors.New("api.auth.enabled must be true in PRODUCTION mode")
        }
        log.Warn("⚠️  API authentication is DISABLED. Set api.auth.enabled=true for production.")
    }
    return nil
}
```

### 1.2 缺少 API 速率限制

**问题**: 无速率限制，易受 DoS 攻击或暴力破解。

**解决方案**:
- [ ] 添加基于 IP 的速率限制中间件
- [ ] 可配置限制阈值（requests/sec, burst）
- [ ] 支持 `X-Forwarded-For` 获取真实 IP（反向代理场景）

```yaml
# 配置示例
api:
  rate_limit:
    enabled: true
    requests_per_second: 100
    burst: 200
    trusted_proxies:
      - "10.0.0.0/8"
      - "172.16.0.0/12"
```

### 1.3 敏感信息泄露风险

**问题**:
- 密码在内存中明文存储
- 日志可能意外打印敏感信息
- API 响应中 `source.password` 字段处理

**解决方案**:
- [ ] 审计日志输出，确保不打印密码/token
- [ ] API 响应确认密码字段已脱敏（`json:"-"` 或返回时清空）
- [ ] 添加安全审计日志（登录/认证失败记录）

### 1.4 缺少输入长度限制

**问题**: 请求体无大小限制，可能被大请求攻击。

**解决方案**:
- [ ] 添加请求体大小限制（如 10MB）
- [ ] 添加请求超时配置

```go
// Gin 中间件
func RequestSizeLimit(maxBytes int64) gin.HandlerFunc {
    return func(c *gin.Context) {
        c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
        c.Next()
    }
}
```

### 1.5 SQL 注入检查

**问题**: 需确认所有 SQL 查询使用参数化。

**解决方案**:
- [ ] 审计所有 SQL 查询，确认使用参数化（当前使用 sqlc 生成，应该是安全的）
- [ ] 添加 gosec 扫描到 CI

### 验收标准
- [ ] 认证关闭时有醒目警告，生产模式强制开启
- [ ] 单 IP 超过 100 req/s 会被限流
- [ ] 日志中不会出现密码明文
- [ ] 请求体大小有限制

---

## Phase 2: 功能补全（P1 - 功能缺失）

**优先级**: 🟡 开启认证后前端无法使用
**工期**: 2 天

### 前端认证支持

**问题**: 开启 `protect_api: true` 后，前端 UI 无法获取数据（401）。

**解决方案**:
- [ ] 前端添加 Token 配置入口（设置页面或首次访问引导）
- [ ] 所有 API 请求自动携带 `Authorization: Bearer <token>`
- [ ] 401 响应时友好提示（跳转配置页面或显示提示）
- [ ] Token 持久化存储（localStorage）

```typescript
// 实现要点
// 1. Token 配置入口
// 2. 请求拦截器添加 Authorization header
// 3. 响应拦截器处理 401
```

### 验收标准
- [ ] 开启认证后，配置 Token 的前端能正常使用
- [ ] Token 配置后能持久化（刷新页面不丢失）
- [ ] Token 错误时有友好的错误提示

---

## Phase 3: 安全文档（P2 - 用户教育）

**优先级**: 🟢 帮助用户正确配置
**工期**: 1 天

### 任务清单

- [ ] 添加 `docs/security.md` 安全最佳实践文档
- [ ] 添加 `docs/troubleshooting.md` 故障排查指南
- [ ] README 添加安全警告框

### README 安全警告示例

```markdown
> ⚠️ **Security Warning**
>
> By default, API authentication is **DISABLED** for development convenience.
>
> **For production deployments, you MUST:**
> 1. Set `api.auth.enabled: true`
> 2. Configure `api.auth.bearer_token` with a secure random token
> 3. Set `api.auth.protect_api: true` and `api.auth.protect_metrics: true`
>
> ```bash
> # Generate a secure token
> openssl rand -hex 32
> ```
>
> Failure to do so exposes your backup tasks to unauthorized access.
```

### 验收标准
- [ ] 用户能通过 security.md 了解安全配置
- [ ] README 有醒目的安全警告

---

## 执行计划

```
Week 1:
├── Day 1: Phase 0 - 开源治理文件
├── Day 2-3: Phase 1.1-1.3 - 认证警告、速率限制、敏感信息
├── Day 4: Phase 1.4-1.5 - 请求限制、SQL审计
└── Day 5: Phase 2 - 前端认证支持

Week 2:
└── Day 1: Phase 3 - 安全文档
```

---

## 验收检查清单

完成以下所有项目后，项目可开源发布：

### 开源治理（必须）
- [ ] GitHub 显示 License
- [ ] CONTRIBUTING.md 存在且内容完整
- [ ] Issue/PR 模板可用

### 安全性（必须）
- [ ] 认证关闭时有警告
- [ ] 生产模式强制认证
- [ ] API 有速率限制
- [ ] 日志不泄露敏感信息
- [ ] 请求体有大小限制

### 功能完整（建议）
- [ ] 前端支持 Token 认证

### 文档（建议）
- [ ] 有安全配置文档
- [ ] README 有安全警告

---

## 相关资源

- [Open Source Guides - Legal](https://opensource.guide/legal/)
- [OWASP API Security Top 10](https://owasp.org/www-project-api-security/)
- [Go Security Policy](https://go.dev/security/policy)
