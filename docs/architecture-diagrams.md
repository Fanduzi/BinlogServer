# 架构图（Architecture Diagrams）

本文档提供 `binlog_server` 的核心架构图和关键时序图，覆盖 `standalone` 与 `cluster` 两种模式，并补充复制文件状态机与 lease 故障保护链路。

## 1. 系统组件与职责（Cluster 推荐）

```mermaid
graph LR
    subgraph 用户入口
        U[运维用户 / 浏览器]
    end

    subgraph 控制面
        API[控制面 API / UI]
        SCH[Scheduler 调度器]
    end

    subgraph 数据面
        WK[Worker 进程]
        RUN[复制执行器（MySQL Runner）]
        BG[抢占循环 / 心跳循环]
    end

    subgraph 元数据面
        META[(元数据 MySQL)]
        T1[backup_tasks]
        T2[task_leases]
        T3[checkpoints / task_runs]
        T4[task_events / binlog_files / worker_heartbeats]
    end

    subgraph 外部依赖
        SRC[(源 MySQL)]
        FS[(Worker 本地磁盘)]
        OBJ[(S3 / OBS)]
    end

    U --> API
    API --> SCH
    SCH -->|任务 CRUD / 启停| META
    SCH -->|标记 STARTING（dispatch）| META

    BG -->|扫描 STARTING 并抢占 lease| META
    WK --> RUN
    RUN -->|复制协议拉流| SRC
    RUN -->|写入 OPEN / SEALED 文件| FS
    RUN -->|封口后最佳努力上传| OBJ

    WK <-->|读写 checkpoint / runs / events / files| META
    WK <-->|续约 lease / 上报 heartbeat| META
    META --> T1
    META --> T2
    META --> T3
    META --> T4
```

## 2. 任务从创建到运行（控制面 + Worker）

```mermaid
sequenceDiagram
    participant U as 用户
    participant CP as 控制面 API
    participant M as 元数据 MySQL
    participant W as Worker
    participant S as 源 MySQL

    U->>CP: POST /api/tasks（创建任务）
    CP->>M: upsert backup_tasks（配置）

    U->>CP: POST /api/tasks/{id}/start
    CP->>M: 更新任务状态 STARTING（dispatch）

    loop claim loop
        W->>M: 扫描可抢占任务（STARTING）
        W->>M: acquire lease(task_id, worker_id, epoch)
    end

    W->>S: COM_BINLOG_DUMP / GTID 拉流
    W->>M: 写 task_run(started_at)
    W->>M: 任务状态 -> RUNNING
    W->>M: 周期写入 checkpoint(file,pos)
    W->>M: 周期写入 replication event / 文件元数据
    W->>M: 周期 heartbeat(ONLINE)
```

## 3. 复制文件状态机（本地 + 上传）

```mermaid
stateDiagram-v2
    [*] --> OPEN_LOCAL: 创建当前 OPEN 文件
    OPEN_LOCAL --> OPEN_LOCAL: 追加 binlog event + fsync
    OPEN_LOCAL --> SEALED_LOCAL: rotate / stop 时 seal

    SEALED_LOCAL --> UPLOADED: 上传成功
    SEALED_LOCAL --> UPLOAD_FAILED: 上传失败（最佳努力）
    UPLOAD_FAILED --> RETRY_PENDING: 进入重试/人工处理
    RETRY_PENDING --> UPLOADED: 重试成功

    SEALED_LOCAL --> [*]
    UPLOADED --> [*]
```

## 4. Lease 续约与故障保护

```mermaid
sequenceDiagram
    participant W as Worker
    participant M as 元数据 MySQL
    participant R as Runner

    W->>M: renew lease（周期）
    alt 续约成功
        M-->>W: ok=true
        W->>W: 保持 RUNNING
    else 续约失败且在 grace 内
        M-->>W: err / timeout
        W->>W: 任务状态 -> LEASE_DEGRADED
        W->>R: 继续运行并等待下次续约
    else 超过 grace 或 lease 丢失
        M-->>W: ok=false 或持续 err
        W->>W: fail-safe stop（状态 -> STOPPING）
        W->>R: cancel 复制协程
        W->>M: release lease + 记录事件
        W->>W: 最终状态 -> STOPPED
    end
```

## 5. 控制面故障恢复（数据面不中断）

```mermaid
sequenceDiagram
    participant W as Worker
    participant M as 元数据 MySQL
    participant CP as 控制面
    participant S as 源 MySQL

    Note over CP: 控制面故障
    W->>S: 持续拉流
    W->>M: 持续 renew lease / 写 checkpoint / heartbeat

    Note over CP: 控制面重启恢复
    CP->>M: 读取 tasks / workers / checkpoints / runs
    CP-->>CP: 重建 API 视图与大盘状态

    Note over CP,W: 控制面恢复，数据面未中断
```

## 6. Standalone 与 Cluster 对比

```mermaid
graph TB
    subgraph Standalone
        S1[单进程<br/>API + Scheduler + Runner]
        S2[元数据可选]
        S3[部署简单]
        S4[扩展与高可用能力有限]
        S1 --> S2 --> S3 --> S4
    end

    subgraph Cluster
        C1[角色分离<br/>控制面 + Worker]
        C2[元数据必需]
        C3[lease fencing + heartbeat]
        C4[扩展性与高可用更好]
        C1 --> C2 --> C3 --> C4
    end
```

## 7. 阅读建议

1. 新同学先看第 1 图，再看第 2 图，建立“控制面 + 数据面 + 元数据面”整体模型。
2. 排障时优先看第 4 图和第 5 图，快速判断是 lease 问题还是控制面问题。
3. 运维侧关注第 3 图，明确 `UPLOAD_FAILED` 是“上传失败但不阻断拉流”的语义。
