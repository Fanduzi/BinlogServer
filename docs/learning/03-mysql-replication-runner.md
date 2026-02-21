# 第 3 节：MySQL 复制拉流主流程

## 全链路导读

- 全链路定位：数据面核心执行链路（拉流、写盘、checkpoint、seal/upload）
- 前置阅读：第 0 节、第 1 节、第 2 节
- 学完你应能：解释 event 到文件落盘的完整路径，以及为何 checkpoint 只能在安全语义后推进

## 目标

看懂 runner 如何从 MySQL 复制协议持续拉取 binlog、落盘、rotate、上传。

## 更新提示（alpha.2）

当前 runner 在 cluster 下受 lease/epoch 约束，并支持 semi-sync（默认关闭，失败自动降级异步）。

## 核心文件

- `internal/replication/mysql_runner.go`

## 一眼看主链路

```text
Run(task)
  -> ResolveStart
  -> (可选) checkpointStore.LoadCheckpoint 覆盖起点
  -> buildSyncerConfig + NewBinlogSyncer
  -> StartSync / StartSyncGTID
  -> openBinlogWriter
  -> for event in streamer:
       -> rotate? finalizeSealedFile + open new writer
       -> writer.Append(raw, next checkpoint)
       -> writer.FlushAndCheckpoint
       -> (可选) checkpointStore.UpsertCheckpoint
```

## 逐函数讲解

### 1) `NewMySQLRunner`

1. 保存 `dataDir`，默认值是 `./data`。
2. 默认使用 `mysqlStatusFetcher` 获取主库位点。
3. 通过 `WithCheckpointStore/WithFileMetaStore/WithUploader` 注入可选能力。

### 2) `Run(ctx, task)`（核心）

关键动作按顺序理解：

1. `ResolveStart` 解析起点策略（`LATEST/FILE_POS/GTID`）。
2. 如果有 checkpoint store，尝试加载已落库位点。  
若存在有效 checkpoint，会覆盖启动参数（`effectiveStartFromCheckpoint`）。
3. 构造 `BinlogSyncerConfig`，创建复制连接。
4. 按起点模式启动流：  
`FILE_POS` -> `StartSync(position)`；`GTID` -> `StartSyncGTID(set)`。
5. 打开当前 binlog 文件 writer（`openBinlogWriter`）。
6. 进入事件循环：  
读取 event -> 处理 rotate -> 写入 raw event -> flush+checkpoint -> 更新 checkpoint store。

### 3) rotate 处理

检测到 `ROTATE_EVENT` 时会做三件事：

1. 封口旧文件（先关闭文件句柄）。
2. 调 `finalizeSealedFile` 落一条 `binlog_files` 元数据，并执行上传（可选）。
3. 打开新文件继续写，更新 `currentStartPos/currentCreatedAt`。

这一步是“单文件生命周期”边界，元数据和上传都依赖它。

### 4) `finalizeSealedFile`（文件元数据 + 上传）

逻辑顺序：

1. 若配置了 `fileMetaStore`，先写本地元数据，默认 `UploadState=LOCAL_ONLY`。
2. 若未配置 uploader，直接返回。
3. 构建 object key（当前实现）：`<prefix>/<cluster_key>/<source_server_uuid>/<fileName>`（prefix 可空）。
4. 调 `UploadFile`。
5. 上传失败：写 `UPLOAD_FAILED` + `upload_error`，然后返回 `nil`（不中断拉流）。
6. 上传成功：写 `UPLOADED` + `object_key` + `uploaded_at`。

## 上传策略（当前实现）

- 策略：最佳努力。
- 行为：上传失败时记录 `UPLOAD_FAILED` 和错误信息，但不中断拉流。
- 意义：保证“备份持续性”优先于“实时上传成功率”。
- 命名语义（已落地）：`cluster_key + source_server_uuid` 组合，用于区分不同集群与切主场景，避免对象 key 冲突。

## cluster 语义补充

1. 文件写入采用 OPEN/SEALED 状态机，避免 failover 期间出现不完整发布文件。
2. worker 在 seal/upload 前会校验 lease/epoch，有疑问则 fail-safe 停止。
3. 任务接管时使用 `rebuild_current_file` 策略保证单文件完整性。

## checkpoint 推进语义

每个 event 都走这条路径：

1. `writer.Append(event.RawData, nextCheckpoint)`
2. `writer.FlushAndCheckpoint()`
3. 读取 `writer.CurrentCheckpoint()`
4. （可选）写入外部 checkpoint store

所以对外可恢复的 checkpoint 由 writer 的 flush 语义保障，不是“收到 event 就立即推进”。

## 辅助函数你要知道

1. `buildSyncerConfig`：补默认 `flavor=mysql`，自动生成 `server_id`。
2. `openBinlogWriter`：创建目录、清理过期文件、写 binlog magic header。
3. `cleanupExpiredBinlogs`：按 retention 天数删除过期文件，跳过活动文件。
4. `buildObjectKey`：统一上传对象路径。

## 动手练习

1. 分别创建 `LATEST` 和 `FILE_POS` 两个任务。
2. 查看 `/api/tasks/{id}/checkpoint` 与 `/api/tasks/{id}/files`。
3. 人为制造上传失败，确认任务仍继续运行。
4. 调整 `storage.retention_days`，观察历史文件是否按预期清理（活动文件不会删）。

## 自测问题

1. 为什么上传失败不能默认中断拉流？
2. rotate 后为什么要立即落一条文件元数据？
3. `effectiveStartFromCheckpoint` 为什么优先级高于请求体里的 start 配置？
4. 如果 `FlushAndCheckpoint` 失败，为什么必须立刻返回错误而不是忽略？

## 5 分钟最小实操

1. 启动一个可拉流任务，连续查看 `checkpoint` 与 `files`。
2. 观察一次 rotate 后文件状态变化（OPEN 到 seal 后元数据写入）。
3. 解释一句：为什么上传失败记录为 `UPLOAD_FAILED` 但任务继续跑。

## 本节实战检查

- 对照 `docs/learning/chapter-dod-matrix.md` 的「第 3 节」。
- 完成本节最小证据后再进入下一节。
