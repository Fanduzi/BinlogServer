# binlog_server 学习路线（目录）

这套文档按“先跑通、再走主链路、最后做小改动”的方式组织。

## 使用方式

1. 先读 `docs/learning-guide.md`（总览）
2. 再按下面 1~9 顺序学习
3. 每节做完都执行一次 `go test ./...`

## 分节文档

1. `docs/learning/00-project-overview.md`
2. `docs/learning/01-startup-and-wiring.md`
3. `docs/learning/02-task-model-and-scheduler.md`
4. `docs/learning/03-mysql-replication-runner.md`
5. `docs/learning/04-metadata-persistence.md`
6. `docs/learning/05-gin-api-layer.md`
7. `docs/learning/06-vue-elementplus-frontend.md`
8. `docs/learning/07-tests-and-safety.md`
9. `docs/learning/08-orchestrator-discovery.md`

## 建议节奏

1. 先看目标（为什么有这一层）
2. 再看调用链（谁调谁）
3. 最后做最小改动（10~20 行）
