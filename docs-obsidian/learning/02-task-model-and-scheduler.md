# 02-task-model-and-scheduler

上级：[[MOC-学习路线]]
来源文件：`docs/learning/02-task-model-and-scheduler.md`

---

# 第 2 节：任务模型与调度器

## 全链路导读

- 全链路定位：控制面核心状态机（任务生命周期与调度决策）
- 前置阅读：第 0 节、第 5 节
- 学完你应能：独立解释 task 状态流转与 start/stop/retry/claim 的关键语义

## 目标

看懂任务对象结构、状态流转、以及 start/stop 的控制逻辑。

## 更新提示（alpha.2）

本节已按 cluster 语义更新：包含 dispatch-only start、worker claim、lease degraded 与两阶段停止。

## 核心文件

- `internal/tasks/model.go`
- `internal/tasks/scheduler.go`

## 一眼看控制面调用链

```text
API Handler
  -> Scheduler.CreateTask / Configure*
  -> Scheduler.StartTask (control-plane 可 dispatch STARTING)
      -> worker ClaimStartingTasks + Acquire lease
      -> goroutine: runTask
          -> runner.Run / RunWithNotify
          -> error => RETRY_BACKOFF -> delay -> STARTING -> RUNNING
  -> Scheduler.StopTask
      -> state: RUNNING/... -> STOPPING -> (goroutine退出) STOPPED
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
4. `LEASE_DEGRADED`
5. `REBUILDING_FILE`
6. `RETRY_BACKOFF`
7. `FAILED`（当前保留）
8. `STOPPING`
9. `STOPPED`

常见流转：

1. `CreateTask` 后是 `CREATED`。
2. `StartTask` 允许从 `CREATED/STOPPED/RETRY_BACKOFF` 启动；cluster control-plane 可 dispatch 到 `STARTING`。
3. worker 可周期性 claim `STARTING` 任务并尝试获取 lease。
4. `runTask` 错误进入 `RETRY_BACKOFF`，延迟后回 `STARTING`，ready 后切 `RUNNING`。
5. `StopTask` 为两阶段：先 `STOPPING`，待 run goroutine 退出后收敛为 `STOPPED`。

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

1. `StartTask(id)`：先校验状态/source。  
   - control-plane 无 runner 时允许 dispatch-only（写 `STARTING`，由 worker 接管）。  
   - worker/all-in-one 会先 acquire lease，再起 run goroutine。
2. `runTask(ctx, id, task)`：核心循环。  
`runner.Run` 成功或被取消就返回；失败则写入事件和 `LastError`，进入 `RETRY_BACKOFF`，按指数退避等待，再恢复到 `STARTING` 并由 runner ready 回调切到 `RUNNING`。
3. `StopTask(id)`：先写 `STOPPING` + cancel；若 goroutine 已退出则立即 `STOPPED`，否则由 defer 收敛。

### 4) 查询与恢复

1. `GetTask`：有 store 时会优先刷新持久化视图，避免 control-plane/worker 观察不一致。
2. `ListTasks`：读取内存任务快照。
3. `GetCheckpoint`：从 `checkpointReader` 读最新位点。
4. `ListEvents`：优先读持久化事件存储，否则读内存事件。
5. `ListFiles`：从 `fileStore` 读文件元数据。
6. `ListRuns/ListWorkerHeartbeats`：cluster 可观测能力入口。
7. `Restore(ctx)`：服务启动时从 `store` 恢复任务，并重建 `seq`。

## 关键点

1. scheduler 是控制面的唯一入口，避免状态分散在 API/runner。
2. 任务状态变化总是伴随事件追加（`appendEventLocked`）。
3. cluster 下 lease 续约失败先降级，再按 grace 做 fail-safe stop。
4. source 密码更新支持“空值保留”，避免被误清空。
5. `cancels` + `runs(done)` 共同支撑两阶段停止语义。

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

---

## 相关

- [[架构图-Mermaid版]]
- [[部署模式]]
- [[可观测性]]

## 5 分钟最小实操

1. 通过 API 创建任务并启动一次，再停止一次。
2. 调 `GET /api/tasks/{id}/events`，确认能看到状态变化事件。
3. 口头复述：`STARTING -> RUNNING -> STOPPING -> STOPPED` 是如何收敛的。

## 本节实战检查

- 对照 [[chapter-dod-matrix]] 的「第 2 节」。
- 完成本节最小证据后再进入下一节。
