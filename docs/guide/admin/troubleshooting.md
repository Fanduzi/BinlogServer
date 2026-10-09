# 故障排查

本文档介绍常见问题的诊断方法和解决方案。

## 1. 排查思路

```
1. 查日志 → 找错误信息
2. 查状态 → API /metrics /events
3. 查数据库 → 任务状态、租约、worker
4. 查网络 → 连接性、延迟
```

## 2. 日志分析

### 2.1 日志位置

- 标准输出（推荐用 systemd/journald 收集）
- 或配置日志文件输出

### 2.2 关键日志模式

**启动成功：**

```
{"level":"info","msg":"server started","listen_addr":":8080"}
{"level":"info","msg":"scheduler restored","tasks_count":5}
```

**任务启动：**

```
{"level":"info","msg":"task started","task_id":"xxx","source":"10.0.0.1:3306"}
```

**复制正常：**

```
{"level":"debug","msg":"binlog event received","task_id":"xxx","file":"mysql-bin.000010","pos":12345}
```

**错误日志：**

```
{"level":"error","msg":"replication failed","task_id":"xxx","error":"connection reset"}
```

### 2.3 常见错误及解决

| 错误信息 | 原因 | 解决方案 |
|----------|------|----------|
| `connection refused` | 无法连接源 MySQL | 检查网络、防火墙、MySQL 状态 |
| `binlog dump thread still open` | Stop 时源库不可达，`KILL` 没打到。拉过这条 dump 的进程在 Close 返回后任务是 `STOPPED`。`GET /api/tasks/{id}` 的 `pending_dump_cleanup.connection_id` 是还可能开着的 Binlog Dump。这个字段在 `RUNNING` 上也会出现：新的 dump 要等 KILL 确认，确认不了就停在 `RETRY_BACKOFF` 并留下同样的警告。schema 3 没有 `pending_dump_cleanup` 列时，这条警告只在拉过这条 dump 的那个进程上（`process_local: true`）；集群控制面看不到，重启也没了。集群 schema 3 的日志或 `last_error` 若写着 migration `000004`，先 `./migrate up` 再重启 | 任何认领到这行的 worker 都会重试 `KILL`，用任务当前密码，先等 5 秒，失败则加倍，最长 30 秒，直到成功、`ER_NO_SUCH_THREAD`、processlist 里已经没有这条 Binlog Dump，或任务被删。不想等的话，在源库上 `KILL` 这个 `connection_id`。对端收到 FIN/RST 时大约一个心跳周期（15 秒，FIN 实测约 26–29 秒）拆掉。半开路径（没有 FIN/RST）上心跳写不阻塞，`net_write_timeout` 不触发；源库要等多久取决于它自己的 TCP 重传（Linux `tcp_retries2` 默认约 15 分钟，实测约 957 秒）。本进程在路径仍不通时缩短不了这段等待 |
| `access denied` | 认证失败 | 检查用户名密码、权限 |
| `server_uuid mismatch` | server_id 冲突 | 检查 server_id 配置 |
| `lease acquire failed` | 租约被其他 worker 持有 | 正常现象，或检查是否有重复 worker |
| `checkpoint save failed` | 无法保存位点 | 检查元数据库连接 |
| `SEGMENT_NOT_ON_WORKER` | 租约已经到这台 worker，分段目录还在死掉的 worker 上。`last_error` 里的路径就是 `binlog_files.file_path`，本机读不到 | 把该路径挂到这台 worker，或把分段拷过来，再 `POST /api/tasks/{id}/start`。不要在新目录上从位置 4 重拉。checkpoint 已在 `UPLOADED` 对象里时任务不会停在这个错误，会从对象续。见部署指南 6.3 第 2 节 |
| `SEALED_FILE_EXISTS` | 轮转要封的文件已经在这块盘上。任务是 `FAILED`，租约已放开，不再连源 | 确认这个封存文件是要留下的那一份。冲突的 open 分段不要再封成同一个名字。处理完再 `POST /api/tasks/{id}/start`。若 `last_error` 或 `storage_alert` 同时写着 Rotate 被追加进了已经打开的文件，这是下面的 `STORAGE_INCONSISTENT`，不要再 Start |
| `STREAM_REGRESSION` | 拉流和已经落盘的内容对不上：Rotate 指向更早的文件，Rotate 带着真实 end_log_pos 又指向当前文件，或事务事件的位点比已落盘位点小。GTID 序号有空洞、乱序、或多个 UUID 交错都不是这个错误（副本多线程复制、手工 `GTID_NEXT` 都会这样）。重连时源重发的文件头（FDE、Previous_GTIDs）落在已落盘位点之前也不算。任务是 `FAILED`，不会把这条流继续写成“已经写过” | 不要从这份文件恢复。对源上仍有的 GTID 新建一个任务。旧任务留着供核对 |
| `STORAGE_INCONSISTENT` | 某一个分段里有指向自己或更早文件的 Rotate，或事件位点在这个分段里倒退。列表、dashboard 和 `GET /api/tasks/{id}` 带 `storage_alert`。再 Start 会直接 `FAILED`，不会变成 `RUNNING` 后把新事务丢掉。副本上 GTID 乱序提交、多个 UUID 交错、序号出现在下一个文件、起点集合或 `gtid_purged` 里的空洞、以及保留删掉的文件，都不是这个错误 | 见下文「分段里的 Rotate 回卷」 |
| `CHECKPOINT_WRITE_FAILED` | checkpoint 写不进去，而且不是会死锁、断连、锁等待、只读切换这类瞬时错误。任务是 `FAILED`，租约已放开 | 看 `last_error` 里的数据库错误。表、权限或语法问题需要先修元数据库，再 `POST /api/tasks/{id}/start`。瞬时元数据错误仍是 `RETRY_BACKOFF`，不会用这个码 |
| `lease/epoch mismatch` | 封文件时这台 worker 的租约 epoch 已经不是当前主人。本进程停止，不把任务写成 `FAILED`，也不再连源 | 看任务行上的 `owner_worker_id`。新主人还在跑就不用管。没有 store 时本进程显示 `STOPPED`，事件 `TASK_LEASE_YIELDED`。需要这台机器再跑时再 Start |
| `api.auth.enabled=false cannot protect` | 鉴权未启用但尝试保护路由 | 设置 `api.auth.enabled=true` 或关闭保护 |
| `bearer_token is required when protection is enabled` | 启用保护但未配置凭证 | 配置 `bearer_token` 或 `api_key` |
| `http.*.read_timeout_sec must be > 0` | 超时参数配置非法 | 确保所有超时参数 > 0 |

