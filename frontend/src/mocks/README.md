# frontend/src/mocks Module

前端共享 mock 模块目录，供 Vite dev 开发态和 Playwright E2E 复用。

## Files

| File | Responsibility |
|------|---------------|
| mock-data.js | 定义共享 mock 场景数据（含 starting、single-process、disk-leftover 与 resume-identity 场景）；任务抄本带 owner/epoch，供列表租约风险使用。disk-leftover 里 id `4` 没有 source，id `5` 是目录任务。resume-identity 里停止任务分别带 LATEST、FILE_POS 和 GTID 起点，以及带 `gtid_set` 的续传位点。healthy 的文件表同时有 `mysql-bin.000002` 封存名和 `.open.e1` / `.open.e4` |
| mock-handler.js | 将 API method/path/query/body 分发到对应 mock 场景，模拟 server pagination/filter、批量任务创建结果，并维护最小状态变化；仅当 worker 列表为空且唯一主人是 standalone 时 overview `single_process` 为 true。`GET /api/tasks/{id}/window` 对已有任务返回连续窗口（`earliest`/`latest`，mysql 带 `gtid_set`，`breaks` 为 `[]`），任务不存在是 404。`GET /api/tasks/{id}/replay` 在文件窗口里每个序号留一条路径，并按 flavor 给出客户端。带 `stop_datetime` 时用 healthy 清单里固定的 UTC 事件跨度选出路径和 `command`；无法解析的时间是 400 纯文本。带 `stop_gtid` 时，夹具 `3e11fa47-71ca-11e1-9e33-c80aa9429562:8` 返回 `--stop-position=154`；无法解析、不在这份夹具里、非 mysql flavor、以及和 `stop_datetime` 同时出现，都是对应的 400 纯文本。带 `start_gtid_set` 时，`:1-2` 丢掉 `mysql-bin.000001` 并返回 `--exclude-gtids` 与 `--stop-position=154`，`:1-100` 返回空命令和 note，`:1-1` 是缺口 400。`GET /api/tasks/{id}/replay/archive` 用同一窗口返回这些 basename，`Content-Type` 是 `application/x-tar`，文件名是 `task-{id}-replay.tar`；任务不存在是 404。`GET /api/tasks/{id}/files/{name}` 在同一文件清单里按磁盘文件名返回一段字节；`/`、`\`、`..` 是 400，任务或名字不在清单里是 404。`POST /api/tasks/{id}/adopt` 把 source 接到遗留 id，密码脱敏，状态保持 `STOPPED`；没传 `start.mode` 时起点是 `FILE_POS`。遗留行的 `PUT` 和 start 返回 400 |

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
