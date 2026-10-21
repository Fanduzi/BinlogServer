# internal/assurance/mutants

把评审用的源码变异重新打到阶段 3 巡检上，确认测试会失败。M01–M25 是 PR #480 第 1 轮 QA 的 25 个变异（按覆盖模型重写了替换文本，含义不变），打在 `internal/assurance/verify.go`、`internal/tasks/sealed_upload.go`、`internal/meta/mysql_store.go` 上。N01 起是覆盖模型自己的变异：关闭表、强制重查、`last_verified_at`、缺失本地副本、时钟偏差、轮换、每轮字节数、提前返回。

## Files

| File | Responsibility |
|------|---------------|
| `kill.go` | `//go:build ignore`。用 `go test -overlay` 逐个替换（不改工作树），跑对应包的测试，打印击杀表 |

## Exports

- `go run ./internal/assurance/mutants/kill.go`（在模块根目录；可在后面列出要跑的变异 id）

退出码 0 表示没有未标等价的变异活下来。退出码 1 表示有。标成等价的变异必须在表里写明理由，活下来也不计数。等价的是 N30、X05、R15。H25 不再等价：取消的一轮没读到的强制段停在 `STAT`，`TestVerify_CancelDoesNotCloseUnreadLocalUnreadable` 打死「任意找到的 `STAT` 就关 `LOCAL_UNREADABLE`」。H 系列是阶段 3 工人之前的收尾：合并条件、混合 errno、悬空符号链接、槽用排序时间、未来的 `uploaded_at`。`WV08`、`WV14`、`WV11`、`WV16`、`WV12`、`WV15` 改 `internal/tasks/verifier.go`（QA 里的 V08、V14、V11、V16、V12、V15；V 号已经被 `verify.go` 用过）。`WVT01`、`WVT02`、`WVT03`、`WVT07` 也改这个文件（Write、Sync、Close 失败不删临时文件，以及写失败的日志不是 warn）。`WVP01`、`WVP02`、`WVP03` 改 `internal/app/app.go` 的探针超时和 `timed out after`。`WVP12` 改 `internal/tasks/finding.go`，degraded 时丢掉 detail。`WVS03` 查完对象范围却不清 strike，`WVS05` 让没查到任务的一轮清 strike，`WVS06` 丢掉 `ObjectErr`，`WVS07` 丢掉 `MethodError` 行，`WVC05` 清扫时跟随符号链接，`WVC06` 删除失败就不再读进度，`WVC07` 删除失败就停下，`WVC08` 删除失败不打 warn。`WVN03` 让全被跳过的一轮增加 strike，`WVN04` 让这一轮清 strike，`WVN05` 把 `Allows` 跳过算成查过，`WVN07` 把列出目录失败算成查过，`WVN08` 去掉临时文件名里的 pid，`WVN09` 让清扫留着 `.verify-<pid>-*.tmp`，`WVN11` 把清扫放宽到 `.verify-*`，`WVN12` 放宽到 `.verify*.tmp`，`WVN16` 让 `startVerifier` 不再传入 `upload.bucket`。r5 之后另有：`WVP03A`/`WVP03D`/`WVP03C`/`WVP06` 改空任务一轮的 strike、detail 和 `skipped`；`WVN03S`/`WVN03D`/`WVP04`/`WVP05` 改全被跳过的一轮；`WVP07`/`WVP07T`/`WVP07S` 改 `tasks` 和 `skipped` 计数；`WVP08`/`WVP08C`/`WVP08F` 改截止时间；`WVS02`/`WVS04`/`WVS03D`/`WVS08` 改 strike 加减、第四次才写 detail、查完不清 detail、以及 `object_incomplete`；`WVS06E`/`WVS06B`/`WVS07K` 改原因里的空字段；`WVN10` 让清扫漏掉旧的 `.verify-123.tmp`；`WVP12K`/`WVP17B`/`WVP17C`/`WVP17K`/`WVP17R`/`WVP17O`/`WVP17S`/`WVP17L` 改 `HealthSummary` 两条返回上的 status、reasons、detail、`side_kill` 和 `lock_detail`。r6 再补：`WVN03H`/`WVP03H` 在 detail 已经写上之后给空任务或全跳过加 1；`WVP06T`/`WVP05T` 把这两轮的 `tasks` 记成 1；`WVP08T`/`WVS08T` 把 `cut_short` 和 `object_incomplete` 写死成 true；`WVS02B` 一次加 2；`WVS02W` 让 warn 的 consecutive 多 1；`WVS04B` 第二次就写 detail；`WVS09` 让 detail 里的次数多 1；`WVS06O` 把 bucket 写在 object 前面；`WVP17X` 对调 degraded 的 `side_kill` 和 `lock_detail`；`WVP17D` 把健康汇总报成 degraded。ADR 0011 PR-3 的 SM01–SM07 打在 `internal/tasks/evaluator.go`、`internal/health/reconcile.go`、`internal/health/fence.go`、`internal/meta/health_store.go`：集群再丢掉 `I6_MISSING_LOCAL`、`OWNER_CHANGED` 关掉 `SEGMENT_MISSING`、未验证就写 `chain_owner`、不完整的 `chain.local` 关掉 `SEGMENT_MISSING`、MySQL 对每个持有者都写 `chain_owner`、读到目录却不置 `ChainVerified`、没读到目录也置 `ChainVerified`。PR #507 QA 之后的 SM08–SM14 打在 `internal/replication/mysql_runner.go`：没配上传时运行器启动删掉保留期内文件已不在的目录行（还原修复）、只看行年龄、只看这一轮删掉的文件、没有时间的行当成过期、不回退 `created_at`、`purgeExpiredAt` 不报告删掉的文件、截止时间用 now。SM15（QA Q02）让没读到目录也算 `chain.local` 完整，SM16（QA Q08，`internal/assurance/chain.go`）让 open 分段也判 `I6_MISSING_LOCAL`，SM17 让读不了的 owner 目录重新不打日志。`internal/tasks`、`internal/health` 和 `internal/meta` 只跑被改函数的测试（`-run`），避免整包里已知不稳定的用例混进结果。

## Dependencies

- Upstream: 变异表里点名的源文件和它们包里的测试。
- Downstream: 无。不参与 `go test ./...`。

## Update Rule

- 变异表或击杀方式变化时，改 `kill.go` 的文件头和本文件。
