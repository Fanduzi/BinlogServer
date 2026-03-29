# Frontend Distill Implementation Plan

> **Design doc:** 2026-03-29-frontend-distill-design.md
> **目标：** 把 App.vue (3160 行) 拆成 composables + 子组件，功能零变更，全程保持构建通过

---

## Implementation Notes

- 工作目录：`frontend/src/`
- 每步完成后运行 `npm run build` 验证无编译错误
- 每步完成后运行 Playwright E2E smoke（`npm run test:e2e`）验证功能
- 每步对应一个 git commit，保持 history 可 bisect
- 不改动 `api.js`、`locales/`、`utils/`、`main.js`、`mocks/`

---

## Step 1: 创建目录结构

创建空目录（通过占位文件）：
- `frontend/src/composables/.gitkeep`
- `frontend/src/components/.gitkeep`

**Commit:** `chore(frontend): scaffold composables/ and components/ dirs`

---

## Step 2: 提取 useWindowState

**文件：** `frontend/src/composables/useWindowState.js`

**提取内容：**
- `windowWidth` ref
- `isMobile` computed
- `nowRef` ref + 1s interval ticker
- `nowRefMs` computed
- `menuCollapsed` ref
- `onMounted` / `onBeforeUnmount` 中的 resize 监听器

**接口：**
```js
export function useWindowState() {
  return { windowWidth, isMobile, nowRef, nowRefMs, menuCollapsed };
}
```

App.vue 中替换为 `const { windowWidth, isMobile, nowRef, nowRefMs, menuCollapsed } = useWindowState()`

**验证：** build + 页面 resize 行为正常，mobile 布局切换正常

**Commit:** `refactor(frontend): extract useWindowState composable`

---

## Step 3: 提取 useAuth

**文件：** `frontend/src/composables/useAuth.js`

**提取内容：**
- `settingsVisible` ref
- `settingsToken` ref
- `authRequiredMessage` ref
- `authRequiredTitle` computed
- `handleAuthRequired()` function
- `openSettings()` function
- `saveSettings()` function
- `onMounted` / `onBeforeUnmount` 中的 `auth-required` 事件监听

**依赖注入：** 需要接收 `refreshAll` 回调作为参数

```js
export function useAuth({ onSaved }) {
  // ...
  function saveSettings() {
    // ...
    onSaved();
  }
  return { settingsVisible, settingsToken, authRequiredMessage, authRequiredTitle, openSettings, saveSettings };
}
```

**Commit:** `refactor(frontend): extract useAuth composable`

---

## Step 4: 提取 useSourceLookup

**文件：** `frontend/src/composables/useSourceLookup.js`

**提取内容：**
- `sourceQuery` reactive
- `lookup` reactive
- `querySource()` function
- `clearLookup()` function

**Commit:** `refactor(frontend): extract useSourceLookup composable`

---

## Step 5: 提取 useDashboard

**文件：** `frontend/src/composables/useDashboard.js`

**提取内容：**
- `dashboard` reactive
- `cluster` reactive
- `loading` ref
- `applyDashboardData()` function
- `applyClusterData()` function
- `prefetchTaskLeasesForPage()` function (依赖 pagedTasks — 通过参数传入)
- `refreshAll()` function
- `buildSourceFilter()` function (依赖 sourceQuery — 通过参数传入)

**接口：**
```js
export function useDashboard({ getSourceFilter, getPagedTaskIds }) {
  // ...
  return { dashboard, cluster, loading, refreshAll };
}
```

**Commit:** `refactor(frontend): extract useDashboard composable`

---

## Step 6: 提取 useTaskFilter

**文件：** `frontend/src/composables/useTaskFilter.js`

**提取内容：**
- `uiFilter` reactive
- `debouncedKeyword` / `debouncedSourceKeyword` refs + watchers
- `pager` reactive
- `filteredTasks` computed (依赖 dashboard.tasks)
- `pagedTasks` computed
- `activeQuickFilter` ref
- `applyQuickFilter()` function
- `resetUiFilter()` function
- pager boundary watch
- `taskStates` / `replicationStatuses` 常量

**接口：**
```js
export function useTaskFilter({ tasks, activeView, onViewChange }) {
  return { uiFilter, pager, filteredTasks, pagedTasks, activeQuickFilter, applyQuickFilter, resetUiFilter };
}
```

