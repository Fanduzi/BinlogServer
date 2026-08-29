# frontend/src/composables Module

前端状态与交互 composable，连接 API 数据、筛选状态和页面组件。

## Files

| File | Responsibility |
|------|---------------|
| `useDashboard.js` | Dashboard/cluster 数据容器、刷新和 source 查询参数；消费 server `total/limit/offset`，旧响应缺少分页字段时保留本地分页兜底，保留 starting/running 计数 |
| `useTaskFilter.js` | 任务状态（服务端/全局）与当前页复制状态筛选、排序和 server 分页查询参数 |
| `useFormatters.js` | 状态、lease、复制信息和时间格式化 |
| `useSourceLookup.js` | source host/port 查询状态 |
| `useTaskDetail.js` | 任务详情抽屉数据加载 |
| `useTaskForm.js` / `useBatchCreate.js` | 单任务/批量创建表单状态与动作 |
| `useAuth.js` / `useWindowState.js` | 认证提示与响应式窗口状态 |

## Interfaces

- `useDashboard()` 返回 `dashboard.summary`、任务/source 列表、server pagination metadata、cluster 状态和刷新 helpers。
- `useTaskFilter(dashboard)` 返回任务筛选、server page 参数、当前页任务和 quick-filter 状态；taskState 是服务端/全局筛选，keyword/sourceKeyword/replicationStatus/onlyAlert/sortBy 仅作用于当前页，不根据当前页推导全局 total。

## Dependencies

- Upstream: `frontend/src/api.js`、Vue 响应式运行时和 i18n。
- Downstream: `frontend/src/App.vue` 及表单/详情组件。

## Update Rule

- API 字段映射、筛选语义、刷新编排或 composable 返回接口变化时，更新本文件。
