# frontend/src/mocks Module

前端共享 mock 模块目录，供 Vite dev 开发态和 Playwright E2E 复用。

## Files

| File | Responsibility |
|------|---------------|
| mock-data.js | 定义共享 mock 场景数据（含 starting 状态场景）；任务抄本带 owner/epoch，供列表租约风险使用 |
| mock-handler.js | 将 API method/path/query/body 分发到对应 mock 场景，模拟 server pagination/filter、批量任务创建结果，并维护最小状态变化 |

## Exports

- 共享 mock 场景数据（含 `pagination`）
- 共享 mock request handler / session factory
- `/api/tasks/batch` mock：校验 1..100 envelope，返回有序 `{index,cluster_key,task|error}`，并脱敏成功任务密码。
- Dashboard summary/source response 中按任务状态分别生成 `starting` 与 `running`，并按全量过滤结果返回 `total/limit/offset`；任务页按数字 id 升序（非数字 id 排在数字之后），pagination 场景覆盖后页当前页匹配，limit 超过 500 返回 400。
- lookup 与 dashboard 的 host 过滤共用同一套源身份：回环别名与 Go `SameSourceHost` 同一 accept/reject 集（含展开 IPv6 `::1`，拒绝 `127.000.0.1` / `[127.0.0.1]`），非回环仍精确匹配。任务观测断言走 dashboard 的 total/summary/source counts，不再把 `/api/tasks` 列表当观测。

## Dependencies

- Upstream: `frontend/src/api.js`、`frontend/tests/e2e/fixtures/mock-routes.ts`
- Downstream: 无外部网络依赖，仅向调用方返回内存中的 mock 响应

## Update Rule

- 场景数据、请求分发规则、最小状态模拟边界或源文件头声明变化时，更新本文件。
