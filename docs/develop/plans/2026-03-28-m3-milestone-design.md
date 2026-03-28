# M3 里程碑设计文档

**日期:** 2026-03-28
**目标版本:** v0.3.0
**前置:** M2 v0.1.0-beta.0 DoD 已满足（crash takeover e2e、文件一致性验证、upload 补偿、runbook）

---

## 背景

M2 以 v0.2.0 发布，完成了前端 i18n 支持。当前 TODO.md 显示 M2 DoD 核心条目已完成，但存在以下遗留问题需要在 M3 清理：

1. **Beta 清单残留** — `rate_limiter` 单元测试缺失（TODO #25）
2. **CI 门禁不完整** — 5 个 e2e profile 仅在本地 `make e2e-full` 运行，从未进入 PR 自动验证
3. **多云 Upload SDK** — 仅支持 S3-compatible，TODO #26 提出支持 GCS / Azure Blob（backlog）
4. **分片上传** — 大文件场景下单次 PUT 不可靠，TODO #27 提出分片上传设计（backlog）

---

## 目标

### P0 — 必须完成（Beta 质量门禁）

| # | 项目 | 说明 |
|---|------|------|
| 25 | `rate_limiter` 单元测试 | 补全 `internal/upload/rate_limiter.go` 的测试覆盖 |
| CI | 上传重试 e2e 进入 CI | `smoke-retry-upload` profile 加入自动 CI 矩阵 |

### P1 — 应该完成（CI 质量）

| # | 项目 | 说明 |
|---|------|------|
| CI | meta-failover e2e 进入 CI | `meta-failover` profile 加入 PR 自动验证 |
| CI | e2e 矩阵策略设计 | 区分 fast/full，PR 跑 fast，main 跑 full |

### P2 — 设计先行（下一轮迭代）

| # | 项目 | 说明 |
|---|------|------|
| 26 | 多云 Upload SDK 设计 | 抽象 Storage 接口，支持 GCS / Azure Blob |
| 27 | 分片上传设计 | 大文件分片 + 断点续传语义设计 |

---

## 成功标准（DoD）

1. `go test ./internal/upload/...` 覆盖 `rate_limiter` 所有路径，无跳过
2. GitHub Actions PR 检查中可见 `smoke-retry-upload` e2e 结果
3. P2 设计文档落地 `docs/develop/plans/`，不要求实现

---

## 不在范围内

- GCS / Azure Blob 实现（仅设计）
- 分片上传实现（仅设计）
- 前端新功能
- API 破坏性变更
