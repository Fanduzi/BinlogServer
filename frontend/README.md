# frontend Module

## Files
| File | Responsibility |
|------|---------------|
| `src/main.js` | Vue 应用入口 |
| `src/App.vue` | 主组件，含左侧功能菜单、多视图分区（总览/任务/源库/Worker/告警）、全局/当前页筛选范围、任务详情与设置对话框 |
| `src/components/MetricGrid.vue` | 首屏任务指标卡，分别展示 starting 与 running |
| `src/api.js` | API 调用封装，含真实后端请求、单/批量任务创建、401 处理与开发态 mock 分发 |
| `src/composables/useBatchCreate.js` | 批量任务表单预校验（最多 100 个有效行）、单次 `/api/tasks/batch` 创建请求与逐成功项自动启动 |
| `src/composables/useDashboard.js` | Dashboard/cluster 响应状态容器，消费 server `total/limit/offset`（旧响应保留本地分页兜底），保留 starting 与 running 独立计数 |
| `src/composables/useTaskFilter.js` | 任务本地筛选与 server page 查询参数编排 |
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
- 批量创建：保留行级预校验和最多 100 个有效行的本地限制，提交时将有效请求合并为一次 `/api/tasks/batch` 调用，并按成功结果逐项执行 autoStart；失败明细以安全文本显示。
- 401 处理：统一为中文运维提示，并直接引导用户进入设置配置 Token
- 多视图运维分区：左侧菜单切换 `总览 / 任务列表 / 源库覆盖 / Worker 运维 / 异常与告警`，降低单页长滚动操作成本
- 启动可见性：首屏指标卡和 source coverage 同时展示独立 `starting`，不把 STARTING 混入 `running`
- 服务端分页：任务列表用 dashboard 的有效 `total/limit/offset` 请求页数据，页面切换不会用当前页长度冒充全局总数；state/host/port 过滤随请求发送，关键词/源库文本/复制状态/告警/排序明确仅作用于当前页。
- URL 深链支持：可直接访问 `/#/tasks`、`/#/sources`、`/#/workers`、`/#/alerts` 分享指定运维视图
- 工具归属拆分：`运维筛选`仅在任务/告警工作区显示，`源库反查`仅在源库工作区显示
- E2E 回归覆盖：Playwright 用例覆盖分视图导航、深链、空态、详情抽屉、上传重试与 starting 指标 mock 场景
- 开发态 mock：显式环境变量打开后，前端可直接使用共享场景数据启动，不依赖真实后端
- 共享 mock 资产：Vite dev 与 Playwright 路由拦截复用同一套 mock 数据与 handler，避免双份漂移
- 内置 mock 场景：`empty`、`healthy`、`pagination`、`starting`、`anomaly`、`upload-failed`、`auth-required`、`cluster-degraded`、`lease-risk`、`control-plane-down-worker-running`

## Update Rule
- 前端模块边界、接口契约、构建流程变化时，更新本文件。
