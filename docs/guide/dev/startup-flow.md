# 启动流程

本文档详细分析 Binlog Server 的启动流程，从 `main()` 到各组件初始化。

## 1. 启动流程概览

```
main()
  │
  ├─► 加载配置（config.Load）
  │
  ├─► 创建信号上下文（signal.NotifyContext）
  │
  └─► app.Run(ctx, cfg)
        │
        ├─► 初始化元数据库（meta.NewMySQLStore）
        │
        ├─► 初始化上传器（upload.NewS3Uploader）
        │
        ├─► 创建 Scheduler
        │     └─► scheduler.Restore() 恢复任务
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
    if err := rootCmd.Execute(); err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
}
```

### 2.2 rootCmd

```go
// cmd/binlog-server/cmd/root.go
var rootCmd = &cobra.Command{
    Use:   "binlog-server",
    Short: "MySQL binlog backup server",
    RunE: func(cmd *cobra.Command, args []string) error {
        // 1. 加载配置
        cfg, err := config.Load(configFile)
        if err != nil {
            return fmt.Errorf("load config: %w", err)
        }

        // 2. 创建信号上下文
        ctx, stop := signal.NotifyContext(context.Background(),
            syscall.SIGINT, syscall.SIGTERM)
        defer stop()

        // 3. 启动应用
        return app.Run(ctx, cfg)
    },
}
```

## 3. 配置加载

### 3.1 配置优先级

```go
// internal/config/config.go
func Load(path string) (*Config, error) {
    cfg := DefaultConfig()

    // 1. 从文件加载
    if path != "" {
        data, err := os.ReadFile(path)
        if err != nil {
            return nil, err
        }
        if err := yaml.Unmarshal(data, cfg); err != nil {
            return nil, err
        }
    }

    // 2. 环境变量覆盖
    if err := loadFromEnv(cfg); err != nil {
        return nil, err
    }

    // 3. 验证配置
    if err := cfg.Validate(); err != nil {
        return nil, err
    }

    return cfg, nil
}
```

### 3.2 环境变量映射

```go
func loadFromEnv(cfg *Config) error {
    // BINLOG_SERVER_LISTEN_ADDR
    if v := os.Getenv("BINLOG_SERVER_LISTEN_ADDR"); v != "" {
        cfg.ListenAddr = v
    }

    // BINLOG_SERVER_META_DSN
    if v := os.Getenv("BINLOG_SERVER_META_DSN"); v != "" {
        cfg.Meta.DSN = v
    }

    // ... 其他配置项
    return nil
}
```

## 4. app.Run 核心流程

### 4.1 初始化顺序

```go
// internal/app/app.go
func Run(ctx context.Context, cfg *config.Config) error {
    log.Printf("starting binlog-server, mode=%s", cfg.Mode)

    // 1. 初始化元数据存储（cluster 模式）
    var store meta.Store
    var leaseManager meta.LeaseManager
    var workerStore meta.WorkerStore

    if cfg.Mode == "cluster" {
        mysqlStore, err := meta.NewMySQLStore(cfg.Meta)
        if err != nil {
            return fmt.Errorf("create meta store: %w", err)
        }
        store = mysqlStore
        leaseManager = mysqlStore
        workerStore = mysqlStore
    }

    // 2. 初始化上传器（可选）
    var uploader upload.Uploader
    if cfg.Upload.Enabled {
        uploader, err = upload.NewS3Uploader(cfg.Upload)
        if err != nil {
            return fmt.Errorf("create uploader: %w", err)
        }
    }

    // 3. 创建 Scheduler
    schedulerOpts := []tasks.Option{
        tasks.WithStore(store),
        tasks.WithLeaseManager(leaseManager),
        tasks.WithUploader(uploader),
        // ...
    }
    scheduler := tasks.NewScheduler(schedulerOpts...)

    // 4. 恢复任务状态
    if err := scheduler.Restore(ctx); err != nil {
        return fmt.Errorf("restore tasks: %w", err)
    }

    // 5. 根据角色启动服务
    // ...
}
```

