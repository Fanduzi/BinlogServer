# BinlogServer 生产级验收清单

基线：main `1bbe012`，最新发布 v0.5.76（2026-10-20 01:59 北京时间）。2026-10-20 核对。

状态口径：
- **MET**：有自动检查，今天每个 PR 或每次 push main 都在 CI 里跑并且通过。
- **PARTIAL**：有检查，但只覆盖一部分，或只能手动触发 / 只在 main 跑 / 在 CI 之外跑。
- **NOT MET**：没有可重跑的自动检查，或者已知缺陷还开着。

只能 `workflow_dispatch` 手动触发的 e2e 一律不算 MET。

CI 现状（`.github/workflows/e2e.yml`，workflow 名叫 `ci`）：
- 每个 PR 和 push main 都跑：`vulncheck`；`race-and-vet`（`go vet ./...`、`go test -race ./...`）；`release-archive`（snapshot 包报的 version 和 commit）；`e2e-verify`（真实 MinIO）；`unit-and-e2e`（`go test ./...` 加 quick 档的 10 个场景）；`e2e-retry-upload`、`e2e-split-retention`、`e2e-epoch-segments`、`e2e-gtid-purge`、`e2e-gtid-pitr`、`e2e-source-switchover`；`e2e-observability`。
- 多节点：`e2e-cluster-roles`、`e2e-worker-crash-recovery`、`e2e-meta-failover` 也是每个 PR 和 push main 都跑。
- 任何 job 都不跑：`orchestrator`、`semisync`、`smoke-control-plane-failover`、`smoke-invalid-inputs`、`smoke-recovery-window`、`smoke-scale`、`upgrade-legacy-failback`。

## A. 数据完整性与零静默丢失

