# 任务状态机

本文档详细分析任务的生命周期和状态转换。

## 1. 状态定义

```go
type State string

const (
    StateCreated        State = "CREATED"         // 任务已创建但尚未启动
    StateStarting       State = "STARTING"        // 任务已进入启动流程
    StateRunning        State = "RUNNING"         // 正在拉取并持久化 binlog
    StateLeaseDegraded  State = "LEASE_DEGRADED"  // lease 续约异常但在 grace 窗口内
    StateRebuildingFile State = "REBUILDING_FILE" // failover 后重建当前 binlog 文件
    StateRetryBackoff   State = "RETRY_BACKOFF"   // 因可重试错误进入退避等待
    StateFailed         State = "FAILED"          // 因不可恢复错误失败停止
    StateStopping       State = "STOPPING"        // 已收到停止请求，等待收敛
    StateStopped        State = "STOPPED"         // 执行路径已完全退出
)
```

## 2. 状态转换图

```
                                    ┌──────────────────────────────┐
                                    │                              │
                                    ▼        Recover              │
┌─────────┐    Start    ┌───────────┴───┐   ┌──────────┐          │
│ CREATED │────────────►│   STARTING   │──►│ RUNNING  │◄─────────┤
└─────────┘             └───────┬───────┘   └────┬─────┘          │
     ▲                           │                │                │
     │                           │ Acquire Lease  │                │
     │                           │ (cluster)      │ Renew Fail     │
     │                           ▼                ▼                │
     │                     ┌───────────┐   ┌────────────┐          │
     │                     │  (直接    │   │  LEASE_    │──────────┘
     │                     │  启动)    │   │  DEGRADED  │
     │                     └───────────┘   └─────┬──────┘
     │                                           │ Grace Exceeded
     │                                           │
     │         Stop              ┌───────────────┼───────────────┐
     │    ┌──────────────────────┼───────────────┼───────────────┤
     │    │                      ▼               ▼               ▼
     └────┤   STOPPING     │    FAILED    │  REBUILDING   │ RETRY_
          └───────┬────────┴──────────────┴───────────────┤ BACKOFF
                  │                                       │
                  │ Done                                  │ Retry
                  ▼                                       ▼
            ┌───────────┐                         ┌────────────┐
            │  STOPPED  │                         │  STARTING  │
            └───────────┘                         └────────────┘
```

## 3. 状态详解

### 3.1 CREATED

**含义：** 任务已创建，等待启动指令。

**进入条件：** 调用 `CreateTask()` 创建任务

**退出条件：** 调用 `StartTask()` 启动任务

```go
func (s *Scheduler) CreateTask(ctx context.Context, name, clusterKey string) (Task, error) {
    task := Task{
        ID:         generateID(),
        Name:       name,
        ClusterKey: clusterKey,
        State:      StateCreated,  // 初始状态
        UpdatedAt:  time.Now(),
    }

    s.mu.Lock()
    s.tasks[task.ID] = task
    s.persistTaskLocked(task)
    s.mu.Unlock()

    return task, nil
}
```

### 3.2 STARTING

**含义：** 任务已收到启动指令，等待 worker 接管（集群模式）或 runner ready。

**进入条件：** 调用 `StartTask()`

**退出条件：**
- 单机模式：直接进入 RUNNING
- 集群模式：Worker 通过 `ClaimStartingTasks()` 接管，获取租约后进入 RUNNING

```go
func (s *Scheduler) StartTask(id string) error {
    s.mu.Lock()
    defer s.mu.Unlock()

    task, exists := s.tasks[id]
    if !exists {
        return ErrTaskNotFound
    }

    // 状态检查
    if task.State != StateCreated && task.State != StateStopped &&
       task.State != StateFailed && task.State != StateRetryBackoff {
        return fmt.Errorf("cannot start task in state %s", task.State)
    }

    // 更新状态
    task.State = StateStarting
    task.UpdatedAt = time.Now()
    s.tasks[id] = task
    s.persistTaskLocked(task)

    // 集群模式：dispatch-only，等待 worker claim
    // 单机模式：直接启动
    if s.leaseManager == nil {
        go s.runTask(ctx, id, task, make(chan struct{}))
    }

    return nil
}
```

### 3.3 RUNNING

**含义：** Worker 已接管任务，正在拉取 binlog。

