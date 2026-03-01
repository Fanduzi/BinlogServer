# 任务状态机

本文档详细分析任务的生命周期和状态转换。

## 1. 状态定义

```go
type TaskState string

const (
    StatePending        TaskState = "PENDING"          // 已创建，等待启动
    StateStarting       TaskState = "STARTING"         // 已分发，等待 worker 接管
    StateRunning        TaskState = "RUNNING"          // 正在执行
    StateLeaseDegraded  TaskState = "LEASE_DEGRADED"   // 租约降级
    StateStopping       TaskState = "STOPPING"         // 正在停止
    StateStopped        TaskState = "STOPPED"          // 已停止
    StateError          TaskState = "ERROR"            // 错误状态
)
```

## 2. 状态转换图

```
                                    ┌──────────────┐
                                    │              │
                                    ▼              │
┌─────────┐    Start    ┌───────────┴──┐   Claim   │
│ PENDING │────────────►│   STARTING   │───────────┘
└─────────┘             └──────┬───────┘
     ▲                          │
     │                          │ Acquire Lease
     │                          ▼
     │                    ┌───────────┐
     │                    │  RUNNING  │◄────────────┐
     │                    └─────┬─────┘             │
     │                          │                   │
     │                          │ Renew Fail        │ Recover
     │                          ▼                   │
     │                    ┌───────────┐             │
     │                    │  LEASE_   │─────────────┘
     │                    │  DEGRADED │
     │                    └─────┬─────┘
     │                          │ Grace Exceeded
     │                          │
     │         Stop             ▼
     │    ┌───────────────┬───────────┐
     └────┤   STOPPING    │   ERROR   │
          └───────┬───────┴───────────┘
                  │
                  │ Done
                  ▼
            ┌───────────┐
            │  STOPPED  │
            └───────────┘
```

## 3. 状态详解

### 3.1 PENDING

**含义：** 任务已创建，等待启动指令。

**进入条件：** 调用 `CreateTask()` 创建任务

**退出条件：** 调用 `StartTask()` 启动任务

```go
func (s *Scheduler) CreateTask(ctx context.Context, req CreateTaskRequest) (Task, error) {
    task := Task{
        ID:        generateID(),
        Name:      req.Name,
        State:     StatePending,  // 初始状态
        CreatedAt: time.Now(),
    }

    s.mu.Lock()
    s.tasks[task.ID] = task
    s.persistTaskLocked(task)
    s.mu.Unlock()

    return task, nil
}
```

### 3.2 STARTING

**含义：** 任务已收到启动指令，等待 worker 接管。

**进入条件：** 调用 `StartTask()`

**退出条件：** Worker 通过 `ClaimStartingTasks()` 接管

```go
func (s *Scheduler) StartTask(id string) error {
    s.mu.Lock()
    defer s.mu.Unlock()

    task, exists := s.tasks[id]
    if !exists {
        return ErrTaskNotFound
    }

    // 状态检查
    if task.State != StatePending && task.State != StateStopped && task.State != StateError {
        return fmt.Errorf("cannot start task in state %s", task.State)
    }

    // 更新状态
    task.State = StateStarting
    task.UpdatedAt = time.Now()
    s.tasks[id] = task
    s.persistTaskLocked(task)

    // 记录事件
    s.appendEventLocked(id, "TASK_STARTING", "task start requested", "")

    return nil
}
```

### 3.3 RUNNING

**含义：** Worker 已接管任务，正在拉取 binlog。

**进入条件：** Worker 成功获取租约并启动 Runner

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
        // 执行出错
        task.State = StateError
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

        ok, err := s.leaseManager.Renew(ctx, id, workerID, epoch, ttl)

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
            s.failSafeStop(id, "LEASE_GRACE_EXCEEDED", "lease renew grace exceeded")
            return
        }
    }
}
```

### 3.5 STOPPING

**含义：** 正在停止任务，等待 Runner 退出。

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

    if task.State != StateRunning && task.State != StateLeaseDegraded {
        return fmt.Errorf("cannot stop task in state %s", task.State)
    }

    // 取消 context
    if cancel, ok := s.cancels[id]; ok {
        cancel()
    }

    // 更新状态
    task.State = StateStopping
    s.tasks[id] = task
    s.persistTaskLocked(task)

    return nil
}
```

### 3.6 STOPPED

**含义：** 任务已停止，可以重新启动。

**进入条件：** Runner 正常退出

### 3.7 ERROR

**含义：** 任务执行出错。

**进入条件：** Runner 执行出错（非 context 取消）

## 4. 关键场景

### 4.1 正常启动流程

```
1. 用户: POST /api/tasks
   → 状态: PENDING

2. 用户: POST /api/tasks/{id}/start
   → 状态: STARTING

3. Worker: ClaimStartingTasks()
   → Acquire Lease 成功
   → 状态: RUNNING
   → 启动 Runner
```

### 4.2 正常停止流程

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

### 4.3 故障接管流程

```
1. Worker A 挂了
   → 租约无法续期

2. 租约过期

3. Worker B: ClaimExpiredTasks()
   → Acquire Lease 成功
   → 状态: RUNNING
   → 启动 Runner（从 checkpoint 继续）
```

### 4.4 网络抖动恢复

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

## 5. 并发安全

### 5.1 锁的使用

```go
type Scheduler struct {
    mu      sync.Mutex
    tasks   map[string]Task
    cancels map[string]context.CancelFunc
    runs    map[string]<-chan struct{}
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
| 状态定义 | `internal/tasks/model.go` | `TaskState` |
| 创建任务 | `internal/tasks/scheduler.go` | `CreateTask` |
| 启动任务 | `internal/tasks/scheduler.go` | `StartTask` |
| 停止任务 | `internal/tasks/scheduler.go` | `StopTask` |
| 执行任务 | `internal/tasks/scheduler.go` | `runTask` |
| 续租循环 | `internal/tasks/scheduler.go` | `renewLeaseLoop` |
| 接管任务 | `internal/tasks/scheduler.go` | `ClaimStartingTasks` |

## 7. 本章小结

1. **7 种状态**：PENDING, STARTING, RUNNING, LEASE_DEGRADED, STOPPING, STOPPED, ERROR
2. **单一职责**：Scheduler 是状态转换的唯一入口
3. **并发安全**：使用 mutex 保护共享状态
4. **优雅处理**：LEASE_DEGRADED 状态提供网络抖动容错
