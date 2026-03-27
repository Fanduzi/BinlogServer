# 前端开发态 Mock 接入 Vite Dev 设计

## 1. 背景

当前前端在本地 `npm run dev` 启动后，默认通过 Vite proxy 访问真实后端 `/api/*`。这对联调是合理的，但对纯前端开发、交互评审、空态/异常态演示并不友好：如果本地没有完整启动 control-plane、worker、meta、source MySQL，页面通常处于“无数据”或“请求失败”状态。

仓库已经具备一套前端 E2E mock 基础：

- `frontend/tests/e2e/fixtures/mock-data.ts`：定义 `empty`、`healthy`、`anomaly`、`upload-failed`、`auth-required` 等场景数据。
- `frontend/tests/e2e/fixtures/mock-routes.ts`：通过 Playwright `page.route()` 拦截 `/api/*`，返回场景化响应。

问题不在于“是否已有 mock 数据”，而在于这套 mock 目前只服务测试，没有进入日常开发入口。因此需要补上开发态 mock 接入能力，但不能因此让测试 mock 与开发 mock 长期分叉。

## 2. 目标

本次设计目标如下：

1. 让前端在本地开发时无需真实后端即可稳定展示页面与交互。
2. 复用现有 Playwright mock 数据，避免出现“测试一套、开发一套”的漂移。
3. 通过显式环境变量开关启用 mock，默认行为仍保持真实 API 联调。
4. 不修改现有 `App.vue` 对 API helper 的调用方式，减少 UI 层改动。
5. 为后续扩展 cluster 异常场景、前端交互回归测试提供统一 mock 基础设施。

## 3. 不做什么

本次设计明确不包含以下内容：

- 不引入新的 mock 框架，如 MSW、Mirage、json-server。
- 不在页面内新增开发者 mock 切换面板。
- 不重构现有前端页面结构或视觉设计。
- 不试图用开发态 mock 替代真实后端 E2E。
- 不修改生产构建产物行为，生产与默认开发模式仍走真实 `/api/*`。

## 4. 需求边界

### 4.1 启用方式

使用显式环境变量控制：

- `VITE_USE_MOCK=true|false`
- `VITE_MOCK_SCENARIO=<scenario>`

第一版默认约定：

- `VITE_USE_MOCK` 未设置或为 `false`：继续走真实后端 API
- `VITE_USE_MOCK=true` 且未指定场景：默认使用 `healthy`

### 4.2 场景范围

第一版直接复用现有 E2E mock 场景：

- `empty`
- `healthy`
- `anomaly`
- `upload-failed`
- `auth-required`

### 4.3 接口覆盖范围

第一版只覆盖当前前端已调用且开发态会影响主流程的接口：

- `GET /api/dashboard`
- `GET /api/cluster/overview`
- `GET /api/workers`
- `GET /api/sources/lookup`
- `GET /api/tasks/:id`
- `POST /api/tasks`
- `PUT /api/tasks/:id`
- `DELETE /api/tasks/:id`
- `POST /api/tasks/:id/start`
- `POST /api/tasks/:id/stop`
- `GET /api/tasks/:id/checkpoint`
- `GET /api/tasks/:id/replication`
- `GET /api/tasks/:id/lease`
- `GET /api/tasks/:id/runs`
- `GET /api/tasks/:id/events`
- `GET /api/tasks/:id/files`
- `POST /api/tasks/:id/files/retry-upload`

对于未覆盖接口，mock 层应显式抛错，而不是静默返回空值。这样可以尽早暴露 UI 新增依赖是否忘记补 mock。

## 5. 方案对比

### 5.1 方案 A：新增独立 dev mock 数据与逻辑

做法：

- 在 `frontend/src/` 新建一套开发专用 mock 数据和分发逻辑。
- Playwright 继续使用测试目录中的 fixtures。

优点：

- 上手最快，几乎不需要抽象现有测试代码。

缺点：

- 开发态与测试态会维护两份数据模型。
- 新增字段或接口后极易只改其中一套。
- 长期成本最高，不适合当前仓库已经有 fixtures 的现状。

### 5.2 方案 B：抽取统一 mock handler，开发与测试共用

做法：

- 将场景数据与请求分发逻辑抽成“纯数据 + 纯 handler”。
- Playwright 层继续负责 route 拦截，但内部复用统一 handler。
- 开发态 `api.js` 在开关打开后直接调用统一 handler 返回结果。

优点：

- 单一数据源，开发和测试保持一致。
- handler 纯函数更容易测试，也更容易新增场景。
- 不污染 UI 调用层，复用价值高。

缺点：

- 需要先做一次轻量重构，把 `mock-routes.ts` 中的 Playwright 依赖抽离。

### 5.3 方案 C：接入 MSW

做法：

- 使用 Service Worker 或 Node 拦截请求。

优点：

- 社区成熟，浏览器与测试都可统一拦截网络层。

缺点：

- 对当前需求过重。
- 增加依赖和接入复杂度，与“先快速解决本地无数据”不匹配。
- 当前仓库已存在 fixtures，不应再重新铺一层基础设施。

## 6. 推荐方案

推荐采用方案 B：抽取统一 mock handler，开发与测试共用。

原因：

1. 它能直接复用现有 `mock-data.ts` 的场景资产。
2. 它不会把 mock 选择逻辑塞进 `App.vue`，可以保持 UI 层无感。
3. 它能同时服务开发态和 Playwright 测试，后续维护成本最低。
4. 它的改动规模仍然可控，属于轻量基础设施整理，不是大重构。

