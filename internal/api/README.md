# internal/api Module

## Files
| File | Responsibility |
|------|---------------|
| `server.go` | HTTP server/router 组装、路由注册（含 `/healthz` 与 `/api/health`） |
| `auth.go` | 路由级鉴权配置与认证中间件、ServerOption 定义 |
| `rate_limiter.go` | 基于 IP 的令牌桶限流器 |
| `metrics_prometheus.go` | `/metrics` 采集与输出：一次 scrape 只读一份 `ListClusterObservation`，失败 5xx，不在 Collect 里再读一遍后记日志并吐空计数。`upload_retry` 计数包含后台对已封存失败文件的补传 |
| `tracing.go` | HTTP 入站 tracing middleware（OTel span） |
| `handlers_tasks.go` | 任务相关 API 处理（CRUD、批量创建、启动停止、`POST /api/tasks/{id}/adopt`、checkpoint 返回下次 Start 的 file/pos 与匹配时的 gtid_set、`GET /api/tasks/{id}/files/{name}` 分段字节（本地优先，缺失时可读已上传封存对象）、`GET /api/tasks/{id}/replay` 在带 `stop_datetime` 时返回 UTC 定点窗口和命令、`GET /api/tasks/{id}/replay/archive` 把同一回放窗口打成一个 ustar、source lookup 读集群观测同一份 store 抄本再 `SameSourceHost` 过滤、summary/dashboard 用状态与按源 GROUP BY 计数，任务行走 LIMIT/OFFSET） |
| `handlers_cluster.go` | 集群观测：overview / workers 任务计数读 `ListClusterObservation`（有 store 时全库所有权抄本，不是任务页过滤，也不是启动内存名单）。无心跳的进程内主人 `standalone` 不计入 `worker_count`，overview 置 `single_process` |
| `cluster_observation_test.go` | HTTP 缝测试：过滤后的 dashboard 汇总 ≠ 集群人数；store 主人/状态变化反映到 overview/workers/metrics；单机 + meta 的 overview `worker_count` 与 `/api/workers` 一致；lookup 读同一份 store 抄本；无 store 仍用内存名单；`/metrics` 一次 scrape 只读一份 store 抄本 |
| `gettask_fail_loud_test.go` | HTTP 缝测试：有 store 时 `GET /api/tasks/{id}` store 未找到 404、其它 store 错误 5xx，不退回内存旧主人/epoch 抄本；没有 store 仍读内存名单 |
| `swagger_docs_only.go` | swagger 注释占位 |

## Exports
- `NewServer(taskService, ...ServerOption) http.Handler` - 创建 API 服务器
- `WithAuth(AuthConfig) ServerOption` - 注入认证配置
- `WithTracing(TracingConfig) ServerOption` - 注入 tracing 配置
- `WithRateLimit(RateLimiterConfig) ServerOption` - 注入限流配置
- `GET /api/summary` - 返回兼容既有字段的任务计数；`starting` 单独统计 STARTING，`running` 仅统计 runner ready 后的 RUNNING。有 `TaskDashboardRollup` 时状态计数走 SQL `GROUP BY`，不把匹配行整表读入内存。
- `GET /api/dashboard` - 控制台任务观测唯一读取：返回同口径 summary（`starting` 与 `running` 独立）、任务明细与 source 聚合。状态计数与按源 `task_count`/`running`/`starting` 来自 `CountTaskStates` / `CountTasksBySource`；任务行是 `ListTasksPage` 的 LIMIT/OFFSET 页，复制进度只取该页。`normal`/`delayed` 与 RUNNING 的 `abnormal` 用 RUNNING id 引用分类，不加载整行。`total`、`summary.total` 与按源 `task_count` 仍是同一过滤集。无 rollup 的 store 仍用一次过滤读取做计数。没有 task store 时，这一页包含仍有分段的磁盘目录，Console 用现有任务表和文件表展示它们。
- `GET /api/sources/lookup` - 按 host/port 查任务 id。有 store 时读集群观测同一份 `ListClusterObservation`（`store.ListTasks`）抄本，再用 `SameSourceHost` 过滤；store 错误返回 5xx，不退回启动时的内存名单。没有 store 时仍读同一份名单，含仍有分段的磁盘目录。
- `GET /api/cluster/overview` / `GET /api/workers` / `GET /metrics` 的任务与主人计数共用 `ListClusterObservation`：有 store 时读 `store.ListTasks` 全库抄本，store 错误返回 5xx，不退回启动时的内存名单；没有 store 时仍读同一份名单，含仍有分段的磁盘目录。主人计数不把无 owner 的磁盘目录算进 worker。`/metrics` 一次 scrape 只读一份抄本。任务页 dashboard 过滤汇总不是集群人数。单机进程的主人 `standalone` 在没有心跳时不进入 overview 的 worker 列表，`worker_count` 为 0 且 `single_process` 为 true（与 `/api/workers` 的空列表同一人数）；该 id 一旦有心跳，overview 与 `/api/workers` 用同一条在线状态和 `last_seen_at`。
- `GET /api/tasks/{id}` - 按 id 读单个任务。有 store 时 store 未找到返回 404，其它 store 错误返回 5xx，不把内存里的旧主人/epoch 抄本当成 200，也不改扫磁盘。没有 store 时先读内存名单；没有该 id 但 `{data_dir}/{id}` 仍有分段时返回只读身份（`STOPPED`，无 source）。
- `GET /api/tasks` - 返回 `{items,total,limit,offset}` 任务页，不是控制台任务观测来源。页序为数字 id 升序；支持 host/port/state 过滤，host 与 lookup/dashboard 共用 `SameSourceHost`；cluster/mysql 走 `ListTasksPage`（COUNT + `ORDER BY CAST(id AS UNSIGNED), id LIMIT/OFFSET`），standalone 切内存快照，并在没有 store 时并上仍有分段的 `{data_dir}/<id>`。默认 limit=100，limit 必须为 1..500，超过 500 返回 400 `invalid limit`。
- `POST /api/tasks/batch` - 接收 `items` 数组（1..100 个现有创建请求），整包 envelope 错误返回 400 且不创建；合法 envelope 按顺序逐项调用 `CreateTaskFromSpec`，返回 200 的 `{index,cluster_key,task|error}` 结果数组。

