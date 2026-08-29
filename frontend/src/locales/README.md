# frontend/src/locales Module

前端多语言文案资源，为指标、状态、source 聚合和操作界面提供可切换文本。

## Files

| File | Responsibility |
|------|---------------|
| `index.js` | 创建 i18n 实例并管理当前语言 |
| `zh-CN.json` | 中文文案，包括启动中、租约降级、文件重建与 source starting 计数文本 |
| `en.json` | 英文文案，包括 Starting Tasks、Lease Degraded、Rebuilding File 与 source Starting 文本 |

## Interfaces

- `setLocale(locale)` / `getLocale()`：语言设置接口。
- `metrics.starting` 与 `source.stats`：首屏 starting 指标和 source 状态聚合文案键。

## Dependencies

- Upstream: `frontend/src/App.vue`、`frontend/src/components/*`。
- Downstream: `vue-i18n` 和 Element Plus locale。

## Update Rule

- 文案键、语言资源或状态显示语义变化时，更新本文件。
