# 启动流程

本文档详细分析 Binlog Server 的启动流程，从 `main()` 到各组件初始化。

## 1. 启动流程概览

```
main()
  │
  ├─► 加载配置（config.LoadConfig）
  │
  ├─► 初始化日志（logging.Setup）
  │
  ├─► 创建信号上下文（signal.NotifyContext）
  │
  └─► app.New(cfg).Run(ctx)
        │
        ├─► 初始化 tracing（可选）
        │
        ├─► 初始化元数据库（meta.NewMySQLTaskStore）
        │
        ├─► 初始化上传器（upload.NewS3Uploader）
        │
        ├─► 创建 Scheduler
        │     └─► scheduler.Restore() 加载任务到内存
        │
        ├─► 恢复集群任务（resumeClusterWorkerTasks）
        │
        ├─► 根据角色启动服务
        │     ├─► control-plane: API + UI
        │     ├─► worker: Claim Loop + Heartbeat
        │     └─► all-in-one: 全部
        │
        └─► 等待信号，优雅关闭
```

## 2. 入口函数

### 2.1 main.go

```go
// cmd/binlog-server/main.go
func main() {
    // Cobra CLI 处理命令行参数
    if err := cmd.NewRootCommand().Execute(); err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
}
```

### 2.2 rootCmd

```go
// cmd/binlog-server/cmd/root.go
func NewRootCommand() *cobra.Command {
    var configPath string

    root := &cobra.Command{
        Use:   "binlog-server",
        Short: "MySQL binlog backup server",
        RunE: func(cmd *cobra.Command, _ []string) error {
            // 1. 加载配置
            cfg, err := config.LoadConfig(configPath)
            if err != nil {
                return fmt.Errorf("load config: %w", err)
            }

            // 2. 初始化日志
            _, _, err = logging.Setup(context.Background(), cfg.Log)
            if err != nil {
                return fmt.Errorf("setup logging: %w", err)
            }

            // 3. 创建信号上下文
            ctx, stop := signal.NotifyContext(context.Background(),
                syscall.SIGINT, syscall.SIGTERM)
            defer stop()

            // 4. 启动应用
            return app.New(cfg).Run(ctx)
        },
    }

    root.Flags().StringVarP(&configPath, "config", "c", "", "config file path")
    return root
}
```

## 3. 配置加载

### 3.1 配置优先级（使用 Viper）

```go
// internal/config/config.go
func LoadConfig(path string) (Config, error) {
    v := viper.New()

    // 1. 设置默认值
    v.SetDefault("listen_addr", ":8080")
    v.SetDefault("mode", "standalone")
    v.SetDefault("cluster.role", "all-in-one")
    // ...

    // 2. 从文件加载
    if path != "" {
        v.SetConfigFile(path)
    } else {
        v.SetConfigName("config")
        v.AddConfigPath(".")
    }
    if err := v.ReadInConfig(); err != nil {
        if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
            return Config{}, err
        }
        // 配置文件不存在是允许的
    }

    // 3. 环境变量覆盖
    v.SetEnvPrefix("BINLOG_SERVER")
    v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
    v.AutomaticEnv()

    // 4. 解析到结构体
    var cfg Config
    if err := v.Unmarshal(&cfg); err != nil {
        return Config{}, err
    }

    // 5. 验证配置
    if err := cfg.Validate(); err != nil {
        return Config{}, err
    }

    return cfg, nil
}
```

### 3.2 环境变量映射

```
配置项: cluster.worker_id
    ↓
环境变量: BINLOG_SERVER_CLUSTER_WORKER_ID

规则: BINLOG_SERVER_ + 大写 + 下划线连接（. 替换为 _）
```

## 4. app.Run 核心流程

### 4.1 初始化顺序

```go
// internal/app/app.go
func (a *App) Run(ctx context.Context) error {
    log.Printf("starting binlog-server, mode=%s", a.cfg.Mode)

    // 1. 初始化 tracing（如果启用）
    var shutdownTracing func(context.Context) error
    if a.cfg.Tracing.Enabled {
        tracerProvider, shutdown, err := initTracing(a.cfg.Tracing)
        if err != nil {
            return fmt.Errorf("init tracing: %w", err)
        }
        shutdownTracing = shutdown
        defer shutdownTracing(ctx)
    }

    // 2. 解析运行角色
    controlPlaneEnabled, workerEnabled := resolveRoleMode(a.cfg)

    // 3. 解析 worker_id（cluster 模式）
    var workerID string
    if a.cfg.Mode == "cluster" {
        var err error
        workerID, err = resolveClusterWorkerID(a.cfg)
        if err != nil {
            return fmt.Errorf("resolve worker id: %w", err)
        }
    }

    // 4. 初始化元数据存储（cluster 模式）
    var store meta.TaskStore
    var leaseManager tasks.LeaseManager
    var workerStore meta.WorkerStore

    if a.cfg.Mode == "cluster" {
        mysqlStore, err := meta.NewMySQLTaskStore(a.cfg.MetaDSN, ...)
        if err != nil {
            return fmt.Errorf("create meta store: %w", err)
        }
        store = mysqlStore
        leaseManager = mysqlStore
        workerStore = mysqlStore
    }

    // 5. 初始化上传器（可选）
    var uploader upload.Uploader
    if a.cfg.UploadBucket != "" {
        uploader, err = upload.NewS3Uploader(a.cfg)
        if err != nil {
            return fmt.Errorf("create uploader: %w", err)
        }
    }

    // 6. 创建 Scheduler
    schedulerOpts := []tasks.Option{
        tasks.WithStore(store),
        tasks.WithClusterLeaseManager(leaseManager),
        tasks.WithFileUploader(uploader),
        // ...
    }
    scheduler := tasks.NewScheduler(schedulerOpts...)

    // 7. 恢复任务状态（从数据库加载到内存）
    if err := scheduler.Restore(ctx); err != nil {
        return fmt.Errorf("restore tasks: %w", err)
    }

    // 8. 启动角色服务
    // ...
}
```

