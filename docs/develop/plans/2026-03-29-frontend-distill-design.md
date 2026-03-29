# Frontend Distill Design: App.vue Decomposition

## 1. 背景

App.vue 当前约 3160 行，是一个完整的单文件单页应用。所有状态、逻辑、UI 均在此文件中。audit report 将其列为 P2 问题：2810-line monolithic component，建议拆分为 composables 和子组件。

## 2. 设计目标

- 保持现有功能和 API 契约完全不变
- 不引入新的 UI 库或构建依赖
- 每个提取单元有清晰的单一职责
- App.vue 瘦身至 template 框架 + 胶水逻辑，script 部分约 100 行以内
- 全程保持可构建、可运行

## 3. 文件结构目标

```
frontend/src/
  composables/
    useDashboard.js        # dashboard + cluster 状态，refreshAll，数据写入
    useTaskFilter.js       # uiFilter，pager，filteredTasks，pagedTasks，quick filter 逻辑
    useTaskDetail.js       # 抽屉状态：detailTask, replication, lease, runs, checkpoint, events, files
    useTaskForm.js         # 表单状态：create/edit form，validation，submit/delete
    useBatchCreate.js      # 批量创建：batchForm, batchPreview, 解析/验证/提交
    useSourceLookup.js     # sourceQuery, lookup state, lookupSource 调用
    useWindowState.js      # windowWidth, isMobile, nowRef (1s ticker), menuCollapsed
    useAuth.js             # settingsToken, settingsVisible, auth-required 事件处理, saveSettings
  components/
    AppHeader.vue          # hero header：title, 4 action buttons
    MetricGrid.vue         # 6 KPI cards
    TaskDetailDrawer.vue   # el-drawer 及其内部所有 detail 内容
    TaskCreateDialog.vue   # 创建/编辑表单 dialog
    BatchCreateDialog.vue  # 批量创建 dialog
    SettingsDialog.vue     # 设置 dialog
    AlertBanner.vue        # auth-required banner (el-alert)
  App.vue                  # 仅剩：shell layout, nav, workspace panel 切换, filter panel, table, pagination
```

## 4. 核心状态边界

### 4.1 共享状态（App 级别，通过 composable 返回值传递）

| 状态 | 所在 composable | 谁消费 |
|---|---|---|
| `dashboard.tasks` | useDashboard | useTaskFilter, TaskDetailDrawer |
| `cluster` | useDashboard | AppHeader (worker count), Overview panel |
| `loading` | useDashboard | AppHeader (refresh button disabled) |
| `activeView` | App.vue (保留) | nav items, workspace 切换 |
| `activeQuickFilter` | useTaskFilter | MetricGrid (active state) |
| `isMobile` | useWindowState | dialogs width, drawer size |

### 4.2 组件间通信

- 子组件通过 **props 接收数据，emit 触发动作**
- composable 在 App.vue 实例化后按需传入子组件
- 不使用 Vuex/Pinia，不使用 provide/inject（避免隐式耦合）

## 5. 提取顺序（风险最低到最高）

1. `useWindowState` — 无 API 依赖，最简单
2. `useAuth` — 独立的认证流，边界清晰
3. `useSourceLookup` — 独立的查询流
4. `useDashboard` — 核心数据层，先提取再拆 UI
5. `useTaskFilter` — 依赖 dashboard.tasks
6. `useTaskDetail` — 依赖 dashboard.tasks + API
7. `useTaskForm` / `useBatchCreate` — 依赖 API + dashboard 刷新
8. 子组件提取（从叶子节点到根节点）：AlertBanner → SettingsDialog → AppHeader → MetricGrid → TaskCreateDialog → BatchCreateDialog → TaskDetailDrawer
9. 最终瘦身 App.vue

## 6. 不在此次范围内

- 不做暗色模式（P2 no dark mode — 独立任务）
- 不做路由库引入（hash 路由当前实现保留）
- 不做状态管理库引入（保持 reactive + composable 模式）
- 不改动 CSS（P2 normalize 已完成）
- 不改动 i18n、api.js、locales
