# API 参考

本文档列出所有 API 端点及其参数，每个端点都附带 **curl 示例**。

## 1. 基础信息

| 项目 | 值 |
|------|-----|
| Base URL | `http://host:port` |
| Content-Type | `application/json` |
| Swagger UI | `http://host:port/swagger/index.html` |

## 2. Swagger 交互式文档

### 2.1 访问 Swagger UI

启动服务后，打开浏览器访问：

```
http://127.0.0.1:8080/swagger/index.html
```

### 2.2 Swagger UI 功能

1. **浏览 API** - 查看所有端点、参数、响应格式
2. **Try it out** - 在线发请求，实时查看响应
3. **Schema** - 查看请求/响应的数据结构

### 2.3 API 分组

| 分组 | 说明 |
|------|------|
| System | 健康检查 |
| Dashboard | 汇总、大盘、源库反查 |
| Tasks | 任务 CRUD、启停、状态查询 |
| Cluster | Worker 列表、集群概览 |

### 2.4 更新 Swagger 文档

修改代码中的 Swagger 注解后，重新生成：

```bash
go run github.com/swaggo/swag/cmd/swag@v1.16.6 init \
  -g cmd/binlog-server/main.go \
  -o internal/swaggerdocs \
  --parseInternal
```

## 3. 任务管理 API

### 3.1 创建任务

```bash
# 从最新位置开始（LATEST）
curl -X POST http://localhost:8080/api/tasks \
  -H "Content-Type: application/json" \
  -d '{
    "name": "backup-mysql-prod",
    "cluster_key": "prod-cluster",
    "source": {
      "host": "10.0.0.1",
      "port": 3306,
      "user": "repl",
      "password": "secret"
    },
    "start": {
      "mode": "LATEST"
    },
    "storage": {
      "retention_days": 30
    }
  }'
```

```bash
# 从指定文件位置开始（FILE_POS）
curl -X POST http://localhost:8080/api/tasks \
  -H "Content-Type: application/json" \
  -d '{
    "name": "backup-mysql-prod",
    "cluster_key": "prod-cluster",
    "source": {
      "host": "10.0.0.1",
      "port": 3306,
      "user": "repl",
      "password": "secret"
    },
    "start": {
      "mode": "FILE_POS",
      "file": "mysql-bin.000010",
      "pos": 12345
    }
  }'
```

```bash
# 从指定 GTID 开始（GTID）
curl -X POST http://localhost:8080/api/tasks \
  -H "Content-Type: application/json" \
  -d '{
    "name": "backup-mysql-prod",
    "cluster_key": "prod-cluster",
    "source": {
      "host": "10.0.0.1",
      "port": 3306,
      "user": "repl",
      "password": "secret"
    },
    "start": {
      "mode": "GTID",
      "gtid_set": "3E11FA47-71CA-11E1-9E33-C80AA9429562:1-100"
    }
  }'
```

**请求字段：**

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| name | string | 是 | 任务名称 |
| cluster_key | string | 是 | 集群标识（全局唯一，`[a-zA-Z0-9._-]`） |
| source.host | string | 是 | MySQL 主机 |
| source.port | int | 是 | MySQL 端口 |
| source.user | string | 是 | 复制用户 |
| source.password | string | 是 | 复制密码 |
| start.mode | string | 是 | LATEST / FILE_POS / GTID |
| start.file | string | 条件 | 文件名（FILE_POS 模式必填） |
| start.pos | int | 条件 | 位置（FILE_POS 模式必填） |
| start.gtid_set | string | 条件 | GTID 集合（GTID 模式必填；也接受别名 `gtid`） |
| storage.retention_days | int | 否 | 保留天数（默认 7，范围 1-3650） |
| storage.dir | string | 否 | **不支持**。文件固定写在 `{data_dir}/{task_id}/`，传入非空值会 400 |

**响应示例：**

```json
{
  "id": "task-xxx",
  "name": "backup-mysql-prod",
  "state": "PENDING",
  "created_at": "2024-01-01T10:00:00Z"
}
```

### 3.2 列出任务

```bash
# 列出所有任务
curl http://localhost:8080/api/tasks

# 按状态过滤
curl "http://localhost:8080/api/tasks?state=RUNNING"

# 按集群过滤
curl "http://localhost:8080/api/tasks?cluster_key=prod-cluster"
```

**查询参数：**

| 参数 | 类型 | 说明 |
|------|------|------|
| state | string | 按状态过滤：PENDING / STARTING / RUNNING / STOPPED / ERROR |
| cluster_key | string | 按集群过滤 |

**响应示例：**

```json
{
  "tasks": [
    {
      "id": "task-xxx",
      "name": "backup-mysql-prod",
      "cluster_key": "prod-cluster",
      "state": "RUNNING",
      "source": {"host": "10.0.0.1", "port": 3306},
      "owner_worker_id": "worker-1",
      "created_at": "2024-01-01T10:00:00Z"
    }
  ]
}
```

### 3.3 获取任务详情

