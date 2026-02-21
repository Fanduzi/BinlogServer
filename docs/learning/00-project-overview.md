# 第 0 节：项目总览（先建立地图）

如果你感觉“单个函数看得懂，但整体不知道在干什么”，先读这节。

## 全链路导读

- 全链路定位：系统地图（控制面 + 数据面 + 可观测面总览）
- 前置阅读：无（课程起点）
- 学完你应能：用 3 分钟讲清系统边界、核心角色与主链路

## 1. 项目要解决什么问题

目标是把大量 MySQL 实例的 binlog 备份统一管理起来：

1. 统一创建/启动/停止备份任务（不再手动维护大量 `mysqlbinlog --stop-never` 进程）
2. 拉取到本地后可选上传到 S3/OBS
3. 记录完整元数据（任务状态、checkpoint、事件、文件与上传状态）

当前版本已支持两种部署范式：

1. standalone（单进程）
2. cluster（control-plane + worker 分离，支持高可用接管）

## 2. 三条主链路（重点掌握）

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

## 4. 启动时序（重点理解）

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

cluster 模式下会额外出现：

1. control-plane dispatch 到 `STARTING`
2. worker claim + lease acquire 后进入 `RUNNING`
3. lease 异常时进入 `LEASE_DEGRADED`，超出 grace 会 fail-safe stop

## 6. 关键术语速查

1. `Restore`：启动恢复任务状态到内存
2. `Checkpoint`：断点续传位点（`file/pos/gtid`）
3. `Rotate`：binlog 文件切换事件
4. `Best-effort upload`：上传失败仅记状态，不停止拉流
5. `Retention`：本地文件保留天数清理策略

## 7. 最推荐的阅读顺序（自顶向下）

先按“系统 -> 控制面 -> 数据面 -> 集群可靠性”的顺序读文档，再下钻到函数级别：

1. 系统地图：`00-project-overview` + `docs/architecture-diagrams.md`
2. 控制面链路：`05-gin-api-layer` -> `02-task-model-and-scheduler` -> `01-startup-and-wiring`
3. 数据面链路：`03-mysql-replication-runner` -> `04-metadata-persistence`
4. 集群与运维：`09/10/11` -> `07` -> `12` -> `08`

这样读完后，再回到代码文件顺序（`main -> app -> handlers -> scheduler -> runner -> store`）会更容易建立完整心智模型。

## 5 分钟最小实操

1. 打开 `docs/architecture-diagrams.md`，对照说出控制面/数据面/可观测面各 1 个职责。
2. 在本地写 6~10 行“项目总览速记”，要求包含 `task`、`checkpoint`、`upload` 三个关键词。
3. 用自己的话回答：为什么这个项目不是“单纯 mysqlbinlog 包装器”。

## 本节实战检查

- 对照 `docs/learning/chapter-dod-matrix.md` 的「第 0 节」。
- 完成本节最小证据后再进入下一节。
