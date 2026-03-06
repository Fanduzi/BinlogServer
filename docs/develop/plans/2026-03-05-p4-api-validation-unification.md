# P4 API 参数校验统一报告

## Scope
- 阶段：P4（API 参数校验统一）
- 范围：`internal/api` 试点 3 个高频接口，仅参数绑定与校验方式治理。
- 约束：不改变 API 状态码、核心错误语义、任务状态机流程。

## Pilot Interfaces
1. `GET /api/sources/lookup`
2. `POST /api/tasks/{id}/files/retry-upload`
3. `GET /api/tasks/{id}/upload-failures/reasons`

## Implementation Summary
- 使用 Gin binding + validator 替代手工 parse/if：
  - `sourceLookupQuery`
  - `retryUploadLimitQuery`
  - `uploadFailureReasonsLimitQuery`
  - `listLimitQuery`（通用 limit 容错路径）
- 新增统一错误映射函数：`mapSourceLookupBindError`。
- 仍保留原有错误文案与状态码语义。

## Swagger Alignment
- `internal/api/swagger_docs_only.go`
  - `retry-upload` 的 `limit` 参数：`minimum(1) default(100) maximum(1000)`
  - `upload-failures/reasons` 的 `limit` 参数：`minimum(1) default(20) maximum(200)`
- `handleSourceLookup` 注释描述更新为必填与端口范围。

## Compatibility: 旧错误响应 vs 新错误响应
| 接口 | 场景 | 旧响应 | 新响应 | 兼容性 |
|---|---|---|---|---|
| `/api/sources/lookup` | 缺少 `host` | `400 host is required` | `400 host is required` | 保持一致 |
| `/api/sources/lookup` | 缺少 `port` | `400 port is required` | `400 port is required` | 保持一致 |
| `/api/sources/lookup` | `port=abc` | `400 invalid port` | `400 invalid port` | 保持一致 |
| `/api/tasks/{id}/files/retry-upload` | `limit=0/1001/abc` | `400 invalid limit` | `400 invalid limit` | 保持一致 |
| `/api/tasks/{id}/upload-failures/reasons` | `limit=0/201/abc` | `400 invalid limit` | `400 invalid limit` | 保持一致 |

## Tests Added/Adjusted
- `TestTaskAPI_RetryUploadLimitValidation`：补充断言错误文案 `invalid limit`。
- `TestTaskAPI_UploadFailureReasonsLimitValidation`：新增非法/越界 limit 用例。
- `TestAPI_SourceLookupValidationErrors`：新增 `host/port` 缺失与非法端口用例。

## Template Reference
- 阶段验收模板：`docs/develop/plans/2026-03-02-phase-acceptance-template.md`
