# 第 5 章：集群模式解决的问题

到目前为止，我们已经有了：
- 持久化（任务、checkpoint、事件）
- 租约机制（防止重复执行、故障接管）

但要让多个 worker 协同工作，还需要解决几个问题：
1. Worker 身份管理
2. 任务分发与接管
3. 健康状态监控

## 1. Worker 身份管理

### 1.1 问题：Worker ID 冲突

**场景：** 你部署了 2 个 worker，但配置了相同的 `worker_id`。

```
┌─────────────┐     ┌─────────────┐
│  Worker A   │     │  Worker B   │
│  worker_id: │     │  worker_id: │
│  "worker-1" │◄───►│  "worker-1" │  ← 相同的 ID！
└─────────────┘     └─────────────┘
        │                   │
        └───────┬───────────┘
                ▼
        谁在续租？分不清！
```

**后果：**
- 两个 worker 都认为自己是 "worker-1"
- 续租时互相覆盖
- 无法区分是哪个实例在工作

### 1.2 解决方案：Worker Registration

每个 worker 启动时，需要"注册"自己的身份：

```go
type WorkerRegistration struct {
    WorkerID   string    // 配置的 worker ID
    SessionID  string    // 启动时生成的唯一 ID
    Role       string    // worker / control-plane / all-in-one
    ExpiresAt  time.Time // 注册过期时间
    UpdatedAt  time.Time // 心跳更新时间
}
```

**关键点：** `SessionID` 是每次启动时生成的 UUID，用于区分同一 `worker_id` 的不同实例。

### 1.3 注册逻辑

```go
func (s *WorkerStore) Register(ctx context.Context, reg WorkerRegistration) (bool, error) {
    // 检查是否已有相同 worker_id 的活跃注册
    existing, err := s.GetActiveRegistration(ctx, reg.WorkerID)
    if err != nil {
        return false, err
    }

    if existing != nil && existing.SessionID != reg.SessionID {
        // 已有其他 session 持有该 worker_id
        if existing.ExpiresAt.After(time.Now()) {
            // 对方还活着，拒绝注册
            return false, nil
        }
        // 对方已过期，允许接管
    }

    // 插入或更新注册
    _, err = s.db.ExecContext(ctx, `
        INSERT INTO worker_registrations (worker_id, session_id, role, expires_at, updated_at)
        VALUES (?, ?, ?, ?, ?)
        ON DUPLICATE KEY UPDATE
            session_id = VALUES(session_id),
            expires_at = VALUES(expires_at),
            updated_at = VALUES(updated_at)
    `, reg.WorkerID, reg.SessionID, reg.Role, reg.ExpiresAt, reg.UpdatedAt)

    return true, err
}
```

### 1.4 心跳续期

Worker 需要定期发送心跳，更新 `expires_at`：

```go
func (s *WorkerStore) Heartbeat(ctx context.Context, workerID, sessionID string, ttl time.Duration) (bool, error) {
    result, err := s.db.ExecContext(ctx, `
        UPDATE worker_registrations
        SET expires_at = ?, updated_at = ?
        WHERE worker_id = ? AND session_id = ?
    `, time.Now().Add(ttl), time.Now(), workerID, sessionID)

    // 检查是否更新成功
    affected, _ := result.RowsAffected()
    return affected > 0, err
}
```

**如果心跳失败：**
- 可能 session 被 其他实例接管
- Worker 应该主动退出，避免"脑裂"

## 2. 任务分发与接管

### 2.1 两种任务状态流转

**Standalone 模式：**

```
PENDING → STARTING → RUNNING → STOPPING → STOPPED
              ↑
         直接启动
```

**Cluster 模式：**

```
PENDING → STARTING → RUNNING → STOPPING → STOPPED
              ↑           ↑
         分发到队列   Worker 接管
```

### 2.2 STARTING 状态的意义

当 control-plane 把任务状态改为 `STARTING` 时：

```go
// Control-plane 的操作
func (s *Scheduler) StartTask(id string) error {
    s.mu.Lock()
    defer s.mu.Unlock()

    task, exists := s.tasks[id]
    if !exists {
        return ErrTaskNotFound
    }

    // 只改状态，不实际启动
    task.State = StateStarting
    s.tasks[id] = task
    s.persistTaskLocked(task)

    return nil
}
```

**含义：** "这个任务需要启动，谁来执行？"

### 2.3 Worker 的 Claim 循环

Worker 定期检查是否有 `STARTING` 的任务需要接管：

```go
func (s *Scheduler) ClaimStartingTasks() (int, error) {
    // 查找所有 STARTING 状态的任务
    tasks, err := s.store.ListTasksByState(ctx, StateStarting)
    if err != nil {
        return 0, err
    }

    claimed := 0
    for _, task := range tasks {
        // 尝试获取租约
        epoch, acquired, err := s.leaseManager.Acquire(ctx, task.ID, s.workerID, s.leaseTTL)
        if err != nil || !acquired {
            continue  // 被其他 worker 抢了
        }

        // 成功获取租约，启动任务
        s.startTaskWithLease(task, epoch)
        claimed++
    }

    return claimed, nil
}
```

### 2.4 接管已过期任务

