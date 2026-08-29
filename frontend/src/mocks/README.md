# frontend/src/mocks Module

前端共享 mock 模块目录，供 Vite dev 开发态和 Playwright E2E 复用。

## Files

| File | Responsibility |
|------|---------------|
| mock-data.js | 定义共享 mock 场景数据（含 starting 状态场景） |
| mock-handler.js | 将 API method/path/query/body 分发到对应 mock 场景，模拟 server pagination/filter，并维护最小状态变化 |

## Exports

- 共享 mock 场景数据（含 `pagination`）
- 共享 mock request handler / session factory
- Dashboard summary/source response 中按任务状态分别生成 `starting` 与 `running`，并按全量过滤结果返回 `total/limit/offset`；pagination 场景覆盖后页当前页匹配，limit 超过 500 返回 400。

## Dependencies

- Upstream: `frontend/src/api.js`、`frontend/tests/e2e/fixtures/mock-routes.ts`
- Downstream: 无外部网络依赖，仅向调用方返回内存中的 mock 响应

## Update Rule

- 场景数据、请求分发规则、最小状态模拟边界或源文件头声明变化时，更新本文件。
