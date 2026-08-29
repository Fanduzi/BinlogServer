# frontend/tests/e2e Module

Playwright 端到端回归模块，验证运维控制台的首屏、导航、筛选与 mock API 合同。

## Files

| File | Responsibility |
|------|---------------|
| `starting-summary.spec.ts` | 锁定 STARTING 指标可见、与 RUNNING 分离、source 映射及缺少 starting 字段时回退 0 |
| `server-pagination.spec.ts` | 锁定 dashboard server page 请求、全局 total 和 state page transition |
| `dashboard-filters.spec.ts` | 指标卡筛选与键盘交互 |
| `dashboard-empty.spec.ts` | 空态和零指标 |
| `mock-handler.spec.ts` / `dev-mock-api.spec.ts` | 共享 mock/API helper 合同 |
| 其他 `*.spec.ts` | 详情、导航、lease、集群和上传重试场景 |
| `fixtures/` | 共享场景类型与路由拦截 |

## Interfaces

- `npm run test:e2e`：运行前端 Playwright 回归。
- `registerMockRoutes(page, options)`：为浏览器测试安装共享 mock API 路由。

## Dependencies

- Upstream: `frontend/src` 应用、mock handler 和 Playwright。
- Downstream: 本地 Vite dev server；不依赖真实 API 数据库。

## Update Rule

- 新增/删除回归场景、mock contract 或测试入口变化时，更新本文件。
