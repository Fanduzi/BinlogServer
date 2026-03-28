# M3 里程碑实施文档

**日期:** 2026-03-28
**关联设计:** 2026-03-28-m3-milestone-design.md

---

## P0.1 — rate_limiter 单元测试

### 现状

`internal/upload/rate_limiter.go` 存在但无对应 `_test.go`。

### 实施步骤

1. 读取 `internal/upload/rate_limiter.go` 了解接口
2. 创建 `internal/upload/rate_limiter_test.go`
3. 覆盖场景：
   - 正常速率限制（令牌桶满/空边界）
   - 并发安全（goroutine 竞争）
   - 零速率 / 极大速率边界
4. 运行 `go test -race ./internal/upload/...` 验证

### 验收

```bash
go test -v -race ./internal/upload/... | grep -E 'PASS|FAIL|rate_limiter'
```

---

## P0.2 — smoke-retry-upload 进入 CI

### 现状

`.github/workflows/e2e.yml` 的自动矩阵不包含 `smoke-retry-upload`。

### 实施步骤

1. 读取 `.github/workflows/e2e.yml` 了解 matrix 结构
2. 将 `smoke-retry-upload` 加入 push/PR 触发的 e2e matrix
3. 评估 CI 时间影响，必要时设置 `timeout-minutes`

### 验收

PR 的 Checks 列表中出现 `e2e / smoke-retry-upload`。

---

## P1 — e2e CI 矩阵分层

### 策略设计

| 触发 | 跑哪些 profile | 预计时间 |
|------|---------------|----------|
| PR | quick + smoke-retry-upload | < 10 min |
| push to main | quick + smoke-retry-upload + meta-failover | < 20 min |
| workflow_dispatch | 所有 profile（full）| < 40 min |

### 实施步骤

1. 在 `e2e.yml` 中用 `github.event_name` 条件区分 matrix
2. `meta-failover` 加入 `push to main` 矩阵
3. 保留 `workflow_dispatch` 支持手动 full 运行

---

## P2 — 设计文档（不实现）

### 多云 Storage 接口设计

文件：`docs/develop/plans/2026-03-28-multi-cloud-storage-design.md`

内容提纲：
- 当前 `upload.Client` 接口分析
- 抽象 `StorageBackend` interface
- S3、GCS、Azure Blob 实现差异
- 配置格式设计

### 分片上传设计

文件：`docs/develop/plans/2026-03-28-multipart-upload-design.md`

内容提纲：
- 触发条件（文件大小阈值）
- 分片大小策略
- 断点续传状态持久化
- 与当前 upload retry 机制的集成点

---

## 执行顺序

```
P0.1 rate_limiter test
  ↓
P0.2 smoke-retry-upload CI
  ↓
P1 e2e matrix 分层
  ↓
P2 设计文档
  ↓
git tag v0.3.0
```

---

## 关键文件

| 文件 | 操作 |
|------|------|
| `internal/upload/rate_limiter.go` | 只读（理解接口）|
| `internal/upload/rate_limiter_test.go` | 新建 |
| `.github/workflows/e2e.yml` | 修改（矩阵调整）|
| `docs/develop/plans/2026-03-28-multi-cloud-storage-design.md` | 新建 |
| `docs/develop/plans/2026-03-28-multipart-upload-design.md` | 新建 |
