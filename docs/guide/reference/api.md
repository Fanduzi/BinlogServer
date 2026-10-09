# API 参考

本文档列出所有 API 端点及其参数，每个端点都附带 **curl 示例**。

## 1. 基础信息

| 项目 | 值 |
|------|-----|
| Base URL | `http://host:port` |
| Content-Type | `application/json` |
| Swagger UI | `http://host:port/swagger/index.html` |

## 2. Swagger 交互式文档

### 2.1 访问 Swagger UI

启动服务后，打开浏览器访问：

```
http://127.0.0.1:8080/swagger/index.html
```

### 2.2 Swagger UI 功能

1. **浏览 API** - 查看所有端点、参数、响应格式
2. **Try it out** - 在线发请求，实时查看响应
3. **Schema** - 查看请求/响应的数据结构

### 2.3 API 分组

| 分组 | 说明 |
|------|------|
| System | 健康检查 |
| Dashboard | 汇总、大盘、源库反查 |
| Tasks | 任务 CRUD、磁盘目录认领、启停、文件、回放 |
| Cluster | Worker 列表、集群概览 |

### 2.4 更新 Swagger 文档

修改代码中的 Swagger 注解后，重新生成：

```bash
go run github.com/swaggo/swag/cmd/swag@v1.16.6 init \
  -g cmd/binlog-server/main.go \
  -o internal/swaggerdocs \
  --parseInternal
```

## 3. 任务管理 API

### 3.1 创建任务

```bash
# 从最新位置开始（LATEST）
curl -X POST http://localhost:8080/api/tasks \
  -H "Content-Type: application/json" \
  -d '{
    "name": "backup-mysql-prod",
    "cluster_key": "prod-cluster",
    "source": {
      "host": "10.0.0.1",
      "port": 3306,
      "user": "repl",
      "password": "secret"
    },
    "start": {
      "mode": "LATEST"
    },
    "storage": {
      "retention_days": 30
    }
  }'
```

```bash
# 从指定文件位置开始（FILE_POS）
curl -X POST http://localhost:8080/api/tasks \
  -H "Content-Type: application/json" \
  -d '{
    "name": "backup-mysql-prod",
    "cluster_key": "prod-cluster",
    "source": {
      "host": "10.0.0.1",
      "port": 3306,
      "user": "repl",
      "password": "secret"
    },
    "start": {
      "mode": "FILE_POS",
      "file": "mysql-bin.000010",
      "pos": 12345
    }
  }'
```

```bash
# 从指定 GTID 开始（GTID）
curl -X POST http://localhost:8080/api/tasks \
  -H "Content-Type: application/json" \
  -d '{
    "name": "backup-mysql-prod",
    "cluster_key": "prod-cluster",
    "source": {
      "host": "10.0.0.1",
      "port": 3306,
      "user": "repl",
      "password": "secret"
    },
    "start": {
      "mode": "GTID",
      "gtid_set": "3E11FA47-71CA-11E1-9E33-C80AA9429562:1-100"
    }
  }'
```

**请求字段：**

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| name | string | 是 | 任务名称 |
| cluster_key | string | 是 | 集群标识（全局唯一，`[a-zA-Z0-9._-]`） |
| source.host | string | 是 | MySQL 主机 |
| source.port | int | 是 | MySQL 端口 |
| source.user | string | 是 | 复制用户 |
| source.password | string | 是 | 复制密码 |
| start.mode | string | 是 | LATEST / FILE_POS / GTID |
| start.file | string | 条件 | 文件名（FILE_POS 模式必填） |
| start.pos | int | 条件 | 位置（FILE_POS 模式必填） |
| start.gtid_set | string | 条件 | GTID 集合（GTID 模式必填）。也接受别名 `gtid`。 |
| storage.retention_days | int | 否 | 保留天数（默认 7，范围 1-3650）。省略下面两键时，本地和桶都用这个数 |
| storage.local_retention_days | int | 否 | 本地磁盘保留天数。0 或省略等于 retention_days，范围 1-3650 |
| storage.bucket_retention_days | int | 否 | 桶和目录行保留天数。0 或省略等于 retention_days。短于本地保留时 HTTP 400 |

校验失败返回 HTTP 400 JSON `{"error","code"}`，**不会落库**。`source.host/port/user/password` 必填；`FILE_POS` 必须带 file/pos；`GTID` 必须带 `gtid_set`（或别名 `gtid`）。不要假设 400 之后任务不存在——实现上 400 就是没写入。

成功时 HTTP 201，正文是任务对象。新建任务的 `state` 是 `CREATED`。`id` 是十进制字符串。`source.password` 不返回。`source.flavor` 省略时写成 `mysql`。`last_error`、`owner_worker_id`、`epoch`、`run_id` 为空时不出现。

**响应示例：**

```json
{
  "id": "1",
  "name": "backup-mysql-prod",
  "cluster_key": "prod-cluster",
  "state": "CREATED",
  "source": {
    "host": "10.0.0.1",
    "port": 3306,
    "user": "repl",
    "flavor": "mysql",
    "server_id": 0
  },
  "start": {
    "mode": "LATEST"
  },
  "storage": {
    "retention_days": 30
  },
  "updated_at": "2024-01-01T10:00:00Z"
}
```

### 3.2 批量创建任务

```bash
curl -X POST http://localhost:8080/api/tasks/batch \
  -H "Content-Type: application/json" \
  -d '{
    "items": [
      {
        "name": "backup-mysql-a",
        "cluster_key": "cluster-a",
        "source": {"host": "10.0.0.1", "port": 3306, "user": "repl", "password": "secret"},
        "start": {"mode": "LATEST"},
        "storage": {"retention_days": 30}
      },
      {
        "name": "backup-mysql-b",
        "cluster_key": "cluster-b",
        "source": {"host": "10.0.0.2", "port": 3306, "user": "repl", "password": "secret"},
        "start": {"mode": "LATEST"},
        "storage": {"retention_days": 30}
      }
    ]
  }'
```

`items` 必须是 1..100 个现有创建请求。缺失、空数组、非数组、JSON 格式错误或超过 100 个时，接口返回 HTTP 400 `{"error","code"}`，且不会创建任何任务。合法 envelope 按顺序逐项校验和创建；单项失败不会阻塞其他项。

响应为有序数组，每项包含 `index`、`cluster_key`，成功时包含已脱敏的 `task`，失败时包含结构化 `error`（`{"error":"...","code":"INVALID_REQUEST"}`）。

### 3.3 列出任务

```bash
# 第一页，默认 limit=100、offset=0
curl http://localhost:8080/api/tasks

# 按状态过滤，offset 是第二页
curl "http://localhost:8080/api/tasks?state=RUNNING&limit=100&offset=100"

# 按源库过滤。FAILED 是失败状态
curl "http://localhost:8080/api/tasks?host=10.0.0.1&port=3306&state=FAILED"
```

处理函数读的查询参数只有下面这些。`cluster_key` 不参与过滤。

**查询参数：**

