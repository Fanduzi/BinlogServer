# Orchestrator Discovery E2E Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 在现有 Docker e2e 环境里增加一个 orchestrator 场景，验证 binlog_server 拉流客户端不会被稳定纳入 orchestrator 拓扑。

**Architecture:** 复用现有 `deploy/e2e/docker-compose.yml`，新增 `orchestrator` 服务（SQLite backend），并新增 `scripts/e2e/smoke-orchestrator.sh`。脚本先触发 orchestrator discover 主库，再启动 binlog task，最后通过 orchestrator API 比较 cluster instance 数量（预期始终为 1，仅主库）。

**Tech Stack:** Docker Compose, bash, orchestrator HTTP API, curl/jq.

---

### Task 1: 新增失败用例脚本（RED）

**Files:**
- Create: `scripts/e2e/smoke-orchestrator.sh`

**Step 1: 编写最小脚本逻辑**
- 依赖 `curl/docker`。
- 调用 `docker compose ... up -d orchestrator`。
- 调用 orchestrator `/api/status`、`/api/discover/mysql80/3306`、`/api/cluster/instance/mysql80/3306`。
- 预留创建/启动 binlog task 的逻辑。

**Step 2: 运行脚本验证失败**
- 在 orchestrator 服务未定义时脚本应失败（例如 `no such service: orchestrator`）。

### Task 2: 最小实现使测试转绿（GREEN）

**Files:**
- Create: `deploy/e2e/orchestrator/orchestrator.conf.json`
- Modify: `deploy/e2e/docker-compose.yml`

**Step 1: 增加 orchestrator 配置**
- 使用 `BackendDB=sqlite`，避免再引入 orchestrator backend MySQL。
- `DiscoverySeeds` 指向 `mysql80:3306`。
- `MySQLHostnameResolveMethod=none`，避免容器内部 `@@hostname` 影响解析。

**Step 2: 增加 orchestrator 服务**
- 挂载 `/etc/orchestrator.conf.json`。
- 暴露 `13000:3000` 便于脚本调用 API。

**Step 3: 补全脚本断言**
- discover 前后获取 `cluster/instance/mysql80/3306`。
- 断言 count before/after 都是 `1`。
- count != 1 时输出实例明细并失败退出。

### Task 3: 文档与校验

**Files:**
- Modify: `README.md`

**Step 1: 更新 README**
- 增加 orchestrator 专项脚本入口和使用方式。

**Step 2: 运行校验**
- `bash -n scripts/e2e/*.sh`
- `docker compose -f deploy/e2e/docker-compose.yml config`
- 可行时执行一轮：
  - `./scripts/e2e/up.sh`
  - `./scripts/e2e/run-server.sh`
  - `./scripts/e2e/smoke-orchestrator.sh`

