# Ops Console Workspace C Task Prompt

本文件用于分发执行任务，目标是按 C 方案重构前端管理平台的信息架构与页面工作流。

对应设计文档：`docs/develop/plans/2026-03-27-ops-console-workspace-c-design.md`  
对应实施计划：`docs/develop/plans/2026-03-27-ops-console-workspace-c-implementation.md`

---

## 通用调度头

```text
你在仓库 /Users/fan/GolangProjects/BinlogServer 工作。
本次任务是“前端运维控制台 C 方案工作区重构”。

硬约束：
1) 先按信息架构分区，再做样式细节，不得只做皮肤调整。
2) 左侧导航必须为整列骨架，可折叠，折叠按钮位于左下。
3) 一级页面必须可 URL 深链：/#/overview /#/tasks /#/sources /#/workers /#/alerts。
4) 运维筛选只在 tasks，源库反查只在 sources。
5) 不抄 vben/chatgpt 视觉样式，只参考交互结构与布局语义。
6) 保持 API 契约不变；不要引入新 UI 框架。
7) 完成后必须给出构建与 E2E 证据。

参考：
- docs/develop/plans/2026-03-27-ops-console-workspace-c-design.md
- docs/develop/plans/2026-03-27-ops-console-workspace-c-implementation.md
```

---

## 主执行 Prompt

```text
请实现“Ops Console Workspace C 方案”：

目标：
1) 将页面重构为 5 个工作区：overview/tasks/sources/workers/alerts。
2) 工具按页面归属：运维筛选仅 tasks，源库反查仅 sources。
3) 左侧导航为整列且可折叠，折叠按钮放在左下。
4) URL 可深链并可分享。
5) 修复 Lease 风险在 mock 场景下全部显示“风险”的问题（优先以 dashboard.generated_at 作为参考时间）。

实施要求：
1) 优先改 App.vue 的信息架构分支，不要先做视觉微调。
2) KPI 行为必须一致，不允许出现某个 KPI 特例跳转。
3) overview 页面只放摘要信息，不承载重操作表格。
4) tasks 页面默认全量列表；alerts 页面默认告警聚焦。
5) 导航项密度应紧凑，避免异常大行高（注意 grid 行拉伸问题）。

测试要求：
1) 至少覆盖：导航切换、URL 深链、任务筛选行为、告警聚焦行为。
2) 执行 frontend build 并给出结果。
3) 给出实际访问地址与验证建议（含强刷说明）。

交付要求：
1) 列出改动文件。
2) 列出执行命令与结果摘要。
3) 列出已知风险与后续优化点。
```

---

## Reviewer Prompt

```text
请 review 本次 Workspace C 改动，重点关注：

1) 是否真正做了信息架构分区，而不是仅换皮。
2) 工具归属是否正确（筛选只在 tasks，反查只在 sources）。
3) 导航是否为整列骨架，折叠按钮是否位于左下。
4) URL 深链是否可稳定直达页面语义。
5) KPI 跳转行为是否一致。
6) Lease 风险判定是否摆脱本地当前时间偏置。
7) 是否补齐关键 E2E 和文档更新。
```

