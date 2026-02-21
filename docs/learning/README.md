# binlog_server 学习路线（目录）

## 课程基线

- Course-Version：`v0.1.0-beta.0-learning-draft.1`
- Baseline-Commit：`d8e2efb`
- Last-Verified-Date：`2026-02-20`

## 使用方式

1. 先读 `docs/learning-guide.md`（总览）
2. 先跑实验环境准备：`docs/learning/lab-bootstrap.md`
3. 按“自顶向下顺序”学习
4. 每节结束后按 `docs/learning/chapter-dod-matrix.md` 做一次实战检查

## 推荐顺序（自顶向下）

1. `docs/learning/00-project-overview.md`
2. `docs/learning/05-gin-api-layer.md`
3. `docs/learning/02-task-model-and-scheduler.md`
4. `docs/learning/01-startup-and-wiring.md`
5. `docs/learning/03-mysql-replication-runner.md`
6. `docs/learning/04-metadata-persistence.md`
7. `docs/learning/09-cluster-mode-and-role-wiring.md`
8. `docs/learning/10-cluster-scheduler-lease-and-heartbeat.md`
9. `docs/learning/11-cluster-api-and-frontend-observability.md`
10. `docs/learning/07-tests-and-safety.md`
11. `docs/learning/12-e2e-and-release-practice.md`
12. `docs/learning/08-orchestrator-discovery.md`
13. `docs/learning/13-capstone.md`

说明：`docs/learning/06-vue-elementplus-frontend.md` 可插入在第 9 步后阅读（准备深入前端时）。

## 角色速通路径

1. Go 开发（最短 7 节）：`00 -> 05 -> 02 -> 03 -> 10 -> 12 -> 13`
2. 运维/SRE（最短 7 节）：`00 -> 09 -> 10 -> 11 -> 12 -> 08 -> 13`
3. 测试/质量（最短 6 节）：`00 -> 07 -> 11 -> 12 -> chapter-dod-matrix -> 13`

## 章节列表（按编号索引）

1. `docs/learning/00-project-overview.md`
2. `docs/learning/01-startup-and-wiring.md`
3. `docs/learning/02-task-model-and-scheduler.md`
4. `docs/learning/03-mysql-replication-runner.md`
5. `docs/learning/04-metadata-persistence.md`
6. `docs/learning/05-gin-api-layer.md`
7. `docs/learning/06-vue-elementplus-frontend.md`
8. `docs/learning/07-tests-and-safety.md`
9. `docs/learning/08-orchestrator-discovery.md`
10. `docs/learning/09-cluster-mode-and-role-wiring.md`
11. `docs/learning/10-cluster-scheduler-lease-and-heartbeat.md`
12. `docs/learning/11-cluster-api-and-frontend-observability.md`
13. `docs/learning/12-e2e-and-release-practice.md`
14. `docs/learning/13-capstone.md`

## 课程支撑文档

- `docs/learning/lab-bootstrap.md`：实验环境搭建与验证
- `docs/learning/chapter-dod-matrix.md`：13 节课程统一验收标准与评分口径

## 你可以这样学

1. 第一遍：只看推荐顺序，先形成全局认知。
2. 第二遍：按编号逐节精读并对照代码。
3. 第三遍：按你的改造任务反查章节（当作手册）。
4. 结业：完成 `13-capstone` 并提交证据链（命令、输出、风险说明）。
