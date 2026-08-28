# internal/swaggerdocs Module

## Files
- `docs.go`: 生成的 swagger/openapi 文档代码。
- `swagger.json` / `swagger.yaml`: 生成的 OpenAPI 规范文件。

## Exports
- swagger 文档元数据供 API 文档页面消费，`BinlogFile` 包含 `OPEN/SEALED` state。

## Dependencies
- Upstream: `internal/api` 注释与接口定义。
- Downstream: swagger handler。

## Update Rule
- API 注释生成流程或文档结构变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
