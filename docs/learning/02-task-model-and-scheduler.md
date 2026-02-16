# 第 2 节：任务模型与调度器

## 目标

看懂任务对象结构、状态流转、以及 start/stop 的控制逻辑。

## 核心文件

- `internal/tasks/model.go`
- `internal/tasks/scheduler.go`

## 一眼看控制面调用链

```text
API Handler
  -> Scheduler.CreateTask / Configure*
  -> Scheduler.StartTask
      -> state: CREATED/STOPPED/RETRY_BACKOFF -> RUNNING
      -> goroutine: runTask
          -> runner.Run
          -> error => RETRY_BACKOFF -> delay -> RUNNING
  -> Scheduler.StopTask
      -> cancel context
      -> state: RUNNING/STARTING/RETRY_BACKOFF -> STOPPED
```

## 任务模型（`model.go`）

`Task` 结构里，你要先掌握 4 个块：

1. 身份与状态：`ID/Name/State/LastError/UpdatedAt`
2. 源库连接：`SourceConfig`（`host/port/user/password/flavor/server_id`）
3. 起点策略：`StartConfig`（`LATEST/FILE_POS/GTID`）
4. 存储策略：`Storage`（当前核心是 `retention_days`）

附属模型：

1. `TaskEvent`：任务事件时间线（含 `sequence`）。
2. `BinlogFile`：文件元数据（含上传状态 `LOCAL_ONLY/UPLOADED/UPLOAD_FAILED`）。

## 状态机（以代码为准）

状态常量：

1. `CREATED`
2. `STARTING`
3. `RUNNING`
4. `RETRY_BACKOFF`
5. `FAILED`（当前保留）
6. `STOPPING`
7. `STOPPED`

常见流转：

1. `CreateTask` 后是 `CREATED`。
2. `StartTask` 允许从 `CREATED/STOPPED/RETRY_BACKOFF` 进入运行态。
3. `runTask` 遇到 runner 错误进入 `RETRY_BACKOFF`，延迟后回 `RUNNING`。
4. `StopTask` 允许从 `RUNNING/RETRY_BACKOFF/STARTING` 进入 `STOPPED`。

## 逐函数讲解（`scheduler.go`）

### 1) 构造与注入

1. `NewScheduler(opts...)` 初始化内存表与默认重试参数（1s 到 30s）。
2. `WithStore/WithEventStore/WithFileStore/WithCheckpointReader/WithRunner` 完成依赖注入。

### 2) 任务配置

1. `CreateTask(name)`：创建任务，默认 `Start.Mode=LATEST`，默认 retention=7。
2. `ConfigureSource(id, source)`：校验 host/port/user，`flavor` 默认 `mysql`。  
当更新请求未传密码时，会保留原密码（`source.Password = task.Source.Password`）。
3. `ConfigureStart(id, start)`：校验 `FILE_POS` 需要 `file+pos`，`GTID` 需要 `gtid_set`。
4. `ConfigureStorage/ConfigureName`：更新策略并持久化。

### 3) 启停与执行

1. `StartTask(id)`：先校验状态和 source，再更新状态并持久化。  
若有 runner，则创建 `context.WithCancel` 保存到 `cancels`，启动 goroutine 执行 `runTask`。
2. `runTask(ctx, id, task)`：核心循环。  
`runner.Run` 成功或被取消就返回；失败则写入事件和 `LastError`，进入 `RETRY_BACKOFF`，按指数退避等待，再恢复到 `RUNNING` 重试。
3. `StopTask(id)`：先 `cancel()` 终止运行 goroutine，再更新状态到 `STOPPED`。

### 4) 查询与恢复

1. `GetTask/ListTasks`：内存读取任务。
2. `GetCheckpoint`：从 `checkpointReader` 读最新位点。
3. `ListEvents`：优先读持久化事件存储，否则读内存事件。
4. `ListFiles`：从 `fileStore` 读文件元数据。
5. `Restore(ctx)`：服务启动时从 `store` 恢复任务，并重建 `seq`。

## 关键点

1. scheduler 是控制面的唯一入口，避免状态分散在 API/runner。
2. 任务状态变化总是伴随事件追加（`appendEventLocked`）。
3. 重试逻辑在 scheduler 层，不在 API 层。
4. source 密码更新支持“空值保留”，避免被误清空。
5. `cancels` map 是任务 stop 可生效的关键。

## 常见坑

1. 重复 start：会触发状态检查错误（不是幂等成功）。
2. source 未配置完整就 start：返回 `ErrInvalidSourceConfig`。
3. 误以为 stop 只改状态：实际上先 cancel 运行上下文，再落状态。
4. 忘记持久化依赖：只配内存 store 会导致重启后任务丢失。

## 动手练习

1. 创建任务，分别触发 start/stop。
2. 调用 `/api/tasks`、`/api/tasks/{id}/events` 对比状态和事件。
3. 故意给 runner 一个会报错的源，观察 `RETRY_BACKOFF` 与恢复事件。
4. 修改一次 source（不传 password），验证密码没有被清空。
5. 试着在 scheduler 新增一个只读统计字段并在 API 展示。

## 自测问题

1. 同一个任务重复 start 会发生什么？
2. 为什么任务生命周期不放在 API 层直接处理？
3. `runTask` 为什么要在重试前后分别更新状态并写事件？
4. 如果你要实现“暂停不丢上下文”，应优先改 `StopTask` 还是 `runTask`？为什么？
