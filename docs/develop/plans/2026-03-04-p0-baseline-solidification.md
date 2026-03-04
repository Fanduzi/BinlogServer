# P0 Baseline Solidification and Regression Guard

## Scope

- 仅做基线与回归护栏固化，不改业务逻辑与对外语义。
- 新增统一验证入口，覆盖 `test/race/vet/e2e-quick`。
- 产出关键路径回归清单、阶段验收模板、基线指标、依赖盘点。

## Unified Verification Entry

- 主入口：`./scripts/verify-phase-acceptance.sh`
- 覆盖命令：
  - `go test ./...`
  - `go test -race ./internal/tasks ./internal/api ./internal/replication`
  - `go vet ./...`
  - `make e2e-quick`
- 输出日志目录：`tmp/phase-acceptance/`

## Key-Path Regression Checklist

### Task create/start/stop

- API 层：
  - `go test ./internal/api -run TestTaskAPI_CreateListStartStop`
  - `go test ./internal/api -run TestTaskAPI_CreateWithSourceAndStart`
- 调度层：
  - `go test ./internal/tasks -run TestScheduler_StartTaskTransitionsToRunning`
  - `go test ./internal/tasks -run TestScheduler_StopTaskTransitionsToStopped`
  - `go test ./internal/tasks -run TestScheduler_StartTaskWithoutRunnerInClusterDispatchMode`

### Lease renew

- `go test ./internal/tasks -run TestScheduler_LeaseRenewFailureEntersDegradedThenStop`
- `go test ./internal/tasks -run TestScheduler_LeaseLostTransitionsToStoppingStopped`
- `go test ./internal/api -run TestAPI_ClusterTaskLease`

### Checkpoint

- `go test ./internal/api -run TestTaskAPI_GetCheckpoint`
- `go test ./internal/tasks -run TestScheduler_GetCheckpointDoesNotRefreshTaskListForKnownTask`
- `go test ./internal/replication -run TestEffectiveStart_UsesCheckpointWhenPresent`

### Retry upload

- `go test ./internal/tasks -run TestScheduler_RetryFailedUploadsOnlyFailedSealed`
- `go test ./internal/tasks -run TestScheduler_RetryFailedUploadsStateTransitionOnError`
- `go test ./internal/api -run TestTaskAPI_RetryUploadReturnsStats`
- `go test ./internal/api -run TestTaskAPI_ListUploadFailureReasons`

## Acceptance Template

- 阶段验收统一模板：`docs/develop/plans/2026-03-02-phase-acceptance-template.md`

## Baseline Metrics (2026-03-04)

原始日志目录：`tmp/p0-metrics/`

- `go test ./...`
  - 结果：PASS
  - 耗时：`real 6.00s` (`tmp/p0-metrics/go-test-all.txt`)
- `go test -race ./internal/tasks ./internal/api ./internal/replication`
  - 结果：PASS
  - 耗时：`real 3.34s` (`tmp/p0-metrics/go-test-race.txt`)
- `go vet ./...`
  - 结果：PASS
  - 耗时：`real 0.38s` (`tmp/p0-metrics/go-vet.txt`)
- `make e2e-quick`
  - 结果：PASS
  - 耗时：`real 27.51s` (`tmp/p0-metrics/make-e2e-quick.txt`)
- `go test -bench=. ./internal/tasks/...`
  - 结果：PASS，当前 `internal/tasks` 无 `Benchmark*` 用例（仅输出 package 级执行耗时）
  - 耗时：`real 0.65s` (`tmp/p0-metrics/go-test-bench-tasks.txt`)

## Dependency Inventory (go.mod Related to Plan)

| 关注项 | 当前状态 | 使用现状 | P0 策略 |
|---|---|---|---|
| backoff | 无外部 backoff 依赖 | 采用仓内实现：`internal/tasks/scheduler_lifecycle.go` 与 `internal/meta/retry.go` 指数退避 | 维持内建实现，暂不引入第三方 backoff 包，避免行为波动 |
| prometheus | 无 `prometheus/client_golang` 依赖 | `/metrics` 由 `internal/api/server.go` 手写 Prometheus exposition 文本 | 维持无 SDK 策略；后续若引入 SDK，需独立阶段评估指标名与标签兼容性 |
| otel | 无 `go.opentelemetry.io/*` 依赖 | 当前未接入 OTel tracing/metrics | P0 不引入；后续单独阶段引入并约束默认关闭、配置可选 |
| validator | 间接依赖 `github.com/go-playground/validator/v10 v10.27.0`（via gin） | API 输入校验主要为显式字段校验与错误映射，不直接依赖 validator API | 保持间接依赖；继续显式校验路径，避免隐式 tag 校验改动语义 |

## Rollback Point

- 本阶段回滚粒度：按 commit 级 `git revert <commit>`
- 回滚后必须执行：
  - `go test ./...`
  - `go test -race ./internal/tasks ./internal/api ./internal/replication`
  - `go vet ./...`
  - `make e2e-quick`

## Open Items

- `internal/tasks` 基准测试目前无 `Benchmark*` 用例，后续可在非 P0 阶段补齐关键路径 benchmark。
- 若后续阶段引入 Prometheus/OTel SDK，需要先冻结并评审指标命名与标签兼容策略。