| # | 要求 | 验证 | 状态 | 阻塞 |
|---|---|---|---|---|
| A1 | 七条链不变量 I1–I7（ADR 0009）在 standalone 和 cluster 下都会被判定，违反就开 finding | `internal/assurance/chain_*_test.go`（CI 单测）。cluster 下删掉封存段：`TestClusterVerifiedOwnerOpensSegmentMissing`、`TestSegmentMissingUnverifiedOwnerDoesNotClose`、`TestP251SealedRowLocalMissing`、`TestRunnerStartKeepsYoungMissingSegment`、`TestSegmentMissingRecordedOwnerReadFailureKeepsOpen`、`TestSegmentMissingNotForOpenSegment`、`TestUnreadableOwnerDirectoryIsLogged`（都在 `go test ./...`，Confirm 后开 `SEGMENT_MISSING`，健康 degraded，finding 与 `/api/health` 点名 `chain_owner` 或 `node`，文件回到同一目录以 `EVAL` 关；未读到目录不关、不写 `chain_owner`；保留期内文件已不在的目录行在真实 Start 和接管后仍在；读不了的 owner 目录写进 health detail）。还缺：这条路径的 cluster e2e（`e2e-cluster-roles` 只是手动触发，本 PR 不改 workflow）；GTID 内部空洞要开 finding | PARTIAL | #352、#415、#406、#397。#462 的静默缺口由上面的单测盖住，issue 留给 QA，不在这里关。#471 不合并。F7 的 catalog 比对仍是 PR-5 |
| A2 | ADR 0009 列出的 15 条静默路径（S1–S15），每条都同时落下 finding、事件和指标 | `internal/tasks/finding_test.go`、`internal/api/metric_source_test.go`、`internal/replication/finding_dump_test.go`；`internal/lintguard` 拦新的 `_ = err` 和只打日志的写法（都在 `unit-and-e2e` 里） | MET | 注意：lintguard 只看语法，allowlist 里还有 5 条 follow-up；`go vet ./...` 已在 `race-and-vet` job 里；ADR 要求的 `errcheck` 还没进 CI |
| A3 | 配了对象存储时，每个封存段都会上传并校验；保留期不删没验证过的段 | `e2e-retry-upload`、`e2e-split-retention`、`e2e-epoch-segments` | MET | #438（P4，审计行可能丢） |
| A4 | 周期巡检：本地段完整解析，对象每轮 HEAD，SHA-256 按队列（强制重查、最近档（未上传和 24h 内上传，验过即离开，份额上限）、按周期数轮换、最久没验过）在预算内比对；可选的还原演练每 7 天跑一次 | 纯检查仍在 `go test ./...`：`TestVerify_ReportsDeletedObjectFlippedObjectAndFlippedLocal`、`TestVerify_CleanChainHasNoFindings`、`TestByteBudget_IsRespected`（`internal/assurance/verify_test.go`），覆盖模型、关闭规则、轮换上界在 `verify_coverage_test.go`，队列顺序、反复短截止时间下的进度上界、付费字节在 `verify_queue_test.go`，最近档新鲜度与份额、本地已删时的对象解析、链 kind 在 `verify_fresh_test.go`，末档、份额封顶、`LOCAL_UNREADABLE`、短正文、`sealed_at` 偏差、强制段每周期一次在 `verify_r5_test.go`，积压排序、`LOCAL_UNREADABLE` 的升级与两次干净读、`ReadMark` 每个字段在 `verify_qa_test.go`。进程里的周期任务：`TestVerifier_APIReportsMissingLocalAndCorruptObject`（`internal/api/verifier_api_test.go`）让运行中的工人发现缺本地文件和损坏对象，经 `GET /api/tasks/{id}/findings` 和 `GET /api/health` 报出；写回（强制段每周期一次、`LOCAL_UNREADABLE` 的 severity 和 detail）在 `internal/tasks/verifier_test.go`。都在 `unit-and-e2e` 的 `go test ./...` 里。进程级再加 `TestApp_VerifierTickerReportsTwoPasses`（第一轮和 ticker 第二轮都经 API 报出）和 `TestStartVerifier_ClusterIsInert`（集群不读、不开 finding、不写 `.verify.json`）。`go run ./internal/assurance/mutants/kill.go` 打 268 个变异，265 个打死、3 个等价（N30、X05、R15；另有工人 WV08、WV14、WV11、WV16、WV12、WV15、WVP01、WVP02、WVP03、WVP12、WVT01、WVT02、WVT03、WVT07、WVS03、WVS05、WVS06、WVS07、WVC05、WVC06、WVC07、WVC08、WVN03、WVN04、WVN05、WVN07、WVN08、WVN09、WVN11、WVN12、WVN16、WVP03A、WVP03D、WVP03C、WVP03H、WVP06、WVP06T、WVN03S、WVN03D、WVN03H、WVP04、WVP05、WVP05T、WVP07、WVP07T、WVP07S、WVP08、WVP08C、WVP08F、WVP08T、WVS02、WVS02B、WVS02W、WVS04、WVS04B、WVS03D、WVS08、WVS08T、WVS09、WVS06E、WVS06B、WVS06O、WVS07K、WVN10、WVP12K、WVP17B、WVP17C、WVP17K、WVP17R、WVP17O、WVP17S、WVP17L、WVP17X、WVP17D；不在 CI）。启动 HEAD 探针失败打 warn，写在 `/api/health` 的 `detail`，`status` 仍是 ok。连续 3 轮对象存储没查完也写 detail，查完就清。可选演练仍关；真实 MinIO（`scripts/e2e/smoke-verify.sh`）由 `e2e-verify` 每个 PR 跑。集群上 `Allows` 恒为 false，这一项因此还不是 MET | PARTIAL | cluster Allows; enabled once ADR 0011 ownership lands (PR-2/PR-4) |
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
| C1 | ADR 0011 被接受；PR-1 的纯过渡模型合入，十条不变量和活性上界都用有界搜索自动检查 | `internal/ownership` 的 `invariant_test.go`（含 `TestSafetyBoundedExhaustive`、`TestSafetyRandomPrefixes`）、`liveness_test.go`（含 `TestLivenessAllFairSchedules`）、`fault_matrix_test.go`，都在 CI 的 `go test ./...` | MET | ADR 0011 已接受；PR-1 经 #481 合入 main（`4a970319`，#478 第 12 轮 QA PASS） |
| C2 | owner 握着 `task_leases` 行锁被暂停时，对端在 RTO 公式之内接管，等待期间 `/api/health` 返回 503 | ADR 0011 PR-2 的 e2e：在 MySQL 8.0 和 MariaDB 上，持有 `FOR UPDATE` 时 SIGSTOP | NOT MET | #476（P1）、#463 |
| C3 | per_node 存储下，接管不写 FAILED 或 desired STOP；原主人恢复后用更大的 epoch 继续，源上始终最多一条 dump | ADR 0011 PR-4 的 e2e：per_node 下 SIGSTOP/SIGCONT | NOT MET | #473（P1）、#455 |
| C4 | 多节点 e2e 是每个 PR 的门禁 | `e2e-cluster-roles`、`e2e-worker-crash-recovery`、`e2e-meta-failover` 在每个 PR 和 push main 都跑（`if:` 与 `e2e-retry-upload` 等同形，`workflow_dispatch` 时只跑选中的 profile）。「每节点独立 data_dir 加 SIGSTOP 接管」的场景属于 C3/C5，随 ADR 0011 PR-4 加进来 | MET | — |
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
| F3 | `/metrics` 和 trace 的 e2e 每个 PR 都跑 | `e2e-observability`（`make e2e-observability`，即 `scripts/e2e/run-observability.sh`）在每个 PR 和 push main 都跑，`workflow_dispatch` 时只在选 `observability` 时跑。`/metrics`：`smoke-observability` 查 5 个核心指标和状态、checkpoint_age 的变化。trace：服务端开 OTLP/HTTP tracing（采样 1）导出到本地 `otlp-sink.py`，场景要求收到本次任务 start 请求的 API span（服务名、`POST /api/tasks/*path`、`http.target`），收不到就失败 | MET | — |
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
| H2 | CI 里跑 `go test -race` | `race-and-vet` job 每个 PR 和 push main 跑 `go vet ./...` 和 `go test -race -count=1 ./...`。已知在 -race 下偶发失败的 6 个测试用 `-skip` 点名跳过：`TestFailurePathMatrixMetaOutage`、`TestOpenEpochENOENTAfterTakeoverDoesNotFail`（#490），`TestOpenReplayArchive_MissingOpenSegmentIsNotOnProcess`、`TestEvaluator_1500SegmentsUnder50ms`（#504），`TestApp_ProductionAuthProtectsRealRoutes`（#439），`TestTaskRecords_StopAndDeleteApplyWhenSaveFails`（#526）。这 6 个没有环境开关或 `-short` 跳过，仍在 `unit-and-e2e` 的普通 `go test ./...` 里跑 | MET | 注意：上面 6 个只在非 race 下跑，修好一个就把它从 `-skip` 里去掉；#386 |