### 4.2 为什么是这个顺序？

| 顺序 | 组件 | 原因 |
|------|------|------|
| 1 | Tracing | 后续组件可能需要 tracing |
| 2 | 解析角色 | 决定后续启动哪些服务 |
| 3 | worker_id | 后续注册和租约都依赖它 |
| 4 | 元数据存储 | Scheduler、Lease 都依赖它 |
| 5 | 上传器 | Scheduler 依赖它 |
| 6 | Scheduler | 核心组件，依赖存储和上传器 |
| 7 | Restore | 必须在启动服务前加载状态 |
| 8 | 角色服务 | 依赖 Scheduler |

## 5. Scheduler 初始化

### 5.1 构造函数

```go
// internal/tasks/scheduler.go
func NewScheduler(opts ...Option) *Scheduler {
    s := &Scheduler{
        tasks:   make(map[string]Task),
        cancels: make(map[string]context.CancelFunc),
        runs:    make(map[string]chan struct{}),

        // 默认值
        leaseTTL:           15 * time.Second,
        leaseRenewInterval: 5 * time.Second,
        leaseGrace:         30 * time.Second,
    }

    // 应用选项
    for _, opt := range opts {
        opt(s)
    }

    return s
}
```

### 5.2 Option 模式

```go
type Option func(*Scheduler)

func WithStore(store TaskStore) Option {
    return func(s *Scheduler) {
        s.store = store
    }
}

func WithClusterLeaseManager(lm LeaseManager) Option {
    return func(s *Scheduler) {
        s.leaseManager = lm
    }
}

func WithRunner(runner Runner) Option {
    return func(s *Scheduler) {
        s.runner = runner
    }
}

func WithClusterWorkerID(workerID string) Option {
    return func(s *Scheduler) {
        s.workerID = workerID
    }
}

func WithClusterLease(ttl, renewInterval, grace time.Duration) Option {
    return func(s *Scheduler) {
        s.leaseTTL = ttl
        s.leaseRenewInterval = renewInterval
        s.leaseGrace = grace
    }
}
```

**优点：**
- 可选参数清晰
- 易于扩展
- 便于测试（可以注入 mock）

## 6. Restore 流程

### 6.1 恢复逻辑

**重要：** Restore 只是将任务从数据库加载到内存，**不会自动启动任务**。

```go
func (s *Scheduler) Restore(ctx context.Context) error {
    // 单机模式没有持久化，跳过
    if s.store == nil {
        return nil
    }

    // 1. 从数据库加载所有任务
    tasks, err := s.store.ListTasks(ctx)
    if err != nil {
        return fmt.Errorf("list tasks: %w", err)
    }

    s.mu.Lock()
    defer s.mu.Unlock()

    // 2. 加载到内存
    s.tasks = make(map[string]Task, len(tasks))
    for _, task := range tasks {
        s.tasks[task.ID] = task

        // 恢复自增序列
        if n, err := strconv.Atoi(task.ID); err == nil && n > maxSeq {
            maxSeq = n
        }
    }
    s.seq = maxSeq

    return nil
}
```

### 6.2 为什么 Restore 不自动启动任务？

**设计决策：**

1. **职责分离**：Restore 只负责加载状态，启动由专门函数处理
2. **避免竞态**：集群模式下，任务可能已被其他 worker 接管
3. **明确语义**：让调用者明确控制何时恢复任务

## 7. 集群任务恢复

### 7.1 resumeClusterWorkerTasks

在 Worker 角色启动时，需要恢复之前可能正在执行的任务：

```go
// internal/app/app.go
func resumeClusterWorkerTasks(scheduler clusterTaskResumer) resumeClusterStats {
    var stats resumeClusterStats

    for _, task := range scheduler.ListTasks() {
        switch task.State {
        case tasks.StateRunning,
             tasks.StateStarting,
             tasks.StateRetryBackoff,
             tasks.StateLeaseDegraded:

            stats.Considered++

            // 两步恢复：先停止再启动
            scheduler.StopTask(task.ID)
            scheduler.StartTask(task.ID)
            stats.Resumed++
        }
    }

    return stats
}
```

