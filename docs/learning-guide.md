# binlog_server 学习手册（总览）

这份文档用于统一学习入口。详细章节在 `docs/learning/`，这里给出推荐顺序、角色路径与结业标准。

分节目录：`docs/learning/README.md`

## 1. 课程版本锚点

- Course-Version：`v0.1.0-beta.0-learning-draft.1`
- Baseline-Commit：`d8e2efb`
- Last-Verified-Date：`2026-02-20`

使用建议：学习/复盘时先确认自己代码版本与课程基线接近，避免“文档对、代码不对”的错位。

## 2. 为什么采用“自顶向下”

如果一开始就按函数细节深挖，很容易出现“局部看懂、全局不通”。

推荐顺序：

1. 先看系统地图（角色、边界、主链路）
2. 再看控制面（任务如何被创建/调度）
3. 再看数据面（binlog 如何拉取/落盘/封口）
4. 最后看集群可靠性与运维

## 3. 实验环境先行

开始正文前，先完成环境准备：`docs/learning/lab-bootstrap.md`。

最小验证：

1. `go run ./cmd/binlog-server` 可启动
2. `GET /healthz` 返回 `ok`
3. 至少 1 条 e2e smoke 场景可运行

## 4. 推荐学习顺序（自顶向下）

### 阶段 A：系统地图

1. `docs/learning/00-project-overview.md`
2. `docs/architecture-diagrams.md`
3. `docs/deployment-modes.md`

目标：建立控制面/数据面/元数据面的职责边界。

### 阶段 B：控制面主链路

1. `docs/learning/05-gin-api-layer.md`
2. `docs/learning/02-task-model-and-scheduler.md`
3. `docs/learning/01-startup-and-wiring.md`

目标：搞清楚“一个 API 请求如何变成任务状态变化并最终拉起 runner”。

### 阶段 C：数据面主链路

1. `docs/learning/03-mysql-replication-runner.md`
2. `docs/learning/04-metadata-persistence.md`

目标：掌握 checkpoint/rotate/seal/upload 的语义边界。

### 阶段 D：集群与可靠性

1. `docs/learning/09-cluster-mode-and-role-wiring.md`
2. `docs/learning/10-cluster-scheduler-lease-and-heartbeat.md`
3. `docs/learning/11-cluster-api-and-frontend-observability.md`
4. `docs/learning/07-tests-and-safety.md`
5. `docs/learning/12-e2e-and-release-practice.md`

目标：搞清楚 control-plane/worker 分离后如何 claim、续租、接管与回归。

### 阶段 E：专题补充与结业

1. `docs/learning/08-orchestrator-discovery.md`
2. `docs/observability.md`
3. `docs/learning/13-capstone.md`

目标：形成可审计的工程闭环表达能力。

## 5. 角色速通路径

1. Go 开发：`00 -> 05 -> 02 -> 03 -> 10 -> 12 -> 13`
2. 运维/SRE：`00 -> 09 -> 10 -> 11 -> 12 -> 08 -> 13`
3. 测试/质量：`00 -> 07 -> 11 -> 12 -> chapter-dod-matrix -> 13`

## 6. 验收与评分（精品课程核心）

每一章都按 `docs/learning/chapter-dod-matrix.md` 做一次“实战检查”（讲得清、做得出、留得住）。

核心要求：

1. 概念能讲清
2. 链路能复述
3. 证据可复现
4. 边界说得明

做不到就先记录卡点并回看章节，不需要用“考试式打分”推进。

## 7. 结业标准

1. 00-12 章全部完成实战检查
2. 完成 `13-capstone`
3. 提交一份学习收官报告（改动、验证、风险）

## 8. 当前你要记住的 5 个结论

1. 起点支持 `LATEST/FILE_POS/GTID`。
2. checkpoint 只在 `flush/fsync` 安全语义后推进。
3. 上传是 best-effort：失败记 `UPLOAD_FAILED`，不中断拉流。
4. cluster 模式支持 `control-plane / worker / all-in-one` 角色分离。
5. object key 采用 `<prefix>/<cluster_key>/<source_server_uuid>/<fileName>`，用于避免跨集群重名与切主覆盖风险。
