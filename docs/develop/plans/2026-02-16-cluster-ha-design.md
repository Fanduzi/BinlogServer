# Cluster 模式详细设计草案（Control Plane + Worker）

## 1. 背景与目标

当前 `standalone` 模式优点是简单，但在多机、多集群场景下会出现：

- 任务分散在多台机器，统一管理困难
- 缺少统一的任务调度与接管机制（HA）
- 控制面能力（审计、全局视图、批量操作）不足

本草案目标：

1. 同一套代码支持双模式：`standalone` 与 `cluster`
2. 在 `cluster` 下提供统一管理页面与多 worker 高可用
3. 满足硬约束：
   - 每个 `mysql-bin.xxxxxx` 必须是单一完整文件
   - 文件字节内容与源库对应 binlog 文件一致
4. 元数据存储使用 MySQL（由 orchestrator 做 HA）时，代码可正确处理 failover 期间异常

---

## 2. 非目标（本阶段）

- 不在本阶段实现跨 Region 多活
- 不在本阶段实现控制面多主写入
- 不要求支持任意对象存储强一致语义差异（默认兼容 S3/OBS）

---

## 3. 运行模式与角色

### 3.1 `standalone`

- 本地调度，本地执行
- 适合小规模、快速部署
- 与现有模式兼容

### 3.2 `cluster`

- 统一 control plane + 多 worker
- 任务由 worker 抢占 lease 执行
- 支持自动接管

### 3.3 role 定义

- `control-plane`
  - 提供 UI/API、任务配置、调度决策、审计
  - 不直接拉 binlog
- `worker`
  - 拉 binlog、写本地、seal/upload、写 checkpoint/元数据
  - 不提供管理 UI
- `all-in-one`
  - 同时运行 control-plane + worker
  - 适合过渡期和小规模集群

说明：`all-in-one` 属于 `cluster` 协议内节点（有 lease/epoch），不是传统 `standalone`。

---

## 4. 总体架构

1. 控制面与执行面解耦：
- 控制面挂了，不应影响已运行 worker 的拉流
- worker 数据路径不经过控制面

2. 元数据中心化：
- 任务配置、lease、checkpoint、文件状态在元数据库

3. 本地盘定位：
- worker 本地盘是执行缓存与写入落地点
- 对外可恢复资产以“seal 后文件 + 元数据”为准

4. 对象存储定位：
- 保存 seal 后完整 binlog 文件

---

## 5. 数据模型（新增/扩展）

基于现有 `backup_tasks / backup_checkpoints / binlog_files` 扩展。

### 5.1 `task_leases`

用途：任务执行权（fencing）。

建议字段：

- `task_id` (PK)
- `owner_worker_id`
- `epoch` (bigint, 单调递增)
- `lease_expire_at`
- `renewed_at`

语义：

- 抢占成功时 `epoch = epoch + 1`
- 任何写入都必须带 `task_id + epoch` 校验

### 5.2 `task_runs`

用途：记录每次执行会话。

- `run_id` (PK)
- `task_id`
- `worker_id`
- `epoch`
- `started_at`
- `ended_at`
- `end_reason` (`NORMAL_STOP/LEASE_LOST/WORKER_CRASH/DB_DEGRADED/...`)

### 5.3 `binlog_files`（扩展）

新增关键字段：

- `epoch`
- `state` (`OPEN/SEALED/UPLOADED/ABANDONED`)
- `checksum` (sha256/md5)
- `size_bytes`
- `start_pos` / `end_pos`
- `source_file`

约束：

- 同一 `task_id + source_file` 最终只能有一个 `SEALED` 记录
- `OPEN` 记录可存在历史垃圾，需清理

### 5.4 `worker_heartbeats`

- `worker_id` (PK)
- `host`
- `version`
- `last_seen_at`
- `status`

---

## 6. Strict 文件一致性策略（核心）

### 6.1 文件级所有权

同一时刻，一个任务只能有一个 `owner_worker + epoch`。

### 6.2 OPEN 文件命名

本地临时名带 epoch，例如：

- `mysql-bin.000123.open.e42`

只有 seal 成功后，才重命名为：

- `mysql-bin.000123`

### 6.3 抢占与恢复

当 worker 在 `OPEN` 期间故障：

1. 新 owner 抢到 lease（epoch+1）
2. 进入 `rebuild_current_file`：
   - 从该文件 `pos=4` 重拉并重建完整文件
3. 达到当前位点后切换实时流

这样保证最终产物仍是单一完整文件，且字节一致。

### 6.4 老 worker 恢复处理（你提到的关键点）

老 worker 恢复时必须：

1. 先读 lease，若 epoch 不匹配立即自我隔离
2. 扫描本地 `.open.e*`：
   - 非当前 epoch 的 OPEN 文件全部清理（或 quarantine 后删）
3. 禁止发布非当前 epoch 产生的任何文件

### 6.5 seal/upload 发布条件

只有同时满足以下条件才允许发布：

1. 当前仍持有 lease 且 epoch 匹配
2. 文件已 rotate 完整
3. checksum 与预期规则通过
4. 元数据事务写入成功（`SEALED`）

---

