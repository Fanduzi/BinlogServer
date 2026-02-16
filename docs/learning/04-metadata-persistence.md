# 第 4 节：元数据持久化（MySQL Store）

## 目标

看懂任务、checkpoint、events、files 如何持久化与恢复。

## 核心文件

- `internal/meta/mysql_store.go`
- `internal/meta/mysql_store_test.go`

## 一眼看数据模型

`MySQLTaskStore` 维护 4 张表：

1. `backup_tasks`：任务配置与当前状态（`source/start/storage` 用 JSON 存）。
2. `backup_checkpoints`：每个任务一条最新位点（`file/pos/gtid_set`）。
3. `task_events`：任务事件时间线（追加写）。
4. `binlog_files`：每个封口文件的元数据与上传状态。

## 初始化与建表

### `NewMySQLTaskStore(dsn)`

1. `sql.Open` 建立连接。
2. 立刻调用 `ensureSchema` 自动建表。
3. 任一步失败都返回错误，避免服务带着半初始化状态运行。

### `ensureSchema`

顺序执行 4 条 `CREATE TABLE IF NOT EXISTS`。  
这让新环境可以“无手工建表”直接启动。

## 逐函数讲解（按对象）

### 1) 任务对象：`UpsertTask` / `ListTasks` / `DeleteTask`

1. `UpsertTask`：把 `source/start/storage` 序列化为 JSON 后 `INSERT ... ON DUPLICATE KEY UPDATE`。
2. `ListTasks`：从行数据读出后反序列化 JSON 回 `tasks.Task`。
3. `DeleteTask`：按 `task_id` 删除任务主记录。

要点：任务结构扩展时，要同步关注 JSON 编解码兼容性。

### 2) 位点对象：`UpsertCheckpoint` / `LoadCheckpoint`

1. `UpsertCheckpoint`：写最新 `file/pos/gtid_set/updated_at`。
2. `LoadCheckpoint`：无记录返回 `(empty, false, nil)`，这点对调用方非常重要。

要点：checkpoint 是“恢复优先级最高”的状态源，runner 启动会优先使用它。

### 3) 事件对象：`AppendEvent` / `ListEvents`

1. `AppendEvent`：只做追加写，保存 `task_id/type/message/detail/time/seq`。
2. `ListEvents`：SQL 按 `id DESC` 取最近 N 条，然后在内存里翻转成升序时间线返回。

要点：接口层拿到的是时间正序，更适合前端直接展示。

### 4) 文件对象：`UpsertBinlogFile` / `ListBinlogFiles`

1. `UpsertBinlogFile`：按 `(task_id,file_name)` 唯一键 upsert，支持反复更新上传状态。
2. 默认值处理：`upload_state` 空值时自动落为 `LOCAL_ONLY`。
3. `uploaded_at` 用 `sql.NullTime`，避免零时间污染。
4. `ListBinlogFiles`：按 `sealed_at DESC` 返回最近文件列表。

## 关键点

1. 服务重启后如何恢复任务与位点。
2. `binlog_files` 的上传字段：`object_key/upload_state/upload_error/uploaded_at`。
3. SQL upsert 设计如何避免重复写入冲突。
4. `task_events` 查询返回前会反转，保证时间顺序稳定。

## 恢复链路（启动时）

1. `app.Run` 注入 `MySQLTaskStore` 给 scheduler 和 runner。
2. `scheduler.Restore` 先恢复任务列表。
3. 任务真正运行时，runner 再通过 `LoadCheckpoint` 恢复精确位点。

这说明：任务恢复和位点恢复是两段式，不在同一个函数一次做完。

## 动手练习

1. 配置 `BINLOG_SERVER_META_DSN` 后启动。
2. 创建并启动任务后重启服务。
3. 验证任务、checkpoint、files 是否被正确恢复。
4. 故意将 `upload_state` 留空写入，确认读取时是否为 `LOCAL_ONLY`。
5. 调大/调小 `events?limit=`，观察返回顺序是否始终是时间正序。

## 自测问题

1. 如果元数据库短暂不可用，系统应该优先保证什么？
2. 为什么 files 元数据需要单独表，而不是塞进任务表？
3. 为什么 `ListEvents` 不是直接 `ORDER BY id ASC LIMIT ?`？
4. 如果将 `source` 从 JSON 改成列存，收益和代价分别是什么？