### 2.3.1 分段里的 Rotate 回卷

v0.5.57 以及该版本之前，GTID 任务的 dump 连接一旦断开重连，可能把上一个 binlog 文件末尾的 Rotate 追加进当前打开的分段，随后把新事务当成“已经写过”丢掉。任务可以显示 `RUNNING`、延迟 0，checkpoint 的 `gtid_set` 仍包含被丢掉的事务。

`STORAGE_INCONSISTENT` 只认这种损坏的硬痕迹：某个分段里的 Rotate 指向自己或更早的文件，或事件的 `end_log_pos` 在这个分段里变小。下面这些不是损坏，任务照常启动。缺了哪些文件由 `GET /api/tasks/{id}/window` 的 `breaks` 说明，不拒绝 Start：

- 源是副本。多线程复制在 `replica_preserve_commit_order=OFF` 时，同一个文件里的 GTID 序号可以倒序；多源复制会把几个 UUID 交错写在一个文件里。
- 某个序号提交得更晚，落在下一个分段里；或者它从来就不在源 binlog 里，只存在于起点 GTID 集合或 `gtid_purged`。
- 本地保留删掉了较新或中间的文件，较旧的文件从前面过期，checkpoint 仍含有已经不在盘上的序号。
- 换主之后多了一个 server UUID。
- 任务的起点 GTID 集合本身有空洞，包括这个空洞落在某个分段已有序号的中间。

出现 `STORAGE_INCONSISTENT` 或 `storage_alert` 时：

1. 不要再 `POST /api/tasks/{id}/start`。新版本会直接失败；旧版本会继续丢事务。
2. 不要用这些分段做恢复。丢掉的事务不在文件里，改 checkpoint 也补不回来。
3. 在源上确认它还留着你需要的 GTID（`SHOW BINARY LOGS` 和 `@@gtid_purged`）。从那个 GTID 新建一个任务。
4. 留下原来的任务目录和 checkpoint，供事后核对。不要在原地改写出一份“看起来连续”的文件。

`storage_alert` 和 `last_error` 会写出损坏的分段名、checkpoint 里有但任何分段都没有保存的 GTID（MySQL），以及损坏分段之前仍然可以恢复的分段。这些较早的分段写于损坏之前，可以照常回放。`GET /api/tasks/{id}/replay`、定点恢复和回放 tar 自动停在损坏分段之前，并在 `warning` 里说明；Console 的回放区也会显示这条警告。`GET /api/tasks/{id}/window` 和 Console 的「可恢复窗口」也停在同一处：只覆盖损坏分段之前的分段，显示有缺口，缺口原因写明损坏分段和之后被排除的分段。`storage_alert.restart_gtid_set` 是新任务的 GTID 起点：起点集合加上这些较早分段里的事务，新任务会紧接着它们继续，并重新拉取缺失的事务。要在源库 purge 这些 binlog 之前建好。

`STREAM_REGRESSION` 是同一种不一致出现在正在拉的流上。处理方式相同：停在这份备份，对新的 GTID 另建任务。

#### 未发布的 main 构建（PR #293 之后，如 0a84917a）误判的 `STREAM_REGRESSION`

只有 PR #293 合入之后、本修复之前的未发布 main 构建（例如 0a84917a）会出现这个误判。v0.5.57 没有 `STREAM_REGRESSION` 这个错误码，v0.5.57 上的任务不会这样失败，从 v0.5.57 直接升级不需要做下面的事。在这些 main 构建上，GTID 任务在重连、进程重启或升级后可能误报 `STREAM_REGRESSION`，`last_error` 形如 `STREAM_REGRESSION: gtid <uuid>:N is ahead of stored set <集合>`。原因是 checkpoint 比已经写进打开分段的内容落后：分段刷盘后、checkpoint 写入前进程退出，或者重连发生在一个事务中间。下一条事务的 GTID 不在存下的集合里，任务就被判成 `FAILED`。文件本身没有坏。

新版本在每次 Start 时先读打开分段的末尾（dump 被 `KILL`、代理或空闲超时断开时，本次运行以 runner error 结束，由调度器重试并完整 Start 一次，所以每次重连都会走这一步；只有运行中被判为 `SOURCE_UNREACHABLE` 的错误才在进程内重开 dump，日志是 `dump reconnect`，按已刷盘的 file/pos 续传）：把分段里已经完整的事务补进 checkpoint 的 GTID 集合；末尾没写完的事务在它开始的位置截掉，从那里重新拉。GTID 空洞和乱序不再让任务失败。升级后直接 `POST /api/tasks/{id}/start` 这些任务即可，不需要改 schema（仍是 6），也不需要新建任务。每次恢复时日志里会有一行 `open tail reconciled ... changed=true|false`（`changed=true` 表示补了 GTID）；截掉半个事务时另有一行 `open tail cut unfinished transaction`。如果同一个任务还带着 `storage_alert`（`STORAGE_INCONSISTENT`），那是真的损坏，按上面的步骤处理，不要再 Start。

### 2.4 API 鉴权错误

