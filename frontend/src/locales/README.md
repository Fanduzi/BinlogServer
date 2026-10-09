# frontend/src/locales Module

前端多语言文案资源，为指标、状态、source 聚合和操作界面提供可切换文本。

## Files

| File | Responsibility |
|------|---------------|
| `index.js` | 创建 i18n 实例并管理当前语言 |
| `check-messages.js` | 用 `@intlify/message-compiler` 编译全部文案；生产态解析 `form.flavorHint`，拒绝会让嵌入式 `/ui/` 丢控件的 `@` 语法 |
| `zh-CN.json` | 中文文案，包括启动中、租约降级、文件重建、全局/当前页筛选范围、source starting 计数、遗留目录「认领」，任务详情「起点」「续传」「复制回放命令」和「下载回放集」，源库 Binlog Dump 残留警告（`process_local` 时说明只有本进程看得到），文件表「下载」，以及 VIP 换主的源服务器、仍在复制、停止原因和下一步（含不要删除 `.source-chain`）；损坏分段告警的逐行中文（`detail.storageAlert*`）和回放排除提示（`detail.replayDamaged`） |
| `en.json` | 英文文案，包括 Starting Tasks、Lease Degraded、Rebuilding File、global/current-page filter scope、source Starting 文本、Flavor 的 mysql/mariadb 选项和 MariaDB 提示、leftover Adopt、Start、Resume、Copy replay、Download replay set，the pending Binlog Dump warning (and the process-local sentence), file Download, and the VIP source-switch chain, still-copying notice, stop reasons, and next step; the line-by-line damaged-segment alert (`detail.storageAlert*`) and the replay exclusion notice (`detail.replayDamaged`) |

## Interfaces

- `setLocale(locale)` / `getLocale()`：语言设置接口。
- `metrics.starting` 与 `source.stats`：首屏 starting 指标和 source 状态聚合文案键。
- `npm run test:locales`：编译 `zh-CN.json` / `en.json`。MySQL `@@var` 写成字面量 `{'@@server_uuid'}`，避免 vue-i18n 把 `@` 当成链接消息。`make ui-build` 与 CI 都会跑这条检查。

## Dependencies

- Upstream: `frontend/src/App.vue`、`frontend/src/components/*`。
- Downstream: `vue-i18n` 和 Element Plus locale。

## Update Rule

- 文案键、语言资源或状态显示语义变化时，更新本文件。
- 任务详情「起点」是配置的 mode，FILE_POS 带 file:pos，GTID 带 gtid_set。「续传」是下次 Start 的 file:pos，有 GTID 时附在后面。
