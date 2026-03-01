# 复制数据流

本文档详细分析 binlog 复制的数据流，从 MySQL 到本地文件。

## 1. 数据流概览

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│   MySQL     │────►│   Runner    │────►│   Writer    │────►│   Uploader  │
│  (binlog)   │     │  (解析)     │     │  (写文件)   │     │  (可选)     │
└─────────────┘     └─────────────┘     └─────────────┘     └─────────────┘
                           │                   │
                           │                   │
                           ▼                   ▼
                    ┌─────────────┐     ┌─────────────┐
                    │ Checkpoint  │     │    文件     │
                    │  (位点)     │     │  (本地)     │
                    └─────────────┘     └─────────────┘
```

## 2. MySQL Runner

### 2.1 Runner 接口

```go
type Runner interface {
    // Run 执行复制任务，阻塞直到 ctx 取消或出错
    Run(ctx context.Context, task Task) error

    // GetStatus 获取当前复制状态
    GetStatus() ReplicationStatus
}
```

### 2.2 MySQLRunner 结构

```go
type MySQLRunner struct {
    binlogWriter    *binlog.Writer       // 文件写入器
    checkpointStore meta.CheckpointStore // 位点存储
    uploader        upload.Uploader      // 上传器（可选）

    // 运行时状态
    mu       sync.Mutex
    status   ReplicationStatus
    progress Progress
}
```

### 2.3 Run 主循环

```go
func (r *MySQLRunner) Run(ctx context.Context, task Task) error {
    // 1. 连接 MySQL
    conn, err := r.connectMySQL(task.Source)
    if err != nil {
        return fmt.Errorf("connect mysql: %w", err)
    }
    defer conn.Close()

    // 2. 获取起始位置
    startPos, err := r.getStartPosition(ctx, task)
    if err != nil {
        return fmt.Errorf("get start position: %w", err)
    }

    // 3. 请求 binlog 流
    stream, err := conn.StartBinlogDump(startPos)
    if err != nil {
        return fmt.Errorf("start binlog dump: %w", err)
    }

    // 4. 主循环：读取事件、写入文件
    for {
        select {
        case <-ctx.Done():
            return nil  // 正常退出
        default:
        }

        event, err := stream.Next()
        if err != nil {
            return fmt.Errorf("read event: %w", err)
        }

        if err := r.handleEvent(ctx, event); err != nil {
            return fmt.Errorf("handle event: %w", err)
        }
    }
}
```

## 3. 起始位置确定

### 3.1 三种启动模式

```go
func (r *MySQLRunner) getStartPosition(ctx context.Context, task Task) (binlog.Position, error) {
    // 优先使用持久化的 checkpoint
    if cp, exists, err := r.checkpointStore.GetCheckpoint(ctx, task.ID); err == nil && exists {
        return binlog.Position{Name: cp.File, Pos: cp.Pos}, nil
    }

    // 根据 task.Start.Mode 确定
    switch task.Start.Mode {
    case "LATEST":
        // 从最新位置开始
        return binlog.Position{}, nil

    case "FILE_POS":
        // 从指定文件位置开始
        return binlog.Position{
            Name: task.Start.File,
            Pos:  task.Start.Pos,
        }, nil

    case "GTID":
        // 从指定 GTID 开始
        return binlog.GTIDSet(task.Start.GTID), nil

    default:
        return nil, fmt.Errorf("unknown start mode: %s", task.Start.Mode)
    }
}
```

### 3.2 Server ID 生成

```go
func (r *MySQLRunner) generateServerID(task Task) uint32 {
    // 基于 cluster_key 生成稳定的 server_id
    // 保证同一任务每次重启使用相同的 server_id
    h := fnv.New32a()
    h.Write([]byte(task.ClusterKey))
    return defaultServerIDBase + (h.Sum32() % defaultServerIDMod)
}
```

**为什么要稳定的 server_id？**
- MySQL 会记住每个 server_id 的复制位置
- 重启后可以继续从上次的位置开始

## 4. 事件处理

### 4.1 事件类型

```go
const (
    EventTypeRotate      = 0x04  // 文件切换
    EventTypeFormatDesc  = 0x0f  // 格式描述
    EventTypeQuery       = 0x02  // 查询事件
    EventTypeXID         = 0x10  // 事务提交
    EventTypeWriteRows   = 0x1e  // 写入行
    EventTypeUpdateRows  = 0x1f  // 更新行
    EventTypeDeleteRows  = 0x20  // 删除行
)
```

### 4.2 handleEvent

```go
func (r *MySQLRunner) handleEvent(ctx context.Context, event BinlogEvent) error {
    // 1. 写入文件
    if err := r.binlogWriter.Write(event); err != nil {
        return fmt.Errorf("write event: %w", err)
    }

    // 2. 更新进度
    r.mu.Lock()
    r.progress.CurrentFile = event.File
    r.progress.CurrentPos = event.Pos
    r.progress.LastEventAt = time.Now()
    r.mu.Unlock()

    // 3. 处理文件切换
    if event.Type == EventTypeRotate {
        if err := r.handleRotate(ctx, event); err != nil {
            return fmt.Errorf("handle rotate: %w", err)
        }
    }

    // 4. 定期保存 checkpoint
    if time.Since(r.lastCheckpointSave) > checkpointSaveInterval {
        if err := r.saveCheckpoint(ctx); err != nil {
            log.Printf("save checkpoint failed: %v", err)
        }
        r.lastCheckpointSave = time.Now()
    }

    return nil
}
```

## 5. 文件写入器

### 5.1 Writer 结构

```go
type Writer struct {
    dataDir      string
    currentFile  *os.File
    currentName  string
    currentSize  int64
    maxFileSize  int64

    mu sync.Mutex
}
```

### 5.2 Write 实现

```go
func (w *Writer) Write(event BinlogEvent) error {
    w.mu.Lock()
    defer w.mu.Unlock()

    // 1. 检查是否需要切换文件
    if w.currentFile == nil || w.currentSize > w.maxFileSize {
        if err := w.rotateFile(event.File); err != nil {
            return err
        }
    }

    // 2. 写入事件
    n, err := w.currentFile.Write(event.Bytes)
    if err != nil {
        return fmt.Errorf("write: %w", err)
    }

    // 3. fsync 确保持久化
    if err := w.currentFile.Sync(); err != nil {
        return fmt.Errorf("sync: %w", err)
    }

    // 4. 更新大小
    w.currentSize += int64(n)

    return nil
}
```

### 5.3 为什么每次都 fsync？

**关键设计决策：先 fsync，再更新 checkpoint**

```
错误做法：
  Write → Update Checkpoint → fsync
  问题：如果 fsync 失败，checkpoint 已经更新，重启后数据丢失