当启用 API 鉴权后，请求可能返回以下错误：

| HTTP 状态码 | 场景 | 原因 | 解决方案 |
|------------|------|------|----------|
| 401 Unauthorized | Bearer 模式 | 缺少 `Authorization` 头 | 添加 `Authorization: Bearer <token>` |
| 401 Unauthorized | API Key 模式 | 缺少 API Key 头 | 添加对应的请求头（如 `X-API-Key: <key>`） |
| 403 Forbidden | Bearer 模式 | Token 格式错误（无 `Bearer ` 前缀） | 确保格式为 `Bearer <token>` |
| 403 Forbidden | 任意模式 | 凭证不匹配 | 检查 Token/API Key 是否正确 |

**排查步骤：**

```bash
# 1. 确认鉴权配置
curl http://localhost:8080/healthz  # 健康检查始终不需要鉴权

# 2. 测试 Bearer Token
curl -H "Authorization: Bearer your-token" \
  http://localhost:8080/api/tasks

# 3. 测试 API Key
curl -H "X-API-Key: your-api-key" \
  http://localhost:8080/api/tasks

# 4. 查看服务日志确认鉴权配置生效
# 日志中会显示 api.auth.enabled=true
```

**常见配置错误：**

```yaml
# 错误：enabled=false 但 protect_api=true
api:
  auth:
    enabled: false
    protect_api: true  # ❌ 启动报错

# 正确：要么启用鉴权，要么关闭保护
api:
  auth:
    enabled: true
    protect_api: true  # ✅
```

### 2.5 HTTP 超时问题

当客户端遇到连接超时或断开时，可能是 HTTP 超时配置问题：

| 症状 | 可能原因 | 解决方案 |
|------|----------|----------|
| 大请求返回 408/超时 | `read_timeout_sec` 过小 | 增大 `http.control_plane.read_timeout_sec` |
| 大响应被截断 | `write_timeout_sec` 过小 | 增大 `http.control_plane.write_timeout_sec` |
| 连接频繁重建 | `idle_timeout_sec` 过小 | 增大 `idle_timeout_sec` |
| 慢客户端攻击 | 无 `read_header_timeout_sec` | 确保 > 0（默认 5 秒） |

**排查步骤：**

```bash
# 1. 检查当前超时配置
# 查看配置文件或环境变量

# 2. 测试慢请求
time curl -X POST http://localhost:8080/api/tasks \
  -H "Content-Type: application/json" \
  -d '{"name":"test","cluster_key":"test","source":{"host":"10.0.0.1","port":3306,"user":"repl","password":"secret"},"start":{"mode":"LATEST"}}'

# 3. 检查服务端日志是否有超时断开记录
```

**生产环境推荐值：**

```yaml
http:
  control_plane:
    read_header_timeout_sec: 5
    read_timeout_sec: 60       # 大任务创建可能需要更长时间
    write_timeout_sec: 60      # 大响应（如文件列表）可能需要更长时间
    idle_timeout_sec: 120
```

## 3. API 诊断

### 3.1 健康检查

```bash
curl http://localhost:8080/healthz
```

正常响应：
```text
ok
```

### 3.2 任务状态

```bash
# 列出所有任务
curl http://localhost:8080/api/tasks

# 查看单个任务
curl http://localhost:8080/api/tasks/{task_id}
```

关键字段：
- `state` - 任务状态
- `last_error` - 最后一次错误
- `owner_worker_id` - 当前执行的 worker

### 3.3 复制状态

```bash
curl http://localhost:8080/api/tasks/{task_id}/replication
```

```json
{
  "task_id": "1",
  "state": "RUNNING",
  "status": "NORMAL",
  "threshold_seconds": 30,
  "has_progress": true,
  "delay_seconds": 0,
  "last_event_at": "2024-01-01T10:00:00Z",
  "last_event_file": "mysql-bin.000010",
  "last_event_pos": 12345
}
```

关键字段：
- `status`: `NORMAL / DELAYED / ABNORMAL`
- `delay_seconds`: lag in seconds. At the source tip (LATEST start after StartSync, or idle dump wait) this is 0 even if `last_event_at` is an old event header. Catch-up that is still behind the source tip uses `now - last_event_at`.
- `has_progress=false`: 当前没有可用复制进度（需结合任务状态判断）

### 3.4 任务事件

```bash
curl http://localhost:8080/api/tasks/{task_id}/events?limit=50
```

查看任务的历史事件：
- `TASK_START_DISPATCHED` - 控制面已分发启动
- `TASK_STARTED` / `TASK_RUNNING` - 任务启动并进入运行
- `TASK_RUNNER_ERROR` / `TASK_RETRY_BACKOFF` - 执行错误与退避
- `TASK_FAILED` - 不可恢复错误，租约已放开
- `TASK_LEASE_YIELDED` - 租约 epoch 已不属于本进程，runner 停止且没有改写新主人的任务行
- `TASK_LEASE_DEGRADED` / `TASK_LEASE_LOST` / `TASK_LEASE_GRACE_EXCEEDED` - 租约异常链路

### 3.5 集群状态

```bash
# 查看在线 workers
curl http://localhost:8080/api/workers

# 查看集群概览
curl http://localhost:8080/api/cluster/overview
```

## 4. 数据库诊断

### 4.1 检查任务状态

```sql
SELECT id, name, state, owner_worker_id, last_error, updated_at
FROM backup_tasks
ORDER BY updated_at DESC;
```

### 4.2 检查租约状态

```sql
SELECT task_id, owner_worker_id, epoch, lease_expire_at, renewed_at
FROM task_leases
WHERE lease_expire_at > NOW(6);
```

### 4.3 检查 Worker 注册

```sql
SELECT worker_id, session_id, lease_expire_at, renewed_at
FROM worker_registrations
WHERE lease_expire_at > NOW(6);
```

