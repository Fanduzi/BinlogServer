# 第 9 节：Cluster 模式与角色装配

## 全链路导读

- 全链路定位：集群部署与角色边界层（control-plane/worker/all-in-one）
- 前置阅读：第 0 节、第 1 节
- 学完你应能：独立判断不同部署模式下哪个进程负责 API、哪个负责执行

## 目标

看懂 `standalone` 与 `cluster` 的差异，以及 `control-plane / worker / all-in-one` 三种角色在启动时到底做了什么。

## 核心文件

- `internal/config/config.go`
- `internal/app/app.go`
- `cmd/binlog-server/main.go`

## 先记住结论

1. `standalone`：一个进程里同时有 API 和 runner。
2. `cluster + control-plane`：只提供管理 API/UI，不执行拉流。
3. `cluster + worker`：只执行拉流，不提供管理 API；可选暴露 worker health probe。
4. `cluster + all-in-one`：同时提供 API 和 worker 执行能力。

## 配置加载语义（当前版本）

`LoadConfig` 采用三层优先级：

1. 默认值
2. 配置文件（显式 `--config`，或默认尝试 `./config.yaml`）
3. 环境变量（`BINLOG_SERVER_*`，优先级最高）

关键字段：

1. `mode`: `standalone|cluster`
2. `cluster.role`: `control-plane|worker|all-in-one`
3. `cluster.worker_id`
4. `cluster.worker_health_listen_addr`

## 启动主链路（`App.Run`）

```text
Run(ctx)
  -> resolveRoleMode(cfg)
  -> 组装 scheduler / store / runner
  -> Restore tasks
  -> (worker && cluster) resumeClusterWorkerTasks
  -> (worker && cluster) 启动 heartbeat loop
  -> (worker && cluster) 启动 claim STARTING loop
  -> control-plane enabled ? 启动 API : 仅阻塞等待 ctx.Done
```

你要重点理解两点：

1. `worker-only` 模式不会启动 `/api/*` 管理接口。
2. 在线 worker 能常驻接管 `STARTING` 任务，不再依赖重启触发恢复。

## worker health probe

当满足这两个条件时才启用独立探针：

1. 当前节点是 worker-only
2. 配置了 `cluster.worker_health_listen_addr`

端点：

1. `GET /healthz` -> 200
2. `GET /readyz` -> 200

该服务不注册 `/api/*`，用于容器探针而非管理面。

## 动手练习

1. 用同一份配置分别启动 `control-plane` 与 `worker`。
2. 访问 control-plane 的 `/api/summary`，确认可用。
3. 访问 worker 的 `/healthz` 与 `/api/tasks`，确认前者 200、后者 404。

## 自测问题

1. 为什么 worker-only 不该暴露任务管理 API？
2. 为什么 `cluster.worker_health_listen_addr` 默认是空？
3. `all-in-one` 与 `standalone` 在架构语义上的关键区别是什么？

## 5 分钟最小实操

1. 分别启动 control-plane 与 worker。
2. 验证 control-plane 有 `/api/*`，worker-only 仅 health probe。
3. 记录一次 role 配错时你会先查哪 2 个配置项。

## 本节实战检查

- 对照 `docs/learning/chapter-dod-matrix.md` 的「第 9 节」。
- 完成本节最小证据后再进入下一节。
