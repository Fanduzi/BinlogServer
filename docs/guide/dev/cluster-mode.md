# 集群模式

本文档详细分析集群模式的实现，包括角色分工、Worker 注册、任务分发。

## 1. 集群架构

```
┌─────────────────────────────────────────────────────────────────┐
│                        集群模式架构                              │
│                                                                 │
│  ┌───────────────────┐     ┌───────────────────┐              │
│  │   Control Plane   │     │      Worker       │              │
│  │                   │     │                   │              │
│  │  ┌─────────────┐  │     │  ┌─────────────┐  │              │
│  │  │  API Server │  │     │  │ Claim Loop  │  │              │
│  │  └─────────────┘  │     │  └─────────────┘  │              │
│  │  ┌─────────────┐  │     │  ┌─────────────┐  │              │
│  │  │  Scheduler  │  │     │  │ Renew Loop  │  │              │
│  │  │ (状态机)    │  │     │  └─────────────┘  │              │
│  │  └─────────────┘  │     │  ┌─────────────┐  │              │
│  │  ┌─────────────┐  │     │  │   Runners   │  │              │
│  │  │     UI      │  │     │  │ (复制任务)   │  │              │
│  │  └─────────────┘  │     │  └─────────────┘  │              │
│  └─────────┬─────────┘     └─────────┬─────────┘              │
│            │                         │                         │
│            └────────────┬────────────┘                         │
│                         ▼                                      │
│                ┌─────────────────┐                            │
│                │  MySQL 元数据    │                            │
│                │  - tasks        │                            │
│                │  - leases       │                            │
│                │  - workers      │                            │
│                └─────────────────┘                            │
└─────────────────────────────────────────────────────────────────┘
```

## 2. 角色定义

### 2.1 三种角色

```go
type Role string

const (
    RoleControlPlane Role = "control-plane"  // 管理面
    RoleWorker       Role = "worker"         // 执行面
    RoleAllInOne     Role = "all-in-one"     // 合并
)
```

### 2.2 角色职责

| 角色 | 职责 | 不负责 |
|------|------|--------|
| control-plane | API、状态机、UI | 执行任务 |
| worker | 接管任务、执行复制 | 提供 API |
| all-in-one | 全部 | - |

### 2.3 角色选择

```go
func (app *App) startRoleServices(ctx context.Context) {
    switch app.cfg.Cluster.Role {
    case "control-plane":
        app.startAPIServer(ctx)
    case "worker":
        app.startWorkerLoops(ctx)
    case "all-in-one":
        app.startAPIServer(ctx)
        app.startWorkerLoops(ctx)
    }
}
```

## 3. Worker 注册

### 3.1 注册流程

```
1. Worker 启动，生成 session_id (UUID)
2. 调用 Register API
3. 定期发送 Heartbeat
4. 如果 Heartbeat 失败，主动退出
```

### 3.2 注册实现

```go
func (s *Scheduler) RegisterWorker(ctx context.Context) error {
    sessionID := uuid.New().String()
    expiresAt := time.Now().Add(s.workerTTL)

    reg := WorkerRegistration{
        WorkerID:  s.workerID,
        SessionID: sessionID,
        Role:      s.role,
        ExpiresAt: expiresAt,
    }

    acquired, err := s.workerStore.Register(ctx, reg)
    if err != nil {
        return fmt.Errorf("register worker: %w", err)
    }

    if !acquired {
        return fmt.Errorf("worker_id %s is already in use", s.workerID)
    }

    s.sessionID = sessionID
    return nil
}
```

### 3.3 心跳循环

```go
func (s *Scheduler) HeartbeatLoop(ctx context.Context) {
    ticker := time.NewTicker(s.heartbeatInterval)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
        }

        ok, err := s.workerStore.Heartbeat(ctx, s.workerID, s.sessionID, s.workerTTL)
        if err != nil {
            log.Printf("heartbeat error: %v", err)
        }

        if !ok {
            // session 被其他实例接管，主动退出
            log.Printf("worker session lost, exiting")
            s.sessionLost <- struct{}{}
            return
        }
    }
}
```