### 4.4 检查最近事件

```sql
SELECT task_id, event_type, message, event_time
FROM task_events
ORDER BY event_time DESC
LIMIT 20;
```

## 5. 常见问题

### 5.1 任务卡在 STARTING

**症状：**

- `GET /api/tasks/{id}` 长时间显示 `state=STARTING`
- `/api/tasks/{id}/events` 只有 `TASK_START_DISPATCHED`，迟迟没有 `TASK_RUNNING`
- 控制面启动成功，但业务不推进

**排查步骤（API / 日志 / SQL）：**

```bash
# API：检查 worker 在线与任务租约
curl http://localhost:8080/api/workers
curl http://localhost:8080/api/tasks/{task_id}/lease
curl http://localhost:8080/api/tasks/{task_id}/events?limit=50
```

```text
# 日志关键字（worker 节点）
worker claim starting tasks failed
worker claimed starting tasks count=
worker registration renew lost ownership
```

```sql
-- SQL：任务状态、租约、注册状态
SELECT id, state, owner_worker_id, epoch, updated_at, last_error
FROM backup_tasks
WHERE id = '{task_id}';

SELECT task_id, owner_worker_id, epoch, lease_expire_at, renewed_at
FROM task_leases
WHERE task_id = '{task_id}';

SELECT worker_id, session_id, lease_expire_at, renewed_at
FROM worker_registrations
ORDER BY renewed_at DESC
LIMIT 20;
```

**修复动作：**

- 无 worker 在线：先恢复 worker 进程，再观察 claim 日志与 `/api/workers`。
- `worker_registrations` 持续失租：检查 worker 到元数据库网络与延迟。
- `STARTING` 为旧脏状态且无运行归属时，可执行一次 `POST /api/tasks/{id}/stop` 后再 `POST /api/tasks/{id}/start` 触发重新分发。

### 5.2 运行中 lease 或 registration 丢失

**症状：**

- 任务从 `RUNNING` 进入 `LEASE_DEGRADED` 或停止
- 事件出现 `TASK_LEASE_DEGRADED` / `TASK_LEASE_LOST` / `TASK_LEASE_GRACE_EXCEEDED`
- worker 日志出现 `worker registration renew lost ownership`

**排查步骤（API / 日志 / SQL）：**

```bash
# API：看任务状态和运行历史
curl http://localhost:8080/api/tasks/{task_id}
curl http://localhost:8080/api/tasks/{task_id}/lease
curl http://localhost:8080/api/tasks/{task_id}/events?limit=100
curl http://localhost:8080/api/tasks/{task_id}/runs?limit=20
```

```text
# 日志关键字
worker registration renew failed
worker registration renew lost ownership
lease renew loop panic
TASK_LEASE_GRACE_EXCEEDED
```

```sql
-- SQL：检查 lease 是否还有效
SELECT task_id, owner_worker_id, epoch, lease_expire_at, renewed_at
FROM task_leases
WHERE task_id = '{task_id}';

-- SQL：检查 worker 注册是否临近/已过期
SELECT worker_id, session_id, lease_expire_at, renewed_at
FROM worker_registrations
WHERE worker_id = '{owner_worker_id}';

-- SQL：检查最近任务事件
SELECT task_id, event_type, message, detail, event_time
FROM task_events
WHERE task_id = '{task_id}'
ORDER BY event_time DESC
LIMIT 20;
```

**修复动作：**

- 按推荐关系调整参数：`renew < ttl < grace`（见 `configuration.md`）。
- 排查元数据库瞬时超时/抖动，优先稳定 worker 到 DB 的网络链路。
- 出现注册失租后，先确保单实例持有同一 `worker_id`，避免重复进程竞争。

### 5.3 checkpoint 不推进

**症状：**

- `/api/tasks/{id}/checkpoint` 的 `pos` 长时间不变化
- `/api/tasks/{id}/replication` 显示任务运行但 `last_event_at` 或 `last_event_pos` 不前进
- 任务事件里反复出现 `TASK_RUNNER_ERROR`

**排查步骤（API / 日志 / SQL）：**

```bash
# API：对比 checkpoint 与复制进度
curl http://localhost:8080/api/tasks/{task_id}/checkpoint
curl http://localhost:8080/api/tasks/{task_id}/replication
curl http://localhost:8080/api/tasks/{task_id}/events?limit=100
curl http://localhost:8080/api/tasks/{task_id}/files?limit=50
```

```text
# 日志关键字
runner error
context deadline exceeded
connection reset
checkpoint
```

```sql
-- SQL：checkpoint 最新位点
SELECT task_id, file_name, pos, gtid_set, updated_at
FROM backup_checkpoints
WHERE task_id = '{task_id}';

-- SQL：最近错误事件（查看 detail）
SELECT task_id, event_type, message, detail, event_time
FROM task_events
WHERE task_id = '{task_id}'
ORDER BY event_time DESC
LIMIT 50;

-- SQL：确认任务是否处于反复重试
SELECT id, state, last_error, updated_at
FROM backup_tasks
WHERE id = '{task_id}';
```

**修复动作：**

- 优先处理 `TASK_RUNNER_ERROR` 对应根因（源库连通、权限、磁盘空间、元数据库可用性）。
- 若任务陷入错误重试且位点不前进，可执行 `stop` / `start` 触发新 run，并观察 `/runs` 与 `/events` 是否恢复推进。
- 若只见 `.open.e*` 文件且未 seal，先确认任务是否仍持有有效 lease，避免失租后继续误操作文件。

### 5.4 VIP 换到了另一台 MySQL

