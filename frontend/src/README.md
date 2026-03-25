# frontend/src Module

前端 Vue 源码目录，提供管理台页面与 API 调用逻辑。

## Files

| File | Responsibility |
|------|---------------|
| main.js | Vue 应用入口 |
| App.vue | 运维控制台主页面，含告警优先指标、任务列表、详情抽屉与设置流程 |
| api.js | 与后端 `/api` 的请求封装，含认证拦截与设置引导事件 |

## Exports

- 浏览器端管理台应用。

## Dependencies

- Upstream: `frontend/` Vite 构建配置。
- Downstream: 后端 HTTP API（`/api/*`）。

## Update Rule

- 页面结构、API 契约或入口装配变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
