# 开源准备改进计划

**创建日期**: 2026-03-08
**目标**: 将项目从 82 分提升到 90+ 分，达到开源发布标准
**预计工期**: 2-3 周

---

## 评分现状

| 维度 | 当前分数 | 目标分数 |
|------|----------|----------|
| 代码质量 | 88 | 90 |
| 架构设计 | 85 | 88 |
| 安全性 | 75 | 85 |
| 测试覆盖 | 78 | 85 |
| 开源准备 | 60 | 90 |
| **总分** | **82** | **90+** |

---

## Phase 0: 开源治理文件（P0 - 必须）

**优先级**: 🔴 阻塞开源
**工期**: 1 天

### 任务清单

- [ ] 添加 `LICENSE` 文件（推荐 Apache 2.0）
- [ ] 添加 `CONTRIBUTING.md` 贡献指南
- [ ] 添加 `CODE_OF_CONDUCT.md` 行为准则
- [ ] 添加 `.github/ISSUE_TEMPLATE/` Issue 模板
- [ ] 添加 `.github/PULL_REQUEST_TEMPLATE.md` PR 模板
- [ ] 在 README 中添加开源徽章（License、Go Report Card 等）

### 验收标准
- GitHub 能正确识别 LICENSE
- 新用户能通过 CONTRIBUTING.md 了解如何贡献
- Issue/PR 页面显示模板选项

---

## Phase 1: 安全性加固（P0 - 必须）

**优先级**: 🔴 安全风险
**工期**: 3-5 天

### 1.1 认证改进

- [ ] 启动时检测认证关闭，打印醒目警告
- [ ] 支持环境变量 `PRODUCTION=true` 强制开启认证
- [ ] 更新部署文档，强调生产环境认证配置

```go
// 建议实现
func validateAuthForProduction(cfg *config.Config) error {
    if os.Getenv("PRODUCTION") == "true" && !cfg.API.Auth.Enabled {
        return errors.New("api.auth.enabled must be true in PRODUCTION mode")
    }
    return nil
}
```

### 1.2 API 速率限制

- [ ] 添加基于 IP 的速率限制中间件
- [ ] 可配置限制阈值（请求/秒、突发大小）
- [ ] 返回标准 `429 Too Many Requests` 响应

```go
// 建议实现
type RateLimiter struct {
    requestsPerSecond int
    burst             int
    store            Store // 可用内存或 Redis
}

func (rl *RateLimiter) Middleware() gin.HandlerFunc {
    return func(c *gin.Context) {
        key := c.ClientIP()
        if !rl.Allow(key) {
            c.AbortWithStatusJSON(429, gin.H{
                "error": "rate limit exceeded",
                "retry_after": rl.RetryAfter(key),
            })
            return
        }
        c.Next()
    }
}
```

### 1.3 敏感信息保护

- [ ] 配置文件中的密码支持加密存储（AES-256）
- [ ] 内存中的密码在日志/API响应中脱敏
- [ ] 添加敏感字段 `json:"-"` 标签

### 验收标准
- 生产环境启动时，认证关闭会报错或警告
- 速率限制能防止 100 req/s 以上的单 IP 攻击
- 密码不会出现在日志中

---

## Phase 2: 前端认证支持（P1 - 重要）

**优先级**: 🟡 功能缺失
**工期**: 2-3 天

### 任务清单

- [ ] 前端添加 Token 配置页面/环境变量
- [ ] API 请求自动携带 `Authorization: Bearer <token>`
- [ ] 401 响应自动跳转登录/配置页面
- [ ] Token 持久化（localStorage）

### 实现方案

```typescript
// frontend/src/utils/auth.ts
const TOKEN_KEY = 'binlog_server_token';

export function getAuthToken(): string | null {
  return localStorage.getItem(TOKEN_KEY);
}

export function setAuthToken(token: string): void {
  localStorage.setItem(TOKEN_KEY, token);
}

export function authHeaders(): HeadersInit {
  const token = getAuthToken();
  return token ? { 'Authorization': `Bearer ${token}` } : {};
}

// 拦截器处理 401
fetchInstance.interceptors.response.use(
  response => response,
  error => {
    if (error.response?.status === 401) {
      // 跳转到 Token 配置页面
      router.push('/settings/token');
    }
    return Promise.reject(error);
  }
);
```

### 验收标准
- 开启认证后，UI 能正常获取数据
- Token 配置后能持久化
- Token 错误时能友好提示

---

## Phase 3: 代码质量提升（P1 - 重要）

