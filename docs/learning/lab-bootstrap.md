# 学习实验环境（Lab Bootstrap）

## 1. 目标

用最少步骤拉起“可学习、可验证、可回归”的本地环境。

## 2. 前置依赖

1. Go（与项目 `go.mod` 兼容版本）
2. Node.js + npm（用于前端）
3. Docker + Docker Compose（用于 e2e 源库与拓扑）
4. `curl` / `jq`（建议安装）

## 3. 最小启动（后端 + 前端）

后端：

```bash
go run ./cmd/binlog-server --config ./config.example.yaml
```

前端（可选）：

```bash
cd frontend
npm install
npm run dev
```

健康检查：

```bash
curl -s http://127.0.0.1:8080/healthz
```

## 4. e2e 学习环境

快速烟雾：

```bash
./scripts/e2e/run-suite.sh --scenarios smoke,compression
```

cluster 角色：

```bash
./scripts/e2e/run-suite.sh --scenarios smoke-cluster-roles
```

control-plane 故障恢复：

```bash
./scripts/e2e/run-suite.sh --scenarios smoke-control-plane-failover
```

## 5. 课程建议使用顺序

1. 先跑 `smoke,compression`，确认基础链路可用。
2. 再跑 `smoke-cluster-roles`，理解 control-plane/worker 分离。
3. 最后跑 `smoke-control-plane-failover`，理解“控面挂掉数据面不停”。

## 6. 常见失败与定位

1. 端口冲突：检查 `8080/5173` 是否被占用。
2. Docker 未就绪：先确认 daemon 状态，再跑 e2e。
3. 元数据连接失败：核对 `meta_dsn` 或 e2e 拓扑初始化是否成功。

## 7. 完成标准（DoD）

1. 你可以在本机稳定复现至少 1 条 smoke 场景。
2. 你能给出失败时的定位入口（日志/API/脚本输出）。
3. 你能说清“哪些验证依赖 Docker，哪些不依赖”。