## Dependencies
- Upstream: `internal/app` - 应用启动时注入
- Downstream: `internal/tasks` - 任务服务接口
- Metrics: `github.com/prometheus/client_golang`
- Tracing: `go.opentelemetry.io/otel`

## Features
- 认证：支持 Bearer Token 或 API Key；`/healthz` 与 `/ui/*` 默认匿名，`/metrics` 与 `/api/*` 可配置保护。`/ui/*` 不挂鉴权中间件，浏览器可以打开 Console，在设置里粘贴 bearer token。`api.auth.enabled=true` 时 `/swagger/*` 使用保护 `/api/*` 的同一鉴权中间件（跟 `enabled`，不跟 `protect_api`）。`sanitizeTask` 在响应中清空 `source.password`（解密仅供内部使用）。
- 创建任务：`CreateTaskFromSpec` 整包校验通过后才落库；400 返回 JSON `{"error","code"}`。批量创建复用同一入口，单项错误不阻塞后续项，成功任务脱敏返回。
- 源身份：`GET /api/sources/lookup` 与 dashboard/summary/list 的 host 过滤共用 `tasks.SameSourceHost`。lookup 任务名单有 store 时走集群观测同一份 `ListClusterObservation` 抄本，不是启动内存快照。回环别名（localhost、127/8、::1，含括号 IPv6）是同一台源，端口仍严格匹配；非回环 host 保持修剪后的原文精确匹配且不做 DNS 解析。
- 健康检查：`GET /healthz` 文本 `ok`；`GET /api/health` JSON `{"status":"ok"}`
- 文件观测：`GET /api/tasks/{id}/files` 返回当前 `OPEN` segment 与历史 `SEALED` 文件。配置了 file store 且该任务有目录行时，仍返回元数据结果，顺序与磁盘扫描相同：源序号升序，同一序号先封存再 open epoch，`limit` 保留序号最大的窗口。未配置 file store，或该任务目录为空时，扫描 `{data_dir}/{task_id}` 的封存文件和 `.open.e<epoch>`，按源序号升序；`file_name` 是源文件名，`file_path` 是磁盘路径。`GET /api/tasks/{id}/checkpoint` 返回下次 Start 会继续的 file/pos。本地 open 分段有完整事件时用该事件，不用文件大小，也不用文件行里的 0。存储的 checkpoint 与该 file+pos 相同则响应带 `gtid_set`。没有本地事件时返回存储的 checkpoint。epoch 大于 1 只有在目录里没有这段尾部的行时才把 pos 回拨到 4。能读到 `binlog_files.file_path` 时，从该目录最后一个完整事件续。checkpoint 已在封存 `UPLOADED` 对象内时不回拨。open 分段读不到时仍返回存储的 checkpoint，Start 以 `SEGMENT_NOT_ON_WORKER` 失败，不新建目录。两者都没有仍是 404 `checkpoint not found`。文件列表的 `start_pos`/`end_pos` 仍不从磁盘编造。没有 task store 时，重启后 dashboard 与 `GET /api/tasks` 列出仍有分段的目录，该 id 的 files 用同一扫描；Console 任务表点开即可看到磁盘路径，表从上到下就是接口返回的顺序。`GET /api/tasks/{id}/files/{name}` 下载清单里的一个文件名（封存名或 `*.open.e*`），响应是 `application/octet-stream`，`Content-Disposition` 的 filename 就是这个 basename。本进程 `{data_dir}/{id}/{name}` 存在时只读本地，正文长度是打开时的文件大小，OPEN 分段在 RUNNING 和 STOPPED 都可下。本地没有这个文件时，封存行 `upload_state=UPLOADED` 且 `object_key` 非空，并且本进程已配置对象上传，则从该对象读取，正文长度是打开对象时的大小。`LOCAL_ONLY`、`UPLOAD_FAILED`、空 `object_key`、没有本地文件的 open 分段、未配置上传，仍是 404 `segment not found on this process`。不跟 catalog `file_path`。`/`、`\`、`..` 返回 400。任务不存在是 404。`GET /api/tasks/{id}/replay` 在同一窗口上每个序号只返回一条 `file_path`：封存名和 `.open.e*` 同时存在时取 epoch 最大的 open 路径；`paths` 为空数组表示没有分段。`client` / `client_hint` 来自 `source.flavor`（`mysql` → `mysqlbinlog` / `MySQL mysqlbinlog`，`mariadb` → `mariadb-binlog`）。`GET /api/tasks/{id}/replay` 带 `stop_datetime` 时改为 UTC 定点窗口：可选 `start_datetime`，每个序号仍一条路径，`command` 带 `--stop-datetime` 和按需的 `--start-datetime`。无法解析的时间是 400 纯文本。空窗口是 200，`paths` 为 `[]`，`command` 为空。不带这两个参数时仍是 `limit` 窗口，响应没有 `command`。`GET /api/tasks/{id}/replay/archive` 用同一个 `limit` 把这些 basename 打成一个 `application/x-tar`（ustar），`Content-Disposition` 文件名是 `task-{id}-replay.tar`。带了 `stop_datetime` 时 tar 成员改为定点窗口。每个成员的字节与 `GET /api/tasks/{id}/files/{name}` 相同：本地文件优先，本地没有时只读封存且 `UPLOADED`、`object_key` 非空的对象。没有分段时 200 且是空 tar。任一选中分段打不开，响应该错误且正文不是半个 tar。鉴权与其它 `/api/tasks/*` 相同。不带时间参数时，files 列表和原来的 JSON replay 窗口不变。更新或启动这种只读身份返回 400 `on-disk backup has no task metadata`。`POST /api/tasks/{id}/adopt` 把 `cluster_key` 和 source 接到同一 id，返回 200，密码脱敏，状态仍是 `STOPPED`。没传 `start.mode` 时起点是最高分段字节大小的 `FILE_POS`。有 task store 时不从磁盘发现任务，也不从磁盘 adopt。
- 状态汇总：summary/dashboard 保留既有计数键，并新增 `starting`；STARTING 不混入 `running`。有元数据 rollup 时计数是 SQL `GROUP BY`。
- Source 聚合：dashboard source 项保留 `running`，并新增独立 `starting` 状态计数。按源计数同样是 `GROUP BY` 存储的 host/port，回环别名不并成一行。
- 任务观测：控制台只信 dashboard。任务列表与 dashboard 任务行走 SQL LIMIT/OFFSET；dashboard/summary 不再为计数调用 `Limit=0` 的整表读取。`total`、`summary.total` 与按源计数仍是同一过滤集。非法 state/limit/offset/port 返回 400，limit 超过 500 返回 `invalid limit`。
- 限流：基于 IP 的令牌桶限流，默认 100 req/s，burst 200
- Tracing：OTel HTTP span（可选）

## Validation Pilot (P4)
- Gin binding + validator 校验：
  - `/api/sources/lookup`（`host`/`port` 必填 + 端口格式校验）
  - `/api/tasks/{id}/files/retry-upload`（`limit` 范围 1..1000，默认 100）
  - `/api/tasks/{id}/upload-failures/reasons`（`limit` 范围 1..200，默认 20）
- task、replication 与 dashboard 响应保留 `FAILED` 状态及稳定的源错误 `last_error`，供管理台直接展示。
- RUNNING 且 dump 已在源 tip（`ReplicationProgress.AtTip`）时，`delay_seconds` 是 JSON 数字 0（字段在，不是省略）且 `status` 为 `NORMAL`，即使 `last_event_at` 仍是旧 event header。还没有事件时间样本时省略 `delay_seconds`，Console 继续显示 `--`。仍在追位点的 catch-up 继续按 `now - last_event_at` 计算 DELAYED。阈值仍是 30 秒。

## Update Rule
- 路由、请求/响应结构、认证/限流配置变化时，更新本文件。