| 参数 | 类型 | 说明 |
|------|------|------|
| host | string | 源库主机。与任务里保存的字符串相同（区分大小写），或者两边都是环回地址时算同一源：`localhost`、`127.0.0.0/8`、`::1`（含方括号 IPv6）。其它主机按保存的拼写精确匹配，不解析 DNS |
| port | int | 源库端口，1–65535，精确匹配，可以单独传。非法值 HTTP 400，正文 `invalid port` |
| state | string | 精确匹配一个状态，区分大小写。见下表。其它值（包括 `PENDING`、`ERROR`）HTTP 400，正文 `invalid state` |
| limit | int | 页大小。省略时 100。必须是 1–500。0、负数、大于 500 或非整数是 HTTP 400，正文 `invalid limit`，不会改成默认值 |
| offset | int | 从 0 开始的偏移。省略时 0。负数或非整数是 HTTP 400，正文 `invalid offset` |

这些 400 和读取失败时的 500 都是纯文本，不是第 9 节的 `{"error","code"}`。

**任务状态：**

| 值 | 说明 |
|----|------|
| CREATED | 已创建，尚未启动 |
| STARTING | 已进入启动流程 |
| RUNNING | 正在拉取并落盘 |
| LEASE_DEGRADED | 租约续约异常，仍在 grace 窗口内 |
| REBUILDING_FILE | failover 后正在重建当前 binlog 文件 |
| RETRY_BACKOFF | 可重试错误，退避等待，租约仍由当前 worker 持有。源不可达连续 10 次后变为 FAILED。瞬时元数据错误、`OBJECT_PURGE_FAILED`、没有已存 GTID 的 MySQL 1236 留在此状态 |
| FAILED | 不可恢复错误，已停止，租约已放开。`last_error` 以稳定错误码开头，例如 `SOURCE_ACCESS_DENIED`、`SOURCE_UNREACHABLE`、`SOURCE_LOG_BIN_OFF`、`SOURCE_IDENTITY_UNAVAILABLE`、`SEGMENT_NOT_ON_WORKER`、`SEALED_FILE_EXISTS`、`CHECKPOINT_WRITE_FAILED`、`SOURCE_SWITCHOVER` |
| STOPPING | 已收到停止请求，等待退出 |
| STOPPED | 执行路径已退出 |

**响应示例：** HTTP 200。`total` 是过滤后的任务数，不是本页条数。`items` 是一页。有元数据库时顺序是 `ORDER BY CAST(id AS UNSIGNED), id`，所以 `2` 在 `10` 前面。没有元数据库时，数字 `id` 按数值升序排在前面，非数字 `id` 按字符串排在后面。没有匹配项时 `items` 是 `[]`。每一项与 `GET /api/tasks/{id}` 是同一个任务对象，密码不返回。没有配置 `meta_dsn` 时，目录里仍有封存分段或 `.open.e*` 的 `{data_dir}/{id}` 也会出现在这一页：`id` 是目录名，`state` 是 `STOPPED`，没有源库账号。认领见 3.8。

```json
{
  "items": [
    {
      "id": "1",
      "name": "backup-mysql-prod",
      "cluster_key": "prod-cluster",
      "state": "RUNNING",
      "owner_worker_id": "worker-1",
      "epoch": 1,
      "source": {
        "host": "10.0.0.1",
        "port": 3306,
        "user": "repl",
        "flavor": "mysql",
        "server_id": 0
      },
      "start": {
        "mode": "LATEST"
      },
      "storage": {
        "retention_days": 30
      },
      "updated_at": "2024-01-01T10:05:00Z"
    }
  ],
  "total": 1,
  "limit": 100,
  "offset": 0
}
```

### 3.4 获取任务详情

```bash
curl http://localhost:8080/api/tasks/{task_id}
```

HTTP 200，正文与列表里的单个 `items` 元素相同。密码不返回。`last_error`、`owner_worker_id`、`epoch`、`run_id` 为空时不出现。任务不存在是 HTTP 404，正文 `task not found`。

列表、dashboard 的 `tasks[].task` 和这条详情在发现重拉损坏时多一个 `storage_alert`：`code` 是 `STORAGE_INCONSISTENT`，`message` 说明某一个分段里有指向自己或更早文件的 Rotate，或事件位点在这个分段里变小，并写明恢复步骤。`segment` 是第一个损坏的分段文件名。`detail` 是在这个分段里发现了什么（英文）。`missing_gtids` 是 checkpoint 里有、起点集合和任何分段都没有的 MySQL GTID。`valid_segments` 是损坏分段之前的分段，写于损坏之前，仍可恢复。`restart_gtid_set` 是新任务的 GTID 起点（起点集合或第一个分段的 Previous_GTIDs，加上 `valid_segments` 里的事务）。这三个字段算不出来时不出现。GTID 乱序、多个 UUID 交错、序号在别的分段或起点集合里、以及保留造成的文件间缺口，都不设置该字段。缺文件看 `/window` 的 `breaks`。没有这个问题时该字段不出现。它是读目录算出来的，不入库，也没有新的迁移。带 `storage_alert` 时，`/window` 与 `/replay` 用同一个截断点：只统计损坏分段之前的分段，`continuous` 为 false，`breaks` 里多一条 `segment <名字> is damaged (STORAGE_INCONSISTENT) ...`，`files` 列出损坏分段和之后被排除的分段；`earliest`、`latest`、`gtid_set` 也只覆盖这些有效分段。`FAILED` 任务的 `last_error` 若还是旧版本写下的 `STORAGE_INCONSISTENT` 文本，读取时显示为当前 `storage_alert.message`。`/window` 的 `continuous: true` 只说明相邻文件接得上；同一个文件内部的 GTID 序号空洞会让 `continuous` 为 false。checkpoint 的 `gtid_set` 不能当作“这些事务已经在文件里”的证明。

这条任务有 `.source-chain`，或有 `SOURCE_SWITCHOVER` 事件时，多一个 `source_chain`。没有这两样时该字段不出现，已有字段仍在顶层。列表和 `PUT` 不带这个字段。没有新的表，也没有新的配置项。

`source_chain.servers` 按写下文件的顺序。第一台的文件没有身份前缀。后面每一台的文件名是 `{identity}.{binlog 文件名}`。`current` 为 true 的是最后一台，也是 `source_chain.current`。磁盘上的 `.source-chain` 存在时，服务器顺序以它为准。停下来的那次切换不会把新身份追加进这个文件。没有这个文件时，服务器从继续复制的切换里还原：第一条的 `old`，然后每一次 `continued` 为 true 的 `new`。停下的 `new` 不拥有文件。

`source_chain.switches` 是 `SOURCE_SWITCHOVER` 事件，旧的在前。`old`、`new` 是两台身份。`file` 是源 binlog 名，不是磁盘前缀。`pos` 为 0 时不出现。`gtid_set` 为空时不出现。`continued` 总是出现：true 表示任务继续复制，false 表示任务停下。`reason` 只在停下时出现，取值 `no_gtid`、`missing_transactions`、`purged`、`mariadb`、`gtid_unreadable`。认不出原因时不出现。`outcome` 是最近一次切换的 `continued` 或 `stopped`。还没切换时不出现。最多返回最早的 1000 条这类事件。

源库不可达的 Stop 仍返回这条 `STOPPED` 任务，并多一个 `pending_dump_cleanup`：`connection_id`、`host`、`port`、`warning`，schema 3 再加 `process_local: true`。`warning` 是 `source Binlog Dump connection <id> may still be open; will KILL when source is reachable`。`process_local` 为 true 时后面还有一句：只有拉过这条 dump 的那个进程看得到，别的进程和重启都看不到，直到迁移 `000004`。没有残留连接时这个字段不出现。正在拉的 dump 把连接号记成 `held`，这个字段不出现。`RUNNING` 上若还有没确认的残留，字段会出现，任务停在 `RETRY_BACKOFF` 时 `last_error` 是同一句警告，并且不会再开一条 dump。`STOPPED` 之后字段还在。源库恢复后，任何认领到这行的 worker 会 `KILL` 该连接号，成功或该号已不在 processlist（含 `ER_NO_SUCH_THREAD`）后字段消失。集群还在 schema 3 时，别的 worker 不能替这条 dump 收尾或接管，`last_error` 要求先跑迁移 `000004`。半开路径上源库线程能留多久不由这个字段保证，见部署指南里的 TCP 重传说明。DBA 可以按 `connection_id` 手动 `KILL`。

