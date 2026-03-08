# frontend Module

## Files
| File | Responsibility |
|------|---------------|
| `src/main.js` | Vue 应用入口 |
| `src/App.vue` | 主组件，含任务列表、详情、设置对话框 |
| `src/api.js` | API 调用封装，认证拦截器与 401 处理 |
| `src/utils/auth.js` | 认证 Token 管理（localStorage 读写） |

## Exports
- 本地开发：`npm run dev`
- 构建产物：`npm run build`，供后端 `internal/ui/static/` 使用

## Dependencies
- Upstream: 浏览器与开发者操作
- Downstream: 后端 `/api/*` 端点

## Features
- 认证支持：Bearer Token 配置（Settings 对话框），API 请求自动携带 Authorization 头
- 401 处理：自动弹出认证提示，引导用户配置 Token

## Update Rule
- 前端模块边界、接口契约、构建流程变化时，更新本文件。