**Commit:** `refactor(frontend): extract useTaskFilter composable`

---

## Step 7: 提取 useTaskDetail

**文件：** `frontend/src/composables/useTaskDetail.js`

**提取内容：**
- `detailVisible` ref
- `detailTask` ref
- `detailReplication` / `detailLease` / `detailRuns` / `checkpoint` / `events` / `files` refs
- `runHistoryLimit` ref
- `detailRunsLimited` computed
- `showDetail()` async function
- `retryFailedUploads()` function
- `onRowClick()` / `onTableKeyEnter()` functions
- pagedTasks watcher for lease prefetch (移至 useDashboard)

**Commit:** `refactor(frontend): extract useTaskDetail composable`

---

## Step 8: 提取 useTaskForm

**文件：** `frontend/src/composables/useTaskForm.js`

**提取内容：**
- `formVisible` ref
- `formMode` ref
- `form` reactive + `defaultForm()`
- `openCreate()` / `openEdit()` functions
- `submitForm()` async function
- `confirmDelete()` async function
- `confirmStop()` / `confirmStart()` async functions
- `validatePayload()` validation function
- 所有校验常量 (`NAME_MAX_LENGTH` 等)

**Commit:** `refactor(frontend): extract useTaskForm composable`

---

## Step 9: 提取 useBatchCreate

**文件：** `frontend/src/composables/useBatchCreate.js`

**提取内容：**
- `batchVisible` ref
- `batchForm` reactive + `defaultBatchForm()`
- `batchPreview` reactive
- `openBatchCreate()` / `clearBatchPreview()` functions
- `previewBatch()` / `submitBatchCreate()` async functions
- `parseBatchLines()` / `validateBatchPayload()` functions
- batch form watcher

**Commit:** `refactor(frontend): extract useBatchCreate composable`

---

## Step 10: 提取子组件

每个子组件单独一个 commit：

### AlertBanner.vue
- props: `title`, `message`
- emit: none

### SettingsDialog.vue
- props: `visible`, `token`, `locale`, `hint`
- emit: `update:visible`, `update:token`, `save`, `locale-change`

### AppHeader.vue
- props: `loading`, `isMobile`
- emit: `create`, `batch-create`, `refresh`, `settings`

### MetricGrid.vue
- props: `summary`, `activeQuickFilter`
- emit: `filter`

### TaskDetailDrawer.vue
- props: `visible`, `task`, `replication`, `lease`, `runs`, `checkpoint`, `events`, `files`, `isMobile`, `runHistoryLimit`
- emit: `update:visible`, `edit`, `start`, `stop`, `delete`, `retry-upload`

### TaskCreateDialog.vue
- props: `visible`, `mode`, `form`, `sources`, `isMobile`
- emit: `update:visible`, `submit`

### BatchCreateDialog.vue
- props: `visible`, `form`, `preview`, `isMobile`
- emit: `update:visible`, `preview`, `submit`, `clear-preview`

**Commits:** `refactor(frontend): extract <ComponentName> component` (一个 commit per component)

---

## Step 11: 最终 App.vue 瘦身

App.vue 只保留：
- `<el-config-provider>` shell
- decorative orbs
- `<AppHeader>` + `<AlertBanner>`
- metric grid area → `<MetricGrid>`
- nav pane (left sidebar nav items — 约 80 行 template，逻辑简单保留)
- workspace panel 切换 (v-if 分支)
- filter panel + task table + pagination (约 150 行 template)
- 所有 dialog/drawer → 子组件
- `<script setup>`: 仅 composable 实例化 + wiring (~100 行)

**Commit:** `refactor(frontend): slim App.vue to shell + composable wiring`

---

## Step 12: 最终验证

```bash
npm run build
npm run test:e2e
```

验证：
- 构建无错误
- task list 加载正常
- filter / quick filter 正常
- detail drawer 打开正常
- create / edit / delete 正常
- settings 保存正常
- locale 切换正常
- mobile 布局正常

**Commit:** `test(frontend): e2e smoke pass after distill`

---

## 文件变更范围

| 文件 | 动作 |
|---|---|
| `frontend/src/App.vue` | 大幅精简（保留 shell + wiring） |
| `frontend/src/composables/use*.js` | 新增 8 个文件 |
| `frontend/src/components/*.vue` | 新增 7 个文件 |
| 其他所有文件 | 不变 |