## 7. 模块设计

### 7.1 目录结构建议

建议把共享 mock 组织到 `frontend/src/mocks/`：

- `frontend/src/mocks/scenarios.js` 或 `scenarios.ts`
  - 统一导出 mock 场景数据
- `frontend/src/mocks/handler.js` 或 `handler.ts`
  - 统一导出纯 handler
- `frontend/src/mocks/dev-client.js` 或并入 `api.js`
  - 开发态 mock 调用适配

Playwright 测试目录中的 `mock-routes.ts` 改为薄适配层：

- 负责拦截浏览器请求
- 将 `method + url + body + scenario` 转交共享 handler
- 将 handler 结果转换为 `route.fulfill()`

### 7.2 handler 设计

统一 handler 输入建议为：

- `scenario`
- `method`
- `path`
- `query`
- `body`
- `state`（可选，用于支持 `retry-upload` 这类单次状态变化）

统一输出建议为：

- `status`
- `body`

必要时可支持最小可变状态，例如：

- `upload-failed` 场景在触发 `retry-upload` 后，`files` 返回值从 `filesBeforeRetry` 切换到 `filesAfterRetry`

第一版不需要完整模拟数据库或复杂状态机，只需要支持当前前端页面已经存在的最小交互闭环。

### 7.3 开发态接入点

开发态接入点建议放在 `frontend/src/api.js`，而不是页面组件中。

原因：

- 当前所有页面请求都已经收敛在 API helper 层。
- `App.vue` 无需知道 mock 是否开启。
- 401、异常、超时等语义更容易与现有 API 层保持一致。

接入方式建议为：

1. 初始化时读取 `import.meta.env.VITE_USE_MOCK`
2. 若开启，则 API helper 改走 mock adapter
3. 若未开启，则继续使用 axios 请求真实后端

### 7.4 场景选择策略

场景选择通过环境变量完成，不做页面内切换器。

原因：

- 这是开发基础设施，不是产品功能。
- 页面内切换器会引入额外 UI、状态和误发布风险。
- 当前目标只是让开发态“打开就有数据”，而不是做交互式 mock playground。

后续如果确实出现频繁在多个异常场景间切换的需求，再考虑新增仅 dev 可见的切换器。

## 8. 数据契约要求

共享 mock 数据必须与当前前端消费结构保持一致，尤其注意以下对象契约：

- dashboard summary
- task row 中 `task` 与 `replication` 的组合结构
- cluster overview
- worker list
- task detail / checkpoint / replication / lease / runs / events / files

如果未来真实后端 API 结构发生变化，应优先更新共享 mock 数据，再由测试与开发共同受益。

## 9. 错误与兼容策略

### 9.1 401 语义

`auth-required` 场景应继续模拟真实 401，以便复用现有 `api.js` 中的认证失效处理逻辑。不能为了“开发方便”把它伪装成正常 200，否则会丢失对设置引导流程的覆盖。

### 9.2 未覆盖接口

handler 对未覆盖接口返回明确错误，例如：

- `500` + `unmocked api request: METHOD /path`

这样可以让新增接口在开发和测试中都尽快暴露，而不是悄悄回退成空白页面。

### 9.3 默认行为

只有 `VITE_USE_MOCK=true` 才启用 mock。默认开发和生产行为保持不变，避免影响已有联调和嵌入式 UI 构建流程。

## 10. 测试与验证策略

本次能力应分三层验证：

### 10.1 共享 handler 级

验证 handler 对关键场景和路径返回正确响应：

- `healthy` 首页数据
- `empty` 空状态
- `auth-required` 的 401
- `upload-failed` 下 `retry-upload` 前后文件状态变化

### 10.2 Playwright E2E 级

现有前端 E2E 继续跑通，重点验证：

- 空态
- 详情抽屉
- 401 引导
- retry-upload

这里的价值不只是“测试没坏”，还要证明新抽取的共享 handler 没有破坏既有 fixtures 行为。

### 10.3 本地开发验证

通过显式环境变量启动 dev server：

```bash
cd frontend
VITE_USE_MOCK=true VITE_MOCK_SCENARIO=healthy npm run dev
```

需要人工确认：

- 首页有数据
- worker 和 cluster 卡片有内容
- 点击任务详情可打开抽屉
- `auth-required` 场景会触发现有认证引导

## 11. 风险与缓解

### 11.1 风险：测试 mock 与开发 mock 行为不一致

缓解：

- 统一使用共享 handler
- Playwright route 层只做适配，不再承载业务分发

### 11.2 风险：mock 逻辑侵入 UI 层

缓解：

- 将判断与切换集中在 `api.js`
- 不在 `App.vue` 增加 `if (mock)` 分支

### 11.3 风险：引入过多状态模拟

缓解：

- 仅支持当前页面所需的最小状态变化
- 不做完整 CRUD 后台模拟
- 超出范围的接口直接报错

## 12. 结果预期

完成后，前端开发将拥有两种明确工作模式：

1. 真实联调模式：默认 `npm run dev`，继续通过 Vite proxy 访问真实后端
2. 开发态 mock 模式：显式打开 `VITE_USE_MOCK=true`，用统一场景数据支撑 UI 开发与演示

这能直接解决“本地打开前端没有数据”的开发痛点，同时不削弱后端 cluster E2E 的必要性。开发态 mock 负责提升前端开发效率，真实 shell E2E 继续负责验证 control-plane、worker、lease、checkpoint 等系统级行为。
