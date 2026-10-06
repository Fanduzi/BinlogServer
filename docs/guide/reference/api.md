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
| FAILED | 不可恢复错误，已停止，租约已放开。`last_error` 以稳定错误码开头，例如 `SOURCE_ACCESS_DENIED`、`SOURCE_UNREACHABLE`、`SOURCE_LOG_BIN_OFF`、`SOURCE_IDENTITY_UNAVAILABLE`、`SEGMENT_NOT_ON_WORKER`、`SEALED_FILE_EXISTS`、`CHECKPOINT_WRITE_FAILED` |
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
  "updated_at": "2024-01-01T10:05:00Z"
}
```

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

`GET /api/tasks/{id}/replay` 用和文件清单相同的 `limit` 窗口，每个源文件序号只返回一条 `file_path`，按序号升序。同一序号既有封存名又有 `.open.e*` 时，留下 epoch 最大的那条 open 路径，不返回封存名，也不返回其余 epoch。

```bash
curl http://localhost:8080/api/tasks/1/replay

curl "http://localhost:8080/api/tasks/1/replay?limit=3"
```

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

带 `stop_datetime` 时，这个 tar 的成员是 [5.7](#pitr-replay) 的同一组 basename，不再用 `limit`。

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

返回 Prometheus 格式的指标。

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
