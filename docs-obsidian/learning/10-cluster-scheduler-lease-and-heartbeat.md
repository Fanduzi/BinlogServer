# 10-cluster-scheduler-lease-and-heartbeat

上级：[[MOC-学习路线]]
来源文件：`docs/learning/10-cluster-scheduler-lease-and-heartbeat.md`

---

# 第 10 节：Cluster 调度核心（Lease / Claim / Heartbeat）

## 全链路导读

- 全链路定位：集群一致性控制层（唯一执行、续租、失联判定与接管）
- 前置阅读：第 2 节、第 9 节
- 学完你应能：解释 lease/epoch 如何避免双写，以及 worker 失联后任务如何转移

## 目标

掌握 cluster 模式下任务是如何“唯一执行”的：谁能 start、谁能续租、谁失联后如何判定离线。

## 核心文件

- `internal/tasks/scheduler.go`
- `internal/tasks/model.go`
- `internal/meta/mysql_store.go`
- `internal/meta/lease_store.go`
- `internal/app/app.go`

## 模型补充（相比基础课程）

`Task` 在 cluster 下新增关键字段：

1. `owner_worker_id`
2. `epoch`
3. `run_id`

状态补充：

1. `LEASE_DEGRADED`
2. `REBUILDING_FILE`

## 关键机制 1：dispatch-only start

control-plane 没有 runner，但允许把任务置为 `STARTING`（dispatch），由 worker 接管。

约束：

1. dispatch 任务必须是 `owner/epoch/run_id` 为空。
2. worker 只有在拿到 lease 后，才能真正把任务拉到 `RUNNING`。

## 关键机制 2：worker 常驻 claim

worker 进程会定期执行 claim loop：

1. 扫描 store 中 `STARTING` 任务
2. 尝试 `StartTask` 触发 lease acquire
3. 成功后启动 runner 会话

这解决了“control-plane start 后必须重启 worker 才接管”的问题。

## 关键机制 3：lease renew 与 fail-safe

任务运行后 worker 持续续租：

1. 续租成功：保持 `RUNNING`
2. 续租异常：进入 `LEASE_DEGRADED`
3. 超过 grace：fail-safe 停止（防止错误写入/封口）

## 关键机制 4：worker heartbeat

worker 每隔固定周期上报：

1. `worker_id/host/version/last_seen_at/status`
2. 停止时 best-effort 写 `OFFLINE`

`/api/workers` 在线状态不再靠任务更新时间推导，而是基于心跳时间窗判断。

## 动手练习

1. 启动 control-plane + worker。
2. 在 control-plane 创建并 start 任务。
3. 观察 task 从 `STARTING` -> `RUNNING`，且无需重启 worker。
4. 停掉 worker，观察 `/api/workers` 由 `online=true` 变 `false`。

## 自测问题

1. 为什么 `epoch` 可以作为 fencing 关键字段？
2. 为什么 `LEASE_DEGRADED` 不能无限持续？
3. dispatch-only start 为什么必须限制为空 owner/epoch/run_id？

---

## 相关

- [[架构图-Mermaid版]]
- [[部署模式]]
- [[可观测性]]

## 5 分钟最小实操

1. 创建并启动任务，观察从 `STARTING` 到 `RUNNING` 的接管过程。
2. 人为停掉 worker，观察 `/api/workers` online 变化。
3. 用一句话解释 epoch/fencing 在防并发写入里的作用。

## 本节实战检查

- 对照 [[chapter-dod-matrix]] 的「第 10 节」。
- 完成本节最小证据后再进入下一节。
