# 第 1 节：程序启动与依赖装配

## 目标

看懂服务从进程启动到 HTTP API 可用的完整路径，以及关键依赖是如何装配的。

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
      -> replication.NewMySQLRunner
      -> tasks.NewScheduler
      -> scheduler.Restore
      -> api.NewServer
      -> net.Listen
      -> http.Server.Serve
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

配置策略是“默认值 + 环境变量覆盖”：

1. 默认 `ListenAddr=":8080"`、`DataDir="./data"`。
2. 若存在 `BINLOG_SERVER_LISTEN_ADDR`、`BINLOG_SERVER_DATA_DIR` 就覆盖。
3. 读取可选 `BINLOG_SERVER_META_DSN`。
4. 读取上传参数（endpoint/bucket/key/secret/region/prefix/use_ssl）。

注意：当前实现里 `path` 参数没有使用，这是为了后续扩展配置文件预留接口。

### 3) `app.New`

这里只做轻量初始化：

1. 把 `cfg` 存入 `App`。
2. 创建 `readyCh`（服务真正开始监听后会 close，测试里可用来等服务就绪）。

### 4) `App.Run`（最关键）

`Run` 是“依赖装配 + 启动 + 优雅关闭”的完整实现：

1. 根据 `MetaDSN` 决定是否创建 `MySQLTaskStore`。  
创建后会把它注入 scheduler 的 store/checkpoint/event/file 接口，同时注入 runner 的 checkpoint/file meta 接口。
2. 根据上传配置是否完整，决定是否构建 `S3Uploader` 并注入 runner。
3. 创建 `MySQLRunner`，再创建 `Scheduler` 并注入 runner。
4. 调 `scheduler.Restore(context.Background())` 做启动恢复（任务/位点）。
5. 创建 Gin handler（`api.NewServer(scheduler)`），再包进 `http.Server`。
6. `net.Listen` 绑定地址，写入 `a.addr`，关闭 `readyCh`。
7. 启 goroutine 等待 `ctx.Done()`，调用 `server.Shutdown` 实现优雅停机。
8. `server.Serve(ln)` 阻塞对外服务；若非 `http.ErrServerClosed` 则返回错误。

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
3. 监听地址冲突：`net.Listen` 报错退出。
4. `Restore` 失败：说明元数据恢复路径异常，服务启动中止。

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
