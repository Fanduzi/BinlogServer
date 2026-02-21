# 12-e2e-and-release-practice

上级：[[MOC-学习路线]]
来源文件：`docs/learning/12-e2e-and-release-practice.md`

---

# 第 12 节：E2E 与发版实战

## 全链路导读

- 全链路定位：交付收口层（把关键语义固化为可重复回归与发布证据）
- 前置阅读：第 7 节、第 9 节、第 10 节、第 11 节
- 学完你应能：用固定脚本验证关键场景，并形成可审计的发布证据链

## 目标

把 cluster 关键语义转成“可重复验证”的脚本与发布流程，做到证据先于结论。

## 核心文件

- `scripts/e2e/run-suite.sh`
- `scripts/e2e/smoke-cluster-roles.sh`
- `scripts/e2e/smoke-control-plane-failover.sh`
- `.github/workflows/e2e.yml`
- `docs/releases/v0.1.0-alpha.2.md`

## 必跑场景（当前冻结版）

1. `smoke,compression`
2. `meta-failover`
3. `meta-failover-override`
4. `smoke-cluster-roles`
5. `smoke-control-plane-failover`

## 你要看懂的两条 cluster e2e

### 1) `smoke-cluster-roles`

验证：

1. control-plane 创建/启动任务
2. worker 接管执行并推进 checkpoint
3. worker offline 检测
4. worker 重启恢复

### 2) `smoke-control-plane-failover`

验证：

1. checkpoint A -> B 正常推进
2. 停 control-plane 后继续写入
3. 重启 control-plane 后 checkpoint C > B
4. 证明控面故障不影响数据面拉流

## CI 策略

`e2e.yml` 已将 cluster 场景独立为专门 job，避免与普通 quick/full 混跑。

## 发版前检查模板

1. `go test ./...`
2. `cd frontend && npm run build`
3. 跑上述 e2e 场景
4. Swagger 同步检查
5. 更新 release notes

## 动手练习

1. 人工制造 worker 短时故障，观察 e2e 是否仍通过。
2. 把 `smoke-control-plane-failover` 挂进你自己的 CI 分支。
3. 按模板写一版 `alpha.3` 的预发布检查清单。

## 自测问题

1. 为什么 cluster 场景要独立 job，而不是并进 quick profile？
2. 仅有单元测试通过，为什么不足以宣布“可发布”？
3. checkpoint 的 A/B/C 断言本质上在证明什么？

---

## 相关

- [[架构图-Mermaid版]]
- [[部署模式]]
- [[可观测性]]

## 5 分钟最小实操

1. 执行一条 cluster e2e 场景（如 `smoke-cluster-roles`）。
2. 摘出 3 行关键输出，说明它们分别证明了什么。
3. 写一句发布判断：当前是“可发布”还是“需补验证”，并说明理由。

## 本节实战检查

- 对照 [[chapter-dod-matrix]] 的「第 12 节」。
- 完成本节最小证据后再进入下一节。
