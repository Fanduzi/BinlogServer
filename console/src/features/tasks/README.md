# console/src/features/tasks

任务列表页：指标卡、筛选和任务表。

## Files

| File | Responsibility |
|------|----------------|
| `tasks-page.tsx` | 任务表。状态列旁边有期望列，显示 `desired_run`（`RUN` 或 `STOP`），`data-testid` 是 `task-desired-<id>` |
| `tasks.test.ts` | 筛选和指标的单元测试 |
| `filter.ts` / `filter-context.tsx` | 当前页筛选 |
| `format.ts` | 列表文案 |
| `metric-grid.tsx` | 指标卡 |
| `sync.tsx` | 刷新 |
| `tone-badge.tsx` | 状态颜色 |

## Interfaces

- 任务行来自 dashboard 的 `task`。`desired_run` 与 `GET /api/tasks` 的同名字段一致。

## Dependencies

- Upstream: `/api/dashboard` 和 `src/locales` 的 `table.desiredRun`。
- Downstream: 任务详情路由。

## Update Rule

- 列表列变化时更新本文件。