**进入条件：**
- 单机模式：StartTask 后直接启动
- 集群模式：成功获取租约并启动 Runner
- 从 LEASE_DEGRADED 恢复续租成功
- 从 REBUILDING_FILE 完成文件重建

**退出条件：**
- 调用 `StopTask()`
- Runner 执行出错
- 租约丢失

```go
func (s *Scheduler) runTask(ctx context.Context, id string, task Task, done chan struct{}) {
    defer close(done)

    // 更新状态为 RUNNING
    s.mu.Lock()
    task.State = StateRunning
    s.tasks[id] = task
    s.persistTaskLocked(task)
    s.mu.Unlock()

    // 执行 Runner
    err := s.runner.Run(ctx, task)

    // 处理执行结果
    s.mu.Lock()
    defer s.mu.Unlock()

    if ctx.Err() != nil {
        // 主动取消，正常停止
        task.State = StateStopped
    } else if err != nil {
        // 执行出错，判断是否可重试
        if isRetryableError(err) {
            task.State = StateRetryBackoff
        } else {
            task.State = StateFailed
        }
        task.LastError = err.Error()
    }
    s.tasks[id] = task
    s.persistTaskLocked(task)
}
```

### 3.4 LEASE_DEGRADED

**含义：** 续租失败，但仍在宽限期内，任务继续执行。

**进入条件：** 续租失败

**退出条件：**
- 续租成功 → RUNNING
- 超过宽限期 → 停止任务

```go
func (s *Scheduler) renewLeaseLoop(ctx context.Context, id, workerID string, epoch int64) {
    var degradedSince time.Time

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
        }

        ok, err := s.leaseManager.Renew(ctx, id, workerID, epoch, time.Now(), ttl)

        if ok {
            // 续租成功，恢复正常
            if !degradedSince.IsZero() {
                degradedSince = time.Time{}
                s.mu.Lock()
                task.State = StateRunning
                s.tasks[id] = task
                s.persistTaskLocked(task)
                s.mu.Unlock()
            }
            continue
        }

        // 续租失败
        if degradedSince.IsZero() {
            degradedSince = time.Now()
            s.mu.Lock()
            task.State = StateLeaseDegraded
            task.LastError = err.Error()
            s.tasks[id] = task
            s.persistTaskLocked(task)
            s.mu.Unlock()
        }

        // 超过宽限期
        if time.Since(degradedSince) >= s.leaseGrace {
            s.failSafeStopLocked(id, "LEASE_GRACE_EXCEEDED", "lease renew grace exceeded")
            return
        }
    }
}
```

### 3.5 REBUILDING_FILE

**含义：** Failover 后正在重建当前 binlog 文件（根据 `rebuild_current_file` 策略）。

**进入条件：** Worker 接管任务后，检测到需要重建文件

**退出条件：** 文件重建完成 → RUNNING

### 3.6 RETRY_BACKOFF

**含义：** 任务因可重试错误（如网络抖动、临时连接失败）进入退避等待。

**进入条件：** Runner 执行出错且错误可重试

**退出条件：** 退避时间结束后自动重新启动 → STARTING

### 3.7 FAILED

**含义：** 任务因不可恢复错误失败停止。

**进入条件：** Runner 执行出错且错误不可重试

**退出条件：** 手动调用 `StartTask()` 重新启动

### 3.8 STOPPING

**含义：** 正在停止任务，等待 Runner 退出（两阶段停止）。

**进入条件：** 调用 `StopTask()`

**退出条件：** Runner 退出 → STOPPED

```go
func (s *Scheduler) StopTask(id string) error {
    s.mu.Lock()
    defer s.mu.Unlock()

    task, exists := s.tasks[id]
    if !exists {
        return ErrTaskNotFound
    }

    if task.State != StateRunning && task.State != StateLeaseDegraded &&
       task.State != StateRetryBackoff && task.State != StateRebuildingFile {
        return fmt.Errorf("cannot stop task in state %s", task.State)
    }

    // 取消 context
    if cancel, ok := s.cancels[id]; ok {
        cancel()
    }

    // 更新状态（第一阶段）
    task.State = StateStopping
    s.tasks[id] = task
    s.persistTaskLocked(task)

    return nil
}
```

### 3.9 STOPPED

**含义：** 任务已停止，可以重新启动。