正确做法：
  Write → fsync → Update Checkpoint
  优点：只有确保数据落盘后才更新 checkpoint
```

## 6. Checkpoint 管理

### 6.1 Checkpoint 结构

```go
type Checkpoint struct {
    TaskID      string
    File        string    // 当前文件名
    Pos         uint32    // 文件内位置
    GTID        string    // GTID 集合（可选）
    UpdatedAt   time.Time
}
```

### 6.2 保存 Checkpoint

```go
func (r *MySQLRunner) saveCheckpoint(ctx context.Context) error {
    r.mu.Lock()
    cp := Checkpoint{
        TaskID:    r.taskID,
        File:      r.progress.CurrentFile,
        Pos:       r.progress.CurrentPos,
        UpdatedAt: time.Now(),
    }
    r.mu.Unlock()

    return r.checkpointStore.SaveCheckpoint(ctx, r.taskID, cp)
}
```

### 6.3 Checkpoint 恢复

```go
func (r *MySQLRunner) getStartPosition(ctx context.Context, task Task) (binlog.Position, error) {
    cp, exists, err := r.checkpointStore.GetCheckpoint(ctx, task.ID)
    if err != nil {
        return nil, err
    }

    if exists {
        log.Printf("resuming from checkpoint: %s:%d", cp.File, cp.Pos)
        return binlog.Position{Name: cp.File, Pos: cp.Pos}, nil
    }

    // 没有checkpoint，根据启动模式确定
    // ...
}
```

## 7. 文件切换处理

### 7.1 触发条件

1. 收到 ROTATE 事件（MySQL 主动切换）
2. 当前文件超过 maxFileSize

### 7.2 处理流程

```go
func (r *MySQLRunner) handleRotate(ctx context.Context, event BinlogEvent) error {
    // 1. 关闭当前文件
    if err := r.binlogWriter.Close(); err != nil {
        return fmt.Errorf("close file: %w", err)
    }

    // 2. 保存 checkpoint
    if err := r.saveCheckpoint(ctx); err != nil {
        return fmt.Errorf("save checkpoint: %w", err)
    }

    // 3. 记录文件元数据
    if err := r.fileStore.UpsertFile(ctx, FileMeta{
        TaskID:   r.taskID,
        FileName: r.binlogWriter.CurrentName(),
        Size:     r.binlogWriter.CurrentSize(),
    }); err != nil {
        log.Printf("save file meta failed: %v", err)
    }

    // 4. 触发上传（异步，不阻塞）
    if r.uploader != nil {
        go r.uploadFile(ctx, r.binlogWriter.CurrentName())
    }

    return nil
}
```

## 8. 上传处理

### 8.1 上传策略

**关键设计：上传是 best-effort，不阻塞复制**

```go
func (r *MySQLRunner) uploadFile(ctx context.Context, fileName string) {
    filePath := filepath.Join(r.dataDir, fileName)

    // 上传
    if err := r.uploader.Upload(ctx, filePath, r.getObjectKey(fileName)); err != nil {
        log.Printf("upload failed: %v", err)

        // 更新文件状态为 UPLOAD_FAILED
        r.fileStore.UpdateUploadStatus(ctx, r.taskID, fileName, "UPLOAD_FAILED", err.Error())
        return
    }

    // 更新文件状态为 UPLOADED
    r.fileStore.UpdateUploadStatus(ctx, r.taskID, fileName, "UPLOADED", "")
}
```

### 8.2 对象 Key 格式

```
<prefix>/<cluster_key>/<source_server_uuid>/<fileName>

