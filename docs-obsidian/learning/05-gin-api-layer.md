# 05-gin-api-layer

上级：[[MOC-学习路线]]
来源文件：`docs/learning/05-gin-api-layer.md`

---

# 第 5 节：Gin API 层

## 全链路导读

- 全链路定位：控制面对外协议层（把用户操作翻译为调度行为）
- 前置阅读：第 0 节
- 学完你应能：定位一个 API 从路由到 scheduler 的调用路径，并判断状态码与错误语义

## 目标

看懂路由组织、请求参数绑定、响应脱敏和错误处理。

## 更新提示（alpha.2）

当前 API 已包含 cluster endpoints 与 Swagger UI。

## 核心文件

- `internal/api/server.go`
- `internal/api/handlers_tasks.go`
- `internal/api/server_test.go`

## 一眼看路由结构

`server.go` 中注册的核心路由：

1. 任务管理：create/list/get/update/delete/start/stop
2. 可观测接口：checkpoint/events/files/summary
3. cluster 接口：workers/cluster overview/task lease/task runs
4. Swagger：`/swagger/*any`
5. 健康检查：`/healthz`
6. UI 兼容入口：`/ui/*` 与 `/` 重定向

映射方式是：

1. `GET /api/tasks`、`POST /api/tasks` 走 `handleTasks`
2. `/api/tasks/*path` 走 `handleTaskAction` 再二次分发

## 请求进入后的分层

```text
Gin Router -> Handler (协议层)
  -> taskService 接口
      -> Scheduler / Store / Runner
```

这层的设计要点是：handler 只做 HTTP 协议处理，不直接操作 MySQL 或复制逻辑。

## 逐函数讲解

### 1) `NewServer` + `routes`

1. `gin.SetMode(gin.ReleaseMode)`，避免默认 debug 噪音。
2. `gin.New()` + `gin.Recovery()`，至少保证 panic 可恢复。
3. 注册 API 路由和 UI 路由，最后 `ServeHTTP` 转发给 Gin 引擎。

### 2) `handleTasks`

1. `POST`：解 JSON -> `CreateTask` -> 可选调用 `ConfigureSource/Start/Storage` -> 回读并返回任务。
2. `GET`：直接返回任务列表。

这里的关键是“创建 + 配置”拆开做，便于部分失败时准确返回错误。

### 3) `handleTaskAction`

职责是解析 `/api/tasks/{id}/{action}`：

1. `{id}` 只有一段时，交给 `handleTaskEntity` 处理 `GET/PUT/DELETE`。
2. `{action}` 是 `start/stop/checkpoint/events/files` 时做对应分发。
3. `{action}` 也支持 `replication/lease/runs`。
4. `events/files/runs` 支持 `limit` 参数，使用 `parseLimit` 解析并兜底。

### 4) `handleTaskEntity`

1. `GET`：按任务 ID 查详情。
2. `PUT`：支持部分更新（name/source/start/storage 任意组合）。
3. `DELETE`：删除任务。

### 5) 响应与脱敏

1. `writeJSON` 统一设置 `Content-Type` 并编码 JSON。
2. `sanitizeTask/sanitizeTaskList` 清空 `source.password`，避免敏感信息泄露。

## 关键点

1. source 密码字段在响应里做脱敏。
2. `events/files/runs/workers` 都有 limit 语义（默认值与上限不同）。
3. handler 负责协议层，业务逻辑交给 scheduler/store。
4. 任务不存在统一映射 `404`，参数/状态错误多为 `400`。
5. 非法方法统一 `405`，路径不匹配返回 `404`。

## 错误码实践（当前代码）

1. JSON 解析失败：`400`
2. 任务不存在：`404`
3. 业务校验失败（比如状态不允许 start）：`400`
4. 存储层异常：`500`
5. 方法不允许：`405`

## 动手练习

1. 用 curl 调用 `GET /api/summary`。
2. 调用 `GET /api/tasks/{id}/files?limit=20` 观察返回结构。
3. 给某个 GET 接口增加一个只读调试字段并补测试。
4. 构造一个非法 `limit`（如 `-1`、`abc`），观察 fallback 行为。

示例命令：

```bash
curl -s http://127.0.0.1:8080/api/summary | jq .
```

```bash
curl -s "http://127.0.0.1:8080/api/tasks/1/files?limit=20" | jq .
```

## 自测问题

1. 哪些错误应返回 4xx，哪些是 5xx？
2. 为什么 API 层不要直接依赖 MySQL 客户端？
3. 如果新增 `/api/tasks/{id}/metrics`，你会放在 `handleTaskAction` 还是新 handler？为什么？
4. 为什么“脱敏在 API 层做”比“数据库不存密码”更现实？

---

## 相关

- [[架构图-Mermaid版]]
- [[部署模式]]
- [[可观测性]]

## 5 分钟最小实操

1. 分别调用一条正常 API 和一条非法参数 API。
2. 对比返回码，确认你理解 4xx/5xx 边界。
3. 在代码中定位这两条请求分别经过的 handler 函数。

## 本节实战检查

- 对照 [[chapter-dod-matrix]] 的「第 5 节」。
- 完成本节最小证据后再进入下一节。