```bash
curl http://localhost:8080/api/tasks/{task_id}
```

**响应示例：**

```json
{
  "id": "task-xxx",
  "name": "backup-mysql-prod",
  "cluster_key": "prod-cluster",
  "state": "RUNNING",
  "source": {"host": "10.0.0.1", "port": 3306, "user": "repl"},
  "start": {"mode": "LATEST"},
  "storage": {"retention_days": 30},
  "owner_worker_id": "worker-1",
  "epoch": 1,
  "last_error": "",
  "created_at": "2024-01-01T10:00:00Z",
  "updated_at": "2024-01-01T10:05:00Z"
}
```

### 3.4 启动任务

```bash
curl -X POST http://localhost:8080/api/tasks/{task_id}/start
```

**响应示例：**

```json
{"id": "task-xxx", "state": "STARTING"}
```

### 3.5 停止任务

```bash
curl -X POST http://localhost:8080/api/tasks/{task_id}/stop
```

**响应示例：**

```json
{"id": "task-xxx", "state": "STOPPING"}
```

### 3.6 删除任务

```bash
curl -X DELETE http://localhost:8080/api/tasks/{task_id}
```

**响应示例：**

```json
{"id": "task-xxx", "deleted": true}
```

## 4. 复制状态 API

### 4.1 获取 Checkpoint

```bash
curl http://localhost:8080/api/tasks/{task_id}/checkpoint
```

**响应示例：**

```json
{
  "task_id": "task-xxx",
  "file": "mysql-bin.000010",
  "pos": 12345,
  "gtid": "3e11fa47-71ca-11e1-9e33-c80aa9429562:1-100",
  "updated_at": "2024-01-01T10:05:00Z"
}
```

### 4.2 获取复制状态

```bash
curl http://localhost:8080/api/tasks/{task_id}/replication
```

**响应示例：**

```json
{
  "task_id": "task-xxx",
  "state": "RUNNING",
  "status": "NORMAL",
  "threshold_seconds": 30,
  "has_progress": true,
  "delay_seconds": 0.5,
  "last_event_at": "2024-01-01T10:05:00Z",
  "last_event_file": "mysql-bin.000010",
  "last_event_pos": 12345
}
```

**status 字段：**

| 值 | 说明 |
|----|------|
| NORMAL | 正常，延迟 < 阈值 |
| DELAYED | 延迟，延迟 >= 阈值 |
| ABNORMAL | 异常，无法获取位点 |
| IDLE | 空闲，长时间无事件 |

## 5. 文件管理 API

### 5.1 列出文件

```bash
# 列出所有文件
curl http://localhost:8080/api/tasks/{task_id}/files

# 按上传状态过滤
curl "http://localhost:8080/api/tasks/{task_id}/files?upload_status=UPLOAD_FAILED"
```

**查询参数：**

| 参数 | 类型 | 说明 |
|------|------|------|
| upload_status | string | LOCAL_ONLY / UPLOADED / UPLOAD_FAILED |

**响应示例：**

```json
{
  "files": [
    {
      "file_name": "mysql-bin.000001",
      "size": 1048576,
      "upload_status": "UPLOADED",
      "created_at": "2024-01-01T10:00:00Z"
    },
    {
      "file_name": "mysql-bin.000002",
      "size": 524288,
      "upload_status": "LOCAL_ONLY",
      "created_at": "2024-01-01T10:05:00Z"
    }
  ]
}
```

### 5.2 重试上传

```bash
# 重试所有失败文件
curl -X POST http://localhost:8080/api/tasks/{task_id}/files/retry-upload

# 重试指定文件
curl -X POST http://localhost:8080/api/tasks/{task_id}/files/retry-upload \
  -H "Content-Type: application/json" \
  -d '{"file_names": ["mysql-bin.000002"]}'
```

**响应示例：**

```json
{"retried": 1, "files": ["mysql-bin.000002"]}
```

### 5.3 查看上传失败原因

```bash
curl "http://localhost:8080/api/tasks/{task_id}/upload-failures/reasons?limit=10"
```

**响应示例：**

```json
{
  "reasons": [
    {"reason": "Access Denied", "count": 5},
    {"reason": "Connection Timeout", "count": 2}
  ]
}
```

## 6. 事件查询 API

### 6.1 列出任务事件

```bash
# 最近 50 条事件
curl http://localhost:8080/api/tasks/{task_id}/events

# 指定数量
curl "http://localhost:8080/api/tasks/{task_id}/events?limit=100"

# 按类型过滤
curl "http://localhost:8080/api/tasks/{task_id}/events?event_type=TASK_ERROR"
```

**查询参数：**

| 参数 | 类型 | 说明 |
|------|------|------|
| limit | int | 返回数量（默认 50） |
| event_type | string | 按类型过滤 |

**响应示例：**

```json
{
  "events": [
    {
      "id": 1,
      "task_id": "task-xxx",
      "event_type": "TASK_STARTED",
      "message": "task started",
      "detail": "",
      "created_at": "2024-01-01T10:00:00Z"
    }
  ]
}
```

