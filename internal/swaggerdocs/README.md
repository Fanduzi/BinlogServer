# internal/swaggerdocs Module

## Files
- `docs.go`: 生成的 swagger/openapi 文档代码。
- `swagger.json` / `swagger.yaml`: 生成的 OpenAPI 规范文件。

## Exports
- swagger 文档元数据供 API 文档页面消费，`BinlogFile` 包含 `OPEN/SEALED` state；summary/dashboard/source 契约包含独立 `starting` 计数，`running` 仅代表 RUNNING。
- `/api/sources/lookup` 文档说明 localhost 与显式 loopback literal 共用 host identity，其他 host 按原文字面匹配。

## Generation Note

- 主线文档中的完整 swag 生成命令当前会产生约 1769 行与本任务无关的历史 churn；本 issue 只同步 `starting` schema 增量，保留该生成债务待后续专门处理。

## Dependencies
- Upstream: `internal/api` 注释与接口定义。
- Downstream: swagger handler。

## Update Rule
- API 注释生成流程或文档结构变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
