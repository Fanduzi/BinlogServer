# P2 Retry Standardization Report

## Scope

- 阶段：P2（重试策略标准化）
- 仅涉及 `internal/meta` 重试实现与测试，不改 API 状态码/任务状态机语义。

## Behavior Alignment Tests

已覆盖并验证以下行为：

1. transient 错误会按策略重试，成功后返回 `nil`
2. permanent 错误（`Permanent(err)`）直接返回，不重试
3. `context cancel/deadline` 触发后终止重试并返回对应上下文错误
4. `max retries` 生效（`MaxRetries=2` -> 总尝试次数为 3）
5. `jitter` 参数参与退避策略且不改变核心错误语义

对应测试：`internal/meta/retry_test.go`

## Old vs New Behavior Summary

| 行为维度 | 旧实现（自研） | 新实现（backoff 适配层） | 兼容性结论 |
|---|---|---|---|
| transient 重试 | 指数退避，直到成功或耗尽 | 指数退避，直到成功或耗尽 | 保持一致 |
| permanent 直返 | `Permanent(err)` 直接返回 | 转换为 `backoff.Permanent` 直接返回 | 保持一致 |
| context cancel/deadline | 返回 `ctx.Err()` 或操作内上下文错误 | 由 `WithContext` 与操作返回共同保证终止 | 保持一致 |
| max retries 语义 | `MaxRetries` 表示额外重试次数 | `WithMaxRetries` 映射同语义 | 保持一致 |
| jitter | 0~1 区间随机抖动 | `RandomizationFactor` 映射 | 保持一致 |

## Adapter Boundary

- 新增统一抽象（位于 `internal/meta/retry.go`）：
  - `type RetryExecutor interface { Do(ctx context.Context, policy Policy, fn func() error) error }`
  - `type Policy struct { BaseDelay, MaxDelay time.Duration; MaxRetries int; Jitter float64; IsTransient func(error) bool }`
- 业务调用方继续通过 `WithRetry(...)` 使用重试能力，不直接依赖第三方类型。

## Dependency Pinning Strategy

- 引入并锁定：`github.com/cenkalti/backoff/v4 v4.3.0`
- 锁定策略：
  1. 固定 major 为 `v4.x`（避免破坏性 API 变更）
  2. 当前固定到 `v4.3.0`（通过 `go.mod` + `go.sum` 固化）
  3. 后续升级需通过同类行为对齐测试回归后再放行