## I. 运维文档与 runbook

| # | 要求 | 验证 | 状态 | 阻塞 |
|---|---|---|---|---|
| I1 | 每个配置项都有文档，文档里的版本号和 VERSION 一致 | `internal/config/config_doc_test.go` | MET | — |
| I2 | 还原、升级/降级、每种 CRITICAL finding 都有 runbook，并且 runbook 里的命令能执行 | `deployment.md` §7/§8 和 `troubleshooting.md` 都有，但没有测试保证每种 finding 都有处置条目，也没人跑文档里的命令。`docs/develop/beta-acceptance.md` 里「runbook」一项还没勾，内容也过时了（写着 smoke-observability 在 CI 里） | PARTIAL | — |

## J. 发布完整性

| # | 要求 | 验证 | 状态 | 阻塞 |
|---|---|---|---|---|
| J1 | 发布时必须有发布说明，4 个平台的包带 checksums，Linux 包过 glibc 兼容检查 | `release.yml`（v0.5.76 有 4 个包加 `checksums.txt`，我核对过 sha256） | MET | — |
| J2 | 发布出来的二进制报告正确的 version 和 commit | `scripts/check-release-archive-identity.sh` 解包 linux_amd64 包，跑 `binlog-server version`，要求 `Version:` 等于 goreleaser 构建的版本（打 tag 时也等于去掉 `v` 的 tag）、`Commit:` 等于 `GITHUB_SHA`。`release.yml` 在发布前和发布后（发布会重新构建）各跑一次；`e2e.yml` 的 `release-archive` job 每个 PR 用 snapshot 构建跑同一个检查 | MET | — |
| J3 | 打 tag 之前，这个 commit 已经过了全部 e2e（包括多节点 job） | `release.yml` 的 `ci` job 调用整个 `e2e.yml`（`on: workflow_call`），`release` job `needs: [ci]`：tag 所在 commit 上每个按 PR 跑的 job（含 `e2e-cluster-roles`、`e2e-worker-crash-recovery`、`e2e-meta-failover`）都通过才构建和发布。`internal/lintguard/release_gate_test.go`（`go test ./...`）锁住这层依赖，并且不许 `e2e.yml` 的 job 条件看 `github.event_name` 或 `github.ref`（被调用时看到的是 release 的事件，会被跳过） | MET | — |

## 计数

共 35 项：**MET 18 / PARTIAL 7 / NOT MET 10**。

## 最卡生产级的 5 项

1. **C2、C3、C5，cluster 故障切换**：两个 P1（#476、#473）都靠 ADR 0011 的 PR-2 到 PR-4；PR-1 的纯模型已合入（C1）。
2. **A1 还不是 MET，C6 仍是 NOT MET**：#462 的「cluster 删封存段没有 finding」已有单测（ADR 0011 PR-3），但 GTID 内部空洞和手动档 cluster e2e 还没进 CI，所以 A1 仍是 PARTIAL。所有权变化时 finding 单调（C6）要等 PR-5。#471 不合并。
3. **F2、F4，失败到不了人**：评估器默认关，没有 `task_healthy`，没有 webhook，也没有随附的告警规则。
4. **D3，备份是否可还原没人持续验证**：standalone 的周期巡检在 CI 里（运行中的工人把缺本地文件和损坏对象报成 finding）。集群仍不巡检（A4 PARTIAL，等 ADR 0011 的主人）。真实 MinIO 由 `e2e-verify` 每个 PR 跑；可选的还原演练还没有；没有覆盖查询和还原计划（ADR 0009 阶段 4、5）。
5. **D2，只剩对象存储时的还原没进 CI**：`e2e-split-retention` 证明本地按保留期删掉后 download/replay/archive 仍能从对象取回，但不还原进 MySQL 比校验和。