**响应示例：**

```json
{
  "id": "1",
  "name": "backup-mysql-prod",
  "cluster_key": "prod-cluster",
  "state": "RUNNING",
  "owner_worker_id": "worker-1",
  "epoch": 1,
  "source": {
    "host": "10.0.0.1",
    "port": 3306,
    "user": "repl",
    "flavor": "mysql",
    "server_id": 0
  },
  "start": {
    "mode": "LATEST"
  },
  "storage": {
    "retention_days": 30
  },
  "updated_at": "2024-01-01T10:05:00Z",
  "source_chain": {
    "current": "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
    "outcome": "continued",
    "servers": [
      {"identity": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "current": false},
      {"identity": "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", "current": true}
    ],
    "switches": [
      {
        "time": "2024-01-01T10:04:00Z",
        "old": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
        "new": "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
        "file": "mysql-bin.000003",
        "pos": 154,
        "gtid_set": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-20",
        "continued": true
      }
    ]
  }
}
```

没有换过服务器时，响应里没有 `source_chain`。

### 3.5 启动任务

```bash
curl -X POST http://localhost:8080/api/tasks/{task_id}/start
```

成功是 HTTP 204，没有 JSON 正文。任务随后进入 `STARTING`。任务不存在是 HTTP 404，正文 `task not found`。拒绝启动是 HTTP 400 纯文本。

### 3.6 停止任务

```bash
curl -X POST http://localhost:8080/api/tasks/{task_id}/stop
```

成功是 HTTP 204，没有 JSON 正文。任务随后进入 `STOPPING`，退出后是 `STOPPED`。任务不存在是 HTTP 404，正文 `task not found`。拒绝停止是 HTTP 400 纯文本。

### 3.7 删除任务

```bash
curl -X DELETE http://localhost:8080/api/tasks/{task_id}
```

成功是 HTTP 204，没有 JSON 正文。任务不存在是 HTTP 404，正文 `task not found`。拒绝删除是 HTTP 400 纯文本。

### 3.8 认领磁盘上的剩余目录

单机、并且没有配置 `meta_dsn` 时，`{data_dir}/{id}` 里还有封存分段或 `.open.e*` 分段，可以用同一 `id` 把源库身份接上去。这个调用不启动复制。配置了 `meta_dsn` 的进程不会从磁盘认领：这个 `id` 还没有任务行时是 HTTP 404 `task not found`。

```bash
curl -X POST http://localhost:8080/api/tasks/4/adopt \
  -H "Content-Type: application/json" \
  -d '{
    "cluster_key": "adopted-4",
    "source": {
      "host": "10.0.0.1",
      "port": 3306,
      "user": "repl",
      "password": "secret",
      "flavor": "mysql"
    }
  }'
```

**请求字段：**

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| cluster_key | string | 是 | 与创建任务相同的集群标识 |
| source.host | string | 是 | 源库主机 |
| source.port | int | 是 | 源库端口 |
| source.user | string | 是 | 复制用户 |
| source.password | string | 是 | 复制密码。响应里会清空 |
| source.flavor | string | 否 | 空则 `mysql`。`mariadb` 用于后面的回放客户端 |
| name | string | 否 | 省略时用路径里的 `id` |
| start | object | 否 | 省略时起点是 `FILE_POS`：`file` 是序号最大的那个分段的源文件名（不带 `.open.e*`），`pos` 是该分段的字节大小。传入 `start.mode` 则按创建任务的规则校验，并覆盖这个默认起点 |
| storage.retention_days | int | 否 | 省略时 7，范围 1–3650。省略下面两键时本地和桶都用这个数 |
| storage.local_retention_days | int | 否 | 0 或省略等于 retention_days |
| storage.bucket_retention_days | int | 否 | 0 或省略等于 retention_days。短于本地保留时 HTTP 400 |

非法 JSON 是 HTTP 400 `{"error":"invalid json","code":"INVALID_REQUEST"}`。缺字段、`cluster_key` 冲突、目录里没有可做位点的分段，是 HTTP 400 纯文本，例如 `source.password is required`、`cluster_key already exists`、`task already has metadata`、`on-disk segment has no resume position`。`id` 已经有任务行时是 `task already has metadata`。没有 `meta_dsn`、且目录不存在或里面没有 binlog 分段时，也是 HTTP 404 `task not found`。

成功是 HTTP 200，状态为 `STOPPED`，密码不返回。下面的 `pos` 是当时最高分段的字节数。

```json
{
  "id": "4",
  "name": "4",
  "cluster_key": "adopted-4",
  "state": "STOPPED",
  "source": {
    "host": "10.0.0.1",
    "port": 3306,
    "user": "repl",
    "flavor": "mysql",
    "server_id": 0
  },
  "start": {
    "mode": "FILE_POS",
    "file": "mysql-bin.000004",
    "pos": 154
  },
  "storage": {
    "retention_days": 7
  },
  "updated_at": "2024-01-01T10:00:00Z"
}
```

认领前对这个 `id` 调用启动或更新会得到 HTTP 400 `on-disk backup has no task metadata`。认领成功后再 `POST /api/tasks/4/start`。已有分段留在原目录，下一次启动使用更高的 `.open.e*`。

### 3.9 更新任务

```bash
curl -X PUT http://localhost:8080/api/tasks/{task_id} \
  -H "Content-Type: application/json" \
  -d '{
    "cluster_key": "prod-cluster",
    "name": "backup-mysql-prod",
    "storage": {"retention_days": 1}
  }'
```

`cluster_key` 必填。`name`、`source`、`start`、`storage` 省略则保持原值。`source.password` 省略或空字符串时保留原密码。成功是 HTTP 200，密码不返回。

任务处于 `RUNNING`、`STARTING`、`LEASE_DEGRADED` 或 `RETRY_BACKOFF` 时，这次拉流用的是启动时抄下的源库、起点、保留和 `cluster_key`。这时如果要改这四项里的任何一项，返回 HTTP 400，正文是纯文本：

```text
stop the task before changing source, start, storage, or cluster_key: state RUNNING
```

`state` 后面是当前状态。已经保存的配置不变，`GET /api/tasks/{id}` 仍是这次拉流正在用的值。先 `POST /api/tasks/{id}/stop`，等到 `STOPPED`，再 `PUT`，再 `start`。下次启动从已有 checkpoint 续上，并用新的密码和保留。只改 `name`，或者把源库、起点、保留、`cluster_key` 原样再提交一次，不会被拒绝。`STOPPING`、`STOPPED`、`FAILED`、`CREATED` 可以直接改。`STOPPING` 期间改的密码留给下一次 start。

进程级配置（监听地址、上传端点、租约 TTL、加密密钥）不在这次 `PUT` 里。改那些要重启进程。

## 4. 复制状态 API

### 4.1 获取 Checkpoint