任务地址是 VIP、DNS 或代理。它连上的 `@@server_uuid`（MariaDB 是 `mariadb:<server_id>:<gtid_domain_id>`）和上次不同。Console 任务详情的「源服务器」列出每一台、哪一台是当前的，以及每次切换的旧身份、新身份、`file:pos` 和 GTID 集合。文件表的「服务器」列写出第几台和完整身份。`GET /metrics` 的 `binlog_server_source_switchovers{task_id,outcome}` 里，`outcome` 是 `continued` 或 `stopped`。`GET /api/tasks/{id}` 的 `source_chain` 是同一份内容。任务连上过源库就有这个字段；没有切换时 `servers` 只有一段、`switches` 是 `[]`、没有 `outcome`。

不要删除 `{data_dir}/{task_id}/.source-chain`。每一行是一段：第一行是没有前缀的那些文件的主人，后面每一行是 `{前缀}.{binlog 文件名}` 的主人。前缀在这台服务器第一次出现时是它的身份，回切到之前出现过的服务器时是 `{身份}~{n}`（A、B、A 的第三行前缀是 `A~2`）。「标成当前」的是最后写下文件的那一段；任务停下时它不一定是正在被复制的服务器。删掉它之后，文件表无法再说清每一份文件是谁写的。

**地址换了，任务仍是运行中**

Console 有一条说明：这份备份仍在复制。这和没有换过服务器的运行中任务不同。`source_chain.outcome` 是 `continued`。`current` 是新身份。指标 `outcome="continued"` 增加，`outcome="stopped"` 仍是 0。

要核对的两件事：

1. `GET /api/tasks/{id}/window` 的 `continuous`。不是 true 时先看 `breaks`，不要拿这份备份做恢复。
2. 回放顺序。`stop_datetime` 或 `stop_gtid` 的 `paths` 先是旧服务器的文件，再是新服务器的文件。Console 回放命令是同一个顺序。旧服务器的文件名没有前缀。新服务器的文件名以新身份开头。

不要为了“看起来像一台服务器”去改文件名，也不要删掉旧服务器的分段。

**地址换了，任务变成 FAILED**

`last_error` 以 `SOURCE_SWITCHOVER:` 开头。Console 用白话写出原因，以及下一步：对新主库新建任务，保留这份备份。不要删除 `.source-chain`。再次 `POST /api/tasks/{id}/start` 也不会把两台服务器写进同一份备份；每次 Start 检查的是同一次切换，「源服务器」里和指标里都只算一次。指标 `outcome="stopped"` 是 1。把 VIP 指回原来那台、再 Start 后任务回到运行时，`outcome` 变成 `resumed`，停止横幅消失，那条停止标成“曾停止，任务已恢复运行”，指标 `outcome="stopped"` 回到 0。之后普通 Stop 或进程重启不会把它变回“停止”：只要停止事件之后任务跑起来过（有 `TASK_RUNNING` 事件），这次停止就一直算已恢复。

| `source_chain` 的 `reason` | 含义 | 怎么做 | 不要做 |
|---|---|---|---|
| `no_gtid` | 这份备份没有已执行 GTID 集合。`LATEST`，以及没有存下 GTID 的 `FILE_POS`，都是这个原因 | 对新主库新建任务。这份备份留到切换前的位点 | 不要把旧的 file:pos 指到新主库上再启动这条任务 |
| `missing_transactions` | 现在连上的服务器缺少这份备份里已经有的事务 | 保留这份备份。需要新主库上更新的写入时，另建任务 | 不要删分段，也不要指望再次启动能把缺口补上 |
| `purged` | 新主库已经清掉了这份备份还没有的事务 | 保留这份备份。新任务只能从新主库仍保留的位点开始，或先用这份备份恢复再追 | 不要在这条任务上继续拉，缺口补不回来 |
| `mariadb` | MariaDB 没有可以换主继续的 GTID 路径 | 对新主库新建任务，保留这份备份 | 不要把两台 MariaDB 的文件并进这一条任务 |
| `gtid_unreadable` | 读不到新服务器的 GTID 状态 | 先恢复到这台服务器的连接和权限，再新建任务 | 不要删 `.source-chain` 之后重试这条任务，期望它把两台拼在一起 |

**再指回旧主库**

任务已经从 A 继续到 B 之后，把 VIP 指回 A。A 没有 B 上新写入的事务，所以任务停在 `missing_transactions`。`.source-chain` 不改。`current` 仍是 B，因为最后写下文件的是 B。最新一条切换是 old=B、new=A、`continued` 为 false。指标上 `continued` 和 `stopped` 都大于 0。已经写下的分段字节不变。A 上这次切换之后的新写入不在这份备份里。需要那些写入时另建任务。不要删 `.source-chain`，也不要删 B 的分段来“回到只有 A”。

**旧主库追平之后再回切（A→B→A）**

DBA 的正常回切：A 以 B 为源 `AUTO_POSITION=1` 追平 B，再提升 A、VIP 指回 A。A 追平前任务会像上面一样停在 `missing_transactions`。A 追平后再 `POST /api/tasks/{id}/start`，这时 A 已经有这份备份的全部事务，任务按“继续”处理：`.source-chain` 追加第三行 A，变成 A、B、A；A 的新文件写成 `{A 的身份}~2.{binlog 文件名}`，不会和 A 切走前写下的 `mysql-bin.00000N` 重名。`source_chain.current` 是 A，文件表里这些新文件标“第 3 台”。回放 `paths` 的顺序是 A 切走前的文件、B 的文件、A 回切后的文件。`/window` 不会因为 B 或 A 的文件头不含刚复制进去的事务而报假的 `gtid hole`；GTID 任务的起点集合（例如 mysqldump 种子里、首个文件头之后被 dump 跳过的事务）也算已存下。之后进程重启或再 Start 不会再记一次切换。B 最后那个 open 分段在回切时按 B 封存，不会丢。A 上只有 A 有、B 没有的事务（例如回切前在 A 上写的行）也会被拉进来，因为它们是 A 的历史。