### 7.2 为什么需要两步恢复？

1. **状态重置**：确保任务从干净的状态开始
2. **租约重新获取**：让当前 worker 尝试获取租约
3. **避免双写**：如果其他 worker 已接管，获取租约会失败

## 8. Worker 注册机制

### 8.1 注册流程

```go
// Worker 启动时注册
func (a *App) startWorkerServices(ctx context.Context, workerID string, store meta.WorkerStore) {
    // 1. 获取注册
    sessionID := uuid.New().String()
    err := store.AcquireWorkerRegistration(ctx, workerID, sessionID, a.cfg.Cluster.WorkerRegistrationTTL)
    if err != nil {
        log.Printf("failed to acquire worker registration: %v", err)
        return
    }

    // 2. 启动续约循环
    go a.workerRegistrationRenewLoop(ctx, workerID, sessionID, store)
}
```

### 8.2 续约循环

```go
func (a *App) workerRegistrationRenewLoop(ctx context.Context, workerID, sessionID string, store meta.WorkerStore) {
    ticker := time.NewTicker(a.cfg.Cluster.WorkerRegistrationRenewInterval)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            err := store.RenewWorkerRegistration(ctx, workerID, sessionID, time.Now(), a.cfg.Cluster.WorkerRegistrationTTL)
            if err != nil {
                log.Printf("worker registration renew failed: %v", err)
            }
        }
    }
}
```

## 9. 角色启动

### 9.1 control-plane

```go
if controlPlaneEnabled {
    // 启动 API 服务
    apiServer := api.NewServer(scheduler, apiOptions...)
    go apiServer.Run(ctx)
}
```

### 9.2 worker

```go
if workerEnabled {
    // 1. 注册 worker
    if err := startWorkerServices(ctx, workerID, workerStore); err != nil {
        return fmt.Errorf("start worker services: %w", err)
    }

    // 2. 恢复集群任务
    resumeClusterWorkerTasks(scheduler)

    // 3. 启动 Claim Loop（认领 STARTING 任务）
    go scheduler.ClaimLoop(ctx)

    // 4. 启动 Heartbeat Loop（发送心跳）
    go scheduler.HeartbeatLoop(ctx)
}
```

### 9.3 all-in-one

```go
// 两者都启动
if controlPlaneEnabled && workerEnabled {
    // 1. 启动 API 服务
    go apiServer.Run(ctx)

    // 2. Worker 服务
    startWorkerServices(ctx, workerID, workerStore)
    resumeClusterWorkerTasks(scheduler)
    go scheduler.ClaimLoop(ctx)
    go scheduler.HeartbeatLoop(ctx)
}
```

## 10. 优雅关闭

### 10.1 信号处理

```go
func (a *App) Run(ctx context.Context) error {
    // ... 初始化 ...

    // 等待信号
    <-ctx.Done()
    log.Println("shutting down...")

    // 给各组件 30 秒时间清理
    shutdownCtx, cancel := context.WithTimeout(
        context.Background(), 30*time.Second)
    defer cancel()

    // 停止所有任务
    scheduler.StopAll(shutdownCtx)

    // 关闭 tracing
    if shutdownTracing != nil {
        shutdownTracing(shutdownCtx)
    }

    return nil
}
```

### 10.2 StopAll 实现

```go
func (s *Scheduler) StopAll(ctx context.Context) error {
    s.mu.Lock()
    defer s.mu.Unlock()

    for id := range s.tasks {
        // 取消 context，通知 goroutine 退出
        if cancel, ok := s.cancels[id]; ok {
            cancel()
        }
    }

    // 等待所有 run goroutine 退出
    for id, done := range s.runs {
        select {
        case <-done:
            // 已退出
        case <-ctx.Done():
            log.Printf("task %s shutdown timeout", id)
        }
    }

    return nil
}
```

## 11. 代码位置

| 组件 | 文件 |
|------|------|
| 入口 | `cmd/binlog-server/main.go` |
| Cobra 配置 | `cmd/binlog-server/cmd/root.go` |
| 配置加载 | `internal/config/config.go` |
| 应用启动 | `internal/app/app.go` |
| Scheduler | `internal/tasks/scheduler*.go`（多文件模块） |

## 12. 本章小结

1. **启动顺序**：配置 → 日志 → Tracing → 存储 → 上传器 → Scheduler → Restore → 角色服务
2. **Option 模式**：灵活的组件配置
3. **Restore**：只加载任务到内存，不自动启动
4. **resumeClusterWorkerTasks**：Worker 启动时恢复活跃任务
5. **Worker 注册**：确保 Worker 身份有效性
6. **优雅关闭**：信号处理 + 超时等待
