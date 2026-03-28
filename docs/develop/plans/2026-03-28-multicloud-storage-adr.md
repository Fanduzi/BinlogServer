# ADR-001: 多云对象存储接入策略

**日期：** 2026-03-28
**状态：** 已接受（Accepted）
**关联：** Task #24.1

---

## 背景

当前上传实现（`internal/upload/s3_uploader.go`）基于 minio-go SDK，通过 S3 兼容 API 对接 MinIO / AWS S3。

生产需求方反映需接入：
- 华为云 OBS（`obs-go-sdk-v3`）
- 腾讯云 COS（`cos-go-sdk-v5`）
- 阿里云 OSS（`aliyun-oss-go-sdk`）

这三家云厂商均未完整兼容 S3 API（尤其在签名、错误码、分片上传语义上存在差异），直接使用 minio-go 会导致不可预期的兼容性问题。

---

## 决策

**采用「多 Provider 多 SDK」路线，不追求单一统一接入层。**

| Provider | SDK | 备注 |
|----------|-----|------|
| AWS S3 / MinIO / S3 兼容 | `github.com/minio/minio-go/v7` | 现有实现，保持不变 |
| 华为云 OBS | `github.com/huaweicloud/huaweicloud-sdk-go-obs` | 官方 SDK |
| 腾讯云 COS | `github.com/tencentyun/cos-go-sdk-v5` | 官方 SDK |
| 阿里云 OSS | `github.com/aliyun/aliyun-oss-go-sdk` | 官方 SDK |

**接入方式：** 各 Provider 实现统一的 `FileUploader` 接口。

```go
// internal/tasks/scheduler.go
type FileUploader interface {
    UploadFile(ctx context.Context, taskID, localPath, objectKey string) error
}
```

调度层（`internal/app/app.go`）根据配置的 `provider` 字段选择对应实现，其余代码无感知。

---

## 方案对比

### 方案 A（被否决）：统一走 S3 兼容层

使用 minio-go 对接所有云厂商的「S3 兼容」端点。

**问题：**
- OBS/COS/OSS 的 S3 兼容层存在已知缺陷（分片上传、ACL、生命周期规则）
- 签名算法差异导致边缘场景失败难以调试
- 官方不保证 100% S3 兼容

**决策：否决。** 短期省事，长期维护成本高。

---

### 方案 B（已接受）：多 Provider 多 SDK

每个云厂商一个实现文件：

```
internal/upload/
├── s3_uploader.go        # MinIO / AWS S3（现有）
├── s3_uploader_test.go
├── obs_uploader.go       # 华为云 OBS（待实现）
├── cos_uploader.go       # 腾讯云 COS（待实现）
└── oss_uploader.go       # 阿里云 OSS（待实现）
```

**优点：**
- 使用官方 SDK，行为可预期
- 各实现独立，互不干扰
- 接口统一，调度层零改动

**缺点：**
- 依赖数量增加（3 个 SDK）
- 各 Provider 需分别维护测试

---

## 配置设计

```yaml
upload:
  provider: obs          # s3 | obs | cos | oss
  endpoint: https://obs.cn-north-4.myhuaweicloud.com
  bucket: my-binlog-bucket
  access_key: AK...
  secret_key: SK...
  region: cn-north-4
  use_ssl: true
  prefix: binlog/
```

`internal/app/app.go` 按 `provider` 字段路由到对应实现：

```go
switch cfg.Upload.Provider {
case \"s3\", \"\":
    uploader, err = upload.NewS3Uploader(...)
case \"obs\":
    uploader, err = upload.NewOBSUploader(...)
case \"cos\":
    uploader, err = upload.NewCOSUploader(...)
case \"oss\":
    uploader, err = upload.NewOSSUploader(...)
}
```

---

## 实施顺序

| 优先级 | Provider | 理由 |
|--------|----------|------|
| P0 | AWS S3 / MinIO | 已完成，保持现状 |
| P1 | 华为云 OBS | 客户需求最强 |
| P2 | 腾讯云 COS | 次优先 |
| P3 | 阿里云 OSS | 与 COS 接口相近，可复用模式 |

---

## 测试策略

- 单元测试：使用 interface mock，不依赖真实云环境
- 集成测试：各 Provider 提供独立 e2e scenario（`smoke-obs-upload`、`smoke-cos-upload`）
- CI 默认不跑云厂商 e2e（需要真实凭证），仅在 `workflow_dispatch` 触发

---

## 非目标

- 运行时动态切换 Provider（配置变更需重启）
- 多 Provider 并行上传
- 跨云数据迁移

---

## 已确认约束（2026-02-19）

> 上传实现采用