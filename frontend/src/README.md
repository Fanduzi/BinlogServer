# frontend/src Module

前端 Vue 源码目录，提供管理台页面与 API 调用逻辑。

## Files

| File | Responsibility |
|------|---------------|
| main.js | Vue 应用入口 |
| App.vue | 运维控制台主页面，含左侧整列可折叠菜单（左下折叠控件）、多工作区分区（总览/任务/源库/Worker/告警）与 `/#/...` 深链定位、明确的全局/当前页筛选范围、详情抽屉与设置流程 |
| components/MetricGrid.vue | 首屏状态指标卡，分别展示 `summary.starting` 与 `summary.running` |
| api.js | 与后端 `/api` 的请求封装，含单/批量任务创建、认证拦截、设置引导事件与开发态 mock 分发 |
| composables/useBatchCreate.js | 批量创建行预校验（最多 100 个有效行）、单次 batch API 调用及逐项自动启动 |
| composables/useDashboard.js | Dashboard/cluster 响应状态与刷新编排，消费 server pagination（旧响应保留本地分页兜底），保留 starting/running 独立计数 |
| composables/useTaskFilter.js | 当前页本地筛选和 server pagination 查询参数 |
| mocks/mock-data.js | 共享 mock 场景数据 |
| mocks/mock-handler.js | 共享 mock 请求分发与最小状态模拟 |

## Exports

- 浏览器端管理台应用。
- 批量创建通过一次 `/api/tasks/batch` 请求提交有效行，消费有序的成功/错误结果并保持每个成功任务的自动启动行为。
- 批量预览超过 100 个有效行时本地拒绝提交；失败明细以安全文本渲染。
- Dashboard summary 兼容后端既有字段，并消费新增 `starting` 计数和 `total/limit/offset`；任务状态筛选为服务端全局筛选，其余任务筛选与排序明确为当前页行为。

## Dependencies

- Upstream: `frontend/` Vite 构建配置。
- Downstream: 后端 HTTP API（`/api/*`）与本地开发态共享 mock handler。

## Update Rule

- 页面结构、API 契约或入口装配变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