## 7. 元数据库 MySQL failover 异常处理（orchestrator 场景）

## 7.1 故障类型

1. 连接中断：`connection reset`, `broken pipe`, `read: EOF`
2. 写入超时：commit 结果不确定（ambiguous commit）
3. 只读错误：新主切换前后短暂只读或路由漂移
4. 短期无法连接：DNS/VIP/代理切换窗口

### 7.2 统一处理原则

1. 所有元数据操作走 `WithRetry`（指数退避+jitter）
2. 所有写操作设计为幂等（`UPSERT` + 唯一键）
3. 通过 `epoch` 做 fencing，避免双写
4. 无法确认 lease 安全时，worker 必须自停（fail-safe）

### 7.3 worker 在 failover 期间的行为

定义 `lease_grace_sec`（例如 30s）：

1. failover 开始，续约失败 -> `LEASE_DEGRADED`
2. grace 内继续尝试续约与元数据写入
3. grace 超时仍失败：
   - 停止拉流
   - 关闭 OPEN 文件，不 seal，不发布
   - 等待重新抢占
4. 抢占成功后按 `rebuild_current_file` 继续

结果：failover 期间可能短暂停拉，但保证不破坏 strict 文件一致性。

### 7.4 控制面在 failover 期间行为

- UI/API 可能报错或只读降级
- 不影响已运行 worker 的数据路径（直到 worker lease 续约超时）

### 7.5 连接池建议

- `SetConnMaxLifetime(30s~120s)`，避免长期持有旧主连接
- `SetMaxOpenConns` / `SetMaxIdleConns` 合理限流
- 查询超时与写超时分开配置

---

## 8. 任务状态机（cluster）

新增或强化状态：

- `ASSIGNED`：任务已分配 owner
- `RUNNING`：正常拉流
- `LEASE_DEGRADED`：元数据库异常，续约失败但在 grace 内
- `REBUILDING_FILE`：接管后重建当前 binlog 文件
- `STOPPING` / `STOPPED`
- `FAILED`

状态转移关键规则：

- `LEASE_DEGRADED` 超时必须转 `STOPPING -> STOPPED`
- `REBUILDING_FILE` 成功后才回 `RUNNING`

---

## 9. API 与 UI 改造方向

### 9.1 控制面 API

新增：

- 节点管理：`/api/workers`
- 任务分配视图：`/api/tasks/{id}/lease`
- 运行会话：`/api/tasks/{id}/runs`
- 集群汇总：`/api/cluster/overview`

### 9.2 UI

新增视图：

- 全局 worker 列表（在线/离线/负载）
- 任务 -> owner worker 映射
- failover 时间窗口告警（lease degraded 次数）

---

## 10. 配置草案

```yaml
mode: cluster # standalone | cluster

cluster:
  role: all-in-one # control-plane | worker | all-in-one
  worker_id: "node-a"
  lease_ttl_sec: 15
  lease_renew_interval_sec: 5
  lease_grace_sec: 30
  failover_policy: rebuild_current_file # strict 模式必选

meta:
  dsn: "user:pass@tcp(meta-vip:3306)/binlog_meta?parseTime=true"
  retry_max: 8
  retry_base_ms: 200
  retry_max_ms: 3000
```

---

## 11. 分阶段落地建议

### Phase 1

- 双模式框架与 role 启动参数
- `task_leases` + epoch fencing
- worker 基础抢占与续约

### Phase 2

- strict 文件语义（OPEN/SEALED/ABANDONED）
- `rebuild_current_file` 接管逻辑
- 老 worker 恢复清理机制

### Phase 3

- 控制面统一大盘（workers + leases + runs）
- 批量运维能力

### Phase 4

- failover 混沌测试（断网、主从切换、只读窗口）
- 性能与稳定性优化

---

## 12. 测试矩阵（必须覆盖）

1. 单 worker 正常拉流 + rotate + 校验字节一致
2. 双 worker 抢占接管，最终仅一个 `SEALED` 文件
3. 老 worker 恢复后清理旧 OPEN 文件
4. 元数据库 failover（orchestrator 切主）期间：
   - worker 进入/退出 `LEASE_DEGRADED`
   - grace 超时自停
   - 恢复后重建文件并继续
5. 控制面宕机时，worker 在 lease 有效期内持续工作

---

## 13. 结论

该方案支持：

- 小规模保持 `standalone` 简单性
- 大规模切换 `cluster` 获得统一管理与 HA
- 在 strict 约束下保证 binlog 文件完整且字节一致
- 在 orchestrator 管理的元数据库 failover 期间保持可恢复、可自保护的行为

---

## 14. 实现差异同步（截至 2026-02-17）

以下为当前实现与设计草案的已知差异，便于运维预期对齐：

1. `GET /api/tasks/{id}/runs` 当前返回“当前 run 信息”为主（通常最多 1 条），尚未提供完整历史会话列表。
2. worker 在线状态当前基于任务 owner/更新时间聚合视图推导，尚未引入独立 `worker_heartbeats` 心跳表与专门心跳上报循环。
3. 前端详情页已按当前能力展示“当前 Run 信息（接口当前最多返回 1 条）”，避免把现状误解为完整 run history 能力。
