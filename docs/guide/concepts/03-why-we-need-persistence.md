# 第 3 章：为什么需要持久化

上一章的架构有个致命问题：**进程重启后，所有状态都丢失了**。

本章我们解决：
1. 重启后任务消失
2. 不知道拉取进度
3. 无法断点恢复

## 1. 需要持久化什么？

### 1.1 任务配置

```go
type Task struct {
    ID          string
    Name        string
    ClusterKey  string
    Source      SourceConfig  // 连接哪个 MySQL
    Storage     StorageConfig // 存储配置
    State       TaskState     // 当前状态
    // ...
}
```

**为什么需要持久化？**
- 重启后能恢复任务列表
- 不需要重新创建任务

### 1.2 Checkpoint（位点）

```go
type Checkpoint struct {
    File     string  // 当前文件名，如 "mysql-bin.000010"
    Pos      uint32  // 文件内位置
    GTID     string  // GTID 集合（如果使用 GTID）
}
```

**为什么需要持久化？**
- 知道拉取到哪里了
- 重启后能从断点继续
- 计算延迟（对比 MySQL 当前位置）

### 1.3 事件历史

```go
type TaskEvent struct {
    TaskID     string
    Type       string    // TASK_STARTED, TASK_STOPPED, TASK_ERROR...
    Message    string
    Timestamp  time.Time
}
```

**为什么需要持久化？**
- 排查问题：什么时候启动的？什么时候报错的？
- 审计：谁在什么时候做了什么操作

### 1.4 文件元数据

```go
type FileMeta struct {
    TaskID       string
    FileName     string
    Size         int64
    UploadStatus string  // LOCAL_ONLY, UPLOADED, UPLOAD_FAILED
}
```

**为什么需要持久化？**
- 知道哪些文件已经上传
- 上传失败的可以重试

## 2. 引入 MySQLStore

### 2.1 架构变化

```
                    ┌─────────┐
                    │   用户   │
                    └────┬────┘
                         │ HTTP
                         ▼
┌─────────────────────────────────────────────────┐
│                    单进程                        │
│  ┌─────────────┐      ┌─────────────────────┐  │
│  │ API Server  │      │     Scheduler       │  │
│  └─────────────┘      └──────────┬──────────┘  │
│                                  │              │
│                       ┌──────────▼──────────┐  │
│                       │    MySQLStore       │  │
│                       │  (持久化接口)        │  │
│                       └──────────┬──────────┘  │
└──────────────────────────────────┼─────────────┘
                                   │
                                   ▼
                        ┌─────────────────────┐
                        │     MySQL 数据库     │
                        │  tasks              │
                        │  checkpoints        │
                        │  events             │
                        │  files              │
                        └─────────────────────┘
```

### 2.2 MySQLStore 的接口设计

```go
// Store 接口：任务持久化
type Store interface {
    UpsertTask(ctx context.Context, task Task) error
    GetTask(ctx context.Context, id string) (Task, bool, error)
    ListTasks(ctx context.Context) ([]Task, error)
    DeleteTask(ctx context.Context, id string) error
}

// CheckpointStore 接口：位点持久化
type CheckpointStore interface {
    SaveCheckpoint(ctx context.Context, taskID string, cp Checkpoint) error
    GetCheckpoint(ctx context.Context, taskID string) (Checkpoint, bool, error)
}

// EventStore 接口：事件持久化
type EventStore interface {
    AppendEvent(ctx context.Context, taskID, eventType, message, detail string) error
    ListEvents(ctx context.Context, taskID string, limit int) ([]TaskEvent, error)
}

// FileStore 接口：文件元数据持久化
type FileStore interface {
    UpsertFile(ctx context.Context, f FileMeta) error
    ListFiles(ctx context.Context, taskID string) ([]FileMeta, error)
}
```

### 2.3 为什么用接口？

**关键设计决策：使用接口而非具体实现**

```go
type Scheduler struct {
    store Store  // 接口类型，不是具体类型
    // ...
}
```

**好处：**
1. **可测试**：测试时用 mock 实现，不需要真实数据库
2. **可替换**：未来可以换 PostgreSQL、etcd 等
3. **解耦**：Scheduler 不关心存储的具体实现

## 3. 持久化后的启动流程

### 3.1 启动时恢复

```go
func (s *Scheduler) Restore() error {
    // 1. 从数据库加载所有任务
    tasks, err := s.store.ListTasks(ctx)
    if err != nil {
        return err
    }

    // 2. 加载到内存
    for _, task := range tasks {
        s.tasks[task.ID] = task
    }

    // 3. 对 RUNNING 状态的任务，重新启动
    for _, task := range tasks {
        if task.State == StateRunning {
            s.StartTask(task.ID)  // 会从 checkpoint 继续
        }
    }

    return nil
}
```

### 3.2 从 Checkpoint 恢复

```go
func (r *MySQLRunner) Run(ctx context.Context, task Task) error {
    // 1. 查询持久化的 checkpoint
    cp, exists, _ := r.checkpointReader.GetCheckpoint(ctx, task.ID)

    var startPos binlog.Position
    if exists {
        // 从 checkpoint 继续
        startPos = binlog.Position{
            Name: cp.File,
            Pos:  cp.Pos,
        }
    } else {
        // 没有 checkpoint，从最新位置开始
        startPos = binlog.Position{Name: "", Pos: 4}
    }

    // 2. 从该位置开始拉取
    // ...
}
```

## 4. 持久化解决了什么？

| 问题 | 解决方案 |
|------|----------|
| 重启丢任务 | 启动时从数据库恢复 |
| 不知道进度 | 查询 checkpoint 表 |
| 无法断点恢复 | 读取 checkpoint，从断点继续 |
| 不知道发生了什么 | 查询 events 表 |

## 5. 还有什么问题？

### 5.1 多实例同时运行

**场景：** 部署了 2 个实例做高可用。

```
┌─────────────┐     ┌─────────────┐
│  Instance A │     │  Instance B │
│  RUNNING    │     │  RUNNING    │
│  Task 1     │◄───►│  Task 1     │  ← 都在拉同一个任务！
└─────────────┘     └─────────────┘
        │                   │
        └───────┬───────────┘
                ▼
        ┌─────────────┐
        │   MySQL     │
        │  (被两个实例 │
        │   同时拉取)  │
        └─────────────┘
```

**问题：**
- 两个实例同时写同一个 binlog 文件
- 上传时可能互相覆盖
- 资源浪费

### 5.2 实例挂了谁来接管

**场景：** Instance A 挂了，Instance B 应该接管它的任务。

```
Instance A (RUNNING Task 1, 2) ──── 挂了
Instance B (RUNNING Task 3, 4) ──── 怎么知道要接管 Task 1, 2？
```

**问题：** 没有机制让 B 知道 A 挂了，也不知道该接管哪些任务。

## 6. 代码位置

| 组件 | 代码位置 |
|------|----------|
| Store 接口 | `internal/tasks/store.go` |
| MySQLStore 实现 | `internal/meta/mysql_store.go` |
| Checkpoint 读取 | `internal/replication/mysql_runner.go` |
| Restore 逻辑 | `internal/tasks/scheduler_observability.go` |

## 7. 本章小结

1. **需要持久化的数据**：任务、checkpoint、事件、文件元数据
2. **MySQLStore**：提供统一持久化接口
3. **启动恢复**：从数据库加载任务，从 checkpoint 继续拉取
4. **遗留问题**：多实例并发、故障接管

---

**下一章**：我们将看到**租约机制**如何解决多实例并发和故障接管问题。
