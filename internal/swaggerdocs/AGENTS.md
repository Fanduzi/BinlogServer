# internal/swaggerdocs AGENTS

## Members
- `docs.go`: 生成的 swagger/openapi 文档代码。

## Interfaces
- swagger 文档元数据供 API 文档页面消费。

## Dependencies
- Upstream: `internal/api` 注释与接口定义。
- Downstream: swagger handler。

## Update Rule
- API 注释生成流程或文档结构变化时，更新本文件。
