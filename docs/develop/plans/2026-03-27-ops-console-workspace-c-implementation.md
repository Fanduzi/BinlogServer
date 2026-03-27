# Ops Console Workspace C 方案实施计划

> 执行目标：将当前运维台重构为工作区模型（Overview / Tasks / Sources / Workers / Alerts），完成导航骨架、页面职责拆分与深链语义统一。

## 1. 变更范围

### 1.1 主要文件

- 修改：`frontend/src/App.vue`
- 可能修改：`frontend/src/README.md`
- 可能修改：`frontend/README.md`
- 新增或修改测试：`frontend/tests/e2e/*.spec.ts`

### 1.2 不变边界

- `frontend/src/api.js` 仍为 API 聚合层，不向页面外泄实现细节。
- 不引入新 UI 框架。
- 不改变后端接口契约。

## 2. 分阶段任务

## Phase 1：工作区骨架与导航收敛

- [ ] 将当前页面拆分为 5 个工作区渲染分支（overview/tasks/sources/workers/alerts）。
- [ ] 左侧导航改为整列布局，折叠按钮移至左下固定区域。
- [ ] 保持 hash 深链可用（`/#/overview` 等）。
- [ ] 修复导航项行高与间距异常（防止 grid 拉伸）。

验收：

- [ ] 导航展开/折叠可用，且不影响主内容区可读性。
- [ ] 所有一级工作区可通过 URL 直接访问。

## Phase 2：工具归属与页面职责拆分

- [ ] 运维筛选仅在 `tasks` 工作区显示。
- [ ] 源库反查仅在 `sources` 工作区显示。
- [ ] `overview` 不再承载重操作表格（仅摘要能力）。
- [ ] `alerts` 页面默认聚焦告警任务视图。

验收：

- [ ] 在非任务页面看不到任务筛选卡。
- [ ] 在非源库页面看不到源库反查卡。
- [ ] `总任务` KPI 点击进入任务全量列表，不回退到 overview。

## Phase 3：状态一致性与风险修正

- [ ] 统一 KPI 点击行为（全部映射到合理页面语义）。
- [ ] Lease 风险判定基准优先 `dashboard.generated_at`。
- [ ] 补充必要的页面模式 query（如 `/#/tasks?mode=alerts`）。

验收：

- [ ] KPI 行为无特例。
- [ ] mock 场景下 Lease 风险不再“全是风险”。

## Phase 4：测试与文档闭环

- [ ] 新增/更新 E2E：导航、深链、筛选归属、告警视图。
- [ ] `npm run build` 通过。
- [ ] 更新 `frontend/README.md` 与 `frontend/src/README.md`。

验收：

- [ ] 关键 E2E 用例通过。
- [ ] 文档反映最新 IA 和 URL 语义。

## 3. 推荐执行顺序（命令）

```bash
cd frontend
npm run build
PLAYWRIGHT_BASE_URL=http://127.0.0.1:4177 npx playwright test tests/e2e/view-navigation.spec.ts --workers=1
PLAYWRIGHT_BASE_URL=http://127.0.0.1:4177 npx playwright test tests/e2e/view-deeplink.spec.ts --workers=1
PLAYWRIGHT_BASE_URL=http://127.0.0.1:4177 npx playwright test tests/e2e/dashboard-filters.spec.ts --workers=1
```

## 4. 风险控制

- UI 结构改动大：优先做可验证的小步提交，避免一次性改完难回归。
- 旧样式缓存：每次验证前做强刷，必要时切换新端口验证。
- 多场景 mock 差异：优先覆盖 `anomaly`、`control-plane-down-worker-running`。