### 4.2 为什么是这个顺序？

| 顺序 | 组件 | 原因 |
|------|------|------|
| 1 | 元数据存储 | 后续组件依赖它 |
| 2 | 上传器 | Scheduler 依赖它 |
| 3 | Scheduler | 核心组件，依赖存储和上传器 |
| 4 | Restore | 恢复运行中的任务 |
| 5 | 角色服务 | 依赖 Scheduler |

## 5. Scheduler 初始化

### 5.1 构造函数

```go
// internal/tasks/scheduler.go
// 注意：Scheduler 模块已拆分为多个文件（scheduler*.go）
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

func WithStore(store Store) Option {
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
```

**优点：**
- 可选参数清晰
- 易于扩展
- 便于测试（可以注入 mock）

## 6. Restore 流程

### 6.1 恢复逻辑

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

    // 2. 加载到内存
    for _, task := range tasks {
        s.tasks[task.ID] = task
    }

    // 3. 处理 RUNNING 状态的任务
    for _, task := range tasks {
        if task.State == StateRunning {
            // 集群模式：尝试获取租约
            if s.leaseManager != nil {
                epoch, acquired, _ := s.leaseManager.Acquire(
                    ctx, task.ID, s.workerID, s.leaseTTL)
                if !acquired {
                    // 租约被其他 worker 持有，将状态改为 STARTING
                    task.State = StateStarting
                    s.tasks[task.ID] = task
                    continue
                }
                task.Epoch = epoch
                task.OwnerWorkerID = s.workerID
            }

            // 重新启动任务
            go s.runTask(ctx, task.ID, task, make(chan struct{}))
        }
    }

    return nil
}
```

### 6.2 为什么 RUNNING 任务需要特殊处理？

**场景：** 进程重启前有 10 个 RUNNING 任务

**单机模式：**
- 直接重启这些任务（没有其他实例竞争）

**集群模式：**
- 可能其他 worker 已经接管了部分任务
- 需要通过租约机制确认执行权
- 获取失败的任务改为 STARTING，等待其他 worker 接管

## 7. 角色启动

### 7.1 control-plane

```go
if cfg.Cluster.Role == "control-plane" || cfg.Cluster.Role == "all-in-one" {
    // 启动 API 服务
    apiServer := api.NewServer(cfg, scheduler, store)
    go apiServer.Run(ctx)
}
```

### 7.2 worker

```go
if cfg.Cluster.Role == "worker" || cfg.Cluster.Role == "all-in-one" {
    // 注册 worker
    if err := scheduler.RegisterWorker(ctx); err != nil {
        return fmt.Errorf("register worker: %w", err)
    }

    // 启动 Claim Loop
    go scheduler.ClaimLoop(ctx)

    // 启动 Heartbeat Loop
    go scheduler.HeartbeatLoop(ctx)
}
```

## 8. 优雅关闭

### 8.1 信号处理

```go
func Run(ctx context.Context, cfg *config.Config) error {
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

    return nil
}
```

### 8.2 StopAll 实现

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

## 9. 代码位置

| 组件 | 文件 |
|------|------|
| 入口 | `cmd/binlog-server/main.go` |
| Cobra 配置 | `cmd/binlog-server/cmd/root.go` |
| 配置加载 | `internal/config/config.go` |
| 应用启动 | `internal/app/app.go` |
| Scheduler | `internal/tasks/scheduler*.go`（多文件模块） |

## 10. 本章小结

1. **启动顺序**：配置 → 存储 → 上传器 → Scheduler → 角色服务
2. **Option 模式**：灵活的组件配置
3. **Restore**：从数据库恢复任务，处理 RUNNING 状态
4. **优雅关闭**：信号处理 + 超时等待
