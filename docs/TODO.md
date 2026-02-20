# TODO 与里程碑（Milestones）

本文用于回答两个问题：

1. 还有哪些没做？
2. 这些事情按什么里程碑收口，而不是无限加 Task？

---

## 0. 执行约束（先立规矩）

从现在开始采用以下规则：

1. **不允许孤立 Task**：每个新 Task 必须归属某个 Milestone。
2. **先里程碑后任务**：先定义 Milestone 目标和 DoD，再拆 Task。
3. **里程碑收口评审**：Milestone 达标后才进入下一个 Milestone。
4. **TODO 是唯一未完成来源**：未写入本文档的“想法”不进入排期。

---

## 1. 当前里程碑状态

### M1: `v0.1.0-alpha.2`（已完成）

目标：
- standalone + cluster 基础能力
- control-plane / worker 角色分离
- 元数据库 failover 回归
- observability 基础指标与 e2e 回归

状态：`DONE`

---

## 2. 下个里程碑（建议）

### M2: `v0.1.0-beta.0`（进行中）

目标：
- 强化“接管一致性”与“上传可靠性”，形成可灰度的 beta 版本。

DoD（完成定义）：
1. worker 异常退出接管链路有 e2e 证据（OPEN 期间崩溃 + takeover）。
2. 文件一致性验证可回归（单文件唯一 sealed + md5 抽样一致）。
3. 上传链路具备可操作的失败补偿方案（至少人工触发补传）。
4. 文档包含明确 runbook 与边界说明。

---

## 3. 未完成项（Backlog）

### A. 一致性与恢复（M2）

- [x] `Task 22`：worker 在 OPEN 阶段崩溃接管 e2e  
  验收：checkpoint 持续推进、stale `.open.e*` 清理、md5 抽样一致。  
  证据：`./scripts/e2e/run-suite.sh --scenarios smoke-worker-crash-recovery`（场景脚本：`scripts/e2e/smoke-worker-crash-recovery.sh`）。

### B. 上传可靠性（M2）

- [x] `Task 23`：上传失败补偿机制（最小版）  
  建议范围：
  - 增加“补传触发入口”（API/CLI 二选一）
  - 只处理 `UPLOAD_FAILED` 文件
  - 保持 best-effort 主链路不变
  证据：`./scripts/e2e/run-suite.sh --scenarios smoke-retry-upload`。

- [x] `Task 23.1`：object key 命名升级（集群唯一标识）  
  建议范围：
  - 新增任务级 `cluster_key`（用户提供、全局唯一、可读）
  - object key 改为：`<prefix>/<cluster_key>/<source_server_uuid>/<fileName>`
  - 更新 API/UI/e2e 测试用例，开发阶段不做旧任务兼容

- [ ] `Task 24`：上传可观测增强  
  建议范围：
  - 补传成功/失败计数
  - 最近失败原因聚合
  - runbook 补充“如何批量补传”

- [ ] `Task 24.1`：多云对象存储接入能力明确化  
  建议范围：
  - 明确“按云厂商 SDK 分实现”，不再追求单一统一接入层
  - AWS S3 继续使用 MinIO（S3 API）
  - 华为云 OBS / 腾讯云 COS / 阿里云 OSS 分别接入各自官方 SDK
  - 定义统一 `Uploader` 抽象，仅在调度层保持一致接口

- [ ] `Task 24.2`：断点续传能力评估与最小落地  
  建议范围：
  - 评估 SDK 对 multipart resume 的支持边界（进程内重试 vs 跨进程恢复）
  - 若可行：持久化 upload session（如 upload_id/part_etag）实现跨重启续传
  - 若不可行：明确降级策略（失败后重传整个 sealed 文件）

### C. 发布收口（M2）

- [ ] `Task 25`：beta 发布验收清单  
  验收：单测 + 关键 e2e + 文档一致性 + 发布说明。

---

## 4. 上传能力现状（避免误解）

已完成：
1. 支持 S3/OBS 兼容上传（MinIO SDK）。
2. sealed 后触发上传，记录 `LOCAL_ONLY/UPLOADED/UPLOAD_FAILED`。
3. 上传失败不阻断拉流（best-effort）。
4. 仅对已 seal 的 binlog 文件触发上传，不上传仍在写入的 OPEN 文件。

未完成：
1. 自动补传队列（后台持续重试）。
2. 批量补传入口（当前无标准化运维入口）。
3. 断点续传跨重启恢复（multipart 会话持久化）尚未落地。

当前非目标（已明确）：
1. 默认不做远端对象回读校验（不在上传后 download 对象做 checksum 回读对比）。

已确认策略（2026-02-19）：
1. 上传实现采用“多 Provider 多 SDK”路线，而非“所有云统一走 S3 兼容层”。
2. AWS S3 使用 MinIO SDK；华为 OBS / 腾讯 COS / 阿里 OSS 使用各自官方 SDK。
3. object key 命名策略采用 `cluster_key + source_server_uuid` 组合，避免跨集群重名与切主覆盖风险。

---

## 5. 里程碑推进方式（你关心的“是不是没完没了”）

是“**按里程碑推进**”，不是“无限 Task 列表”：

1. 先锁 `M2` 的 DoD（上面 4 条）。
2. 只做 `M2` 相关 Task（22-25）。
3. `M2` 收口后再讨论 `M3`，不提前开新战线。
