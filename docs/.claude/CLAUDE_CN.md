# CLAUDE_CN.md

本文件为 Claude Code (claude.ai/code) 在本仓库工作时提供指导。

## 构建与测试命令

```bash
# 运行服务
go run ./cmd/binlog-server
go run ./cmd/binlog-server --config ./config.yaml

# 构建二进制
go build -o binlog-server ./cmd/binlog-server

# 单元测试
go test ./...
make test

# 运行单个测试
go test -run TestFunctionName ./path/to/package
go test -v -run TestSmoke ./internal/app/...

# E2E 测试
make e2e-quick                                    # smoke + compression
make e2e-full                                     # 全量场景
make e2e SCENARIOS=smoke,compression              # 自定义场景

# 前端开发
cd frontend && npm install && npm run dev         # 开发服务器 :5173
make ui-build                                     # 构建并同步到 internal/ui/static

# 重新生成 Swagger 文档
go run github.com/swaggo/swag/cmd/swag@v1.16.6 init -g cmd/binlog-server/main.go -o internal/swaggerdocs --parseInternal
```

## 架构总览

### 三层设计

1. **控制面** (`internal/api/`, `internal/tasks/`)
   - Gin HTTP 处理器，负责任务 CRUD
   - 任务状态机与调度器
   - 集群协调（lease、heartbeat）

2. **数据面** (`internal/replication/`, `internal/binlog/`)
   - 基于 go-mysql 实现 MySQL 复制协议
   - Binlog 流式写入本地文件
   - 文件生命周期：OPEN_LOCAL → SEALED_LOCAL → UPLOADED | UPLOAD_FAILED

3. **元数据面** (`internal/meta/`)
   - 基于 MySQL 持久化任务、checkpoint、事件、lease
   - Worker 心跳与 epoch 追踪

### 任务状态机

```
CREATED → STARTING → RUNNING → STOPPING → STOPPED
                    ↓
              LEASE_DEGRADED（集群模式，grace 期内）
                    ↓
              RETRY_BACKOFF / STOPPED
```

### 关键设计决策

1. **Checkpoint 语义**：仅在 fsync 成功后推进——保证重启不丢数据
2. **不可变数据结构**：所有修改都创建新对象——防止副作用，支持安全并发
3. **上传策略**：最佳努力、非阻塞。上传失败不中断拉流。仅封口后的文件才上传。
4. **Object Key 模式**：`<prefix>/<cluster_key>/<source_server_uuid>/<fileName>` 防止跨集群冲突

### 部署模式

- **standalone**：单进程，全功能（默认）
- **cluster + all-in-one**：单节点，启用 lease 语义
- **cluster + control-plane/worker**：生产分离部署（control-plane = API/UI，worker = 复制）

### 配置优先级

`默认值 < YAML 配置文件 < 环境变量`

环境变量前缀：`BINLOG_SERVER_*`（如 `BINLOG_SERVER_LISTEN_ADDR`）

## 项目结构

```
cmd/binlog-server/     # 入口
internal/
├── app/               # 应用组装与启动
├── api/               # HTTP 处理器（Gin）
├── config/            # Viper 配置管理
├── tasks/             # 任务调度与状态机
├── replication/       # MySQL 复制 runner
├── binlog/            # Binlog 文件处理
├── meta/              # MySQL 元数据持久化
├── upload/            # S3 兼容上传
└── ui/                # 内嵌前端静态文件
frontend/              # Vue3 + Element Plus SPA
deploy/e2e/            # Docker Compose e2e 环境
scripts/e2e/           # E2E 测试脚本
docs/learning/         # 学习路径文档
```

## 任务启动模式

- `LATEST`：从最新 binlog 位点开始
- `FILE_POS`：从指定 (file, pos) 开始
- `GTID`：从 GTID 集合开始

## 集群协调（cluster 模式）

- Worker 通过 lease 抢占任务（fencing token = epoch）
- 心跳上报 control-plane 表存活
- Grace 期允许短暂失联
- Failover 策略：`rebuild_current_file`（重建进行中文件，保证字节一致）

## 可观测性端点

- `GET /metrics` - Prometheus 指标
- `GET /healthz` - 健康检查
- `GET /api/tasks/{id}/events` - 任务事件审计日志
- `GET /api/tasks/{id}/checkpoint` - Checkpoint 状态
- `GET /api/cluster/overview` - 集群汇总

## 文档索引

- 学习路径：`docs/learning-guide.md` → `docs/learning/`
- 架构图：`docs/architecture-diagrams.md`
- 部署模式：`docs/deployment-modes.md`
- API 指南：`docs/swagger-api-guide.md`
- 可观测性：`docs/observability.md`

## 核心结论速记

1. 起点支持 `LATEST/FILE_POS/GTID`
2. checkpoint 只在 flush/fsync 安全语义后推进
3. 上传是 best-effort：失败记 `UPLOAD_FAILED`，不中断拉流
4. cluster 模式支持 `control-plane / worker / all-in-one` 角色分离
5. object key 采用 `<prefix>/<cluster_key>/<source_server_uuid>/<fileName>` 避免跨集群重名与切主覆盖
