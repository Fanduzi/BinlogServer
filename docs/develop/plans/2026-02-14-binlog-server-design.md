# Binlog Server 设计文档（MVP）

- 日期：2026-02-14
- 状态：已确认
- 范围：MVP（单机单进程）

## 1. 背景与目标

现有 `mysqlbinlog --stop-never` 在单实例场景可用，但在上百集群规模下存在三类核心问题：

1. 任务管理困难：需要维护大量独立进程，启停、排障和监控成本高。
2. 扩展能力不足：无法自然支持后续 OBS/S3 上传、统一生命周期管理。
3. 元数据缺失：缺乏任务、文件、位点、错误事件的系统化记录。

本项目目标是在不依赖 `mysqlbinlog` 子进程的前提下，使用 MySQL 复制协议直接拉取 binlog，实现集中任务管理、可靠本地落盘和断点续传能力，并通过 API + Web UI 提供可运维能力。

## 2. MVP 边界（已确认）

- 部署形态：单机单进程。
- 开发语言：Go。
- 支持版本：MySQL 5.7 + 8.0。
- 管理面：Admin API + Web UI（完整管理台）。
- 拉取方式：原生复制协议（`COM_BINLOG_DUMP` / GTID 相关流程）。
- 规模目标：100-300 实例任务并发管理。
- 元数据存储：外部 MySQL。
- 启动策略：
  - 默认从最新位点（LATEST）启动。
  - 可配置从指定 `file/pos` 启动。
  - 可配置从指定 GTID 启动。
- 落盘格式：MySQL 原生 binlog 文件格式。
- 清理策略：按保留天数自动清理。
- 可靠性策略：可靠优先，必须在文件 `fsync` 成功后推进 checkpoint。

## 3. 总体架构

采用单体进程、模块化分层架构，模块如下：

1. `Web UI`：任务配置、列表、状态、错误、启停操作。
2. `Admin API`：提供任务 CRUD、启停、查询、审计接口。
3. `Task Scheduler`：任务生命周期管理、并发控制、重试与退避。
4. `Replication Engine`：与源库建立复制连接并持续消费 binlog event。
5. `Local Storage`：原生 binlog 文件落盘、滚动、封口、校验。
6. `Metadata Store`（外部 MySQL）：任务配置、状态、文件清单、位点、事件。

该架构满足 MVP 快速交付要求，同时保留后续扩展空间（例如上传管道、多节点调度）。

## 4. 核心数据模型

建议最小表集合：

1. `clusters`
  - 源库连接信息、环境标签、逻辑分组。
2. `backup_tasks`
  - 任务配置（cluster 绑定、启动策略、保留天数、运行参数）。
3. `task_runtime`
  - 任务当前状态、心跳、最近错误摘要、最后更新时间。
4. `checkpoints`
  - 安全位点（`file/pos`、`gtid_set`、更新时间）。
5. `binlog_files`
  - 文件名、路径、大小、起止位点、创建/封口时间、校验值。
6. `task_events`
  - 生命周期与错误事件审计（类型、详情、时间、关联任务）。

### 4.1 状态机

任务状态建议：

`CREATED -> STARTING -> RUNNING -> RETRY_BACKOFF -> FAILED`

`RUNNING -> STOPPING -> STOPPED`

- 可恢复错误（网络抖动、短暂不可达）进入 `RETRY_BACKOFF`，指数退避后自动重连。
- 不可恢复错误（鉴权失败、非法位点）进入 `FAILED`，等待人工干预。

## 5. 数据流与可靠性语义

每个任务启动流程：

1. `Scheduler` 读取任务配置与最后 checkpoint。
2. 根据启动策略确定起点：
  - `LATEST`：先查询当前最新可用位点。
  - `FILE_POS`：校验后从指定位置启动。
  - `GTID`：校验 GTID 集合后启动。
3. `Replication Engine` 拉取 event 并交给 `Storage`。
4. `Storage` 追加写入原生 binlog 文件，按阈值触发 `fsync`。
5. 仅当 `fsync` 成功后，更新 `checkpoints`（安全位点推进）。
6. 文件滚动（rotate event 或大小阈值）后封口并写入 `binlog_files`。

### 5.1 崩溃恢复

- 进程重启后从最后“已 fsync 的 checkpoint”恢复。
- 语义保证：宁可重复拉取，不跳过未持久化数据。

### 5.2 清理策略

- 独立清理线程按保留天数删除过期文件。
- 删除前校验文件不晚于安全位点，避免误删活跃文件。

## 6. API 与 UI（MVP）

MVP 管理台应具备：

1. 任务管理：创建、编辑、删除、启停。
2. 状态面板：运行中/重试中/失败数，任务级健康状态。
3. 错误查看：最近错误列表与详情。
4. 文件与位点可见性：最近文件、当前 checkpoint。

API 最小集（示例）：

- `POST /api/tasks`
- `GET /api/tasks`
- `GET /api/tasks/{id}`
- `PUT /api/tasks/{id}`
- `DELETE /api/tasks/{id}`
- `POST /api/tasks/{id}/start`
- `POST /api/tasks/{id}/stop`
- `GET /api/tasks/{id}/events`
- `GET /api/tasks/{id}/files`

## 7. 测试与验收标准

测试分层：

1. 单元测试：位点解析、状态机、checkpoint 推进条件、清理逻辑、重试策略。
2. 集成测试：MySQL 5.7/8.0 拉流、LATEST 启动、指定起点启动、重启恢复、rotate 一致性。
3. 故障注入：断网、权限错误、磁盘满、I/O 异常、主库切换。
4. 规模压测：100-300 任务下 CPU/内存/IOPS/延迟与恢复时长。

MVP 验收门槛：

1. 支持 MySQL 5.7 + 8.0。
2. 支持 `LATEST`、`FILE_POS`、`GTID` 三种起点。
3. 本地文件可被 `mysqlbinlog` 正常解析。
4. 崩溃恢复不丢失已确认（`fsync` 成功）的数据。
5. UI + API 可完成任务管理与状态观测。
6. 保留天数自动清理生效且无误删。

## 8. 里程碑计划

1. `M1`：复制协议拉流 + 本地原生落盘（单任务到多任务）。
2. `M2`：元数据模型 + checkpoint 恢复链路 + 状态机。
3. `M3`：Admin API + Web UI（完整任务管理台）。
4. `M4`：稳定性增强 + 故障注入 + 规模压测。

## 9. 非目标（MVP 不做）

1. 多节点高可用调度与任务分片。
2. OBS/S3 上传链路（作为二期扩展）。
3. 跨机房复制拓扑自动发现。
