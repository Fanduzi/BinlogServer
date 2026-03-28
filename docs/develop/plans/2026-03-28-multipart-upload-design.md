# ADR-002: 分片上传（Multipart Upload）评估与设计

**日期：** 2026-03-28
**状态：** 提案（Proposed）
**关联：** Task #24.2

---

## 背景

当前上传链路使用 `FPutObject`（单次 PUT），对大文件（>100 MB）有以下风险：

1. **网络抖动导致整体重传**：单次 PUT 失败必须从头重传整个文件。
2. **无进度可观测**：上传过程无法细粒度监控。
3. **内存压力**：部分 SDK 实现会将整个文件读入内存。

binlog 文件大小取决于 `max_binlog_size`（默认 1 GB），生产环境常见 500 MB–1 GB sealed 文件。

---

## 评估目标

| 问题 | 需要回答 |
|------|----------|
| 进程内重试 vs 跨进程恢复 | 是否需要持久化 upload session？ |
| SDK 支持边界 | minio-go / OBS SDK / COS SDK / OSS SDK 各自支持到什么程度？ |
| 降级策略 | 若跨进程恢复不可行，如何保证最终一致？ |

---

## SDK 分片上传能力矩阵

| SDK | Multipart 上传 API | 进程内断点续传 | 跨进程恢复（持久化 UploadID） | 自动分片 |
|-----|--------------------|----------------|-------------------------------|----------|
| minio-go v7 | `PutObject`（自动分片，>5 MB 触发）| ✅ 内置 | ❌ UploadID 不暴露给调用方 | ✅ |
| 华为 OBS Go SDK | `InitiateMultipartUpload` / `UploadPart` | ✅ | ✅ 可持久化 UploadID | ❌（需手动分片）|
| 腾讯 COS Go SDK | `InitiateMultipartUpload` / `UploadPart` | ✅ | ✅ | ❌ |
| 阿里 OSS Go SDK | `InitiateMultipartUpload` / `UploadPart` | ✅ | ✅ | ❌ |

**关键发现：**
- minio-go 的分片上传对调用方透明（自动分片），但 UploadID 不暴露，**无法实现跨进程恢复**。
- OBS/COS/OSS 官方 SDK 支持手动管理 UploadID，可实现跨进程续传，但需要持久化层。

---

## 方案对比

### 方案 A：进程内自动重试（当前 + 小改）

```
上传失败 → 等待退避 → 重新 FPutObject（整个文件）
```

**优点：**
- 实现已完成（Task #23）
- 无需持久化额外状态

**缺点：**
- 大文件（>500 MB）重传代价高
- 进程重启后需等待调度器重新触发

**适用场景：** 文件 <100 MB，网络稳定，可接受整体重传。

---

### 方案 B：跨进程 Multipart 续传（持久化 UploadID）

```
文件分片 → 逐片上传 → 持久化 (UploadID, 已完成 Parts) → 进程重启 → 恢复已上传 Parts → 继续
```

**优点：**
- 大文件场景节省带宽和时间
- 进程重启后可从断点继续

**缺点：**
- 需要在 meta DB 新增 `upload_sessions` 表（或文件系统持久化）
- minio-go 不支持，需按云厂商分别实现（与 ADR-001 多云路线一致）
- 实现复杂度高：需处理 UploadID 过期（各厂商默认 7 天）、Part 重传幂等、AbortMultipartUpload 清理

---

### 方案 C：降级策略（推荐当前阶段）

```
分片上传失败 → AbortMultipartUpload → 回退到整体 FPutObject 重传
```

**优点：**
- 利用 SDK 自动分片提升大文件可靠性
- 失败时有明确降级路径
- 无需持久化额外状态

**缺点：**
- 跨进程续传仍不支持

---

## 决策

**当前阶段（M2/M3）：采用方案 C**

理由：
1. 生产环境尚未出现因大文件重传导致的实际问题。
2. Task #23 已实现补传触发机制，手动补传可兜底。
3. 方案 B 需要 schema 变更和多云 SDK 联调，复杂度超出当前里程碑范围。

**后续里程碑（M4+）：** 若监控数据显示大文件重传频繁（>5 次/天），再实施方案 B。

---

## 实施路线（方案 C）

### 阶段 1：为 minio-go 启用自动分片阈值配置

当前 `FPutObject` 已内置分片逻辑（>5 MB 自动分片），无需改动。
建议在 `S3Config` 增加 `PartSize` 配置项，允许运维调整分片大小。

```go
type S3Config struct {
    // ... 现有字段
    PartSize uint64 // 分片大小，字节，0 = SDK 默认（5 MB）
}
```

### 阶段 2：OBS/COS/OSS 实现时采用手动分片 API

待 ADR-001 多云 SDK 接入落地后，各 Provider 实现 `FileUploader` 接口时使用各自的 Multipart API。

### 阶段 3：监控指标

新增指标：
- `binlog_server_upload_part_total{provider, status}` — 分片上传次数
- `binlog_server_upload_bytes_total{provider}` — 累计上传字节数

---

## 非目标

- 跨进程 UploadID 持久化（M4+ 评估）
- 远端对象回读校验（已明确不做）
- 流式上传 OPEN 文件（不上传未 seal 文件）

---

## 参考

- [minio-go FPutObject](https://pkg.go.dev/github.com/minio/minio-go/v7#Client.FPutObject)
- [华为 OBS 分段上传](https://support.huaweicloud.com/sdk-go-devg-obs/obs_23_0402.html)
- [腾讯 COS 分块上传](https://cloud.tencent.com/document/product/436/65935)
- [阿里 OSS 分片上传](https://help.aliyun.com/zh/oss/developer-reference/multipart-upload)
