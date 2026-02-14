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
  
默认数据目录：`./data`  
可通过环境变量覆盖：`BINLOG_SERVER_DATA_DIR=/path/to/data`

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
- `POST /api/tasks/{id}/start`
- `POST /api/tasks/{id}/stop`
- `GET /healthz`

说明：如果服务启用了 MySQL runner（当前默认启用），任务 `start` 前必须配置有效 `source`。

## 测试

```bash
go test ./...
```
