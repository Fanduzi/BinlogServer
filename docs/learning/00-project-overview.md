# 第 0 节：项目总览（先建立地图）

如果你感觉“单个函数看得懂，但整体不知道在干什么”，先读这节。

## 1. 项目要解决什么问题

目标是把大量 MySQL 实例的 binlog 备份统一管理起来：

1. 统一创建/启动/停止备份任务（不再手动维护大量 `mysqlbinlog --stop-never` 进程）
2. 拉取到本地后可选上传到 S3/OBS
3. 记录完整元数据（任务状态、checkpoint、事件、文件与上传状态）

## 2. 三条主链路（必须记住）

1. 控制面链路  
前端/API 接收操作 -> 调度器变更任务状态
2. 数据面链路  
runner 连接 MySQL 复制协议 -> 写本地 binlog -> 推进 checkpoint
3. 可观测链路  
events/files/checkpoint 持久化 -> API 查询 -> 前端展示

## 3. 组件职责总表

1. 启动装配层  
`cmd/binlog-server/main.go` + `internal/app/app.go`  
负责读取配置、注入依赖、启动 HTTP 服务、优雅停机。
2. 配置层  
`internal/config/config.go`  
负责默认值和环境变量读取。
3. API 层（协议层）  
`internal/api/server.go` + `internal/api/handlers_tasks.go`  
负责路由、参数解析、状态码、响应脱敏。
4. 调度层（业务控制面）  
`internal/tasks/scheduler.go`  
负责任务状态机、start/stop、重试、事件。
5. 复制层（数据面）  
`internal/replication/mysql_runner.go`  
负责 MySQL 拉流、文件滚动、checkpoint、上传。
6. 持久化层  
`internal/meta/mysql_store.go`  
负责任务、位点、事件、文件元数据入库和查询。
7. 前端层  
`frontend/src/App.vue` + `frontend/src/api.js`  
负责管理台展示和交互。

## 4. 启动时序（高频面试题）

1. `main` 加载配置并创建带信号的 `context`
2. `app.Run` 组装 store/uploader/runner/scheduler
3. `scheduler.Restore` 恢复任务控制面状态
4. 启动 Gin HTTP 服务
5. 收到 `SIGINT/SIGTERM` 后走 `Shutdown`

注意：`Restore` 恢复的是任务，不是 checkpoint。checkpoint 在 runner 启动时读取。

## 5. 一次任务的生命周期

1. 创建任务：保存 `source/start/storage`
2. 启动任务：进入 `RUNNING`，runner goroutine 开始拉流
3. 持续写入：event 写文件，flush 成功后推进 checkpoint
4. rotate 封口：写一条 `binlog_files` 元数据
5. 上传（可选）：成功 `UPLOADED`，失败 `UPLOAD_FAILED`（最佳努力，不中断拉流）
6. 停止任务：cancel 上下文，状态转 `STOPPED`

## 6. 关键术语速查

1. `Restore`：启动恢复任务状态到内存
2. `Checkpoint`：断点续传位点（`file/pos/gtid`）
3. `Rotate`：binlog 文件切换事件
4. `Best-effort upload`：上传失败仅记状态，不停止拉流
5. `Retention`：本地文件保留天数清理策略

## 7. 最推荐的阅读顺序

1. `main.go` -> `app.go`（先懂系统如何拼起来）
2. `handlers_tasks.go` -> `scheduler.go`（懂控制面）
3. `mysql_runner.go`（懂数据面）
4. `mysql_store.go`（懂恢复与可观测）

读到这里后，再进入第 1~7 节细讲会轻松很多。

