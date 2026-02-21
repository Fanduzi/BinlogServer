# 01-startup-and-wiring

上级：[[MOC-学习路线]]
来源文件：`docs/learning/01-startup-and-wiring.md`

---

# 第 1 节：程序启动与依赖装配

## 全链路导读

- 全链路定位：系统入口与装配层（把控制面/数据面拼起来）
- 前置阅读：第 0 节
- 学完你应能：准确说出进程启动后依赖如何注入，以及不同 role 如何被装配

## 目标

看懂服务从进程启动到 HTTP API 可用的完整路径，以及关键依赖是如何装配的。

## 更新提示（alpha.2）

本节保留基础装配主线；cluster 角色分离与 worker-only 健康探针请结合第 9 节一起看。

## 代码入口

- `cmd/binlog-server/main.go`
- `internal/app/app.go`
- `internal/config/config.go`

## 一眼看全链路

```text
main.main
  -> config.LoadConfig
  -> signal.NotifyContext
  -> app.New(cfg)
  -> App.Run(ctx)
      -> (可选) meta.NewMySQLTaskStore
      -> (可选) upload.NewS3Uploader
      -> resolveRoleMode (standalone / control-plane / worker / all-in-one)
      -> tasks.NewScheduler + (可选) replication.NewMySQLRunner
      -> scheduler.Restore
      -> (worker && cluster) heartbeat + claim loop
      -> (control-plane) api.NewServer -> net.Listen -> http.Server.Serve
```

如果你只记一件事：`main.go` 只负责“启动流程”，`app.Run` 才是系统装配中心。

## 逐函数讲解

### 1) `main.main`

职责很纯粹，只有四步：

1. 调 `config.LoadConfig("")` 读配置。
2. 用 `signal.NotifyContext` 绑定 `SIGINT/SIGTERM`，实现优雅退出。
3. `app.New(cfg)` 创建应用实例。
4. `Run(ctx)` 阻塞运行，失败直接 `log.Fatalf` 退出。

这里没有业务逻辑，只有进程生命周期管理。

### 2) `config.LoadConfig`

配置策略是“默认值 + 配置文件 + 环境变量覆盖”：

1. 默认 `ListenAddr=":8080"`、`DataDir="./data"`。
2. 若传入 `--config`，先读指定 YAML；否则尝试 `./config.yaml`（不存在则忽略）。
3. 若存在 `BINLOG_SERVER_*` 环境变量则覆盖配置文件值。
4. 读取 cluster 配置（`mode/role/worker_id/worker_health_listen_addr`）。
5. 读取可选 `BINLOG_SERVER_META_DSN` 与上传参数。

注意：当前 `LoadConfig(path)` 中 `path` 已生效，不再是预留参数。

### 3) `app.New`

这里只做轻量初始化：

1. 把 `cfg` 存入 `App`。
2. 创建 `readyCh`（服务真正开始监听后会 close，测试里可用来等服务就绪）。

### 4) `App.Run`（最关键）

`Run` 是“依赖装配 + 启动 + 优雅关闭”的完整实现：

1. 根据 `MetaDSN` 决定是否创建 `MySQLTaskStore`。  
创建后会把它注入 scheduler 的 store/checkpoint/event/file 接口，同时注入 runner 的 checkpoint/file meta 接口。
2. 根据上传配置是否完整，决定是否构建 `S3Uploader` 并注入 runner。
3. 按 role 装配：
   - control-plane：不注入 runner
   - worker/all-in-one：注入 `MySQLRunner`
4. 调 `scheduler.Restore(context.Background())` 做启动恢复（任务/位点）。
5. worker + cluster 下会启动 heartbeat loop 和 claim loop。
6. 若是 worker-only 且配置了 `cluster.worker_health_listen_addr`，会启动独立 health probe 服务。
7. 仅当 control-plane enabled 时才创建 Gin handler 并对外提供管理 API。
8. 启 goroutine 等待 `ctx.Done()`，调用 `server.Shutdown` 实现优雅停机。

### 5) `Ready/Addr`

这是给测试和外部观察用的两个辅助方法：

1. `Ready()` 返回只读 channel，用于等待“已开始监听”。
2. `Addr()` 返回实际监听地址（当端口是 `:0` 动态分配时非常有用）。

## 你要抓住的点

1. 配置来源：默认值 + 环境变量。
2. 依赖注入位置：`app.go` 是“总装线”。
3. 监听地址、数据目录、元数据 DSN 在哪里生效。
4. 恢复动作在 `scheduler.Restore`，不是在 API 层。
5. 优雅停机依赖 `ctx` 取消 + `server.Shutdown`。

## 常见失败点（先建立排错地图）

1. `MetaDSN` 错误：`meta.NewMySQLTaskStore` 会直接失败，服务起不来。
2. 上传参数不完整：不会启用 uploader（但拉流仍可运行）。
3. role 配置不符合预期：会出现“有 API 但不执行”或“执行了但无 API”。
4. 监听地址冲突：`net.Listen` 报错退出。
5. `Restore` 失败：说明元数据恢复路径异常，服务启动中止。

## 动手练习

1. 设置 `BINLOG_SERVER_LISTEN_ADDR=127.0.0.1:18080` 启动后端。
2. 打开 `http://127.0.0.1:18080/healthz`。
3. 设置一个错误的 `BINLOG_SERVER_META_DSN`，观察启动失败点并定位到对应函数。
4. 恢复正确配置后再次启动，确认可恢复服务。

可用命令：

```bash
BINLOG_SERVER_LISTEN_ADDR=127.0.0.1:18080 go run ./cmd/binlog-server
```

```bash
BINLOG_SERVER_META_DSN='bad-dsn' go run ./cmd/binlog-server
```

## 自测问题

1. 如果想新增一个全局组件，应该放在什么文件初始化？
2. 为什么不建议在 handler 内直接创建底层依赖？
3. `Ready()` 和 `Addr()` 这两个方法主要解决了什么测试问题？
4. 如果要支持配置文件 + 环境变量共存，最自然应改哪个函数？

---

## 相关

- [[架构图-Mermaid版]]
- [[部署模式]]
- [[可观测性]]

## 5 分钟最小实操

1. 执行 `go run ./cmd/binlog-server --config ./config.example.yaml`。
2. 执行 `curl -s http://127.0.0.1:8080/healthz`，确认返回 `ok`。
3. 改一次 `listen_addr` 后重启，确认你知道配置在哪一层生效。

## 本节实战检查

- 对照 [[chapter-dod-matrix]] 的「第 1 节」。
- 完成本节最小证据后再进入下一节。
