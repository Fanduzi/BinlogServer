# binlog_server

Binlog Server MVP（进行中）。

当前已实现：

- 单进程 Go 服务启动骨架
- 任务状态机与内存调度器
- 管理 API（创建任务、列表、启动、停止）
- 健康检查 `/healthz`
- `fsync` 成功后才推进 checkpoint 的可靠性语义
- MySQL 复制协议拉流（`LATEST/FILE_POS/GTID` 起点）

## 运行

```bash
go run ./cmd/binlog-server
```

默认监听地址：`:8080`  
可通过环境变量覆盖：`BINLOG_SERVER_LISTEN_ADDR=127.0.0.1:18080`

启动后打开管理台：`http://127.0.0.1:8080/ui/`
  
默认数据目录：`./data`  
可通过环境变量覆盖：`BINLOG_SERVER_DATA_DIR=/path/to/data`

可选元数据 MySQL DSN：`BINLOG_SERVER_META_DSN=user:pass@tcp(127.0.0.1:3306)/binlog_meta?parseTime=true`

可选上传（S3/OBS 兼容）：
- `BINLOG_SERVER_UPLOAD_ENDPOINT`
- `BINLOG_SERVER_UPLOAD_BUCKET`
- `BINLOG_SERVER_UPLOAD_ACCESS_KEY`
- `BINLOG_SERVER_UPLOAD_SECRET_KEY`
- `BINLOG_SERVER_UPLOAD_REGION`（可选）
- `BINLOG_SERVER_UPLOAD_PREFIX`（可选）
- `BINLOG_SERVER_UPLOAD_USE_SSL=true|false`

## API

- `POST /api/tasks` body 示例：

```json
{
  "name": "cluster-a",
  "source": {
    "host": "127.0.0.1",
    "port": 3306,
    "user": "repl",
    "password": "secret",
    "flavor": "mysql",
    "server_id": 200001
  },
  "start": {
    "mode": "LATEST"
  },
  "storage": {
    "retention_days": 7
  }
}
```

- `POST /api/tasks` 指定 FILE/POS 示例：

```json
{
  "name": "cluster-a",
  "source": {
    "host": "127.0.0.1",
    "port": 3306,
    "user": "repl",
    "password": "secret",
    "flavor": "mysql"
  },
  "start": {
    "mode": "FILE_POS",
    "file": "mysql-bin.000123",
    "pos": 4
  }
}
```

- `POST /api/tasks` 指定 GTID 示例：

```json
{
  "name": "cluster-a",
  "source": {
    "host": "127.0.0.1",
    "port": 3306,
    "user": "repl",
    "password": "secret",
    "flavor": "mysql"
  },
  "start": {
    "mode": "GTID",
    "gtid_set": "24BC785E-9A61-11E1-8A5D-080027635EF5:1-10"
  }
}
```

- `GET /api/tasks`
- `GET /api/tasks/{id}`
- `PUT /api/tasks/{id}`
- `DELETE /api/tasks/{id}`
- `POST /api/tasks/{id}/start`
- `POST /api/tasks/{id}/stop`
- `GET /api/tasks/{id}/checkpoint`
- `GET /api/tasks/{id}/events`
- `GET /api/tasks/{id}/files`
- `GET /healthz`

说明：如果服务启用了 MySQL runner（当前默认启用），任务 `start` 前必须配置有效 `source`。
`storage.retention_days` 默认 7 天，runner 会在打开 binlog 文件时清理过期文件（跳过当前活动文件）。

如果配置了 `BINLOG_SERVER_META_DSN`，任务配置与状态会持久化到外部 MySQL，服务重启后会自动恢复任务元数据。
同时会持久化每个任务的最新 checkpoint（`file/pos`），重启后优先从 checkpoint 位点继续拉取。
任务事件（创建、启动、重试、错误等）也会持久化到 MySQL，可通过 `/api/tasks/{id}/events` 查询。
开启上传后，binlog rotate 封口后会上传到对象存储，object key 规则：`<prefix>/<taskID>/<fileName>`（prefix 可空）。

## 测试

```bash
go test ./...
```