## 4. 任务分发

### 4.1 分发流程

```
1. 用户调用 API 启动任务
2. Control Plane 将任务状态改为 STARTING
3. Worker 的 Claim Loop 发现 STARTING 任务
4. Worker 尝试获取租约
5. 成功则启动任务，失败则等待
```

### 4.2 Control Plane 端

```go
func (s *Scheduler) StartTask(id string) error {
    s.mu.Lock()
    defer s.mu.Unlock()

    task, exists := s.tasks[id]
    if !exists {
        return ErrTaskNotFound
    }

    // 只更新状态，不实际启动
    task.State = StateStarting
    task.UpdatedAt = time.Now()
    s.tasks[id] = task

    // 持久化
    if s.store != nil {
        s.persistTaskLocked(task)
    }

    // 记录事件
    s.appendEventLocked(id, "TASK_STARTING", "task start requested", "")

    return nil
}
```

### 4.3 Worker 端 - Claim Loop

```go
func (s *Scheduler) ClaimLoop(ctx context.Context) {
    ticker := time.NewTicker(s.claimInterval)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
        }

        // 1. 接管 STARTING 任务
        s.claimStartingTasks(ctx)

        // 2. 接管过期任务（故障恢复）
        s.claimExpiredTasks(ctx)
    }
}
```

### 4.4 Claim Starting Tasks

```go
func (s *Scheduler) claimStartingTasks(ctx context.Context) (int, error) {
    // 查询 STARTING 状态的任务
    tasks, err := s.store.ListTasksByState(ctx, StateStarting)
    if err != nil {
        return 0, err
    }

    claimed := 0
    for _, task := range tasks {
        // 尝试获取租约
        epoch, acquired, err := s.leaseManager.Acquire(
            ctx, task.ID, s.workerID, s.leaseTTL)
        if err != nil {
            log.Printf("acquire lease failed: %v", err)
            continue
        }

        if !acquired {
            continue  // 被其他 worker 抢了
        }

        // 成功获取租约，启动任务
        task.State = StateRunning
        task.OwnerWorkerID = s.workerID
        task.Epoch = epoch

        done := make(chan struct{})
        ctx, cancel := context.WithCancel(context.Background())

        s.mu.Lock()
        s.cancels[task.ID] = cancel
        s.runs[task.ID] = done
        s.tasks[task.ID] = task
        s.mu.Unlock()

        // 启动 Runner
        go s.runTask(ctx, task.ID, task, done)
        // 启动续租循环
        go s.renewLeaseLoop(ctx, task.ID, s.workerID, epoch)

        claimed++
    }

    return claimed, nil
}
```

### 4.5 Claim Expired Tasks

```go
func (s *Scheduler) claimExpiredTasks(ctx context.Context) (int, error) {
    // 查询租约已过期的任务
    tasks, err := s.store.ListTasksWithExpiredLease(ctx)
    if err != nil {
        return 0, err
    }

    claimed := 0
    for _, task := range tasks {
        // 尝试获取租约
        epoch, acquired, err := s.leaseManager.Acquire(
            ctx, task.ID, s.workerID, s.leaseTTL)
        if err != nil || !acquired {
            continue
        }

        // 接管任务
        log.Printf("taking over expired task %s", task.ID)

        task.State = StateRunning
        task.OwnerWorkerID = s.workerID
        task.Epoch = epoch

        // ... 启动任务（同上）

        claimed++
    }

    return claimed, nil
}
```

## 5. 租约续期

### 5.1 Renew Loop

