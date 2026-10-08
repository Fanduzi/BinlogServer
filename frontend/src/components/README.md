# frontend/src/components Module

前端 Vue 展示组件，负责把 dashboard 状态数据渲染为首屏指标和操作视图。

## Files

| File | Responsibility |
|------|---------------|
| `MetricGrid.vue` | 渲染任务指标卡；`starting` 与 `running` 使用不同字段和 quick-filter 映射 |
| `NavPane.vue` | 多工作区导航与数量徽标 |
| `TaskDetailDrawer.vue` | 任务详情、状态和操作入口。`storage_alert` 出现时显示 `STORAGE_INCONSISTENT` 和恢复说明。`pending_dump_cleanup` 有连接号时显示源库 Binlog Dump 仍可能开着、可达后会 KILL 的警告。`process_local` 为 true 时补一句：只有当时那个 worker 进程看得到，重启后消失。基础信息显示配置起点（`LATEST` / `FILE_POS file:pos` / `GTID gtid_set`）和续传 `file:pos`（checkpoint 的 `gtid_set` 在 file+pos 一致时附上），以及有效本地保留和桶保留。遗留目录显示「认领」，目录任务显示「编辑」。文件区先显示可恢复窗口（`earliest` → `latest`，MySQL 时带 `gtid_set`）。`breaks` 非空时用警告列出每个缺口的文件和原因。接着显示回放命令并复制 `replay.paths`，旁边发出 `download-replay`。`location` 为 `bucket` 时提示先下载再交给 mysqlbinlog。定点恢复填写 UTC 停止时间、可选的停止 GTID `stop_gtid`、可选开始时间，以及已执行 GTID `start_gtid_set`，调用 `loadPitr` 后复制返回的 `command`，并发出 `download-pitr`。停止时间和停止 GTID 都填时两个参数一起送出。已执行 GTID 和开始时间都填时两个参数一起送出。`command` 为空且响应带 `note` 时显示这句说明。文件表每一行发出 `download-file`，名字是该行磁盘文件名，并显示 `location`、`checksum`（`match` / `mismatch`）和「服务器」列（第几台、完整 `source_identity`、是否当前）。`source_chain.outcome` 为 `continued` 且任务仍是 `RUNNING` 时显示仍在复制。`outcome` 为 `stopped` 或 `last_error` 以 `SOURCE_SWITCHOVER` 开头时显示原因和下一步 |
| `AppHeader.vue` | 页面标题与刷新/创建/设置操作 |
| `AlertBanner.vue` | 顶层认证/告警提示 |
| `TaskCreateDialog.vue` / `BatchCreateDialog.vue` | 单任务、目录任务编辑与遗留目录认领表单。Flavor 是 `mysql` / `mariadb` 下拉，默认 mysql；MariaDB 源必须选 mariadb。认领的默认起点不传 `start`，不会启动复制 |
| `SettingsDialog.vue` | 前端设置与语言切换 |

## Interfaces

- `MetricGrid` props：`summary`、`activeQuickFilter`；emits：`filter(kind)`。
- 组件通过 Vue props/emits 与 `App.vue` 交互，不直接请求后端。

## Dependencies

- Upstream: `frontend/src/App.vue` 与 `frontend/src/composables/*`。
- Downstream: Vue、Element Plus、i18n 文案。

## Update Rule

- 指标字段、状态卡、组件 props/emits 或跨组件职责变化时，更新本文件。
