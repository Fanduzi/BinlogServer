# 开发指南概览

本文档从全局视角讲解 Binlog Server 的代码架构，帮助开发者快速理解"代码是怎么跑起来的"。

## 1. 从 main() 到 app.Run()

### 1.1 调用链

```
main()
  │
  └─► cmd.NewRootCommand().Execute()     # Cobra CLI 入口
        │
        └─► RunE: func(cmd, args)         # Cobra 命令处理
              │
              ├─► config.LoadConfig(path) # 加载配置（Viper）
              │
              ├─► signal.NotifyContext()  # 创建信号上下文
              │
              ├─► logging.Setup()         # 初始化日志
              │
              └─► app.New(cfg).Run(ctx)   # 启动应用 ⭐
                    │
                    ├─► 初始化 tracing（可选）
                    ├─► 初始化存储层
                    ├─► 创建 Scheduler
                    ├─► 恢复任务状态
                    └─► 启动角色服务
```

### 1.2 代码位置

```
cmd/binlog-server/
├── main.go           # func main()
└── cmd/
    └── root.go       # Cobra rootCmd 定义

internal/app/
└── app.go            # func (a *App) Run(ctx) - 核心启动逻辑
```

## 2. 配置加载流程

### 2.1 三层优先级

```
┌─────────────────────────────────────────────────────┐
│                    配置优先级                        │
│                                                     │
│   默认值  <  配置文件  <  环境变量        │
│                                                     │
│   例：listen_addr                                   │
│   1. 默认: ":8080"                                  │
│   2. YAML: listen_addr: ":9090"                     │
│   3. ENV:  BINLOG_SERVER_LISTEN_ADDR=":7070"        │
│                                                     │
│   最终值: ":7070"                                   │
└─────────────────────────────────────────────────────┘
```

### 2.2 加载代码（使用 Viper）

```go
// internal/config/config.go
func LoadConfig(path string) (Config, error) {
    v := viper.New()

    // 1. 设置默认值
    v.SetDefault("listen_addr", ":8080")
    v.SetDefault("mode", "standalone")
    // ...

    // 2. 从文件加载
    if path != "" {
        v.SetConfigFile(path)
    } else {
        v.SetConfigName("config")
        v.AddConfigPath(".")
    }
    v.ReadInConfig()

    // 3. 环境变量覆盖
    v.SetEnvPrefix("BINLOG_SERVER")
    v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
    v.AutomaticEnv()

    // 4. 解析到结构体
    var cfg Config
    v.Unmarshal(&cfg)

    // 5. 验证
    cfg.Validate()

    return cfg, nil
}
```

### 2.3 环境变量映射规则

```
配置项: cluster.worker_id
    ↓
环境变量: BINLOG_SERVER_CLUSTER_WORKER_ID

规则: BINLOG_SERVER_ + 大写 + 下划线连接（. 替换为 _）
```

### 2.4 配置结构

```go
type Config struct {
    // 基础
    ListenAddr string
    DataDir    string
    Mode       string  // standalone / cluster

    // 集群配置
    Cluster ClusterConfig

    // 上传配置
    UploadEndpoint  string
    UploadBucket    string
    UploadAccessKey string
    UploadSecretKey string
    UploadRegion    string
    UploadPrefix    string
    UploadUseSSL    bool

    // 日志配置
    Log LogConfig

    // API 认证配置
    API APIConfig

    // HTTP 超时配置
    HTTP HTTPConfig

    // 元数据调用超时配置
    Meta MetaTimeoutConfig

    // OpenTelemetry tracing 配置
    Tracing TracingConfig
}
```

## 3. app.Run() 内部流程

### 3.1 完整流程图

