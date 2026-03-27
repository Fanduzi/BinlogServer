# Frontend Dev Mock Task Prompt

本文件提供可直接分发给执行者的任务提示词。  
对应设计文档：`docs/develop/plans/2026-03-26-frontend-dev-mock-design.md`  
对应实施计划：`docs/develop/plans/2026-03-26-frontend-dev-mock-implementation.md`

---

## 通用调度头

```text
你在仓库 /Users/fan/GolangProjects/BinlogServer 工作。
本次任务只实现“前端开发态 mock 接入 Vite dev”，不要顺手做页面改版、后端 mock、MSW 引入或 cluster e2e 扩展。

必须遵守仓库约束：
1) 使用三层文档协议；若改动职责边界或使用方式，更新相应 README/AGENTS 与文件头注释。
2) 仅在显式 `VITE_USE_MOCK=true` 时启用开发态 mock；默认行为保持真实 API 联调。
3) 不在页面中添加 mock 场景切换器。
4) 不引入新的 mock 框架。
5) 优先复用现有 Playwright fixtures，不允许长期维护两套 mock 数据。
6) 所有代码编辑前遵循 TDD：先写失败测试，再实现最小代码。
7) 完成前必须给出验证证据；不要口头声称“应该可以”。

参考文档：
- docs/develop/plans/2026-03-26-frontend-dev-mock-design.md
- docs/develop/plans/2026-03-26-frontend-dev-mock-implementation.md
```

---

## 主执行 Prompt

```text
请实现“前端开发态 mock 接入 Vite dev”，目标是在 frontend 本地开发时通过环境变量开关直接使用 mock 数据，而不依赖真实后端。

严格遵循以下边界：
1) 只在 `VITE_USE_MOCK=true` 时启用 mock。
2) `VITE_MOCK_SCENARIO` 控制场景；未设置时默认 `healthy`。
3) 不改 `App.vue` 的 API 调用方式，不把 mock 判断逻辑散落到 UI 层。
4) 不增加页面内 mock toggle UI。
5) 不引入 MSW 或其他新框架。
6) 必须复用并共享现有 Playwright mock 数据与路由语义。

实现要求：
1) 将测试目录中现有 mock 数据抽到可复用的共享模块，建议放到 `frontend/src/mocks/`。
2) 抽出纯 request handler，输入至少包含：
   - scenario
   - method
   - path
   - query
   - body
3) Playwright `mock-routes.ts` 改为薄适配层，调用共享 handler，而不是继续维护独立分发逻辑。
4) `frontend/src/api.js` 在 mock 模式下改走共享 handler，在正常模式下仍走 Axios。
5) 保持 `auth-required` 的 401 行为与现有认证引导逻辑一致。
6) 支持 `upload-failed` 场景下 `retry-upload` 前后文件状态变化。
7) 对未覆盖接口显式报错，不允许静默返回空值。

测试要求：
1) 先写失败测试，至少覆盖：
   - `healthy` 场景下 `GET /api/dashboard`
   - mock 模式下 `getDashboard()` 不依赖真实后端
   - `auth-required` 场景保留 401 语义
   - `upload-failed` 场景下 `retry-upload` 状态变化
2) 跑现有 `frontend` Playwright E2E，确认没有回归。
3) 跑 `frontend` build，确认生产构建未被破坏。
4) 给出本地 dev mock 启动命令与结果摘要。

交付要求：
1) 列出实际改动文件。
2) 列出执行过的测试命令与结果。
3) 如有未完成项或风险，明确写出。
4) 不要提交与本任务无关的重构。
```

---

## Reviewer Prompt

```text
请 review 本次“frontend dev mock”改动，重点看以下方面：

1) 是否真正复用了共享 mock 数据，而不是复制出第二套。
2) `frontend/src/api.js` 的 mock 接入是否保持了默认真实 API 行为不变。
3) Playwright route 层是否已变成薄适配，而不是继续承载业务分发。
4) `auth-required` 的 401 语义是否仍能触发现有认证引导。
5) `upload-failed` 的 retry-upload 状态切换是否可重复验证。
6) 是否存在把 mock 条件散落进 `App.vue` 或其他 UI 组件的坏味道。
7) 文档是否补充了 mock 启动方式和场景说明。

若发现问题，优先指出：
- 行为回归
- 开发态与测试态漂移风险
- 未覆盖接口静默失败
- 生产行为被 mock 污染
```
