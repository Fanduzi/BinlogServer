# internal/swaggerdocs Module

## Files
- `docs.go`: 生成的 swagger/openapi 文档代码。
- `swagger.json` / `swagger.yaml`: 生成的 OpenAPI 规范文件。

## Exports
- swagger 文档元数据供 API 文档页面消费，`BinlogFile` 包含 `OPEN/SEALED` state；summary/dashboard/source 契约包含独立 `starting` 计数，`running` 仅代表 RUNNING；任务列表与 dashboard 暴露 state/host/port 过滤及 `total/limit/offset` 分页字段。
- `/api/sources/lookup` 文档说明 localhost 与显式 loopback literal 共用 host identity，其他 host 按原文字面匹配。

## Generation Note

- 使用 `go run github.com/swaggo/swag/cmd/swag@v1.16.6 init -g cmd/binlog-server/main.go -o internal/swaggerdocs --parseInternal` 生成 `docs.go`、`swagger.json` 与 `swagger.yaml`。

## Dependencies
- Upstream: `internal/api` 注释与接口定义。
- Downstream: swagger handler。

## Update Rule
- API 注释生成流程或文档结构变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