```go
func (s *Scheduler) renewLeaseLoop(ctx context.Context, id, workerID string, epoch int64) {
    ticker := time.NewTicker(s.leaseRenewInterval)
    defer ticker.Stop()

    var degradedSince time.Time

    for {
        select {
        case <-ctx.Done():
            // 任务停止，释放租约
            s.releaseLease(id, workerID, epoch)
            return
        case <-ticker.C:
        }

        ok, err := s.leaseManager.Renew(ctx, id, workerID, epoch, s.leaseTTL)

        if ok {
            // 续租成功
            if !degradedSince.IsZero() {
                degradedSince = time.Time{}
                s.updateTaskState(id, StateRunning)
            }
            continue
        }

        // 续租失败
        if err != nil {
            log.Printf("renew lease failed: %v", err)
        }

        if degradedSince.IsZero() {
            degradedSince = time.Now()
            s.updateTaskState(id, StateLeaseDegraded)
            s.appendEvent(id, "TASK_LEASE_DEGRADED", "lease renew failed", "")
        }

        // 超过宽限期
        if time.Since(degradedSince) >= s.leaseGrace {
            s.failSafeStop(id, "TASK_LEASE_GRACE_EXCEEDED", "lease renew grace exceeded")
            return
        }
    }
}
```

### 5.2 Fail-Safe Stop

```go
func (s *Scheduler) failSafeStop(id, eventType, message string) {
    s.mu.Lock()
    defer s.mu.Unlock()

    // 取消 context
    if cancel, ok := s.cancels[id]; ok {
        cancel()
        delete(s.cancels, id)
    }

    // 更新状态
    if task, ok := s.tasks[id]; ok {
        task.State = StateStopped
        task.LastError = message
        s.tasks[id] = task
        s.persistTaskLocked(task)
    }

    // 记录事件
    s.appendEventLocked(id, eventType, message, "")

    // 释放租约
    if s.leaseManager != nil && task.OwnerWorkerID != "" {
        s.leaseManager.Release(context.Background(), id, task.OwnerWorkerID, task.Epoch)
    }
}
```

## 6. 时序图

### 6.1 正常启动任务

```
User          Control Plane       MySQL            Worker
 │                  │               │                │
 │  POST /tasks/1/start             │                │
 ├─────────────────►│               │                │
 │                  │  UPDATE tasks │                │
 │                  │  SET STARTING │                │
 │                  ├──────────────►│                │
 │                  │               │                │
 │                  │               │  (Claim Loop)  │
 │                  │               │  SELECT STARTING
 │                  │               │◄───────────────┤
 │                  │               │                │
 │                  │               │  INSERT lease  │
 │                  │               │◄───────────────┤
 │                  │               │      OK        │
 │                  │               ├───────────────►│
 │                  │               │                │
 │                  │               │                │──► Start Runner
 │                  │               │                │──► Start Renew Loop
 │                  │               │                │
 │◄─────────────────┤               │                │
 │   200 OK         │               │                │
```

### 6.2 故障接管

```
Worker A           MySQL            Worker B
   │                 │                 │
   │  (挂了)         │                 │
   X                 │                 │
                     │                 │
   (lease expires)  │                 │
                     │                 │
                     │  (Claim Loop)  │
                     │  SELECT expired│
                     │◄────────────────┤
                     │                 │
                     │  INSERT lease   │
                     │◄────────────────┤
                     │      OK         │
                     ├────────────────►│
                     │                 │
                     │                 │──► Start Runner
                     │                 │    (from checkpoint)
```

## 7. 配置参数

```yaml
cluster:
  role: "worker"              # 角色
  worker_id: "worker-1"       # 唯一标识
  claim_interval: "5s"        # Claim 检查间隔
  heartbeat_interval: "10s"   # 心跳间隔

lease:
  ttl: "30s"                  # 租约有效期
  renew_interval: "10s"       # 续租间隔
  grace_period: "60s"         # 宽限期
```

## 8. 代码位置

| 功能 | 文件 |
|------|------|
| 角色判断 | `internal/app/app.go` |
| Worker 注册 | `internal/app/app.go` |
| Claim Loop | `internal/tasks/scheduler_lifecycle.go` |
| Renew Loop | `internal/tasks/scheduler_cluster_lease.go` |
| LeaseStore | `internal/meta/lease_store.go` |

## 9. 本章小结

1. **角色分工**：control-plane 管理，worker 执行
2. **Worker 注册**：session_id 防止 ID 冲突
3. **任务分发**：STARTING 状态 + Claim Loop
4. **故障接管**：租约过期 + 自动接管
5. **容错机制**：LEASE_DEGRADED + 宽限期
