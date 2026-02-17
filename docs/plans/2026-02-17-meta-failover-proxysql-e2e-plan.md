# Meta Failover (Percona57 + ProxySQL + Orchestrator) E2E Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 在现有 e2e 环境中增加一条真实元数据库 failover 回归路径：`Percona57 主从 + ProxySQL 入口 + orchestrator 切主`，验证 binlog_server 在切主窗口后可恢复推进元数据写入。

**Architecture:** 新增一组“元数据库专用”容器，不复用现有 source MySQL；binlog_server 的 `meta_dsn` 指向 ProxySQL（固定入口），由 orchestrator 负责主从切换。场景脚本触发 failover 后，按 checkpoint 连续推进与任务状态恢复作为通过标准。

**Tech Stack:** docker-compose, Percona Server 5.7, ProxySQL 2.x, orchestrator API, bash, curl, jq.

---

### Task 1: 增加元数据库专用拓扑（primary/replica/proxysql）

**Files:**
- Modify: `deploy/e2e/docker-compose.yml`
- Create: `deploy/e2e/my.cnf/meta-primary.cnf`
- Create: `deploy/e2e/my.cnf/meta-replica.cnf`
- Create: `deploy/e2e/proxysql/proxysql-meta.cnf`

**Step 1: Write failing scenario expectation**

在 e2e 脚本中预设需要这些服务名存在：
- `meta-primary`
- `meta-replica`
- `meta-proxysql`

**Step 2: Run to verify it fails**

Run: `docker compose -f deploy/e2e/docker-compose.yml config --services | rg "meta-primary|meta-replica|meta-proxysql"`  
Expected: FAIL（当前服务不存在）。

**Step 3: Write minimal implementation**

新增三类服务：
- `meta-primary` / `meta-replica`（`percona/percona-server:5.7`）
- `meta-proxysql`（`proxysql/proxysql:2.3.x`）

并为两台 meta MySQL 分别设置 `server_id`、`log_bin`、`gtid_mode=ON`、`enforce_gtid_consistency=ON`。

`proxysql-meta.cnf` 使用新密码（替换参考工程旧密码）：
- `admin_credentials="admin:MetaAdmin!2026;cluster:MetaCluster!2026"`
- `cluster_password="MetaCluster!2026"`
- `monitor_username="proxysql_monitor"`
- `monitor_password="MetaMon!2026"`
- `stats_credentials="stats:MetaStats!2026"`

**Step 4: Run to verify it passes**

Run: `docker compose -f deploy/e2e/docker-compose.yml config --services | rg "meta-primary|meta-replica|meta-proxysql"`  
Expected: PASS.

**Step 5: Commit**

```bash
git add deploy/e2e/docker-compose.yml deploy/e2e/my.cnf/meta-primary.cnf deploy/e2e/my.cnf/meta-replica.cnf deploy/e2e/proxysql/proxysql-meta.cnf
git commit -m "test(e2e): add meta mysql and proxysql topology"
```

---

### Task 2: 初始化主从复制与 ProxySQL 监控账号

**Files:**
- Modify: `deploy/e2e/init/00-init.sql`
- Create: `scripts/e2e/setup-meta-replication.sh`

**Step 1: Write failing script behavior**

新增断言：
- `meta-replica` 执行 `SHOW SLAVE STATUS\G` 时 `Slave_IO_Running` / `Slave_SQL_Running` 为 `Yes`
- `meta-primary` 存在用户 `proxysql_monitor`

**Step 2: Run to verify it fails**

Run: `./scripts/e2e/setup-meta-replication.sh`  
Expected: FAIL（脚本未实现）。

**Step 3: Write minimal implementation**

在脚本中完成：
- 在 `meta-primary` 创建复制账号：`repl_meta / MetaRepl!2026`
- 在 `meta-primary` 创建 ProxySQL 监控账号：`proxysql_monitor / MetaMon!2026`
- `meta-replica` 执行 `CHANGE MASTER TO ... MASTER_AUTO_POSITION=1` + `START SLAVE`

**Step 4: Run to verify it passes**

Run: `./scripts/e2e/setup-meta-replication.sh`  
Expected: PASS.

**Step 5: Commit**

```bash
git add deploy/e2e/init/00-init.sql scripts/e2e/setup-meta-replication.sh
git commit -m "test(e2e): add meta replication bootstrap script"
```

---