**进入条件：** Runner 正常退出（context 取消）

## 4. 关键场景

### 4.1 正常启动流程（单机模式）

```
1. 用户: POST /api/tasks
   → 状态: CREATED

2. 用户: POST /api/tasks/{id}/start
   → 状态: STARTING
   → 直接启动 goroutine

3. runTask 开始执行
   → 状态: RUNNING
   → 启动 Runner
```

### 4.2 正常启动流程（集群模式）

```
1. 用户: POST /api/tasks
   → 状态: CREATED

2. 用户: POST /api/tasks/{id}/start
   → 状态: STARTING
   → control-plane 只分发，不执行

3. Worker: ClaimStartingTasks()
   → Acquire Lease 成功
   → 状态: RUNNING
   → 启动 Runner
```

### 4.3 正常停止流程

```
1. 用户: POST /api/tasks/{id}/stop
   → 状态: STOPPING
   → 取消 context

2. Runner: 检测到 ctx.Done()
   → 停止拉取
   → 关闭文件
   → 退出

3. runTask: 检测到 ctx.Err()
   → 状态: STOPPED
```

### 4.4 故障接管流程（集群模式）

```
1. Worker A 挂了
   → 租约无法续期

2. 租约过期

3. Worker B: ClaimExpiredTasks()
   → Acquire Lease 成功
   → 状态: RUNNING
   → 启动 Runner（从 checkpoint 继续）
```

### 4.5 网络抖动恢复

```
1. Worker A 续租失败
   → 状态: LEASE_DEGRADED
   → 继续执行

2. 网络恢复

3. 续租成功
   → 状态: RUNNING

4. 如果超过宽限期仍失败
   → failSafeStop
   → 状态: STOPPED
```

### 4.6 错误重试流程

```
1. Runner 执行出错（可重试错误，如连接超时）
   → 状态: RETRY_BACKOFF
   → 记录错误信息

2. 退避时间结束（指数退避）
   → 状态: STARTING
   → 重新启动

3. 如果错误不可重试
   → 状态: FAILED
   → 等待手动干预
```

## 5. 并发安全

### 5.1 锁的使用

```go
type Scheduler struct {
    mu      sync.Mutex
    tasks   map[string]Task
    cancels map[string]context.CancelFunc
    runs    map[string]chan struct{}
}
```

**原则：**
- 所有对 `tasks` 的读写都需要持锁
- 持锁时间尽量短
- 持锁期间不执行 I/O（如数据库操作）

### 5.2 persistTaskLocked 的特殊处理

```go
func (s *Scheduler) persistTaskLocked(task Task) error {
    if s.store == nil {
        return nil
    }

    // 释放锁再执行 I/O
    s.mu.Unlock()
    err := s.store.UpsertTask(context.Background(), task)
    s.mu.Lock()

    return err
}
```

## 6. 代码位置

| 功能 | 文件 | 函数 |
|------|------|------|
| 状态定义 | `internal/tasks/model.go` | `State` |
| 创建任务 | `internal/tasks/scheduler_task_ops.go` | `CreateTask` |
| 启动任务 | `internal/tasks/scheduler_lifecycle.go` | `StartTask` |
| 停止任务 | `internal/tasks/scheduler_lifecycle.go` | `StopTask` |
| 执行任务 | `internal/tasks/scheduler_lifecycle.go` | `runTask` |
| 续租循环 | `internal/tasks/scheduler_cluster_lease.go` | `renewLeaseLoop` |
| 接管任务 | `internal/tasks/scheduler_lifecycle.go` | `ClaimStartingTasks` |
| 错误重试 | `internal/tasks/scheduler_lifecycle.go` | `runTask` + 退避逻辑 |

## 7. 本章小结

1. **9 种状态**：CREATED, STARTING, RUNNING, LEASE_DEGRADED, REBUILDING_FILE, RETRY_BACKOFF, FAILED, STOPPING, STOPPED
2. **单一职责**：Scheduler 是状态转换的唯一入口
3. **并发安全**：使用 mutex 保护共享状态
4. **优雅处理**：LEASE_DEGRADED 状态提供网络抖动容错
5. **自动恢复**：RETRY_BACKOFF 状态支持可重试错误的自动恢复
6. **两阶段停止**：STOPPING → STOPPED 确保资源正确释放