**早期版本写下的回切目录**

v0.5.57 和 0a84917a 在真实回切（A→B→A，A 先追平 B）后继续复制，但 A 回切后的文件沿用了 A 切走前的名字（例如第一段的 `mysql-bin.000004` 和第三段的 `mysql-bin.000004.open.e1` 或 `.sealed.e1`），`.source-chain` 仍是 A、B，目录行 `mysql-bin.000004` 被改指到后一份文件。早于本版本的 build 上，`/files` 和 `/replay` 会漏掉第一段那份文件，恢复报 ERROR 1032 或静默少事务。

怎么认：同一个 binlog 序号有几份文件（无后缀加 `.sealed.eN` / `.open.eN`），`.source-chain` 只有 A、B，而事件里有 B→A 的继续复制切换（重启时旧版本还会多记几条同样的 B→A）。

本版本的处理：

1. 读接口（任务列表、详情）给出 `storage_alert`，`code` 是 `STORAGE_INCONSISTENT`，`detail` 以 `legacy failback:` 开头并列出文件；`/replay` 和 `/window` 停在第一份有问题的文件之前，`warning` 说明原因。
2. Start（含升级后进程启动时自动续跑）先修：按每次切换事件里记下的 GTID 集合，核对每份文件里的事务属于哪一段。能唯一确定时，把后一段改名为 `{A 的身份}~2.{binlog 文件名}`（没有事务的空 open 文件跟着同名的那份走），补回第一段的目录行并标成待重新上传（后台补传会按原对象键重传并核对校验和），把 checkpoint 的文件名改成新名字，`.source-chain` 写成 A、B、A，记一条 `STORAGE_REPAIRED` 事件。不删任何文件。修复先写 `{data_dir}/{task_id}/.legacy-failback-repair` 计划，进程中途退出后下次 Start 接着做完。修复完成前，后台补传不碰这些文件的目录行（旧 build 把它们的对象键写成了错误的目录，提前补传会把第三段的字节传到旧键下）。
3. 第一段那份文件本地已经没有（例如按 `local_retention_days` 清掉，只剩对象）：`storage_alert.detail` 带 `not on disk: mysql-bin.00000N`。Start 先从对象存储里 A 的目录取回 `{prefix}/{cluster}/{A 的身份}/mysql-bin.00000N`，核对其中的事务确实属于第一段（不含 A 进入这一段之前已存的事务，并且都在切到 B 时记下的集合里），放回任务目录，再按上面修复。取不到、对象里的事务不对，或没有配对象存储时，Start 拒绝，什么都不改，见下面“手工取回”。
4. 不能证明时（某份文件的事务不属于任何一段、属于多段、改名目标已存在，或读不了文件）：Start 拒绝，任务 `FAILED`，`last_error` 以 `STORAGE_INCONSISTENT` 开头并写出原因和文件；文件、目录行、checkpoint、对象存储都不改。切换事件里的 `gtid_set` 解析不了时，`segment` 是会回来的服务器离开时那个文件，`/replay` 停在它之前。

拒绝后怎么做：

- 不要按 `/replay` 恢复过 `storage_alert.segment` 这一点。它之前的文件（`valid_segments`）仍可用。
- 对现在的主库按它的 `@@gtid_executed` 新建任务，旧任务保留做取证。
- 需要旧任务里那段时间的数据时，用 `mysqlbinlog --include-gtids`/`--exclude-gtids` 按 GTID 从这些文件里手工拼：先第一段（A 切走前的 GTID），再 B 的文件，再 A 回切后的 GTID。
- 不要删 `.source-chain`，不要手工改文件名或删分段；保留目录和对象存储里的对象。

手工取回第一段（Start 因为取不到或对不上而拒绝时）：

1. 任务保持 `FAILED`，不要删任何东西。从 `storage_alert.detail` 的 `not on disk:` 读出文件名，例如 `mysql-bin.000008`。
2. 在对象存储里找它：`mc ls -r <别名>/<bucket>/<prefix>/<cluster>/<A 的身份>/`，要的是不带 `.sealed.eN` 后缀、也不带 `~n.` 前缀的那个对象。别的副本（异地备份、旧主库上还没 purge 的同名 binlog）也可以。
3. 先核对：`mysqlbinlog <文件> | grep GTID_NEXT` 里应是 A 的、在切到 B 那条 `SOURCE_SWITCHOVER` 的 `gtid_set` 之内、并且晚于前一个文件的事务。
4. 复制到 `{data_dir}/{task_id}/<文件名>`（同属主、同权限），再 Start。Start 会再核对一遍，然后修复。
5. 对象和别的副本都没有时，这些事务已不在这份备份里：按上面“拒绝后怎么做”处理。

修复后的对象：改名的文件按新名字上传到新对象键（`{A 的身份}/{A 的身份}~2.{文件名}`）。旧 build 写下的对象（例如 `{A}/mysql-bin.00000N.sealed.e1`，或被错放在 B 目录下的 `{B}/mysql-bin.00000N.sealed.e1`）不再被任何目录行引用，保留策略也不会清理它们。`STORAGE_REPAIRED` 事件的 `detail` 在 `orphaned_objects=` 后面列出这些键（可能有的已不存在）。确认按 `/replay` 恢复无误后，可以按这个清单手工删除；服务不会自动删。

GTID 缺口兜底：任务换过服务器时，读接口还会核对每次继续复制的切换里记下的“已存下”集合。某些事务被记成已存下，本机却没有任何分段存着，而它们前后的事务都在（或任务的第一个文件还在、缺口紧接在起点集合之后；例如手工删过一个文件），`storage_alert.detail` 以 `stored gtid hole:` 开头，`missing_gtids` 写出缺的事务，`/replay` 和 `/window` 停在缺口后的第一个分段之前。按上面手工取回的办法把那个文件放回去即可；它不影响 Start。缺口之前有目录行是 `UPLOADED`、只在对象存储里的文件时不报（回放会从对象存储读它）。`/window` 另外保证：同一个来源的已存 GTID 序号中间有空缺（且不在起点集合里）时，`continuous` 一定是 false，`breaks` 写出缺的序号和两边的文件。

