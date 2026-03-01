# 术语表

本文档定义 Binlog Server 中使用的核心术语。

## A

### Acquire
获取租约。Worker 通过 Acquire 操作获得任务的执行权。

### All-in-One
集群角色之一，同时提供 API 服务和执行任务能力。

## B

### Binlog
MySQL 二进制日志，记录所有数据变更操作。

### Best-effort Upload
尽力上传模式。上传失败不会阻塞复制流程，可以后续手动重试。

## C

### Checkpoint
位点，记录复制进度（文件名 + 位置 + GTID）。

### Claim
接管。Worker 从 STARTING 状态的任务中获取执行权的过程。

### Cluster Key
集群标识，用于区分不同 MySQL 集群的备份，必须全局唯一。

### Control Plane
集群角色之一，负责 API 服务、状态机管理、任务分发，不执行实际复制任务。

## E

### Epoch
租约版本号。每次租约被新 worker 获取时 +1，用于检测租约是否被抢占。

## F

### Fail-safe Stop
安全停止。当租约丢失或超过宽限期时，强制停止任务以保证数据一致性。

### fsync
系统调用，将文件缓冲区数据刷入磁盘。关键操作：只有 fsync 成功后才更新 checkpoint。

## G

### GTID (Global Transaction Identifier)
MySQL 全局事务标识符，用于唯一标识事务。格式：`UUID:序号`。

## H

### Heartbeat
心跳。Worker 定期向元数据库发送心跳，证明自己在线。

## L

### Lease
租约。一段时间内的任务执行独占权，有过期时间，需要定期续租。

### Lease Degraded
租约降级状态。续租失败但仍在宽限期内，任务继续执行但被标记为降级。

### LATEST
启动模式之一，从 MySQL 当前最新位置开始复制。

## M

### MVP (Minimum Viable Product)
最小可行产品。本项目中指最简单的 binlog 备份实现。

## P

### PENDING
任务状态：已创建，等待启动指令。

### Persistence
持久化。将运行状态保存到数据库，重启后可恢复。

## R

### Renew
续租。Worker 定期续租以保持任务执行权。

### Restore
恢复。启动时从数据库加载任务状态并恢复运行。

### Retention
保留策略。控制本地文件保留天数。

### Rotate
文件切换。MySQL 切换到新的 binlog 文件时发送 ROTATE 事件。

### Runner
执行器。实际执行 binlog 复制的组件。

## S

### Scheduler
调度器。管理任务生命周期和状态转换的核心组件。

### Server ID
MySQL 复制协议中的服务器标识。Binlog Server 作为伪从库需要唯一的 Server ID。

### Session ID
会话标识。Worker 每次启动时生成的 UUID，用于区分同一 worker_id 的不同实例。

### STARTING
任务状态：已收到启动指令，等待 Worker 接管。

### Standalone
单机模式。单进程运行，不依赖外部元数据库。

### State Machine
状态机。定义任务状态及其转换规则。

### Store
存储接口。提供任务、事件、文件等数据的持久化能力。

## T

### Task
任务。一个备份配置单元，对应一个 MySQL 实例的 binlog 备份。

### TTL (Time To Live)
生存时间。租约或注册的有效期。

## U

### Upload
上传。将本地 binlog 文件上传到云存储（S3/OBS/COS/OSS）。

## W

### Worker
集群角色之一，负责接管并执行任务。

### Worker ID
Worker 标识。配置文件中指定的 worker 唯一名称。

### Worker Registration
Worker 注册。Worker 启动时向元数据库注册自己的身份。

## 缩写对照

| 缩写 | 全称 |
|------|------|
| API | Application Programming Interface |
| DSN | Data Source Name |
| GTID | Global Transaction Identifier |
| JSON | JavaScript Object Notation |
| MVP | Minimum Viable Product |
| S3 | Simple Storage Service |
| SQL | Structured Query Language |
| TTL | Time To Live |
| UUID | Universally Unique Identifier |