示例：
binlog-backup/prod-cluster/3e11fa47-71ca-11e1-9e33-c80aa9429562/mysql-bin.000010
```

### 8.3 手动重试上传

```bash
# API 端点
POST /api/tasks/{task_id}/files/retry-upload
```

## 9. 错误处理

### 9.1 可恢复错误

| 错误 | 处理 |
|------|------|
| 网络断开 | 自动重连 |
| MySQL 临时不可用 | 重试 |

### 9.2 不可恢复错误

| 错误 | 处理 |
|------|------|
| 认证失败 | 任务失败，记录错误 |
| 权限不足 | 任务失败，记录错误 |
| binlog 已删除 | 任务失败，记录错误 |

### 9.3 重连逻辑

```go
func (r *MySQLRunner) Run(ctx context.Context, task Task) error {
    for {
        err := r.runOnce(ctx, task)
        if err == nil {
            return nil  // ctx 取消，正常退出
        }

        // 判断是否可重试
        if !isRetriable(err) {
            return err
        }

        log.Printf("replication error, retrying: %v", err)
        select {
        case <-ctx.Done():
            return nil
        case <-time.After(retryInterval):
            continue
        }
    }
}
```

## 10. 代码位置

| 组件 | 文件 |
|------|------|
| Runner 接口 | `internal/tasks/runner.go` |
| MySQLRunner | `internal/replication/mysql_runner.go` |
| Writer | `internal/binlog/writer.go` |
| Checkpoint | `internal/meta/mysql_store.go` |

## 11. 本章小结

1. **数据流**：MySQL → Runner → Writer → Checkpoint
2. **可靠性**：先 fsync 再更新 checkpoint
3. **上传**：best-effort，不阻塞复制
4. **断点恢复**：从 checkpoint 继续
