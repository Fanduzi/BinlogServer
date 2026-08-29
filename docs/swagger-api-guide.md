# Swagger API 使用指南

本文档面向日常运维和开源协作者，说明如何用 Swagger UI 快速浏览、调试 `binlog_server` 的 API。

## 1. 访问入口

启动后端后，打开：

- `http://127.0.0.1:8080/swagger/index.html`

如果你的服务监听在其他地址，请替换 host/port。

## 2. 页面里能做什么

在 Swagger UI 中，你可以直接完成这些动作：

1. 浏览所有 API endpoint
2. 查看请求参数与响应 schema
3. 点击 `Try it out` 在线发请求
4. 修改 query/path/body 参数并查看实时响应

这正好覆盖了日常联调中“先看接口定义，再试一次真实请求”的完整流程。

## 3. 接口分组说明

当前文档主要分为四个分组：

1. `System`
2. `Dashboard`
3. `Tasks`
4. `Cluster`

### 3.1 System

- `GET /healthz`：服务健康检查，返回 `ok`。

### 3.2 Dashboard

- `GET /api/summary`
  作用：返回任务总览计数（running/stopped/failed 等）。
- `GET /api/dashboard?host=...&port=...&state=...&limit=...&offset=...`
  作用：返回大盘聚合数据（summary + tasks + sources）。过滤后的全量结果用于 summary/sources 聚合，只有 tasks 明细按页返回；响应顶层带有效 `total`、`limit`、`offset`。
- `GET /api/sources/lookup?host=...&port=...`
  作用：按源库 `host:port` 判断是否已有拉取任务，返回存在性与匹配数量。

### 3.3 Tasks

- `POST /api/tasks`：创建任务
- `POST /api/tasks/batch`：批量创建任务（`items` 为 1..100 个创建请求；envelope 错误整体 400，合法 envelope 返回有序逐项结果）
- `GET /api/tasks`：任务列表（支持 `host`/`port`/`state` 过滤与 `limit`/`offset` 分页；默认 100，limit 仅允许 1..500，超过 500 返回 400 `invalid limit`）
- `GET /api/tasks/{id}`：任务详情
- `PUT /api/tasks/{id}`：更新任务
- `DELETE /api/tasks/{id}`：删除任务
- `POST /api/tasks/{id}/start`：启动任务
- `POST /api/tasks/{id}/stop`：停止任务
- `GET /api/tasks/{id}/checkpoint`：查看 checkpoint
- `GET /api/tasks/{id}/events`：查看事件流
- `GET /api/tasks/{id}/files`：查看 binlog 文件元数据（`state=OPEN` 表示当前未封存 segment，`SEALED` 表示已封存）
- `POST /api/tasks/{id}/files/retry-upload`：手动补传 `UPLOAD_FAILED` 文件
- `GET /api/tasks/{id}/upload-failures/reasons`：按错误原因聚合上传失败记录
- `GET /api/tasks/{id}/replication`：查看复制延迟与最新位点

### 3.4 Cluster

- `GET /api/workers`：worker 列表与在线状态（支持 `limit`）
- `GET /api/cluster/overview`：cluster 汇总信息
- `GET /api/tasks/{id}/lease`：任务 lease 持有者与风险状态
- `GET /api/tasks/{id}/runs`：任务 run history（支持 `limit`）

## 4. 常用调试场景（建议顺序）

### 场景 A：确认服务与文档正常

1. 打开 Swagger UI
2. 执行 `GET /healthz`
3. 返回 `200` 且 body 为 `ok`

### 场景 B：检查某主库是否已配置拉取任务

1. 执行 `GET /api/sources/lookup`
2. 填写 `host` 与 `port`
3. 观察返回：
   - `exists=true`：已有任务
   - `exists=false`：尚未配置
   - `count`：匹配任务数量

### 场景 C：检查任务是否延迟

1. 执行 `GET /api/tasks/{id}/replication`
2. 关注字段：
   - `status`：`NORMAL/DELAYED/ABNORMAL/IDLE`
   - `delay_seconds`：当前延迟秒数
   - `last_event_file/last_event_pos`：最近位点

### 场景 D：大盘视角排查

1. 执行 `GET /api/dashboard?state=FAILED&limit=20&offset=0`
2. 观察：
   - `total/limit/offset`：过滤结果总数与有效分页参数
   - `summary`：全部过滤任务统计，不受当前页大小影响
   - `tasks[].replication`：当前页单任务复制状态
   - `sources[]`：全部过滤任务按源库聚合统计

### 场景 E：上传失败后的批量补传

1. 先执行 `GET /api/tasks/{id}/upload-failures/reasons?limit=20`，确认主要失败原因与频次。
2. 修复凭证/网络/Bucket 权限后，执行 `POST /api/tasks/{id}/files/retry-upload?limit=100`。
3. 再执行 `GET /api/tasks/{id}/files`，确认失败记录逐步转为 `UPLOADED`。

### 场景 F：批量创建任务

1. 使用 `POST /api/tasks/batch`，请求体为 `{ "items": [ ... ] }`，最多提交 100 个现有创建请求。
2. 缺失/空/格式错误 envelope 或超过 100 项时返回 HTTP 400，且不会创建任务。
3. 合法 envelope 返回 HTTP 200 的有序结果数组；每个失败项带 `INVALID_REQUEST` 错误，后续项仍会继续处理，成功任务中的 source 密码不会返回。

## 5. 示例请求与返回

### 5.1 源库反查

请求：

```http
GET /api/sources/lookup?host=10.0.0.9&port=3306
```

返回示例：

```json
{
  "host": "10.0.0.9",
  "port": 3306,
  "exists": true,
  "count": 2,
  "task_ids": ["1", "2"]
}
```

### 5.2 复制状态

请求：

```http
GET /api/tasks/1/replication
```

返回示例：

```json
{
  "task_id": "1",
  "state": "RUNNING",
  "status": "NORMAL",
  "threshold_seconds": 30,
  "has_progress": true,
  "delay_seconds": 3,
  "last_event_at": "2026-02-16T16:30:01Z",
  "last_event_file": "mysql-bin.000001",
  "last_event_pos": 123
}
```

## 6. 更新文档（开发者）

当你新增/修改了 Swagger 注解后，重新生成文档：

```bash
go run github.com/swaggo/swag/cmd/swag@v1.16.6 init -g cmd/binlog-server/main.go -o internal/swaggerdocs --parseInternal
```

建议在 PR 中一并提交这些文件：

1. `internal/swaggerdocs/docs.go`
2. `internal/swaggerdocs/swagger.json`
3. `internal/swaggerdocs/swagger.yaml`