除了接管 `STARTING` 任务，Worker 还需要接管"租约已过期"的任务：

```go
func (s *Scheduler) ClaimExpiredTasks() (int, error) {
    // 查找所有 RUNNING 但租约已过期的任务
    tasks, err := s.store.ListTasksWithExpiredLease(ctx)
    if err != nil {
        return 0, err
    }

    for _, task := range tasks {
        // 尝试获取租约
        epoch, acquired, err := s.leaseManager.Acquire(ctx, task.ID, s.workerID, s.leaseTTL)
        if acquired {
            // 接管任务
            s.takeOverTask(task, epoch)
        }
    }

    return claimed, nil
}
```

## 3. 健康状态监控

### 3.1 Worker 心跳表

```sql
CREATE TABLE worker_heartbeats (
    worker_id   VARCHAR(64) PRIMARY KEY,
    session_id  VARCHAR(64) NOT NULL,
    role        VARCHAR(32) NOT NULL,
    expires_at  DATETIME(3) NOT NULL,
    updated_at  DATETIME(3) NOT NULL,
    INDEX idx_expires (expires_at)
);
```

### 3.2 查询在线 Worker

```go
func (s *WorkerStore) ListActiveWorkers(ctx context.Context) ([]WorkerRegistration, error) {
    rows, err := s.db.QueryContext(ctx, `
        SELECT worker_id, session_id, role, expires_at, updated_at
        FROM worker_registrations
        WHERE expires_at > ?
        ORDER BY worker_id
    `, time.Now())

    // ...
}
```

### 3.3 通过 API 查询

```
GET /api/workers

Response:
{
    "workers": [
        {
            "worker_id": "worker-1",
            "session_id": "uuid-xxx",
            "role": "worker",
            "expires_at": "2024-01-01T10:05:00Z",
            "updated_at": "2024-01-01T10:00:00Z"
        }
    ]
}
```

## 4. 完整的集群架构

### 4.1 角色分工

```
┌─────────────────────────────────────────────────────────────────┐
│                        Cluster 架构                              │
│                                                                 │
│  ┌───────────────────┐           ┌───────────────────┐         │
│  │   Control Plane   │           │      Worker       │         │
│  │                   │           │                   │         │
│  │  ┌─────────────┐  │           │  ┌─────────────┐  │         │
│  │  │ API Server  │  │           │  │ Claim Loop  │  │         │
│  │  └─────────────┘  │           │  └─────────────┘  │         │
│  │  ┌─────────────┐  │           │  ┌─────────────┐  │         │
│  │  │  Scheduler  │  │           │  │  Renew Loop │  │         │
│  │  │ (状态机)    │  │           │  └─────────────┘  │         │
│  │  └─────────────┘  │           │  ┌─────────────┐  │         │
│  │  ┌─────────────┐  │           │  │   Runners   │  │         │
│  │  │    UI       │  │           │  │ (执行任务)   │  │         │
│  │  └─────────────┘  │           │  └─────────────┘  │         │
│  └─────────┬─────────┘           └─────────┬─────────┘         │
│            │                               │                   │
│            └───────────────┬───────────────┘                   │
│                            ▼                                   │
│                  ┌─────────────────────┐                       │
│                  │    MySQL 元数据      │                       │
│                  │  - tasks            │                       │
│                  │  - leases           │                       │
│                  │  - checkpoints      │                       │
│                  │  - worker_reg       │                       │
│                  └─────────────────────┘                       │
└─────────────────────────────────────────────────────────────────┘
```

### 4.2 交互流程

```
1. 用户通过 API 启动任务
   │
   ▼
2. Control Plane 把任务状态改为 STARTING
   │
   ▼
3. Worker 的 Claim Loop 发现 STARTING 任务
   │
   ▼
4. Worker 尝试 Acquire 租约
   │
   ├─► 成功：启动 Runner，开始 Renew Loop
   │
   └─► 失败：等待下次 Claim
```

## 5. 代码位置

| 组件 | 代码位置 |
|------|----------|
| Worker 注册 | `internal/meta/mysql_store.go` |
| Claim Loop | `internal/tasks/scheduler.go` (ClaimStartingTasks) |
| Renew Loop | `internal/tasks/scheduler.go` (renewLeaseLoop) |
| 角色配置 | `internal/config/config.go` |

## 6. 本章小结

| 问题 | 解决方案 |
|------|----------|
| Worker ID 冲突 | Session ID + 注册机制 |
| 任务分发 | STARTING 状态 + Claim Loop |
| 故障接管 | 租约过期检测 + 自动接管 |
| 健康监控 | 心跳表 + API 查询 |

---

**概念篇总结**：

至此，我们已经完整理解了 Binlog Server 的核心设计：

1. **第 1 章**：理解系统要解决的问题
2. **第 2 章**：从最简单的 MVP 开始
3. **第 3 章**：引入持久化解决重启丢状态
4. **第 4 章**：引入租约解决多实例并发
5. **第 5 章**：引入集群机制解决高可用

接下来，你可以继续阅读：
- **运维指南**：学习如何部署、配置、排查故障
- **开发指南**：深入理解启动流程、状态机、数据流
