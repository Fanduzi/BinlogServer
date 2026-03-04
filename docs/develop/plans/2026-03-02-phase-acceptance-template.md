# Phase Acceptance Template

> 用途：每个阶段交付时复用本模板，固化“变更 + 验证 + 回滚 + 风险”最小闭环。

## 1) 变更点摘要

- 阶段：`P?`
- 范围：
- 非范围（明确未改动项）：
- 代码变更清单：
- 测试变更清单：

## 2) 验证结果（test/race/vet/e2e）

### 2.1 统一验证入口

- 执行入口：`./scripts/verify-phase-acceptance.sh`
- 备用命令集合：
  - `go test ./...`
  - `go test -race ./internal/tasks ./internal/api ./internal/replication`
  - `go vet ./...`
  - `make e2e-quick`

### 2.2 结果摘要

- `go test ./...`：
- `go test -race ./internal/tasks ./internal/api ./internal/replication`：
- `go vet ./...`：
- `make e2e-quick`：
- 关键日志路径：

## 3) 配置兼容性检查

- 默认配置是否可直接启动：
- 新增/变更配置项：
- 兼容策略（默认值、降级路径、旧配置行为）：
- 不兼容风险（若无填“无”）：

## 4) 性能基线/对比

- 基线日期：
- `go test ./...` 耗时：
- `go test -race ...` 耗时：
- `make e2e-quick` 耗时：
- `go test -bench=. ./internal/tasks/...` 结果：
- 与上次对比（如有）：

## 5) 回滚命令与回滚后验证结果

- 回滚目标 commit：
- 回滚命令：`git revert <commit>`
- 回滚后验证命令：
  - `go test ./...`
  - `go test -race ./internal/tasks ./internal/api ./internal/replication`
  - `go vet ./...`
  - `make e2e-quick`
- 回滚后验证结果：

## 6) 未决事项

- [ ] 项目 1：
- [ ] 项目 2：