```
app.Run(ctx)
│
├─► 1. 初始化 tracing（如果启用）
│     tracerProvider, shutdownTracing = initTracing(cfg.Tracing)
│
├─► 2. 解析角色
│     controlPlaneEnabled, workerEnabled = resolveRoleMode(cfg)
│
├─► 3. 解析 worker_id（cluster 模式）
│     workerID = resolveClusterWorkerID(cfg)
│     - 优先使用配置值
│     - 否则读取/生成 .worker-id 文件
│
├─► 4. 初始化元数据存储（cluster 模式）
│     mysqlStore = meta.NewMySQLTaskStore(cfg.MetaDSN)
│
├─► 5. 初始化上传器（可选）
│     uploader = upload.NewS3Uploader(cfg.Upload)
│
├─► 6. 创建 Scheduler
│     scheduler = tasks.NewScheduler(
│         WithStore(mysqlStore),
│         WithClusterLeaseManager(leaseStore),
│         WithRunner(runner),
│         ...
│     )
│
├─► 7. 恢复任务状态
│     scheduler.Restore(ctx)
│     - 从数据库加载任务到内存
│
├─► 8. Worker 注册（cluster + worker 角色）
│     mysqlStore.AcquireWorkerRegistration(workerID, sessionID)
│     startWorkerRegistrationRenewLoop(...)
│
├─► 9. 恢复集群任务（cluster + worker 角色）
│     resumeClusterWorkerTasks(scheduler)
│     - 重启 RUNNING/STARTING/RETRY_BACKOFF 状态的任务
│
├─► 10. 启动角色服务
│     │
│     ├─► control-plane: API Server + UI
│     │     apiServer.Run(ctx)
│     │
│     ├─► worker: Claim Loop + Heartbeat
│     │     go scheduler.ClaimLoop(ctx)
│     │     go scheduler.HeartbeatLoop(ctx)
│     │
│     └─► all-in-one: 两者都启动
│
└─► 11. 等待信号，优雅关闭
      <-ctx.Done()
      scheduler.StopAll(shutdownCtx)
```

### 3.2 初始化顺序为什么这样设计？

| 顺序 | 组件 | 原因 |
|------|------|------|
| 1 | Tracing | 后续组件可能需要 tracing |
| 2 | 解析角色 | 决定后续启动哪些服务 |
| 3 | worker_id | 后续注册和租约都依赖它 |
| 4 | 元数据存储 | Scheduler、Lease 都依赖它 |
| 5 | 上传器 | Runner 依赖它 |
| 6 | Scheduler | 核心组件，依赖上述所有 |
| 7 | Restore | 必须在启动服务前加载状态 |
| 8 | Worker 注册 | 必须在执行任务前完成 |
| 9 | 恢复集群任务 | 必须在 Claim Loop 前执行 |
| 10 | 角色服务 | 最后启动，开始对外服务 |

## 4. 核心组件关系

### 4.1 组件依赖图

```
┌─────────────────────────────────────────────────────────────────┐
│                          app.Run                                │
│                                                                 │
│  ┌─────────────┐                                ┌─────────────┐│
│  │   Config    │───────────────────────────────►│  Scheduler  ││
│  └─────────────┘                                └──────┬──────┘│
│                                                        │       │
│  ┌─────────────┐         ┌─────────────┐             │       │
│  │ MySQLStore  │────────►│  LeaseStore │◄────────────┘       │
│  │  (元数据)   │         │   (租约)    │                     │
│  └──────┬──────┘         └─────────────┘                     │
│         │                                                    │
│         │             ┌─────────────┐                        │
│         └────────────►│   Runner    │◄───────────────────────┤
│                       │  (复制)     │                        │
│                       └──────┬──────┘                        │
│                              │                               │
│  ┌─────────────┐             │                               │
│  │  Uploader   │◄────────────┘                               │
│  │  (上传)     │                                             │
│  └─────────────┘                                             │
│                                                              │
│  ┌─────────────┐                                             │
│  │ API Server  │◄──────── Scheduler (提供任务管理 API)        │
│  │  (HTTP)     │                                             │
│  └─────────────┘                                             │
└─────────────────────────────────────────────────────────────────┘
```

### 4.2 组件职责

