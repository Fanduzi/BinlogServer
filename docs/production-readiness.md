# BinlogServer 生产级验收清单

基线：main `1bbe012`，最新发布 v0.5.76（2026-10-20 01:59 北京时间）。2026-10-20 核对。

状态口径：
- **MET**：有自动检查，今天每个 PR 或每次 push main 都在 CI 里跑并且通过。
- **PARTIAL**：有检查，但只覆盖一部分，或只能手动触发 / 只在 main 跑 / 在 CI 之外跑。
- **NOT MET**：没有可重跑的自动检查，或者已知缺陷还开着。

只能 `workflow_dispatch` 手动触发的 e2e 一律不算 MET。

CI 现状（`.github/workflows/e2e.yml`，workflow 名叫 `ci`）：
- 每个 PR 和 push main 都跑：`vulncheck`；`unit-and-e2e`（`go test ./...` 加 quick 档的 10 个场景）；`e2e-retry-upload`、`e2e-split-retention`、`e2e-epoch-segments`、`e2e-gtid-purge`、`e2e-gtid-pitr`、`e2e-source-switchover`。
- 只在 push main 跑：`e2e-meta-failover`。
- 只能手动触发：`e2e-cluster-roles`、`e2e-worker-crash-recovery`、`e2e-observability`。
- 任何 job 都不跑：`orchestrator`、`semisync`、`smoke-control-plane-failover`、`smoke-invalid-inputs`、`smoke-recovery-window`、`smoke-scale`、`upgrade-legacy-failback`。

## A. 数据完整性与零静默丢失

| # | 要求 | 验证 | 状态 | 阻塞 |
|---|---|---|---|---|
| A1 | 七条链不变量 I1–I7（ADR 0009）在 standalone 和 cluster 下都会被判定，违反就开 finding | `internal/assurance/chain_*_test.go`（CI 单测）。还缺：cluster 模式删掉一个封存段要开 finding 的 e2e；GTID 内部空洞要开 finding | PARTIAL | #462（P1，cluster 丢掉 I6_MISSING_LOCAL）、#352、#415、#406、#397；按 ADR 0011 由 PR-3 修，#471 不合并 |
| A2 | ADR 0009 列出的 15 条静默路径（S1–S15），每条都同时落下 finding、事件和指标 | `internal/tasks/finding_test.go`、`internal/api/metric_source_test.go`、`internal/replication/finding_dump_test.go`；`internal/lintguard` 拦新的 `_ = err` 和只打日志的写法（都在 `unit-and-e2e` 里） | MET | 注意：lintguard 只看语法，allowlist 里还有 5 条 follow-up；ADR 要求的 `errcheck`/vet 步骤要改 workflow，还没进 CI |
| A3 | 配了对象存储时，每个封存段都会上传并校验；保留期不删没验证过的段 | `e2e-retry-upload`、`e2e-split-retention`、`e2e-epoch-segments` | MET | #438（P4，审计行可能丢） |
| A4 | 周期巡检：本地段完整解析，对象 HEAD 加抽样 SHA-256 比对；可选的还原演练每 7 天跑一次 | 没有。要做 ADR 0009 阶段 3 的 Verifier，再加一个 e2e：删一个 MinIO 对象、改一个字节、改本地一个字节，下一轮必须全部报出来 | NOT MET | ADR 0009 阶段 3 没开工 |
| A5 | 数据盘写满时，备份不能看起来还健康，空间释放后续传不留缺口 | `smoke-standalone-full-disk`（quick；只测 standalone，任务没在跑）。还缺：正在 dump 的任务和 cluster owner 写满的场景（ADR 0011 §4.2 第 15 行） | PARTIAL | ADR 0011 PR-4 |

## B. 崩溃与重启恢复

| # | 要求 | 验证 | 状态 | 阻塞 |
|---|---|---|---|---|
| B1 | standalone（没有 meta）被 kill -9 后重启，不用人工干预自动续传，还原逐字节一致；密钥错误、缺失或文件损坏时明确报错 | `smoke-standalone-restart`（quick，MySQL 8.0 和 MariaDB 11.4） | MET | — |
| B2 | 带 meta 的进程负载中被 kill -9，在同一个 data_dir 重启后续传，没有缺口也没有重复 | `smoke-gtid-redump`（quick） | MET | — |
| B3 | 源库重启、半开连接、源已 purge，或 meta 短暂不可写时：自动重试，或显式 FAILED 加 CRITICAL，不会静默停住 | `smoke-source-outage`、`smoke-unreachable-giveup`、`smoke-fail-alert`（quick）、`e2e-gtid-purge`、S12 单测（`finding_dump_test.go`、`source_conn_test.go`） | MET | 小问题：#345、#343、#344、#365、#387 |

## C. 故障切换与集群（ADR 0011，5 个 PR）

