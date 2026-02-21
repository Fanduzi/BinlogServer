# lab-bootstrap

上级：[[README]]
来源文件：`docs/learning/lab-bootstrap.md`

---

# 学习实验环境（Lab Bootstrap）

## 目标

用最少步骤拉起“可学习、可验证、可回归”的本地环境。

## 前置依赖

1. Go（与项目 `go.mod` 兼容版本）
2. Node.js + npm
3. Docker + Docker Compose
4. `curl` / `jq`（建议）

## 最小启动

```bash
go run ./cmd/binlog-server --config ./config.example.yaml
```

```bash
cd frontend
npm install
npm run dev
```

```bash
curl -s http://127.0.0.1:8080/healthz
```

## e2e 学习场景

```bash
./scripts/e2e/run-suite.sh --scenarios smoke,compression
./scripts/e2e/run-suite.sh --scenarios smoke-cluster-roles
./scripts/e2e/run-suite.sh --scenarios smoke-control-plane-failover
```

## 完成标准（DoD）

1. 至少 1 条 smoke 场景可稳定复现。
2. 你能给出失败时定位入口（日志/API/脚本输出）。
3. 你能区分“依赖 Docker 的验证”和“不依赖 Docker 的验证”。

---

## 相关

- [[chapter-dod-matrix]]
- [[13-capstone]]
