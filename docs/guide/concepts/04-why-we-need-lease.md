# 第 4 章：为什么需要租约

上一章我们解决了持久化问题，但还剩两个关键问题：
1. 多个实例可能同时执行同一个任务
2. 实例挂了，其他实例不知道该接管哪些任务

本章引入**租约（Lease）机制**来解决这些问题。

## 1. 什么是租约？

### 1.1 生活中的类比

想象你在图书馆借书：

```
┌─────────────────────────────────────────────┐
│  你借了一本书，图书管理员给你一张借条：        │
│                                             │
│  借书人：张三                                │
│  书名：《MySQL 权威指南》                     │
│  借出时间：2024-01-01                        │
│  归还期限：30 天                             │
│                                             │
│  → 30 天内，这本书"属于"张三                  │
│  → 30 天后不续借，自动归还                    │
└─────────────────────────────────────────────┘
```

**租约的本质**：一段时间内的"独占权"，有过期时间，需要定期续期。

### 1.2 在分布式系统中的应用

```
任务 Task-1 的租约：

┌──────────────────────────────────────┐
│  task_id: Task-1                     │
│  owner: worker-A                     │
│  epoch: 100                          │
│  expires_at: 2024-01-01 10:05:00     │
└──────────────────────────────────────┘

含义：
- worker-A 在 10:05:00 前拥有 Task-1 的执行权
- worker-A 必须定期续租（比如每 10 秒）
- 如果 worker-A 挂了，租约自动过期，其他 worker 可以接管
```

## 2. 租约解决了什么问题？

### 2.1 防止多实例同时执行

**没有租约时：**

```
┌─────────────┐     ┌─────────────┐
│  Worker A   │     │  Worker B   │
│  执行 Task-1│◄───►│  执行 Task-1│  ← 两个都在执行！
└─────────────┘     └─────────────┘
        │                   │
        └───────┬───────────┘
                ▼
        写同一个文件，互相覆盖
```

**有租约时：**

```
┌─────────────┐     ┌─────────────┐
│  Worker A   │     │  Worker B   │
│  持有租约    │     │  获取租约失败│
│  执行 Task-1│     │  等待...    │
└─────────────┘     └─────────────┘
        │
        ▼
  只有 A 在执行，B 等待
```

### 2.2 实现故障自动接管

**场景：Worker A 挂了**

```
时间线：
────────────────────────────────────────────────────►

Worker A: [持有租约] [续租] [续租] [挂了!]
              │       │      │
              ▼       ▼      ▼
租约状态:   [A持有] [A持有] [过期]

                              ↓ 租约过期
Worker B:                   [尝试获取] [成功!]
                                         │
                                         ▼
                                      接管执行
```

**关键点：**
- Worker A 挂了后，无法续租
- 租约过期后，其他 worker 可以获取
- 实现了"自动故障接管"

## 3. LeaseStore 的设计

### 3.1 接口定义

```go
type LeaseManager interface {
    // 获取租约（如果当前没有持有者或已过期）
    Acquire(ctx context.Context, taskID, workerID string, ttl time.Duration) (epoch int64, acquired bool, err error)

    // 续租（只有持有者能续）
    Renew(ctx context.Context, taskID, workerID string, epoch int64, ttl time.Duration) (bool, error)

    // 释放租约（主动放弃）
    Release(ctx context.Context, taskID, workerID string, epoch int64) (bool, error)
}
```

### 3.2 数据库表结构

```sql
CREATE TABLE leases (
    task_id      VARCHAR(64) PRIMARY KEY,
    worker_id    VARCHAR(64) NOT NULL,
    epoch        BIGINT NOT NULL,        -- 租约版本号
    expires_at   DATETIME(3) NOT NULL,   -- 过期时间
    updated_at   DATETIME(3) NOT NULL,
    INDEX idx_expires (expires_at)
);
```

### 3.3 Acquire 的实现逻辑

