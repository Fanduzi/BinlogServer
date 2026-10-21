# internal/assurance/mutants

把评审用的源码变异重新打到阶段 3 巡检上，确认测试会失败。M01–M25 是 PR #480 第 1 轮 QA 的 25 个变异（按覆盖模型重写了替换文本，含义不变），打在 `internal/assurance/verify.go`、`internal/tasks/sealed_upload.go`、`internal/meta/mysql_store.go` 上。N01 起是覆盖模型自己的变异：关闭表、强制重查、`last_verified_at`、缺失本地副本、时钟偏差、轮换、每轮字节数、提前返回。

## Files

| File | Responsibility |
|------|---------------|
| `kill.go` | `//go:build ignore`。用 `go test -overlay` 逐个替换（不改工作树），跑对应包的测试，打印击杀表 |

## Exports

- `go run ./internal/assurance/mutants/kill.go`（在模块根目录；可在后面列出要跑的变异 id）

退出码 0 表示没有未标等价的变异活下来。退出码 1 表示有。标成等价的变异必须在表里写明理由，活下来也不计数。等价的是 N30、X05、R15。H25 不再等价：取消的一轮没读到的强制段停在 `STAT`，`TestVerify_CancelDoesNotCloseUnreadLocalUnreadable` 打死「任意找到的 `STAT` 就关 `LOCAL_UNREADABLE`」。H 系列是阶段 3 工人之前的收尾：合并条件、混合 errno、悬空符号链接、槽用排序时间、未来的 `uploaded_at`。`WV08`、`WV14`、`WV11`、`WV16`、`WV12`、`WV15` 改 `internal/tasks/verifier.go`（QA 里的 V08、V14、V11、V16、V12、V15；V 号已经被 `verify.go` 用过）。`internal/tasks` 和 `internal/meta` 只跑被改函数的测试（`-run`），避免整包里已知不稳定的用例混进结果。

## Dependencies

- Upstream: 上面三个源文件和它们包里的测试。
- Downstream: 无。不参与 `go test ./...`。

## Update Rule

- 变异表或击杀方式变化时，改 `kill.go` 的文件头和本文件。