**常见事件类型：**

| 类型 | 说明 |
|------|------|
| TASK_CREATED | 任务创建 |
| TASK_STARTED | 任务启动 |
| TASK_STOPPED | 任务停止 |
| TASK_ERROR | 任务错误 |
| TASK_FILE_ROTATED | 文件切换 |
| TASK_FILE_UPLOADED | 文件上传成功 |
| TASK_FILE_UPLOAD_FAILED | 文件上传失败 |
| TASK_LEASE_ACQUIRED | 获取租约 |
| TASK_LEASE_LOST | 租约丢失 |

## 7. 集群管理 API

### 7.1 列出 Workers

```bash
curl http://localhost:8080/api/workers

# 限制返回数量
curl "http://localhost:8080/api/workers?limit=10"
```

**响应示例：**

```json
{
  "workers": [
    {
      "worker_id": "worker-1",
      "session_id": "abc123",
      "role": "worker",
      "host": "10.0.1.1",
      "version": "v0.4.1",
      "status": "ONLINE",
      "expires_at": "2024-01-01T10:10:00Z",
      "updated_at": "2024-01-01T10:00:00Z"
    }
  ]
}
```

### 7.2 集群概览

```bash
curl http://localhost:8080/api/cluster/overview
```

**响应示例：**

```json
{
  "mode": "cluster",
  "workers": {"total": 2, "active": 2},
  "tasks": {
    "total": 10,
    "running": 8,
    "starting": 2,
    "stopped": 0,
    "error": 0
  }
}
```

### 7.3 查看任务 Lease

```bash
curl http://localhost:8080/api/tasks/{task_id}/lease
```

**响应示例：**

```json
{
  "task_id": "task-xxx",
  "worker_id": "worker-1",
  "epoch": 1,
  "expires_at": "2024-01-01T10:05:00Z",
  "is_valid": true
}
```

## 8. 监控端点

### 8.1 健康检查

```bash
curl http://localhost:8080/api/health
```

**响应：**

```json
{"status": "ok"}
```

### 8.2 汇总信息

```bash
curl http://localhost:8080/api/summary
```

**响应示例：**

```json
{
  "tasks_total": 10,
  "tasks_running": 8,
  "tasks_starting": 2,
  "workers_active": 2
}
```

### 8.3 Dashboard 数据

```bash
curl http://localhost:8080/api/dashboard
```

**响应示例：**

```json
{
  "summary": {"total": 10, "running": 8},
  "tasks": [...],
  "sources": [...]
}
```

### 8.4 源库反查

```bash
curl "http://localhost:8080/api/sources/lookup?host=10.0.0.1&port=3306"
```

**响应示例：**

```json
{
  "host": "10.0.0.1",
  "port": 3306,
  "exists": true,
  "count": 2,
  "task_ids": ["task-1", "task-2"]
}
```

### 8.5 Prometheus 指标

```bash
curl http://localhost:8080/metrics
```

返回 Prometheus 格式的指标。

## 9. 错误响应

**格式：**

```json
{
  "error": "task not found",
  "code": "TASK_NOT_FOUND"
}
```

**常见错误码：**

| HTTP 状态码 | 错误码 | 说明 |
|------------|--------|------|
| 400 | INVALID_REQUEST | 请求参数错误（校验失败不落库） |
| 404 | TASK_NOT_FOUND | 任务不存在 |
| 409 | TASK_ALREADY_EXISTS | 任务已存在 |
| 409 | INVALID_STATE_TRANSITION | 状态转换非法 |
| 500 | INTERNAL_ERROR | 内部错误 |

任务 `last_error` 里的稳定源库错误码：

| 错误码 | 说明 | 重试 |
|--------|------|------|
| SOURCE_ACCESS_DENIED | 源库 ERROR 1045 | 否，进入 FAILED |
| SOURCE_LOG_BIN_OFF | 源库未开 binlog | 否，进入 FAILED |
| SOURCE_IDENTITY_UNAVAILABLE | 无法读取源库身份 | 否，进入 FAILED |

## 10. 常用调试场景

### 场景 A：确认服务正常

```bash
curl http://localhost:8080/api/health
# 期望：{"status": "ok"}
```

### 场景 B：检查源库是否已配置备份

```bash
curl "http://localhost:8080/api/sources/lookup?host=10.0.0.1&port=3306"
# exists=true：已有任务
# exists=false：尚未配置
```

### 场景 C：检查任务复制延迟

```bash
curl http://localhost:8080/api/tasks/{task_id}/replication
# 关注：status, delay_seconds
```

### 场景 D：上传失败后批量补传

```bash
# 1. 查看失败原因
curl "http://localhost:8080/api/tasks/{task_id}/upload-failures/reasons"

# 2. 修复问题后重试
curl -X POST http://localhost:8080/api/tasks/{task_id}/files/retry-upload

# 3. 确认结果
curl "http://localhost:8080/api/tasks/{task_id}/files?upload_status=UPLOAD_FAILED"
```
