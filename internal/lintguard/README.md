# lintguard

`go test` guard against new silent error paths in the backup data path (ADR 0009 phase 0, #326).

- `guard.go`: syntactic scanner for `_ = call(...)` and `if err != nil { log... }` with a log-only body.
- `ignored_errors_test.go`: scans `internal/{tasks,replication,binlog,upload}` and fails on any site not in `allowlist.go` at its reviewed count.
- `allowlist.go`: reviewed sites keyed `file:func:kind`, each with a reason. Entries marked `follow-up:` are known gaps for phase 1. `killLeftover` returns the KILL error (a TLS verify failure is warned, other failures are logged) so it is not a log-only site. `fillSealDigest` logs a failed `.seal-sha256` append and still uploads: the catalog digest is already on the row, and a missing ledger line only hides the segment from the rehasher when there is no catalog column. `Verifier.save` returns on a failed `.verify.json` write and leaves the progress dirty, so the next pass tries again; findings stay in the finding store. That return is not a log-only site.
- `guard_test.go` + `testdata/bad`: proves the scanner finds both shapes and ignores handled errors.

Not covered (by design, syntactic only): `v, _ := f()` blank error results, and errors dropped by not assigning a call's result at all. The full `errcheck`/`go vet` CI step needs a workflow edit and is tracked in the token-gaps handoff.

When the test fails: report a finding, return the error, or add the site to `allowlist.go` with a reason in the same PR.
- `task_state_test.go`：AST 检查。`internal/tasks` 里设置任务 `State` 只能在 `commitTransitionLocked` 或列明理由的 runner/控制回路配方里；操作员动作函数不得直接赋值 `s.tasks[...]`、删除 `s.tasks`、调用 `persistTaskLocked`，必须调用 `commitTransitionLocked`/`commitDeleteLocked`。白名单里不存在的函数也会报错。`yieldForResumeLocked`（§29）记的是本进程已经放弃的这一轮 dump，不落盘 desired STOP，所以和 `yieldLostLeaseLocked` 一样在白名单里。`yieldFencedEpochLocked`（§29.10，#454）同样只把观察状态留在可认领的 `LEASE_DEGRADED`，不写 STOPPED。
- `managed_writer.go` + `managed_writer_test.go`（P2-4，#342）：任何名为 `Report`/`Resolve`/`report`/`resolve` 的调用（方法、logger 方法或函数值）若参数提到健康评估器 Managed 的 kind（`tasks.FindingX` 常量或其字符串）即失败；热路径改用 `SignalFailure`/`SignalSuccess`。只由评估器推导、没有 `tasks.FindingX` 常量的 kind 记在 `managedWithoutConstant`（含 P2-5.4 的 `ORPHAN_ROW` 与 `SEAL_FACTS_ABSENT`，以及 P2-6.1 的 `HEALTH_STALE`）。`testdata/managed` 覆盖各种写法。
- `release_gate_test.go`（production-readiness J3）：读 `.github/workflows/release.yml` 和 `e2e.yml`。`release.yml` 必须有 `ci` job 调用 `./.github/workflows/e2e.yml`，其余 job 都 `needs: ci`；`e2e.yml` 必须有 `on: workflow_call`，且没有 job 的 `if:` 看 `github.event_name` 或 `github.ref`（被调用的 workflow 看到的是调用方的事件，这样的 job 发布前会被跳过）；手动选 profile 用 `github.event.inputs.e2e_profile`。