```bash
curl http://localhost:8080/api/tasks/{task_id}/checkpoint
```

**响应示例：**

```json
{
  "task_id": "task-xxx",
  "file": "mysql-bin.000010",
  "pos": 12345,
  "gtid_set": "3e11fa47-71ca-11e1-9e33-c80aa9429562:1-100",
  "updated_at": "2024-01-01T10:05:00Z"
}
```

`file` 和 `pos` 是下一次 Start 续传的位置。GTID 任务的 `gtid_set` 是存下的集合（还没有 checkpoint 行时是起点集合），加上打开分段里已经完整的事务，和 Start 时 runner 用的集合相同。打开分段末尾有没写完的事务、Start 会截掉它时，`pos` 是截断点。这个 GET 只读文件，不截断。存下的行指向别的文件或在本地位置之后时，不附 `gtid_set`。还没有写过 checkpoint 行、但打开分段里已有完整事件时（例如升级上来的任务），返回由分段推出的位置，不返回 `updated_at`。既没有 checkpoint 行、本地也没有可续的分段（例如从未启动过的任务）时是 HTTP 404，正文 `checkpoint not found`；这表示还没有续传位置，Start 会从任务的起点开始。

### 4.2 获取复制状态

```bash
curl http://localhost:8080/api/tasks/{task_id}/replication
```

**响应示例：**

```json
{
  "task_id": "task-xxx",
  "state": "RUNNING",
  "status": "NORMAL",
  "threshold_seconds": 30,
  "has_progress": true,
  "delay_seconds": 0.5,
  "last_event_at": "2024-01-01T10:05:00Z",
  "last_event_file": "mysql-bin.000010",
  "last_event_pos": 12345
}
```

**status 字段：**

| 值 | 说明 |
|----|------|
| NORMAL | 正常，延迟 < 阈值；已在源 tip 时延迟视为 0（即使 `last_event_at` 是旧 event header） |
| DELAYED | 延迟，延迟 >= 阈值 |
| ABNORMAL | 异常，无法获取位点 |
| IDLE | 空闲，长时间无事件 |

## 5. 文件管理 API

### 5.1 列出文件

```bash
curl "http://localhost:8080/api/tasks/1/files?limit=200"
```

查询参数只有 `limit`。省略、小于 1 或不是整数时按 200 处理，不返回 400。上传结果在每条记录的 `upload_state`，没有 `upload_status` 查询参数。

HTTP 200 的正文是 JSON 数组。顺序按源文件序号升序；同一序号先是封存名，再是 `.open.e*` 从小到大。`limit` 保留序号最大的那段窗口，窗口内仍是这个顺序。`file_name` 是源文件名。`file_path` 是清单里的路径，未封存时带 `.open.e*`。任务不存在是 HTTP 404，正文 `task not found`。没有分段时正文是 `[]`。

磁盘扫描没有事件位点，`start_pos` 和 `end_pos` 是 0。目录行带封存时记下的位点。`sealed_at`、`uploaded_at` 没有值时仍会输出，时间是 `0001-01-01T00:00:00Z`。`object_key`、`upload_error`、`checksum` 为空时不出现。

```json
[
  {
    "task_id": "1",
    "file_name": "mysql-bin.000001",
    "file_path": "/data/binlog-server/data/1/mysql-bin.000001",
    "state": "SEALED",
    "size_bytes": 1048576,
    "start_pos": 4,
    "end_pos": 1048576,
    "created_at": "2024-01-01T10:00:00Z",
    "sealed_at": "2024-01-01T10:05:00Z",
    "object_key": "prefix/prod-cluster/source-uuid/mysql-bin.000001",
    "upload_state": "UPLOADED",
    "checksum": "match",
    "uploaded_at": "2024-01-01T10:05:02Z",
    "location": "both"
  }
]
```

`state` 是 `OPEN` 或 `SEALED`。`upload_state` 是 `LOCAL_ONLY`、`UPLOADED` 或 `UPLOAD_FAILED`。`location` 是列出时算出来的，不入库：`local` 表示字节在本机，`bucket` 表示只有已上传对象（`file_path` 是目录路径，磁盘上已经没有这个文件），`both` 表示两边都有。空则不出现。只在桶里的分段仍能下载，回放的 `locations` 与 `paths` 对齐，值为 `bucket` 时先下载再交给 `mysqlbinlog`。

`source_identity` 也是列出时算出来的，不入库。任务还没有服务器链时不出现。第一台身份拥有没有前缀的文件名。后面某一台身份拥有以 `{identity}.` 开头的文件名。`.open.e*` 和 `.sealed.e*` 先去掉再比较。空则不出现。

封存分段到达对象存储并且核对完成时带 `checksum`。`match` 表示桶里的对象与封存字节一致（对象 HEAD 的 ETag）。`mismatch` 表示这次核对已经完成且字节不同，拉流继续，这一行仍是 `UPLOADED`。对象 HEAD 失败时该字段不出现，这一行仍是 `UPLOADED`，不算已校验。没有上传的分段也不带该字段。

### 5.2 下载单个分段

`{name}` 是文件清单里的 basename：有 `file_path` 时用它的最后一段，否则用 `file_name`。封存文件是源文件名，例如 `mysql-bin.000001`。未封存文件带 `.open.e*`。这不是对象键。

```bash
curl -o mysql-bin.000001 http://localhost:8080/api/tasks/1/files/mysql-bin.000001

curl -o mysql-bin.000004.open.e2 \
  http://localhost:8080/api/tasks/1/files/mysql-bin.000004.open.e2
```

HTTP 200 的正文是原始字节。`Content-Type` 是 `application/octet-stream`。`Content-Disposition` 是 `attachment`，`filename` 是这个 basename。`Content-Length` 是打开时看到的长度。任务是 `RUNNING` 或 `STOPPED` 时，本机上的 `.open.e*` 也按打开那一刻的长度返回。

本机 `{data_dir}/{id}/{name}` 存在时只读这个文件。本机没有时，只有已封存、`upload_state` 为 `UPLOADED`、并且 `object_key` 非空的行，才会从已配置的对象存储读取，长度是打开对象时的大小。`LOCAL_ONLY`、`UPLOAD_FAILED`、空的 `object_key`、本机没有的 open 分段、以及没有配置对象存储，都是 HTTP 404，正文 `segment not found on this process`。响应不跟随目录里的 `file_path`，也不为 open 分段编造对象字节。

名字含 `/`、`\` 或 `..`，或者不是单个 basename，是 HTTP 400，正文 `invalid segment name`。任务不存在是 HTTP 404，正文 `task not found`。名字不在文件清单里也是 HTTP 404 `segment not found on this process`。

### 5.3 重试上传

`POST /api/tasks/{id}/files/retry-upload` 调用 `RetryFailedUploads(taskID, limit)`。请求体不读取。不能在 JSON 里提交文件名。

```bash
# 省略 limit 时处理 100 条
curl -X POST http://localhost:8080/api/tasks/1/files/retry-upload

