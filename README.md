# binlog_server

Binlog Server MVP（进行中）。

当前已实现：

- 单进程 Go 服务启动骨架
- 任务状态机与内存调度器
- 管理 API（创建任务、列表、启动、停止）
- 健康检查 `/healthz`
- `fsync` 成功后才推进 checkpoint 的可靠性语义

## 运行

```bash
go run ./cmd/binlog-server
```

默认监听地址：`:8080`  
可通过环境变量覆盖：`BINLOG_SERVER_LISTEN_ADDR=127.0.0.1:18080`

## API

- `POST /api/tasks` body: `{"name":"cluster-a"}`
- `GET /api/tasks`
- `POST /api/tasks/{id}/start`
- `POST /api/tasks/{id}/stop`
- `GET /healthz`

## 测试

```bash
go test ./...
```
