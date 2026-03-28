# Beta 发布验收清单（Task #25）

> 版本目标：`v0.1.0-beta.0` → 当前进度按 M2 DoD 逐项核查。

---

## 1. 单元测试

| 包 | 命令 | 状态 |
|----|------|------|
| 全量 | `go test ./...` | ✅ PASS |
| api/rate_limiter | `go test ./internal/api/ -run TestIPRateLimiter` | ✅ PASS（Task #25 补测）|

验收标准：`go test ./...` 零失败，zero data race（`-race` flag）。

```bash
go test -race ./...
```

---

## 2. 关键 E2E 场景

| 场景 | 脚本 | 覆盖能力 | CI 门禁 |
|------|------|----------|---------|
| `smoke-worker-crash-recovery` | `scripts/e2e/smoke-worker-crash-recovery.sh` | OPEN 期间崩溃接管、checkpoint 推进、md5 一致 | ⚠️ 待纳入 |
| `smoke-retry-upload` | `scripts/e2e/smoke-retry-upload.sh` | 上传失败补偿、手动触发补传 | ⚠️ 待纳入 |
| `smoke-observability` | `scripts/e2e/smoke-observability.sh` | Prometheus 指标、OTel trace | ✅ CI |
| `quick`（默认矩阵） | — | 基础拉流、任务生命周期 | ✅ CI |

验收标准：`smoke-worker-crash-recovery` 和 `smoke-retry-upload` 在 PR CI 自动运行且通过。

---

## 3. M2 DoD 逐项核查

### DoD 1：Worker 异常退出接管链路有 e2e 证据

- [x] `Task 22` 完成：`smoke-worker-crash-recovery` 场景存在
- [ ] CI 门禁：该场景尚未纳入 PR 自动矩阵（P1 待做）

### DoD 2：文件一致性可验证

- [x] 单文件唯一 sealed 逻辑已实现
- [x] md5 抽样一致在 `smoke-worker-crash-recovery` e2e 中验证
- [x] object key 采用 `cluster_key + source_server_uuid` 避免跨集群覆盖（Task #23.1）

### DoD 3：上传失败补偿可操作

- [x] 手动触发补传 API：`POST /api/tasks/{id}/files/retry-upload`（Task #23）
- [x] 失败原因聚合：`GET /api/tasks/{id}/upload-failures/reasons`（Task #24）
- [x] 可观测指标：`binlog_server_upload_retry_total`、`binlog_server_upload_retry_last_ts`（Task #24）
- [ ] 自动补传队列（后台持续重试）：**明确为非 M2 范围**，推迟到 M3

### DoD 4：文档包含 runbook 与边界说明

- [ ] 运维 runbook 文档（待补充）
- [ ] 边界声明文档（待补充）

---

## 4. 文档一致性

| 文档 | 状态 |
|------|------|
| README.md / README_ZH.md | ✅ 双语，badges 完整 |
| CHANGELOG.md | ✅ 存在 |
| SECURITY.md | ✅ 存在 |
| docs/releases/release-notes-v0.2.0.md | ✅ 双语发布说明 |
| 运维 runbook | ❌ 缺失 |

---

## 5. Beta 发布阻断项（Blockers）

以下任意一项未完成，不得打 `v0.1.0-beta.0` tag：

1. [ ] `smoke-worker-crash-recovery` 纳入 PR CI 门禁
2. [ ] `smoke-retry-upload` 纳入 PR CI 门禁
3. [ ] 运维 runbook 文档存在（至少覆盖：补传操作、crash 接管验证步骤）
4. [x] `go test -race ./...` 全量通过
5. [x] rate_limiter 单元测试覆盖

---

## 6. 非阻断项（可在 beta 后处理）

- 自动补传队列（后台重试）
- 多云官方 SDK 接入（OBS/COS/OSS）
- 断点续传跨重启恢复
- 前端 i18n Playwright 测试

---

## 7. 如何执行验收

```bash
# 1. 单元测试（含 race detector）
go test -race ./...

# 2. 关键 e2e（本地）
./scripts/e2e/run-suite.sh --scenarios smoke-worker-crash-recovery
./scripts/e2e/run-suite.sh --scenarios smoke-retry-upload

# 3. 确认 CI 门禁已包含上述场景
git log --oneline -5
```
