# console/src/lib

Console 的 HTTP 客户端和共用小工具。

## Files

| File | Responsibility |
|------|----------------|
| `api.ts` | 请求 `/api/*`。`Task.desired_run` 是 `RUN` 或 `STOP`，与任务 JSON 一致 |

## Interfaces

- `Task`：任务列表、详情和 dashboard 行里的任务抄本。

## Dependencies

- Upstream: BinlogServer HTTP API。
- Downstream: `src/features/*`。

## Update Rule

- 响应字段变化时更新本文件。