curl -X POST "http://localhost:8080/api/tasks/1/files/retry-upload?limit=100"
```

| 参数 | 类型 | 说明 |
|------|------|------|
| limit | int | 查询参数。省略时 100。允许 1–1000。0、负数、大于 1000 或非整数是 HTTP 400，正文 `invalid limit` |

候选是该任务里 `upload_state` 为 `UPLOAD_FAILED`、且 `file_name` 不含 `.open.e` 的目录行，按 `sealed_at` 从新到旧，最多 `limit` 条。会上传的是已封存行：`sealed_at` 有值，并且 `file_name` 和 `file_path` 都不含 `.open.e`。未封存的候选计入 `skipped`。`file_path` 为空，或 `object_key` 为空，计入 `failed`，行保持 `UPLOAD_FAILED`，`upload_error` 写成 `retry upload skipped: empty file_path` 或 `retry upload skipped: empty object_key`。上传成功的行变成 `UPLOADED`，并按上一节写入 `checksum`。上传失败的行保持 `UPLOAD_FAILED`。这次调用不改变任务状态，也不停止拉流。

本机没有该封存文件时，手动调用仍会尝试上传，失败计入 `failed`。后台补传遇到本机没有的封存文件会跳过，目录行保持原样。配置了对象存储的 worker 会在后台重试已封存的 `UPLOAD_FAILED`；桶恢复后，这些行可以不再调用这个接口就变成 `UPLOADED`。open 分段不会上传。

同一任务的补传正在进行（包括后台那一轮）时，HTTP 409，正文 `upload retry already in progress`。没有配置文件库或对象存储时，HTTP 400，正文 `upload retry is not available`。任务不存在是 HTTP 404，正文 `task not found`。其它失败是 HTTP 500，正文 `internal server error`。这些都是纯文本。

HTTP 200 时四个计数都会出现。`scanned` 是这次拿到的候选行数。

```json
{"scanned": 2, "succeeded": 1, "failed": 0, "skipped": 1}
```

### 5.4 查看上传失败原因

```bash
curl "http://localhost:8080/api/tasks/1/upload-failures/reasons?limit=20"
```

`limit` 省略时 20，允许 1–200。0、负数、大于 200 或非整数是 HTTP 400，正文 `invalid limit`。HTTP 200 的正文是 JSON 数组。相同原因合并成一条，空白收成一个空格，空原因是 `unknown`。按 `count` 从高到低，次数相同则 `latest_time` 较新的在前。任务不存在是 HTTP 404，正文 `task not found`。

```json
[
  {"reason": "Access Denied", "count": 5, "latest_time": "2024-01-01T10:05:00Z"},
  {"reason": "Connection Timeout", "count": 2, "latest_time": "2024-01-01T09:00:00Z"}
]
```

### 5.5 回放窗口

`GET /api/tasks/{id}/replay` 用和文件清单相同的 `limit` 窗口。同一台源上按 binlog 序号升序。VIP 切到另一台服务器之后，先列出更早那台源的文件（按该源最早的 `created_at`），再列出新源的文件；新源自己的 `mysql-bin.000001` 不会排到旧源更高序号的前面。同一序号既有封存名又有 `.open.e*` 时，留下 epoch 最大的那条 open 路径，不返回封存名，也不返回其余 epoch。跨过切换的 `stop_datetime` 或 `stop_gtid` 也按这个顺序给出 `paths` 和 `command`。

```bash
curl http://localhost:8080/api/tasks/1/replay

curl "http://localhost:8080/api/tasks/1/replay?limit=3"
```

任务带 `storage_alert` 时，回放停在损坏分段之前：损坏分段、同一 binlog 序号的其它副本、以及之后的分段都不在 `paths` 里，响应多一个 `warning` 写明排除了哪个分段和新任务的 `gtid_set`。定点恢复（`stop_datetime`）、`stop_gtid`、`start_gtid_set` 和 `/replay/archive` 用同一个排除规则，前三者的响应同样带 `warning`。没有损坏时该字段不出现。

`limit` 与文件清单相同：省略、小于 1 或不是整数时按 200 处理，不返回 400。`paths` 是清单里的 `file_path`，不是 basename。窗口为空时 `paths` 是 `[]`，HTTP 仍是 200。任务不存在是 HTTP 404，正文 `task not found`。

`source.flavor` 为 `mysql` 时，`client` 是 `mysqlbinlog`，`client_hint` 是 `MySQL mysqlbinlog`。`mariadb` 时 `client` 和 `client_hint` 都是 `mariadb-binlog`。其它 flavor（含空字符串）这两个字段是空字符串。

```json
{
  "flavor": "mysql",
  "client": "mysqlbinlog",
  "client_hint": "MySQL mysqlbinlog",
  "paths": [
    "/data/binlog-server/data/1/mysql-bin.000001",
    "/data/binlog-server/data/1/mysql-bin.000002.open.e3"
  ]
}
```

### 5.6 回放窗口的 tar

`GET /api/tasks/{id}/replay/archive` 打包的是上一节同一 `limit` 选出的那些分段，每个源序号一个成员。成员名是 basename，按 `paths` 的顺序，不是主机路径。每个成员的字节与单段下载相同：本机文件优先，包括 `RUNNING` 或 `STOPPED` 时的 `.open.e*`；本机没有时，只读已封存、`UPLOADED`、且 `object_key` 非空的对象。

```bash
curl -o task-1-replay.tar http://localhost:8080/api/tasks/1/replay/archive

curl -o task-1-replay.tar "http://localhost:8080/api/tasks/1/replay/archive?limit=3"
```

HTTP 200 时 `Content-Type` 是 `application/x-tar`，`Content-Disposition` 是 `attachment`，文件名是 `task-{id}-replay.tar`。窗口为空时仍是 HTTP 200，正文是没有成员的 ustar，长度 1024 字节。选出的分段里有一个打不开或读不完时，响应是错误，正文不是半个 tar。任务不存在是 HTTP 404 `task not found`。分段不在本进程上是 HTTP 404 `segment not found on this process`。非法分段名是 HTTP 400 `invalid segment name`。

带 `stop_datetime` 时，这个 tar 的成员是 [5.7](#pitr-replay) 的同一组 basename，不再用 `limit`。带 `stop_gtid` 时，成员是 [5.8](#gtid-replay) 的同一组 basename，不再用 `limit`。带 `start_gtid_set` 时，成员是 [5.9](#executed-gtid-replay) 的同一组 basename，不再用 `limit`。停止位置和 `--exclude-gtids` 只写在 JSON 的 `command` 里。

### 5.7 按时间点选取分段

<a id="pitr-replay"></a>

全量备份已经由 DBA 自己恢复。这一节只选出要应用到某个时间点的 binlog 分段，并给出一条可以直接管道到恢复库的命令。

`GET /api/tasks/{id}/replay` 带上 `stop_datetime` 就走这条路径。`start_datetime` 可选。两个参数都不出现时，仍是上一节的 `limit` 窗口，响应里没有 `command`。

```bash
curl -G -sS "http://localhost:8080/api/tasks/1/replay" \
  --data-urlencode "stop_datetime=2024-01-01 01:30:00"

curl -G -sS "http://localhost:8080/api/tasks/1/replay" \
  --data-urlencode "start_datetime=2024-01-01 00:30:00" \
  --data-urlencode "stop_datetime=2024-01-01 01:30:00"
