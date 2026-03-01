# frontend Module

## Files
- `src/`: Vue 前端源码。
- `vite` 配置与构建脚本。

## Exports
- 本地开发：`npm run dev`。
- 构建产物：`npm run build`，供后端 `internal/ui/static/` 使用。

## Dependencies
- Upstream: 浏览器与开发者操作。
- Downstream: 后端 `/api` 与构建产物同步流程。

## Update Rule
- 前端模块边界、接口契约、构建流程变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