| # | 要求 | 验证 | 状态 | 阻塞 |
|---|---|---|---|---|
| C1 | ADR 0011 被接受；PR-1 的纯过渡模型合入，十条不变量和活性上界都用有界搜索自动检查 | PR-1 的模型测试，合入后进 `go test ./...` | NOT MET | ADR 还是 Proposed；#478 第 6 轮 QA FAIL，第 7 轮（`77cb41f`）待 QA |
| C2 | owner 握着 `task_leases` 行锁被暂停时，对端在 RTO 公式之内接管，等待期间 `/api/health` 返回 503 | ADR 0011 PR-2 的 e2e：在 MySQL 8.0 和 MariaDB 上，持有 `FOR UPDATE` 时 SIGSTOP | NOT MET | #476（P1）、#463 |
| C3 | per_node 存储下，接管不写 FAILED 或 desired STOP；原主人恢复后用更大的 epoch 继续，源上始终最多一条 dump | ADR 0011 PR-4 的 e2e：per_node 下 SIGSTOP/SIGCONT | NOT MET | #473（P1）、#455 |
| C4 | 多节点 e2e 是每个 PR 的门禁 | 已有 `e2e-cluster-roles`、`e2e-worker-crash-recovery`，但只能手动触发（最近一次是 10-19 在分支上手动跑的）；`e2e-meta-failover` 只在 push main 跑。现有场景都没覆盖「每节点独立 data_dir 加 SIGSTOP 接管」 | PARTIAL | 要改 workflow，需要有 `workflow` 权限的 token（`HANDOFF-ci-cluster-e2e.md` 里有现成的 diff） |
| C5 | ADR 0011 §4.2 的故障注入矩阵（4 个阶段 × SIGSTOP/kill -9/分区/重启，再加 17 行补充）作为发布档 e2e，没跑的格子写明没跑 | 没有，PR-2 和 PR-4 要建 | NOT MET | ADR 0011 PR-2、PR-4 |
| C6 | 所有权变化时 finding 保持单调（不先关再开），不完整的观察既不开也不关 finding | 生产 `Reconcile` 上的属性测试（PR-5） | NOT MET | ADR 0011 PR-5；#451、#452、#366 |
| C7 | 源库 VIP 切到 GTID 一致的新主后继续备份，链连续，还原后校验和一致 | `e2e-source-switchover`（每个 PR） | MET | #440（偶发的 pipefail 问题） |

## D. 还原 / PITR 演练

| # | 要求 | 验证 | 状态 | 阻塞 |
|---|---|---|---|---|
| D1 | 用备份按 GTID 停点还原到一台新 MySQL，数据和预期一致（open 段和封存段都测） | `e2e-gtid-pitr`（`smoke-gtid-pitr`、`smoke-gtid-executed`）；只有 MySQL 8.0 | MET | — |
| D2 | 本地文件已按保留期删掉、只剩对象存储时，仍能下载回放并还原 | `e2e-split-retention`（只证明 download/replay/archive 能从对象取回，不还原进 MySQL 比校验和）；`upgrade-legacy-failback.sh FBUP_CASE=object-only` 会做还原，但不在任何 job 里 | PARTIAL | — |
| D3 | 给一个时间点或 GTID，能查出覆盖它的文件和对象（`/coverage`），并一步生成还原计划和预检（`/restore-plan`） | 没有接口。验收按 ADR 0009 阶段 4/5：窗口内、窗口外、断点上各 10 例，和 mysqlbinlog 的独立解析一致；向导还原 5 个时间点，校验和一致 | NOT MET | ADR 0009 阶段 4、5 |
| D4 | 各支持版本上都能拉流并且能还原 | `smoke.sh` 在 mysql57、mysql80、percona57、percona80 上拉流（quick）；MariaDB 11.4 和 8.0 在 `smoke-standalone-restart` 里做了还原。缺：5.7 上的还原、8.4 LTS（compose 里没有 8.4） | PARTIAL | — |

## E. 升级、迁移与回滚

| # | 要求 | 验证 | 状态 | 阻塞 |
|---|---|---|---|---|
| E1 | 每个 schema 迁移都能 up/down 往返；新二进制拒绝太旧的 schema，旧二进制拒绝自己不认识的 schema | `smoke-task-desired-migration`（拿已发布的 v0.5.49/v0.5.52 实测）、`smoke-control-loop`（逐步 down 到 schema 3，每一步核对表和索引），都在 quick | MET | — |
| E2 | 从上一个发布版带数据原地升级，续传没有缺口，旧数据目录能被修复 | `upgrade-legacy-failback.sh` 有，但不在任何 profile 或 job 里；CI 里只有同一个二进制的重启（B2） | PARTIAL | #437（在线 migrate up 后，重启前的行不会被标记） |
| E3 | cluster 滚动升级有栅栏：预检、`row_generation` 触发器、`MIXED_VERSION`；降级会拒绝不安全的状态 | ADR 0011 §5 的 000011 迁移，加 §4.2 第 12 行的 e2e | NOT MET | ADR 0011 PR-4 |

## F. 可观测与告警

