# internal/ui AGENTS

## Members
- `ui.go`: UI 静态资源路由暴露。
- `static/`: 前端构建产物。

## Interfaces
- `/ui/` 静态资源服务入口。

## Dependencies
- Upstream: `internal/api` server route mounting。
- Downstream: embed/static file serving。

## Update Rule
- UI 路由映射或静态资源装载方式变化时，更新本文件。
