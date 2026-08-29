# frontend/src/components Module

前端 Vue 展示组件，负责把 dashboard 状态数据渲染为首屏指标和操作视图。

## Files

| File | Responsibility |
|------|---------------|
| `MetricGrid.vue` | 渲染任务指标卡；`starting` 与 `running` 使用不同字段和 quick-filter 映射 |
| `NavPane.vue` | 多工作区导航与数量徽标 |
| `TaskDetailDrawer.vue` | 任务详情、状态和操作入口 |
| `AppHeader.vue` | 页面标题与刷新/创建/设置操作 |
| `AlertBanner.vue` | 顶层认证/告警提示 |
| `TaskCreateDialog.vue` / `BatchCreateDialog.vue` | 单任务与批量任务表单 |
| `SettingsDialog.vue` | 前端设置与语言切换 |

## Interfaces

- `MetricGrid` props：`summary`、`activeQuickFilter`；emits：`filter(kind)`。
- 组件通过 Vue props/emits 与 `App.vue` 交互，不直接请求后端。

## Dependencies

- Upstream: `frontend/src/App.vue` 与 `frontend/src/composables/*`。
- Downstream: Vue、Element Plus、i18n 文案。

## Update Rule

- 指标字段、状态卡、组件 props/emits 或跨组件职责变化时，更新本文件。
