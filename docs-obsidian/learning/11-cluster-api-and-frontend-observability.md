# 11-cluster-api-and-frontend-observability

上级：[[MOC-学习路线]]
来源文件：`docs/learning/11-cluster-api-and-frontend-observability.md`

---

# 第 11 节：Cluster 可观测 API 与前端大盘

## 全链路导读

- 全链路定位：集群可观测面（把调度与执行状态聚合为可运维视图）
- 前置阅读：第 5 节、第 10 节、第 6 节（建议）
- 学完你应能：判断页面指标的真实来源，并定位“显示异常”是 API 问题还是调度问题

## 目标

看懂 cluster 视图的数据来源，避免把“页面展示值”误解成“调度真实状态”。

## 核心文件

- `internal/api/handlers_cluster.go`
- `internal/api/server.go`
- `internal/api/swagger_docs_only.go`
- `frontend/src/api.js`
- `frontend/src/App.vue`

## Cluster 相关 API（当前实现）

1. `GET /api/workers`
2. `GET /api/cluster/overview`
3. `GET /api/tasks/{id}/lease`
4. `GET /api/tasks/{id}/runs`

### `/api/workers`

返回 worker 心跳与任务聚合视图，核心字段：

1. `worker_id`
2. `online`
3. `last_seen_at`
4. `task_count/running/leased`

### `/api/tasks/{id}/runs`

返回 run 历史列表（来自 `task_runs`）：

1. 默认 `limit=10`
2. 最大 `limit=200`
3. 按 `started_at DESC`

## 前端联调要点

1. 详情页 `lease/runs` 采用容错加载，不阻断主详情打开。
2. Run History 文案已与接口能力对齐（真实历史列表，不是单条当前 run）。
3. dashboard 中的“延迟/异常/在线状态”是聚合视图，排障应回到 task events 与 checkpoint。

## Swagger 与契约同步

cluster API 是 release 前必检项：

1. Swagger UI 可打开
2. 关键路径在 `/swagger/doc.json` 中存在
3. 注解与产物无 diff

## 动手练习

1. 用 `/api/workers` 对比 worker 重启前后的 `online` 与 `last_seen_at`。
2. 连续 stop/start 任务，观察 `/api/tasks/{id}/runs` 历史是否增长。
3. 手工改一个 cluster endpoint 字段名，验证 Swagger 测试会失败。

## 自测问题

1. 为什么 runs/history 需要独立表而不是只看 task 当前字段？
2. UI 里“online=true”与“任务 RUNNING”为什么不能完全等价？
3. 为什么 release 前必须做 Swagger 产物一致性检查？

---

## 相关

- [[架构图-Mermaid版]]
- [[部署模式]]
- [[可观测性]]

## 5 分钟最小实操

1. 调用 `/api/workers` 与 `/api/tasks/{id}/runs?limit=10`。
2. 验证 runs 返回顺序与 limit 行为。
3. 给出一个例子说明：worker online 与任务 running 为什么不等价。

## 本节实战检查

- 对照 [[chapter-dod-matrix]] 的「第 11 节」。
- 完成本节最小证据后再进入下一节。
