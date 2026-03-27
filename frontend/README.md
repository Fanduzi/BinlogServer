# frontend Module

## Files
| File | Responsibility |
|------|---------------|
| `src/main.js` | Vue 应用入口 |
| `src/App.vue` | 主组件，含左侧功能菜单、多视图分区（总览/任务/源库/Worker/告警）、任务详情与设置对话框 |
| `src/api.js` | API 调用封装，含真实后端请求、401 处理与开发态 mock 分发 |
| `src/mocks/mock-data.js` | 共享 mock 场景数据，供 Vite dev 与 Playwright E2E 复用 |
| `src/mocks/mock-handler.js` | 共享 mock 请求分发与最小状态模拟 |
| `src/utils/auth.js` | 认证 Token 管理（localStorage 读写） |

## Exports
- 本地开发：`npm run dev`
- 开发态 mock：`VITE_USE_MOCK=true VITE_MOCK_SCENARIO=healthy npm run dev`
- 构建产物：`npm run build`，供后端 `internal/ui/static/` 使用

## Dependencies
- Upstream: 浏览器与开发者操作
- Downstream: 后端 `/api/*` 端点

## Features
- 认证支持：Bearer Token 配置（设置对话框），API 请求自动携带 Authorization 头
- 401 处理：统一为中文运维提示，并直接引导用户进入设置配置 Token
- 多视图运维分区：左侧菜单切换 `总览 / 任务列表 / 源库覆盖 / Worker 运维 / 异常与告警`，降低单页长滚动操作成本
- URL 深链支持：可直接访问 `/#/tasks`、`/#/sources`、`/#/workers`、`/#/alerts` 分享指定运维视图
- 工具归属拆分：`运维筛选`仅在任务/告警工作区显示，`源库反查`仅在源库工作区显示
- 开发态 mock：显式环境变量打开后，前端可直接使用共享场景数据启动，不依赖真实后端
- 共享 mock 资产：Vite dev 与 Playwright 路由拦截复用同一套 mock 数据与 handler，避免双份漂移
- 内置 mock 场景：`empty`、`healthy`、`anomaly`、`upload-failed`、`auth-required`、`cluster-degraded`、`lease-risk`、`control-plane-down-worker-running`

## Update Rule
- 前端模块边界、接口契约、构建流程变化时，更新本文件。