| # | 要求 | 验证 | 状态 | 阻塞 |
|---|---|---|---|---|
| F1 | finding 指标（`binlog_server_finding_open` 等）总有输出；某个任务的读数失败时显式输出 0 并开 finding，不会直接丢掉这条序列 | `internal/api/metric_source_test.go`（S10/S11）、`metrics_findings` 单测 | MET | — |
| F2 | 每个任务都有 `binlog_server_task_healthy` 和 SLO 指标；仓库里随附推荐告警规则文件（用 promtool 测）和一个通用 webhook（ADR 0009 的决定 1） | 没有：代码里没有 `task_healthy`、没有 webhook；告警规则只写在 `docs/guide/admin/observability.md` 里，也没用 finding 指标 | NOT MET | ADR 0009 §2.2、决定 1 |
| F3 | `/metrics` 和 trace 的 e2e 每个 PR 都跑 | `e2e-observability` 只能手动触发。全部 80 次手动运行（最早 10-15）里一次都没选过它 | PARTIAL | 要改 workflow（需要 token） |
| F4 | 健康评估器在默认配置下生效，结论只有一条写入路径，CI 测的就是生产跑的路径 | `health.evaluator` 默认是 `off`。CI 的 e2e 配置没有 health 段，所以除了 standalone 的两个场景显式设了 `on`，其余都按 `off` 在跑 | NOT MET | 已经决定删掉这个开关（卡 1），要等 ADR 0011 前两步落地 |

## G. 安全

| # | 要求 | 验证 | 状态 | 阻塞 |
|---|---|---|---|---|
| G1 | PRODUCTION 模式下鉴权失败就拒绝启动；源密码和 standalone 任务记录加密存放；依赖做漏洞扫描 | `internal/app/http_server_test.go`、`smoke_test.go`、`internal/taskrecords` 单测、`smoke-standalone-restart`（密钥错误、缺失）、`vulncheck` job | MET | #439（-race 下偶发失败） |
| G2 | 到源库和 meta 的连接支持并校验 TLS，S3 默认走 TLS | 没有：复制连接和 meta 都没有 TLS 配置，`upload.use_ssl` 默认是 false，也没有测试 | NOT MET | 没有对应 issue |

## H. 性能与资源上界

| # | 要求 | 验证 | 状态 | 阻塞 |
|---|---|---|---|---|
| H1 | 在约定负载下（ADR 0009 §6：≥500 TPS、24h、注入各类故障）延迟有界，RSS/fd/goroutine 不会单调增长，用独立判定器验收 | `smoke-scale` 只能手动 opt-in，不在任何 job 里；`scripts/soak/` 不存在 | NOT MET | ADR 0009 阶段 6 |
| H2 | CI 里跑 `go test -race` | 不跑。CI 是 `go test ./...`，`-race` 只在本地脚本 `scripts/verify-phase-acceptance.sh` 里 | NOT MET | #386、#439（已知的偶发失败）；要改 workflow |

## I. 运维文档与 runbook

| # | 要求 | 验证 | 状态 | 阻塞 |
|---|---|---|---|---|
| I1 | 每个配置项都有文档，文档里的版本号和 VERSION 一致 | `internal/config/config_doc_test.go` | MET | — |
| I2 | 还原、升级/降级、每种 CRITICAL finding 都有 runbook，并且 runbook 里的命令能执行 | `deployment.md` §7/§8 和 `troubleshooting.md` 都有，但没有测试保证每种 finding 都有处置条目，也没人跑文档里的命令。`docs/develop/beta-acceptance.md` 里「runbook」一项还没勾，内容也过时了（写着 smoke-observability 在 CI 里） | PARTIAL | — |

## J. 发布完整性

| # | 要求 | 验证 | 状态 | 阻塞 |
|---|---|---|---|---|
| J1 | 发布时必须有发布说明，4 个平台的包带 checksums，Linux 包过 glibc 兼容检查 | `release.yml`（v0.5.76 有 4 个包加 `checksums.txt`，我核对过 sha256） | MET | — |
| J2 | 发布出来的二进制报告正确的 version 和 commit | v0.5.76 的 linux_amd64 实测报告 `b1de5576…`，是对的；但 `release.yml` 里没有任何断言，坏了也不会挡 | PARTIAL | — |
| J3 | 打 tag 之前，这个 commit 已经过了全部 e2e（包括多节点 job） | 没有：`release.yml` 只跑 `go test ./...`，不检查 tag 所在 commit 的 CI 结果 | NOT MET | 要改 workflow |

## 计数

共 35 项：**MET 12 / PARTIAL 9 / NOT MET 14**。

## 最卡生产级的 5 项

1. **C1–C3、C5，cluster 故障切换**：两个 P1（#476、#473）都靠 ADR 0011 的 PR-1 到 PR-4，PR-1（#478）连续 6 轮 QA 没过。
2. **A1、C6，cluster 下缺段没有 finding**：#462（P1）；对应的 PR #471 不会合，要等 PR-3 和 PR-5。
3. **F2、F4，失败到不了人**：评估器默认关，没有 `task_healthy`，没有 webhook，也没有随附的告警规则。
4. **A4、D3，备份是否可还原没人持续验证**：没有巡检、没有还原演练、没有覆盖查询和还原计划（ADR 0009 阶段 3–5 都没做）。
5. **C4、J3、H2，CI 门禁有洞**：多节点 e2e 不挡 PR，发布不看 e2e，也不跑 -race。改 workflow 需要有 workflow 权限的 token。