已知限制：

- 检测靠 `SOURCE_SWITCHOVER` 事件。meta 库从较早的备份恢复、或这些事件被人工清理过时，本版本认不出这种目录，也不会拒绝；这时 `/window` 仍会因为 GTID 空缺报不连续，但 `/replay` 不截断。不要清理 `task_events`；meta 从备份恢复过的任务，按“拒绝后怎么做”处理。
- MariaDB 和没有 GTID 的任务不会继续复制到另一台，所以没有这种目录。
- 旧版本重启时多记的 B→A 切换不算新的一段，但仍计入 `binlog_server_source_switchovers{outcome="continued"}`。

English: The task address is a VIP, DNS name, or proxy, and it reached a different server identity. The Console task view lists each server, which one is current, and each switch (old, new, file:pos, GTID set). The files table names the server, not only the filename prefix. `binlog_server_source_switchovers{task_id,outcome}` is `continued` or `stopped`. `GET /api/tasks/{id}` returns the same view as `source_chain` when a chain or a switch exists. Do not delete `{data_dir}/{task_id}/.source-chain`. The first line owns the unprefixed files.

When the task stays `RUNNING`, the Console says this backup is still copying. Check that `GET /window` is continuous before you restore, and that a replay lists the older server's files before the newer server's files. Do not rename files so they look like one server.

When the task is `FAILED` and `last_error` starts with `SOURCE_SWITCHOVER:`, the Console states the reason and the next step: start a new task against the new primary and keep this backup. `no_gtid` means this backup has no executed GTID set, so do not point the old file:pos at the new primary. `missing_transactions` means the server now reached lacks transactions already stored. `purged` means the new primary has purged transactions this backup still needs. `mariadb` means there is no GTID path. `gtid_unreadable` means the new server's GTID state could not be read; fix connectivity, then start a new task. Do not delete `.source-chain` and start this same task hoping it will mix the two servers.

Failing back to the old primary after a continued switch stops with `missing_transactions` while the old primary lacks the newer server's transactions. `.source-chain` stays as it was, so current remains the server that wrote the latest files. Bytes already stored do not change. Do not delete `.source-chain` or the newer server's segments.

A proper failback re-syncs A from B first (`AUTO_POSITION=1`), then points the VIP back at A. Start the task after A has caught up: it continues, `.source-chain` gains a third line (A, B, A), and A's new files are named `{A identity}~2.{binlog file}` so they never reuse a name A wrote before the switch. `source_chain.current` is A, the files table names stint 3, replay lists A's old files, then B's, then A's new files, and `/window` does not report a false hole at either switch. A restart or another Start on A records no new switch. Each line of `.source-chain` is one stint; current is the stint that wrote the newest files, not necessarily the server a stopped task is copying. `GET /api/tasks/{id}` has `source_chain` for every task that has reached its source once; with no switch it has one server and an empty `switches` list. Repeated Starts of a stopped task check the same switch and count once. When the task runs again after a stop, `outcome` is `resumed`, the stop is `resolved`, and `outcome="stopped"` on the metric returns to 0. A later normal Stop or a process restart keeps it resolved, because the check is whether a `TASK_RUNNING` event follows the stop. A GTID task's start set (for example transactions in a mysqldump seed that the dump skipped in the first file) counts as stored for `/window`.

