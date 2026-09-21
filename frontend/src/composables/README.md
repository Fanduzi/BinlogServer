# frontend/src/composables Module

前端状态与交互 composable，连接 API 数据、筛选状态和页面组件。

## Files

| File | Responsibility |
|------|---------------|
| `useDashboard.js` | Dashboard/cluster 数据容器和唯一的 `refreshAll` 编排（任务观测、集群观测、workers）；要求 dashboard 携带 `total/limit/offset`，缺少分页字段时拒绝应用，保留 starting/running 计数；列表不预取 `/lease` |
| `useTaskFilter.js` | 任务状态（服务端/全局）与当前页复制状态筛选、排序和 server 分页查询参数 |
| `useFormatters.js` | 状态、lease、复制信息和时间格式化；列表租约风险只用任务上的主人/epoch 抄本 |
| `useSourceLookup.js` | source host/port 查询状态 |
| `useTaskDetail.js` | 任务详情抽屉数据加载；单任务查询仍打 GET `/lease` |
| `useTaskForm.js` / `useBatchCreate.js` | 单任务/批量创建表单状态与动作；批量预览最多 100 个有效行，提交使用一次 `/api/tasks/batch` 请求 |
| `useAuth.js` / `useWindowState.js` | 认证提示与响应式窗口状态 |

## Interfaces

- `useDashboard()` 返回 `dashboard.summary`、任务/source 列表、server pagination metadata、cluster 状态和唯一的 `refreshAll`；`refreshAll` 并行拉 dashboard、cluster overview、workers，不按行打 `/lease`。
- `useTaskFilter(dashboard)` 返回任务筛选、server page 参数、当前页任务和 quick-filter 状态；taskState 是服务端/全局筛选，keyword/sourceKeyword/replicationStatus/onlyAlert/sortBy 仅作用于当前页，不根据当前页推导全局 total。
- `useBatchCreate(...)` 保留行预校验和 100 项上限，将有效 payload 合并为一次 batch 请求，按结果索引安全显示失败项，并对每个成功任务执行可选 autoStart。

## Dependencies

- Upstream: `frontend/src/api.js`、Vue 响应式运行时和 i18n。
- Downstream: `frontend/src/App.vue` 及表单/详情组件。

## Update Rule

- API 字段映射、筛选语义、刷新编排或 composable 返回接口变化时，更新本文件。
