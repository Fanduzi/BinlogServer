# frontend/tests/e2e/fixtures Module

Playwright 测试夹具，提供共享 mock 场景类型和 API 路由适配。

## Files

| File | Responsibility |
|------|---------------|
| `mock-data.ts` | 暴露共享场景的 TypeScript union 与测试类型 |
| `mock-routes.ts` | 将 Page 的 `/api/**` 请求转发给共享 mock session |

## Interfaces

- `MockScenario`：包含 `starting`、`pagination` 场景的场景名合同。
- `registerMockRoutes(page, options)`：安装可配置的 mock route。

## Dependencies

- Upstream: `frontend/src/mocks/mock-data.js`、`frontend/src/mocks/mock-handler.js`。
- Downstream: 各 Playwright E2E spec 与本地 Vite 页面。

## Update Rule

- 场景 union、路由拦截、测试夹具参数或源文件头声明变化时，更新本文件。