```go
func (s *LeaseStore) Acquire(ctx context.Context, taskID, workerID string, ttl time.Duration) (int64, bool, error) {
    now := time.Now()
    expiresAt := now.Add(ttl)

    // 尝试插入或更新
    result, err := s.db.ExecContext(ctx, `
        INSERT INTO leases (task_id, worker_id, epoch, expires_at, updated_at)
        VALUES (?, ?, 1, ?, ?)
        ON DUPLICATE KEY UPDATE
            worker_id = IF(expires_at < ?, VALUES(worker_id), worker_id),
            epoch = IF(expires_at < ?, epoch + 1, epoch),
            expires_at = IF(expires_at < ?, VALUES(expires_at), expires_at),
            updated_at = VALUES(updated_at)
    `, taskID, workerID, expiresAt, now, now, now, now)

    // 检查是否获取成功
    // 如果 expires_at < now，说明旧租约已过期，我们成功获取
    // 否则，租约被其他 worker 持有
}
```

**关键点：** 使用 `ON DUPLICATE KEY UPDATE` + 条件判断，保证原子性。

## 4. 租约的生命周期

### 4.1 正常流程

```
1. Worker 尝试获取租约
   └─► Acquire(taskID, workerID, ttl=30s)
       └─► 成功，返回 epoch=1

2. Worker 开始执行任务

3. Worker 定期续租（每 10 秒）
   └─► Renew(taskID, workerID, epoch=1, ttl=30s)
       └─► 成功

4. 任务完成或停止
   └─► Release(taskID, workerID, epoch=1)
       └─► 释放租约
```

### 4.2 故障接管流程

```
1. Worker A 获取租约
   └─► epoch=1, expires_at=T+30s

2. Worker A 在 T+15s 时挂了

3. T+30s 租约过期

4. Worker B 尝试获取租约
   └─► Acquire 发现 expires_at < now
   └─► 成功获取，epoch=2

5. Worker B 开始执行任务
```

### 4.3 Epoch 的作用

**问题：** 如果 Worker A 网络抖动，续租请求延迟了，它怎么知道自己已经失去租约？

**答案：** 通过 epoch。

```go
// Worker A 尝试续租
ok, err := leaseManager.Renew(ctx, taskID, "worker-A", epoch=1, ttl)

// 如果租约已经被 Worker B 接管（epoch=2）
// Renew 会失败，因为 epoch 不匹配
```

**设计原则：** 每次 Acquire 成功，epoch + 1。Renew/Release 必须提供正确的 epoch 才能成功。

## 5. 降级与容错

### 5.1 续租失败怎么办？

不是立即停止任务，而是进入"降级"状态：

```go
func (s *Scheduler) renewLeaseLoop(ctx context.Context, taskID, workerID string, epoch int64) {
    ticker := time.NewTicker(10 * time.Second)
    defer ticker.Stop()

    degradedSince := time.Time{}

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
        }

        ok, err := s.leaseManager.Renew(ctx, taskID, workerID, epoch, ttl)

        if ok {
            // 续租成功，恢复正常
            degradedSince = time.Time{}
            continue
        }

        // 续租失败，进入降级
        if degradedSince.IsZero() {
            degradedSince = time.Now()
        }

        // 超过宽限期，停止任务
        if time.Since(degradedSince) > s.leaseGrace {
            s.failSafeStop(taskID)
            return
        }
    }
}
```

**为什么需要宽限期？**
- 网络抖动可能导致短暂续租失败
- 给系统一定的容错时间
- 通常设置为 TTL 的 2-3 倍

### 5.2 降级状态的行为

| 状态 | 行为 |
|------|------|
| 正常 | 继续拉取 binlog，续租成功后恢复正常 |
| 降级 | 继续拉取 binlog，但标记为 LEASE_DEGRADED |
| 超过宽限期 | 停止任务，释放资源 |

## 6. 代码位置

| 组件 | 代码位置 |
|------|----------|
| LeaseStore 接口 | `internal/meta/lease_store.go` |
| LeaseStore 实现 | `internal/meta/lease_store.go` |
| 续租循环 | `internal/tasks/scheduler_cluster_lease.go` |
| 降级处理 | `internal/tasks/scheduler_cluster_lease.go` |

## 7. 本章小结

| 问题 | 租约如何解决 |
|------|-------------|
| 多实例同时执行 | 只有持有租约的 worker 能执行 |
| 故障无法接管 | 租约过期后自动释放，其他 worker 可获取 |
| 不知道谁在执行 | 查询 leases 表可知当前持有者 |
| 网络抖动误杀 | 宽限期机制，短暂失败不会立即停止 |

---

**下一章**：我们将看到集群模式的完整架构，包括 worker 注册、心跳、任务分发等机制。