```

时间是 UTC。`YYYY-MM-DD HH:MM:SS` 和 `YYYY-MM-DDTHH:MM:SS` 按 UTC 理解。带时区的 RFC3339 先换算成 UTC。命令里的时钟也是 UTC，并且已经带 `TZ=UTC`，这样 `mysqlbinlog` / `mariadb-binlog` 用事件头的时间戳比较这两个参数。

每个源序号仍只留一条路径，规则与 5.5 相同：同一序号既有封存名又有 `.open.e*` 时，留下 epoch 最大的 open 路径。这条路径不再先按 `limit` 裁掉序号较小的分段，而是看整份清单。分段要能在本进程打开：本地文件优先；本地没有时，只读封存且 `UPLOADED`、`object_key` 非空的对象。事件头时间落在窗口里才选中。`stop_datetime` 是开区间的右端，与 `--stop-datetime` 相同：时间戳大于等于停止时间的事件不进这条命令。设置了 `start_datetime` 时，左端含这个时刻，与 `--start-datetime` 相同。没有事件时间的分段不进入窗口。

`source.flavor` 的客户端与 5.5 相同。`paths` 按序号升序。`command` 是一条命令：有客户端时以 `TZ=UTC mysqlbinlog` 或 `TZ=UTC mariadb-binlog` 开头，然后是 `--stop-datetime='<UTC>'`，设置了开始时间时还有 `--start-datetime='<UTC>'`，最后是这些路径。

```json
{
  "flavor": "mysql",
  "client": "mysqlbinlog",
  "client_hint": "MySQL mysqlbinlog",
  "paths": [
    "/data/binlog-server/data/1/mysql-bin.000003",
    "/data/binlog-server/data/1/mysql-bin.000004.open.e1"
  ],
  "command": "TZ=UTC mysqlbinlog \\\n  --stop-datetime='2024-01-01 01:30:00' \\\n  /data/binlog-server/data/1/mysql-bin.000003 \\\n  /data/binlog-server/data/1/mysql-bin.000004.open.e1"
}
```

窗口里没有分段时仍是 HTTP 200，`paths` 是 `[]`，`command` 是空字符串，`client` 仍按 flavor 填写。

| 请求 | HTTP | 正文 |
|------|------|------|
| `stop_datetime` 无法解析，或参数在但值为空 | 400 | `invalid stop_datetime` |
| 只给了 `start_datetime` | 400 | `stop_datetime is required` |
| `start_datetime` 无法解析 | 400 | `invalid start_datetime` |
| 开始时间晚于停止时间 | 400 | `start_datetime is after stop_datetime` |
| 任务不存在 | 404 | `task not found` |
| 某个已选序号的分段打不开 | 404 | `segment not found on this process` |

正文是纯文本句子。`GET /api/tasks/{id}/replay/archive` 接受同一对时间参数，成员是上面的 basename。空窗口仍是空 tar。

Console 任务详情在回放命令下面可以填停止时间（和可选的开始时间），生成并复制定点命令，或下载这个窗口的 tar。

要停在某一条事务之前，用 [5.8](#gtid-replay) 的 `stop_gtid`。`stop_datetime` 和 `stop_gtid` 不能同时出现。恢复库已经执行过的 GTID 集合用 [5.9](#executed-gtid-replay) 的 `start_gtid_set`，它和 `start_datetime` 不能同时出现。

### 5.8 停在某个 GTID 之前

<a id="gtid-replay"></a>

同一秒里可以有很多事务。`stop_datetime` 按事件头的秒切开，这一秒里坏事务前面的好事务会一起被丢掉，或者坏事务也被放进去。坏事务的 GTID 已经从 `mysqlbinlog -v`、BinlogViz 或 `SHOW BINLOG EVENTS` 里知道时，用 `stop_gtid`。

`GET /api/tasks/{id}/replay` 带上 `stop_gtid` 就走这条路径。`start_datetime` 仍可选。`limit` 在这条路径上不起作用。两个停止参数都不出现时，仍是 5.5 的 `limit` 窗口，响应里没有 `command`。

```bash
curl -G -sS "http://localhost:8080/api/tasks/1/replay" \
  --data-urlencode "stop_gtid=3e11fa47-71ca-11e1-9e33-c80aa9429562:8"

curl -G -sS "http://localhost:8080/api/tasks/1/replay" \
  --data-urlencode "start_datetime=2024-01-01 01:00:00" \
  --data-urlencode "stop_gtid=3e11fa47-71ca-11e1-9e33-c80aa9429562:8"
```

`stop_gtid` 是一条 MySQL GTID，形状是 `<server_uuid>:<seq>`。UUID 不区分大小写。序号是正整数，不能是 0，不能有前导 0，不能是区间，也不能是 GTID 集合。空字符串、以及既不是这个形状也不是 MariaDB `domain-server-seq` 的字符串，在查任务之前就是 `invalid stop_gtid`。

MariaDB 的形状是 `domain-server-seq`，例如 `0-1-10`。`source.flavor` 为 `mysql` 时，这个形状是 `invalid stop_gtid`。`source.flavor` 不是 `mysql` 时，MySQL 形状和 MariaDB 形状都是 `stop_gtid is not supported for this flavor`。这条路径不解析 MariaDB GTID。

服务端按 binlog 顺序找第一个 `GTID_EVENT`，UUID 和序号都相同才算命中。previous-GTIDs 不是这条事务。找到之后，`paths` 收到这条事件所在的分段为止，包含它前面的分段，不包含后面的分段。每个源序号留下全部分封存分段，再加该序号 epoch 最大的 open。更早的封存路径还在。这条 GTID 在封存文件、`.open.e*`、或只存在于桶里的对象上，用的是同一条规则。本地文件优先。本地没有时，只读封存且 `UPLOADED`、`object_key` 非空的对象。`location` 为 `bucket` 的路径仍是目录里的 `file_path`。先下载，再交给 `mysqlbinlog`。

`command` 以 `TZ=UTC mysqlbinlog` 开头。设置了 `start_datetime` 时先写 `--start-datetime='<UTC>'`。然后是 `--stop-position=<N>`，最后是这些路径。`N` 是这条 GTID 事件在最后一个文件里的起始字节。它不是事件头里的 `end_log_pos`。中部复制进来的分段前面有一段格式描述，这两个数不一样。`mysqlbinlog` 只对命令行上的最后一个文件使用 `--stop-position`，从 `N` 开始的事件不解码。所以这条命令放进该 GTID 之前的每一个事务，不放进该事务，也不放进它后面的事务。

`start_datetime` 与 5.7 一样，含这个时刻。它等于这条 GTID 事件的时间戳时可以一起用。比这条事件的时间戳更晚是 400 `start_datetime is after stop_gtid`。更早的分段如果复制事件都在开始时间之前，不进入 `paths`。这条 GTID 所在的分段留下。

```json
{
  "flavor": "mysql",
  "client": "mysqlbinlog",
  "client_hint": "MySQL mysqlbinlog",
  "paths": [
    "/data/binlog-server/data/1/mysql-bin.000003",
    "/data/binlog-server/data/1/mysql-bin.000004.open.e1"
  ],
  "command": "TZ=UTC mysqlbinlog \\\n  --stop-position=154 \\\n  /data/binlog-server/data/1/mysql-bin.000003 \\\n  /data/binlog-server/data/1/mysql-bin.000004.open.e1"
}
```

| 请求 | HTTP | 正文 |
|------|------|------|
| `stop_gtid` 为空，或既不是 MySQL 形状也不是 MariaDB 形状 | 400 | `invalid stop_gtid` |
| MySQL 任务上的 MariaDB 形状 | 400 | `invalid stop_gtid` |
| UUID 或序号不是这份备份里的一条 `GTID_EVENT` | 400 | `stop_gtid is not in this task's backed-up range` |
| `source.flavor` 不是 `mysql` | 400 | `stop_gtid is not supported for this flavor` |
| `stop_datetime` 和 `stop_gtid` 都出现，含其中一个值为空 | 400 | `stop_datetime and stop_gtid cannot both be set` |
| `start_datetime` 无法解析 | 400 | `invalid start_datetime` |
| `start_datetime` 晚于这条 GTID 事件的时间戳 | 400 | `start_datetime is after stop_gtid` |
| 只给了 `start_datetime` | 400 | `stop_datetime is required` |
| 任务不存在，且 `stop_gtid` 是 MySQL 形状或 MariaDB 形状 | 404 | `task not found` |
| 已选分段打不开 | 404 | `segment not found on this process` |