**优先级**: 🟡 可维护性
**工期**: 3-5 天

### 3.1 错误处理优化

- [ ] 减少 `log.Fatal` 使用（当前 35 处）
- [ ] 改为返回 error 让调用方处理
- [ ] 保留 `log.Fatal` 仅用于真正不可恢复的场景

### 3.2 大文件拆分

| 文件 | 当前行数 | 目标 | 拆分方案 |
|------|----------|------|----------|
| `internal/app/app.go` | 900+ | <400 | 拆分为 app_startup.go, app_cluster.go, app_shutdown.go |
| `internal/tasks/scheduler.go` | 较大 | <400 | 已拆分为多个 scheduler_*.go |

### 3.3 Lint 配置

- [ ] 添加 `.golangci.yml` 配置
- [ ] 启用 linters: errcheck, staticcheck, gosec, ineffassign
- [ ] 修复现有 lint 问题

```yaml
# .golangci.yml
linters:
  enable:
    - errcheck
    - staticcheck
    - gosec
    - ineffassign
    - govet
    - ineffassign

run:
  timeout: 5m
  skip-dirs:
    - frontend
    - scripts
```

### 验收标准
- `golangci-lint run` 无错误
- 单文件不超过 500 行
- 无 `log.Fatal` 在可恢复路径中

---

## Phase 4: 测试覆盖提升（P2 - 建议）

**优先级**: 🟢 质量提升
**工期**: 3-5 天

### 任务清单

- [ ] 添加测试覆盖率统计（`go test -cover`）
- [ ] 目标覆盖率 80%
- [ ] 添加关键路径的单元测试
- [ ] 添加 Benchmark 测试

### Makefile 目标

```makefile
.PHONY: test-cover
test-cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

.PHONY: test-bench
test-bench:
	go test -bench=. -benchmem ./...
```

### CI 集成

```yaml
# .github/workflows/test.yml
- name: Run tests with coverage
  run: |
    go test -coverprofile=coverage.out ./...
    go tool cover -func=coverage.out

- name: Enforce coverage threshold
  run: |
    COVERAGE=$(go tool cover -func=coverage.out | grep total | awk '{print $3}' | tr -d '%')
    if [ $(echo "$COVERAGE < 80" | bc) -eq 1 ]; then
      echo "Coverage $COVERAGE% is below 80% threshold"
      exit 1
    fi
```

### 验收标准
- 测试覆盖率 ≥ 80%
- CI 中强制检查覆盖率
- Benchmark 能运行

---

## Phase 5: 文档完善（P2 - 建议）

**优先级**: 🟢 用户体验
**工期**: 1-2 天

### 任务清单

- [ ] 添加 `docs/troubleshooting.md` 故障排查指南
- [ ] 添加 `docs/security.md` 安全最佳实践
- [ ] 添加 `CHANGELOG.md` 变更日志
- [ ] README 添加安全警告框

### README 安全警告

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
> Failure to do so exposes your backup tasks to unauthorized access.
```

### 验收标准
- 新用户能通过 troubleshooting.md 解决常见问题
- 安全文档清晰说明最佳实践

---

## 执行优先级

```
Week 1:
├── Phase 0: 开源治理文件（1天）
├── Phase 1: 安全性加固（3天）
│   ├── 1.1 认证改进
│   ├── 1.2 API 速率限制
│   └── 1.3 敏感信息保护
└── Phase 2: 前端认证支持（2天）

Week 2:
├── Phase 3: 代码质量提升（3天）
│   ├── 3.1 错误处理优化
│   ├── 3.2 大文件拆分
│   └── 3.3 Lint 配置
└── Phase 4: 测试覆盖提升（2天）

Week 3:
└── Phase 5: 文档完善（1-2天）
```

---

## 验收检查清单

完成以下所有项目后，项目可达到 90+ 分：

- [ ] GitHub 显示 Apache 2.0 License
- [ ] `CONTRIBUTING.md` 存在且内容完整
- [ ] 生产环境启动时会检查认证配置
- [ ] API 有速率限制保护
- [ ] 前端支持 Bearer Token 认证
- [ ] `golangci-lint run` 无错误
- [ ] 测试覆盖率 ≥ 80%
- [ ] README 有安全警告

---

## 相关资源

- [Open Source Guides](https://opensource.guide/)
- [Go Security](https://golang.org/doc/security/)
- [Rate Limiting Patterns](https://cloud.google.com/architecture/rate-limiting-strategies-techniques)
