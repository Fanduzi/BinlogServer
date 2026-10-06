# internal/ui Module

## Files
- `ui.go`: UI 静态资源路由暴露。
- `static/`: 前端构建产物。任务详情在 `pending_dump_cleanup.connection_id` 有值时显示源库 Binlog Dump 仍可能开着的警告。`process_local` 为 true 时说明只有当时那个 worker 进程看得到。

## Exports
- `/ui/` 静态资源服务入口。

## Dependencies
- Upstream: `internal/api` server route mounting。
- Downstream: embed/static file serving。

## Update Rule
- UI 路由映射或静态资源装载方式变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
