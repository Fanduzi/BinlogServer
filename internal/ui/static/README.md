# internal/ui/static Module

前端构建产物目录，由后端以静态资源方式对外提供 `/ui/`。

## Files

| File | Responsibility |
|------|---------------|
| index.html | 管理台入口页面，引用当前 Vite hashed bundles |
| app.js | 打包后的前端脚本 |
| styles.css | 打包后的样式文件 |
| assets/ | 构建资源（图标、字体、chunk 等） |

## Exports

- 被 `internal/ui` 挂载并通过 `/ui/` 提供访问。
- 当前构建产物包含独立 starting 指标卡、服务端分页任务列表、全局/当前页筛选范围提示和 source 状态计数，使用 `make ui-build` 同步。

## Dependencies

- Upstream: `frontend` 构建流程。
- Downstream: 浏览器静态资源加载。

## Update Rule

- 构建产物布局或挂载约定变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
