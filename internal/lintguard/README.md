# lintguard

`go test` guard against new silent error paths in the backup data path (ADR 0009 phase 0, #326).

- `guard.go`: syntactic scanner for `_ = call(...)` and `if err != nil { log... }` with a log-only body.
- `ignored_errors_test.go`: scans `internal/{tasks,replication,binlog,upload}` and fails on any site not in `allowlist.go` at its reviewed count.
- `allowlist.go`: reviewed sites keyed `file:func:kind`, each with a reason. Entries marked `follow-up:` are known gaps for phase 1.
- `guard_test.go` + `testdata/bad`: proves the scanner finds both shapes and ignores handled errors.

Not covered (by design, syntactic only): `v, _ := f()` blank error results, and errors dropped by not assigning a call's result at all. The full `errcheck`/`go vet` CI step needs a workflow edit and is tracked in the token-gaps handoff.

When the test fails: report a finding, return the error, or add the site to `allowlist.go` with a reason in the same PR.
- `task_state_test.go`：AST 检查。`internal/tasks` 里设置任务 `State` 只能在 `commitTransitionLocked` 或列明理由的 runner/控制回路配方里；操作员动作函数不得直接赋值 `s.tasks[...]`、删除 `s.tasks`、调用 `persistTaskLocked`，必须调用 `commitTransitionLocked`/`commitDeleteLocked`。白名单里不存在的函数也会报错。
- `managed_writer.go` + `managed_writer_test.go`（P2-4，#342）：任何名为 `Report`/`Resolve`/`report`/`resolve` 的调用（方法、logger 方法或函数值）若参数提到健康评估器 Managed 的 kind（`tasks.FindingX` 常量或其字符串）即失败；热路径改用 `SignalFailure`/`SignalSuccess`。`testdata/managed` 覆盖各种写法。