正文是纯文本句子。`GET /api/tasks/{id}/replay/archive` 接受同一个 `stop_gtid` 和可选的 `start_datetime`。成员是上面的 basename。停止位置不在 tar 里。

Console 任务详情的定点恢复可以填停止 GTID。停止时间和停止 GTID 至少填一个就可以生成命令，也可以下载这个 tar。两个都填时，请求同时带上 `stop_datetime` 和 `stop_gtid`，页面显示 `stop_datetime and stop_gtid cannot both be set`。

已经恢复的全量备份带有已执行 GTID 集合时，用 [5.9](#executed-gtid-replay) 的 `start_gtid_set` 代替 `start_datetime`。`stop_gtid` 的停止语义不变。

### 5.9 从已执行 GTID 集合续上

<a id="executed-gtid-replay"></a>

全量备份（xtrabackup、mysqldump 或 clone）已经在恢复库上还原。备份告诉你它已经包含哪些事务：`xtrabackup_binlog_info` 里的 GTID 集合、mysqldump 里的 `SET @@GLOBAL.GTID_PURGED`，或恢复实例上的 `SELECT @@gtid_executed`。把这个集合交给 `start_gtid_set`，再带上 [5.7](#pitr-replay) 的 `stop_datetime` 或 [5.8](#gtid-replay) 的 `stop_gtid`。返回的分段和命令只应用这个集合里还没有的事务，停在原来的停止点。

`GET /api/tasks/{id}/replay` 带上 `start_gtid_set` 就走这条路径。必须同时给出 `stop_datetime` 或 `stop_gtid`。`limit` 被忽略。`start_datetime` 不能一起出现。只适用于 `source.flavor` 为 `mysql`。

```bash
curl -G -sS "http://localhost:8080/api/tasks/1/replay" \
  --data-urlencode "start_gtid_set=3e11fa47-71ca-11e1-9e33-c80aa9429562:1-20" \
  --data-urlencode "stop_gtid=3e11fa47-71ca-11e1-9e33-c80aa9429562:40"

curl -G -sS "http://localhost:8080/api/tasks/1/replay" \
  --data-urlencode "start_gtid_set=3e11fa47-71ca-11e1-9e33-c80aa9429562:1-20" \
  --data-urlencode "stop_datetime=2024-01-01 01:30:00"
```

集合可以含空格和换行，服务端会收成规范形式。MariaDB 的 `domain-server-seq` 不是 MySQL 集合。

服务端按 binlog 顺序读每个分段里的 `GTID_EVENT`。停止规则与 5.7、5.8 相同：`stop_datetime` 不含这个时刻，`stop_gtid` 停在该事件的起始字节。窗口里的事务如果已经在 `start_gtid_set` 中，不算还要应用。一个分段里、停止点之前的事务全都已经在集合里时，这个分段不进入 `paths`，也不进 tar。还剩事务要应用的分段留下。`location` 为 `bucket` 的对象与本地文件同一条规则。

`command` 以 `TZ=UTC mysqlbinlog` 开头。先写 `--exclude-gtids=<规范集合>`，所以 mysqlbinlog 跳过集合里已有的事务，只放进还没有的事务。`stop_datetime` 时接着写 `--stop-datetime='<UTC>'`。`stop_gtid` 落在最后一个路径所在的文件里时，再写 `--stop-position=<N>`，`N` 仍是该事件的起始字节。停止点所在的文件因为停止点之前没有新事务而被省略时，命令不写 `--stop-position`，前面留下的文件整段应用。不写 `--start-datetime`。

停止点之前的每一条事务都已经在集合里时，仍是 HTTP 200。`paths` 是 `[]`，`command` 是空字符串，`note` 是 `every transaction up to the stop is already in start_gtid_set`。这不是错误，也不会给出一条会重复应用的命令。停止点落在第一条事务上、窗口里本来就没有事务时，`paths` 和 `command` 仍为空，不带这条 `note`。

要应用的第一段如果带有 previous-GTIDs，这个集合必须是 `start_gtid_set` 的子集。无论有没有 previous-GTIDs，该段里每个 UUID 的最小序号 `N` 还要求 `1` 到 `N-1` 已经被 previous-GTIDs、`start_gtid_set` 和更早保留分段里的事务合起来盖住。GTID 拉流会留下源文件开头的 previous-GTIDs，并跳过任务启动前、仍写在同一个源文件里的事务；这些序号不在分段里，必须已经在集合中。盖不住时是 400，正文 `start_gtid_set has a gap before this task's backed-up range`。备份比保留的 binlog 更老、中间缺了事务时必须拒绝，不能静默跳过。集合与段内事务没有交集、但 `1` 到 `N-1` 已经被盖住时，这是从保留范围的开头续上，仍返回这些分段。

```json
{
  "flavor": "mysql",
  "client": "mysqlbinlog",
  "client_hint": "MySQL mysqlbinlog",
  "paths": [
    "/data/binlog-server/data/1/mysql-bin.000004.open.e1"
  ],
  "command": "TZ=UTC mysqlbinlog \\\n  --exclude-gtids=3e11fa47-71ca-11e1-9e33-c80aa9429562:1-20 \\\n  --stop-position=154 \\\n  /data/binlog-server/data/1/mysql-bin.000004.open.e1"
}
```

| 请求 | HTTP | 正文 |
|------|------|------|
| `start_gtid_set` 为空、只有空白，或不是 MySQL GTID 集合 | 400 | `invalid start_gtid_set` |
| `source.flavor` 不是 `mysql` | 400 | `start_gtid_set is not supported for this flavor` |
| `start_gtid_set` 和 `start_datetime` 都有非空值 | 400 | `start_gtid_set and start_datetime cannot both be set` |
| 给了 `start_gtid_set`，没有 `stop_datetime` 也没有 `stop_gtid` | 400 | `stop_datetime or stop_gtid is required` |
| 集合盖不住要应用的第一段之前的事务 | 400 | `start_gtid_set has a gap before this task's backed-up range` |
| `stop_datetime` 和 `stop_gtid` 都出现 | 400 | `stop_datetime and stop_gtid cannot both be set` |
| `stop_gtid` 无法解析，或不在这份备份里 | 400 | 与 [5.8](#gtid-replay) 相同 |
| 任务不存在，且 `start_gtid_set` 是合法的 MySQL 集合 | 404 | `task not found` |
| 已选分段打不开 | 404 | `segment not found on this process` |

`stop_datetime` 和 `stop_gtid` 同时出现时，先返回 `stop_datetime and stop_gtid cannot both be set`，即使 `start_gtid_set` 也无法解析。`start_gtid_set` 与非空 `start_datetime` 同时出现时，先返回互斥那一句。集合无法解析、并且停止参数也有问题时，返回 `invalid start_gtid_set`。`stop_gtid` 不在备份范围内时，仍返回 `stop_gtid is not in this task's backed-up range`，不先报缺口。

正文是纯文本句子。`GET /api/tasks/{id}/replay/archive` 接受同一个 `start_gtid_set` 和同一个停止参数。成员是上面的 basename。已经全部包含的窗口是空 tar。

Console 任务详情的定点恢复可以填已执行 GTID。停止时间或停止 GTID 仍至少填一个才能生成命令。已执行 GTID 和开始时间都填时，页面显示 `start_gtid_set and start_datetime cannot both be set`。`note` 有文字而 `command` 为空时，页面显示这句说明。

## 6. 事件查询 API

### 6.1 列出任务事件

```bash
# 最近 50 条事件
curl http://localhost:8080/api/tasks/{task_id}/events

# 指定数量
curl "http://localhost:8080/api/tasks/{task_id}/events?limit=100"

# 按类型过滤
curl "http://localhost:8080/api/tasks/{task_id}/events?event_type=TASK_ERROR"
```

**查询参数：**

| 参数 | 类型 | 说明 |
|------|------|------|
| limit | int | 返回数量（默认 50） |
| event_type | string | 按类型过滤 |

**响应示例：**

```json
{
  "events": [
    {
      "id": 1,
      "task_id": "task-xxx",
      "event_type": "TASK_STARTED",
      "message": "task started",
      "detail": "",
      "created_at": "2024-01-01T10:00:00Z"
    }
  ]
}
```

**常见事件类型：**

| 类型 | 说明 |
|------|------|
| TASK_CREATED | 任务创建 |
| TASK_STARTED | 任务启动 |
| TASK_STOPPED | 任务停止 |
| DUMP_CLEANUP_PENDING | Stop 没能 KILL 掉的源库 Binlog Dump，连接号还可能开着 |
| DUMP_CLEANUP_CLEARED | 该连接已 KILL，或源库上已经没有这个号 |
| TASK_ERROR | 任务错误 |
| TASK_FILE_ROTATED | 文件切换 |
| TASK_FILE_UPLOADED | 文件上传成功 |
| TASK_FILE_UPLOAD_FAILED | 文件上传失败 |
| TASK_LEASE_ACQUIRED | 获取租约 |
| TASK_LEASE_LOST | 租约丢失 |
| SOURCE_SWITCHOVER | 任务地址换到了另一台源。`message` 写出旧身份和新身份。能继续时说明从已执行 GTID 集合继续。不能继续时说明原因，并告诉 DBA 对新主库新建任务、保留这份备份。`detail` 是 `old=<旧身份> new=<新身份> gtid_set=<集合> file=<文件> pos=<位点>` |

## 7. 集群管理 API

### 7.1 列出 Workers

```bash
curl http://localhost:8080/api/workers

# 限制返回数量
curl "http://localhost:8080/api/workers?limit=10"
```

**响应示例：**

```json
{
  "workers": [
    {
      "worker_id": "worker-1",
      "session_id": "abc123",
      "role": "worker",
      "host": "10.0.1.1",
      "version": "v0.5.26",
      "status": "ONLINE",
      "expires_at": "2024-01-01T10:10:00Z",
      "updated_at": "2024-01-01T10:00:00Z"
    }
  ]
}
```

### 7.2 集群概览

```bash
curl http://localhost:8080/api/cluster/overview
```

**响应示例：**

```json
{
  "mode": "cluster",
  "workers": {"total": 2, "active": 2},
  "tasks": {
    "total": 10,
    "running": 8,
    "starting": 2,
    "stopped": 0,
    "error": 0
  }
}
```

### 7.3 查看任务 Lease

```bash
curl http://localhost:8080/api/tasks/{task_id}/lease
```

**响应示例：**

```json
{
  "task_id": "task-xxx",
  "worker_id": "worker-1",
  "epoch": 1,
  "expires_at": "2024-01-01T10:05:00Z",
  "is_valid": true
}
```

## 8. 监控端点

### 8.1 健康检查

```bash
curl http://localhost:8080/api/health
```

**响应：**

```json
{"status": "ok"}
```

### 8.2 汇总信息

```bash
curl http://localhost:8080/api/summary
```

**响应示例：**

```json
{
  "tasks_total": 10,
  "tasks_running": 8,
  "tasks_starting": 2,
  "workers_active": 2
}
```

### 8.3 Dashboard 数据

```bash
curl http://localhost:8080/api/dashboard
```

**响应示例：**

```json
{
  "summary": {"total": 10, "running": 8},
  "tasks": [...],
  "sources": [...]
}
```

### 8.4 源库反查

```bash
curl "http://localhost:8080/api/sources/lookup?host=10.0.0.1&port=3306"
```

**响应示例：**

```json
{
  "host": "10.0.0.1",
  "port": 3306,
  "exists": true,
  "count": 2,
  "task_ids": ["task-1", "task-2"]
}
```

### 8.5 Prometheus 指标

```bash
curl http://localhost:8080/metrics
```

返回 Prometheus 格式的指标。`binlog_server_source_switchovers{task_id,outcome}` 是 gauge。`outcome` 是 `continued` 或 `stopped`。值是该任务已保存的 `SOURCE_SWITCHOVER` 事件里，这种结果的条数。每次采集重算。读到的任务两个序列都有，包含 0。没有任务时 `task_id=""` 的两个序列为 0。某一条任务的链读失败时，这一条不发出 0，避免把已经发生的切换盖成没有。告警示例见 [可观测性](../admin/observability.md)。

## 9. 错误响应

**格式：**

```json
{
  "error": "task not found",
  "code": "TASK_NOT_FOUND"
}
```

创建任务、批量创建的校验失败，以及 adopt 的非法 JSON，使用这个 JSON。任务列表的参数错误、启停删除、认领磁盘目录的查找和校验错误、单段下载、补传、回放和回放 tar 的 400/404/409/500，正文是纯文本句子，没有 `code`。各节写了实际句子。

**常见错误码：**

| HTTP 状态码 | 错误码 | 说明 |
|------------|--------|------|
| 400 | INVALID_REQUEST | 请求参数错误 |
| 404 | TASK_NOT_FOUND | 任务不存在 |
| 409 | TASK_ALREADY_EXISTS | 任务已存在 |
| 409 | INVALID_STATE_TRANSITION | 状态转换非法 |
| 500 | INTERNAL_ERROR | 内部错误 |

## 10. 常用调试场景

### 场景 A：确认服务正常

```bash
curl http://localhost:8080/api/health
# 期望：{"status": "ok"}
```

### 场景 B：检查源库是否已配置备份

```bash
curl "http://localhost:8080/api/sources/lookup?host=10.0.0.1&port=3306"
# exists=true：已有任务
# exists=false：尚未配置
```

### 场景 C：检查任务复制延迟

```bash
curl http://localhost:8080/api/tasks/{task_id}/replication
# 关注：status, delay_seconds
```

### 场景 D：上传失败后批量补传

```bash
# 1. 看失败原因。省略 limit 时最多 20 条
curl "http://localhost:8080/api/tasks/1/upload-failures/reasons?limit=20"

# 2. 补传最多 100 条已封存的 UPLOAD_FAILED。没有请求体
curl -X POST "http://localhost:8080/api/tasks/1/files/retry-upload?limit=100"

# 3. 看 scanned、succeeded、failed、skipped，再核对清单里的 upload_state
curl "http://localhost:8080/api/tasks/1/files?limit=200"
```