### Task 3: 让 binlog_server 在 meta-failover 场景走 ProxySQL DSN

**Files:**
- Modify: `deploy/e2e/config.yaml`
- Modify: `scripts/e2e/run-suite.sh`

**Step 1: Write failing expectation**

新增 `meta-failover` 场景时，期望 `meta_dsn` 指向 `127.0.0.1:16036`（ProxySQL MySQL 端口），不是单机 MySQL 端口。

**Step 2: Run to verify it fails**

Run: `./scripts/e2e/run-suite.sh --scenarios meta-failover --keep-env`  
Expected: FAIL（场景尚未注册/DSN 仍为旧值）。

**Step 3: Write minimal implementation**

- 为 run-suite 增加场景 `meta-failover`
- 运行该场景前注入独立配置（或临时配置）让 `meta_dsn` 指向：
  - `meta:metapass@tcp(127.0.0.1:16036)/binlog_meta?parseTime=true`

**Step 4: Run to verify it passes**

Run: `./scripts/e2e/run-suite.sh --scenarios meta-failover --keep-env`  
Expected: 至少进入场景执行阶段（非参数错误）。

**Step 5: Commit**

```bash
git add deploy/e2e/config.yaml scripts/e2e/run-suite.sh
git commit -m "test(e2e): wire proxysql dsn for meta failover scenario"
```

---

### Task 4: 实现 `smoke-meta-failover.sh`（核心）

**Files:**
- Create: `scripts/e2e/smoke-meta-failover.sh`

**Step 1: Write failing assertions**

脚本必须断言：
- failover 前任务 `RUNNING`
- 触发 failover 后，checkpoint 在超时窗口内继续推进
- 最终任务状态回到 `RUNNING`（或短暂 `RETRY_BACKOFF` 后恢复）

**Step 2: Run to verify it fails**

Run: `./scripts/e2e/smoke-meta-failover.sh`  
Expected: FAIL（脚本未实现）。

**Step 3: Write minimal implementation**

脚本流程：
1. 启动 `orchestrator + meta-primary + meta-replica + meta-proxysql`
2. 调用 `setup-meta-replication.sh`
3. 创建并启动一个复制任务，记录初始 checkpoint
4. 通过 orchestrator API 触发恢复：
   - `POST /api/recover-lite/<cluster-alias>`（或 `graceful-master-takeover`，按可用 API）
5. 轮询 checkpoint 与任务状态，确认恢复

**Step 4: Run to verify it passes**

Run: `./scripts/e2e/smoke-meta-failover.sh`  
Expected: PASS.

**Step 5: Commit**

```bash
git add scripts/e2e/smoke-meta-failover.sh
git commit -m "test(e2e): add metadata failover recovery scenario"
```

---

### Task 5: 文档与入口收尾

**Files:**
- Modify: `scripts/e2e/README.md`
- Modify: `scripts/e2e/run-suite.sh`

**Step 1: Write failing docs/entry expectation**

要求 `README` 和 `run-suite.sh --help` 中都出现 `meta-failover`。

**Step 2: Run to verify it fails**

Run: `./scripts/e2e/run-suite.sh --help | rg meta-failover`  
Expected: FAIL（当前未列出）。

**Step 3: Write minimal implementation**

- `run-suite.sh` 增加 scenario 分支与 profile 说明
- README 增加场景目的、前置条件、常见排障

**Step 4: Run to verify it passes**

Run:
- `./scripts/e2e/run-suite.sh --help | rg meta-failover`
- `./scripts/e2e/run-suite.sh --scenarios meta-failover`

Expected: PASS.

**Step 5: Commit**

```bash
git add scripts/e2e/README.md scripts/e2e/run-suite.sh
git commit -m "docs(e2e): document meta failover scenario"
```

---

## 验证标准（场景通过定义）

1. failover 触发后，任务允许短时波动，但在窗口内恢复推进（checkpoint 继续增加）。
2. 元数据库入口始终固定为 ProxySQL 地址，不依赖应用重启切换 DSN。
3. ProxySQL 配置文件不再使用参考目录中的旧密码，全部替换为本计划新值。

## 备注（刻意简化）

1. 仅用 1 个 ProxySQL 节点，不引入 ProxySQL cluster 同步。
2. 仅用 1 主 1 从元数据库，不引入多从与跨机房权重。
3. 不在本阶段验证“无损事务级连续性”，只验证“故障后可恢复推进”。