Task directory from an earlier build after a failback: v0.5.57 and 0a84917a kept copying after a real failback (A→B→A with A re-synced first) but wrote A's third stint under the names A used before the switch (for example the first stint's `mysql-bin.000004` and the third stint's `mysql-bin.000004.open.e1` or `.sealed.e1`), left `.source-chain` as A,B, and pointed the `mysql-bin.000004` catalog row at the later file. On builds before this one `/files` and `/replay` leave out the first-stint file, and a restore fails with ERROR 1032 or silently loses rows. Recognize it by several files for one binlog index, `.source-chain` with only A,B, and a continued B→A switch in the events (an old build adds more identical B→A on restart). This build reports it on reads: `storage_alert` with code `STORAGE_INCONSISTENT` and a `detail` starting `legacy failback:` that names the files, and `/replay` and `/window` stop before the first affected file with a warning. On Start, including the automatic resume after an upgrade, it checks every file's transactions against the GTID sets recorded at each switch. When each file fits exactly one stint, it renames the later stint to `{A identity}~2.{binlog file}` (an empty open file follows the file of the same name), lists the first-stint file in the catalog again and marks it for re-upload and checksum verification, fixes the checkpoint file name, writes `.source-chain` as A, B, A, and records `STORAGE_REPAIRED`. It deletes nothing, and a plan in `.legacy-failback-repair` lets the next Start finish a repair interrupted by a crash. Until the repair completes, the background upload retry leaves those files' catalog rows alone (the old build gave them a key in the wrong server's directory). When the first-stint file is no longer on disk (for example removed by `local_retention_days`, kept only as an object), `detail` adds `not on disk: mysql-bin.00000N`; Start first fetches `{prefix}/{cluster}/{A identity}/mysql-bin.00000N` back, checks that its transactions belong to the first stint (none stored before that stint began, all within the set recorded at the switch to B), puts it back in the task directory, and then repairs. If the object cannot be read, holds other transactions, or no object storage is configured, Start refuses and changes nothing. When the order cannot be proven (a file fits no stint or more than one, the target name exists, or a file cannot be read), Start refuses with `STORAGE_INCONSISTENT` naming the files and changes nothing: not the files, the catalog, the checkpoint, or object storage. An unparseable switch `gtid_set` still names a segment (the file the returning server left at) and `/replay` stops before it. Then do not restore past `storage_alert.segment` (`valid_segments` still restore), start a new task from the current primary's `@@gtid_executed`, keep the old task for evidence, and if you need its data assemble it by GTID with `mysqlbinlog --include-gtids`/`--exclude-gtids`: first stint, then B, then A's later GTIDs. Do not delete `.source-chain`, rename files by hand, or delete segments or objects. To fetch a first-stint file by hand after such a refusal: keep the task `FAILED`; take the name from `not on disk:`; find the object with `mc ls -r <alias>/<bucket>/<prefix>/<cluster>/<A identity>/` (the one without a `.sealed.eN` suffix or `~n.` prefix), or another copy of that binlog; check with `mysqlbinlog <file> | grep GTID_NEXT` that it holds A's transactions within the switch-to-B `gtid_set`; copy it to `{data_dir}/{task_id}/<name>` and Start again, which checks it again and repairs. If no copy exists, those transactions are not in this backup: follow the refusal steps above. After a repair, renamed files upload under new keys (`{A identity}/{A identity}~2.{file}`). Objects the old build wrote (for example `{A}/mysql-bin.00000N.sealed.e1`, or `{B}/mysql-bin.00000N.sealed.e1` filed under the wrong server) are no longer referenced and retention does not remove them; `STORAGE_REPAIRED` lists them after `orphaned_objects=` (some may not exist). Delete them by hand once a restore from `/replay` is verified; the server never deletes them. Safety net: on a task that switched servers, reads also compare the stored set each continued switch recorded with the local segments. Transactions recorded as stored that no segment holds, with stored transactions on both sides (or the start set before them while the task's first file is still on disk; for example a file removed by hand), set `storage_alert` with `detail` `stored gtid hole: ...` and `missing_gtids`, and `/replay` and `/window` stop before the first segment after the hole; put the file back as above. It does not block Start, and it is not reported while a file before the cut is `UPLOADED` and only in object storage (replay reads it from there). `/window` is never `continuous` when the stored sequences of one source have a gap inside them that the start set does not cover; the break names the missing GTIDs and the files on both sides. Known limits: detection relies on the `SOURCE_SWITCHOVER` events, so if the meta database is restored from an older backup or those events are cleaned up, this build cannot recognize the directory and does not refuse it (`/window` still reports the GTID gap, but `/replay` does not cut); do not clean up `task_events`, and treat a task whose meta was restored as refused. The extra B→A events an old build recorded on restart do not start a stint but still count in `binlog_server_source_switchovers{outcome="continued"}`.

## 6. 性能问题

### 6.1 CPU 使用率高

检查：
- 是否有大量任务
- 复制速度是否过快
- 是否有频繁的 GC

```bash
# 查看 goroutine 数量
curl http://localhost:8080/metrics | grep goroutines

# 查看 GC 信息
curl http://localhost:8080/metrics | grep go_gc
```

### 6.2 内存使用增长

检查：
- 是否有内存泄漏
- 缓冲区是否过大

```bash
# 查看内存指标
curl http://localhost:8080/metrics | grep go_memstats
```

### 6.3 磁盘空间不足

```bash
# 查看数据目录大小
du -sh /data/binlog

# 检查保留策略是否生效
# retention_days 配置是否合理
```

## 7. 紧急操作

### 7.1 强制停止任务

```bash
curl -X POST http://localhost:8080/api/tasks/{task_id}/stop
```

### 7.2 强制释放租约

```sql
-- 谨慎操作！确保没有其他 worker 在执行
UPDATE task_leases
SET owner_worker_id = '', lease_expire_at = NOW(6), renewed_at = NOW(6)
WHERE task_id = 'xxx';
```

### 7.3 清理过期数据

```sql
-- 清理过期 worker 注册
DELETE FROM worker_registrations WHERE lease_expire_at < NOW(6);

-- 清理过期租约
DELETE FROM task_leases WHERE lease_expire_at < NOW(6);
```

### 7.4 源库不可用，回放本地分段

`{data_dir}/{task_id}/` 里的封存文件和 `.open.e<epoch>` 按序号回放。回放 MySQL 源必须用 MySQL 自带的 `mysqlbinlog`，不要用 MariaDB 的。回放 MariaDB 源必须用 `mariadb-binlog`。先执行 `mysqlbinlog --version`：输出印着 MariaDB 时不要拿它回放 MySQL，管道会在 `check_constraint_checks` 处失败，MySQL 返回 `ERROR 1193 (HY000)`，一条数据都不进，文件没有坏。`.open.e<epoch>` 原样传入。standalone 没配 `meta_dsn` 时，`GET /api/tasks` 和 dashboard 能看到仍有分段的 `{data_dir}/<task_id>/`（重启后也一样，id 就是目录名）。`GET /api/tasks/{id}/files` 按序号升序列出这些分段（`file_path` 带 `.open.e<epoch>`），checkpoint 在没有位点行时仍是 `404 checkpoint not found`。回放命令每个序号只传一个文件。步骤见 [部署指南「源库不可用时，用本地分段回放」](deployment.md#replay-local-segments)（English: [Replay local segments when the source is gone](deployment.md#replay-local-segments-en)）。

## 8. 获取支持

收集以下信息：

1. 服务版本：`binlog-server --version`
2. 配置文件（脱敏）
3. 错误日志（最近 100 行）
4. 任务状态：`curl /api/tasks/{task_id}`
5. 复制状态：`curl /api/tasks/{task_id}/replication`
6. 最近事件：`curl /api/tasks/{task_id}/events`
7. Metrics：`curl /metrics`

---

**下一步**：阅读 [可观测性](./observability.md) 了解如何设置监控和告警。