| 组件 | 职责 | 代码位置 |
|------|------|----------|
| Config | 配置加载和验证（Viper） | `internal/config/` |
| MySQLStore | 任务/事件/文件元数据持久化 | `internal/meta/mysql_*.go` |
| LeaseStore | 租约管理（获取/续租/释放） | `internal/meta/lease_store.go` |
| Scheduler | 任务状态机、分发、调度 | `internal/tasks/scheduler*.go`（多文件） |
| Runner | 实际执行 binlog 复制 | `internal/replication/mysql_runner.go` |
| Uploader | 上传文件到云存储 | `internal/upload/s3_uploader.go` |
| API Server | HTTP API 和 UI | `internal/api/` |

**Scheduler 模块文件结构：**

```
internal/tasks/
├── model.go                   # 核心数据模型：Task、State、配置结构等
├── scheduler.go               # 核心：Scheduler 结构、Option、接口定义
├── scheduler_lifecycle.go     # 生命周期：Start/Stop/Run/Claim
├── scheduler_task_ops.go      # 任务操作：Create/Update/Delete/Configure
├── scheduler_cluster_lease.go # 集群租约：renewLeaseLoop、failSafeStop
├── scheduler_retry_upload.go  # 上传重试：RetryFailedUploads
└── scheduler_observability.go # 可观测性：Prometheus 指标
```

## 5. 两种运行模式

### 5.1 Standalone 模式

```
┌─────────────────────────────────────────┐
│             单进程                       │
│                                         │
│  ┌─────────┐  ┌─────────┐  ┌─────────┐│
│  │   API   │  │Scheduler│  │ Runner  ││
│  │ Server  │  │         │  │         ││
│  └─────────┘  └─────────┘  └─────────┘│
│                                         │
│  内存存储（无持久化）                     │
└─────────────────────────────────────────┘
```

**特点：**
- 无需 MySQL 元数据库
- 任务状态存在内存
- 重启后任务丢失
- 适合开发/测试

### 5.2 Cluster 模式

```
┌─────────────────┐     ┌─────────────────┐
│  Control Plane  │     │     Worker      │
│  ┌───────────┐  │     │  ┌───────────┐  │
│  │API Server │  │     │  │Claim Loop │  │
│  ├───────────┤  │     │  ├───────────┤  │
│  │Scheduler  │  │     │  │Renew Loop │  │
│  │(状态机)   │  │     │  ├───────────┤  │
│  └───────────┘  │     │  │  Runner   │  │
└────────┬────────┘     │  └───────────┘  │
         │              └────────┬────────┘
         │                       │
         └───────────┬───────────┘
                     │
                     ▼
           ┌─────────────────┐
           │  MySQL 元数据库  │
           │  tasks/leases/  │
           │  checkpoints/   │
           └─────────────────┘
```

**特点：**
- 需要 MySQL 元数据库
- 角色分离（API vs 执行）
- 租约机制保证单执行
- 适合生产环境

## 6. 关键代码入口

| 想了解... | 看这个文件 |
|-----------|-----------|
| 程序入口 | `cmd/binlog-server/main.go` |
| CLI 参数 | `cmd/binlog-server/cmd/root.go` |
| 启动流程 | `internal/app/app.go` |
| 配置加载 | `internal/config/config.go` |
| 任务状态机 | `internal/tasks/scheduler*.go`（多文件模块） |
| 复制执行 | `internal/replication/mysql_runner.go` |
| 元数据存储 | `internal/meta/mysql_*.go` |
| 租约管理 | `internal/meta/lease_store.go` |
| HTTP API | `internal/api/server.go` |
| API 鉴权 | `internal/api/auth.go` |

## 7. 阅读建议

**第一次阅读：**

1. `00-overview.md`（本文）- 建立全局认知
2. `startup-flow.md` - 理解启动细节
3. `task-state-machine.md` - 理解任务生命周期
4. `replication-flow.md` - 理解数据流

**深入理解：**

5. `metadata-layer.md` - 理解存储设计
6. `cluster-mode.md` - 理解集群机制

**查阅：**

- `../reference/api.md` - API 参考
- `../reference/data-model.md` - 数据模型
