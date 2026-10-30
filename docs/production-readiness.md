# BinlogServer 生产级验收清单

基线：main `1bbe012`，最新发布 v0.5.76（2026-10-20 01:59 北京时间）。2026-10-20 核对。

状态口径：
- **MET**：有自动检查，今天每个 PR 或每次 push main 都在 CI 里跑并且通过。
- **PARTIAL**：有检查，但只覆盖一部分，或只能手动触发 / 只在 main 跑 / 在 CI 之外跑。
- **NOT MET**：没有可重跑的自动检查，或者已知缺陷还开着。

只能 `workflow_dispatch` 手动触发的 e2e 一律不算 MET。

CI 现状（`.github/workflows/e2e.yml`，workflow 名叫 `ci`）：
- 每个 PR 和 push main 都跑：`vulncheck`；`e2e-upgrade-failback`（`upgrade-legacy-failback.sh` 的 `plain`、`object-only`、`object-gone` 从 v0.5.57，以及 `plain` 从 v0.5.82）；`race-and-vet`（`go vet ./...`、`go test -race ./...`）；`release-archive`（snapshot 包报的 version 和 commit）；`e2e-verify`（真实 MinIO）；`unit-and-e2e`（`go test ./...` 加 quick 档的 11 个场景）；`e2e-retry-upload`、`e2e-split-retention`、`e2e-epoch-segments`、`e2e-gtid-purge`、`e2e-gtid-pitr`、`e2e-coverage`、`e2e-source-switchover`；`e2e-observability`；`e2e-tls`（`TestLiveMySQLTLS`、`TestLiveAppTLS`、`TestLiveS3TLS`）；`promtool`（`promtool test rules deploy/prometheus/binlog-server-alerts.test.yml`）。
- 多节点：`e2e-cluster-roles`、`e2e-worker-crash-recovery`、`e2e-meta-failover` 也是每个 PR 和 push main 都跑。
- 任何 job 都不跑：`orchestrator`、`semisync`、`smoke-control-plane-failover`、`smoke-invalid-inputs`、`smoke-recovery-window`、`smoke-scale`。

## A. 数据完整性与零静默丢失

| # | 要求 | 验证 | 状态 | 阻塞 |
|---|---|---|---|---|
| A1 | 七条链不变量 I1–I7（ADR 0009）在 standalone 和 cluster 下都会被判定，违反就开 finding | `internal/assurance/chain_*_test.go`（CI 单测）。cluster 下删掉封存段：`TestClusterVerifiedOwnerOpensSegmentMissing`、`TestSegmentMissingUnverifiedOwnerDoesNotClose`、`TestP251SealedRowLocalMissing`、`TestRunnerStartKeepsYoungMissingSegment`、`TestSegmentMissingRecordedOwnerReadFailureKeepsOpen`、`TestSegmentMissingNotForOpenSegment`、`TestUnreadableOwnerDirectoryIsLogged`（都在 `go test ./...`，Confirm 后开 `SEGMENT_MISSING`，健康 degraded，finding 与 `/api/health` 点名 `chain_owner` 或 `node`，文件回到同一目录以 `EVAL` 关；未读到目录不关、不写 `chain_owner`；保留期内文件已不在的目录行在真实 Start 和接管后仍在；读不了的 owner 目录写进 health detail）。cluster e2e：`smoke-cluster-roles` 末格把封存段 mv 出任务目录，`SEGMENT_MISSING` 开 CRITICAL、`/api/health` 503，mv 回后 `EVAL` 关、健康回 200（`e2e-cluster-roles` 每个 PR 都跑）。GTID 内部空洞：`smoke-gtid-hole`（quick 档，随 `unit-and-e2e` 每个 PR 跑）`GTID_NEXT` 跳号开出 `GTID_HOLE`+503，`/api/alerts/waive` 精确 evidence 豁免后关闭，存量增长豁免保持有效，新段新洞以新 evidence 重开 | MET | #352、#415、#406、#397。#462 的静默缺口由上面的单测盖住，issue 留给 QA，不在这里关。#471 不合并。F7 的 catalog 比对仍是 PR-5 |
| A2 | ADR 0009 列出的 15 条静默路径（S1–S15），每条都同时落下 finding、事件和指标 | `internal/tasks/finding_test.go`、`internal/api/metric_source_test.go`、`internal/replication/finding_dump_test.go`；`internal/lintguard` 拦新的 `_ = err` 和只打日志的写法（都在 `unit-and-e2e` 里） | MET | 注意：lintguard 只看语法，allowlist 里还有 5 条 follow-up；`go vet ./...` 已在 `race-and-vet` job 里；ADR 要求的 `errcheck` 还没进 CI |
| A3 | 配了对象存储时，每个封存段都会上传并校验；保留期不删没验证过的段 | `e2e-retry-upload`、`e2e-split-retention`、`e2e-epoch-segments` | MET | #438（P4，审计行可能丢） |
| A4 | 周期巡检：本地段完整解析，对象每轮 HEAD，SHA-256 按队列（强制重查、最近档（未上传和 24h 内上传，验过即离开，份额上限）、按周期数轮换、最久没验过）在预算内比对；可选的还原演练每 7 天跑一次 | 纯检查仍在 `go test ./...`：`TestVerify_ReportsDeletedObjectFlippedObjectAndFlippedLocal`、`TestVerify_CleanChainHasNoFindings`、`TestByteBudget_IsRespected`（`internal/assurance/verify_test.go`），覆盖模型、关闭规则、轮换上界在 `verify_coverage_test.go`，队列顺序、反复短截止时间下的进度上界、付费字节在 `verify_queue_test.go`，最近档新鲜度与份额、本地已删时的对象解析、链 kind 在 `verify_fresh_test.go`，末档、份额封顶、`LOCAL_UNREADABLE`、短正文、`sealed_at` 偏差、强制段每周期一次在 `verify_r5_test.go`，积压排序、`LOCAL_UNREADABLE` 的升级与两次干净读、`ReadMark` 每个字段在 `verify_qa_test.go`。进程里的周期任务：`TestVerifier_APIReportsMissingLocalAndCorruptObject`（`internal/api/verifier_api_test.go`）让运行中的工人发现缺本地文件和损坏对象，经 `GET /api/tasks/{id}/findings` 和 `GET /api/health` 报出；写回（强制段每周期一次、`LOCAL_UNREADABLE` 的 severity 和 detail）在 `internal/tasks/verifier_test.go`。都在 `unit-and-e2e` 的 `go test ./...` 里。进程级再加 `TestApp_VerifierTickerReportsTwoPasses`（第一轮和 ticker 第二轮都经 API 报出）和 `TestStartVerifier_ClusterIsInert`（集群不读、不开 finding、不写 `.verify.json`）。`go run ./internal/assurance/mutants/kill*.go` 打 268 个变异，265 个打死、3 个等价（N30、X05、R15；另有工人 WV08、WV14、WV11、WV16、WV12、WV15、WVP01、WVP02、WVP03、WVP12、WVT01、WVT02、WVT03、WVT07、WVS03、WVS05、WVS06、WVS07、WVC05、WVC06、WVC07、WVC08、WVN03、WVN04、WVN05、WVN07、WVN08、WVN09、WVN11、WVN12、WVN16、WVP03A、WVP03D、WVP03C、WVP03H、WVP06、WVP06T、WVN03S、WVN03D、WVN03H、WVP04、WVP05、WVP05T、WVP07、WVP07T、WVP07S、WVP08、WVP08C、WVP08F、WVP08T、WVS02、WVS02B、WVS02W、WVS04、WVS04B、WVS03D、WVS08、WVS08T、WVS09、WVS06E、WVS06B、WVS06O、WVS07K、WVN10、WVP12K、WVP17B、WVP17C、WVP17K、WVP17R、WVP17O、WVP17S、WVP17L、WVP17X、WVP17D；不在 CI）。启动 HEAD 探针失败打 warn，写在 `/api/health` 的 `detail`。普通失败（例如 NoSuchBucket）`status` 仍是 ok。TLS 传输失败（x509，或 HTTPS 客户端收到 HTTP）把 `status` 写成 degraded，`/api/health` 返回 503，reason 以 `UPLOAD_TLS` 开头；每次上传重试对这类失败打 WARN。连续 3 轮对象存储没查完也写 detail，查完就清。可选演练仍关；真实 MinIO（`scripts/e2e/smoke-verify.sh`）由 `e2e-verify` 每个 PR 跑。集群上 `Allows` 已接 `Scheduler.VerifyAllows`：任务租约持有者巡检，未持有者跳过（`TestVerifyAllows_LeaseHolderOnly`、`TestVerifier_ClusterOwnerVerifies`；停止的任务无持有者暂无人巡检）。仍缺：可选还原演练 | PARTIAL | drill + stopped-task verify |
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
| C2 | owner 握着 `task_leases` 行锁被暂停时，对端在 RTO 公式之内接管，等待期间 `/api/health` 返回 503 | `smoke-session-kill`（quick 档，`unit-and-e2e` 每个 PR 都跑）：在 MySQL 8.0 和 MariaDB 上各建持有 `task_leases` `FOR UPDATE` 的会话，`SIGSTOP` 后对端先拿不到行锁、在会话超时上限之内拿到（`TestSessionKillDropsSleepingForUpdate`、`TestQA484DeadlineMargin`、`TestQA484PoolPressure` 两台库都必须 PASS，SKIP 算失败）；没有 `PROCESS` 权限的账号下 `SIGSTOP` 持有者由本节点评估器开 `META_LOCK_STALLED`，期间 `GET /api/health` 为 503；另有一格确认只有一条已停止任务的集群进程在默认 60s sweep 下 `/api/health` 保持 200 且无 `HEALTH_STALE` | MET | ADR 0011 PR-2 已合入；#476（P1）、#463 |
| C3 | per_node 存储下，接管不写 FAILED 或 desired STOP；原主人恢复后用更大的 epoch 继续，源上始终最多一条 dump | `smoke-sigstop-takeover`（CI `e2e-sigstop-takeover`，每个 PR）：per_node 独立 `data_dir` 的双 worker 上 `kill -STOP` owner 超过 `lease_ttl`，对端认领出更大 epoch 后因链尾巴在冻结节点盘上而让出成 `LEASE_DEGRADED`；每个采样点断言不是 `FAILED`、`desired_run` 不是 `STOP`、源库 `Binlog Dump` 线程至多一条；`SIGCONT` 后原主人让出旧 epoch、用更大的 epoch 重新认领回 `RUNNING` | MET | ADR 0011 PR-4 已合入（#524）；#473、#455 由该场景回归 |
| C4 | 多节点 e2e 是每个 PR 的门禁 | `e2e-cluster-roles`、`e2e-worker-crash-recovery`、`e2e-meta-failover` 在每个 PR 和 push main 都跑（`if:` 与 `e2e-retry-upload` 等同形，`workflow_dispatch` 时只跑选中的 profile）。「每节点独立 data_dir 加 SIGSTOP 接管」的场景属于 C3/C5，随 ADR 0011 PR-4 加进来 | MET | — |
| C5 | ADR 0011 §4.2 的故障注入矩阵（4 个阶段 × SIGSTOP/kill -9/分区/重启，再加 17 行补充）作为发布档 e2e，没跑的格子写明没跑 | `smoke-fault-matrix.sh`（`e2e-fault-matrix` job 每个 PR 跑）：7 格实跑（kill -9+同 id 重启、连续让出、目录拷贝拒绝语义、owner 死亡有界降级、三节点单目录、操作员竞态含 N8 控制面重启、行锁重试），5 格 COVERED 由 session-kill/sigstop-takeover/upgrade-failback/meta-failover 覆盖，其余显式 SKIP；矩阵发现两个真实缺口开为 issue：死主任务在 per_node 下永远 LEASE_DEGRADED 且无任何 finding（#580）、拷贝的目录因路径命名空间不可接管（#581）、kill-9 落在 .open 无完整事件窗口时同主同目录重启拒绝自己尾巴（#583） | PARTIAL | 分区原语、事务窗口注入点、共享存储 runner、PR-5 依赖格、#580/#581/#583 |
| C6 | 所有权变化时 finding 保持单调（不先关再开），不完整的观察既不开也不关 finding | 生产 `Reconcile` 上的属性测试（PR-5） | MET | PR-5 已删 `OWNER_CHANGED` 关闭路径；`pr5_property_test.go` 在生产 `Reconcile` 上证明三条属性；#451、#452、#366 |
| C7 | 源库 VIP 切到 GTID 一致的新主后继续备份，链连续，还原后校验和一致 | `e2e-source-switchover`（每个 PR） | MET | #440（偶发的 pipefail 问题） |

## D. 还原 / PITR 演练

| # | 要求 | 验证 | 状态 | 阻塞 |
|---|---|---|---|---|
| D1 | 用备份按 GTID 停点还原到一台新 MySQL，数据和预期一致（open 段和封存段都测） | `e2e-gtid-pitr`（`smoke-gtid-pitr`、`smoke-gtid-executed`）；只有 MySQL 8.0 | MET | — |
| D2 | 本地文件已按保留期删掉、只剩对象存储时，仍能下载回放并还原 | `e2e-upgrade-failback` 每个 PR 跑 `upgrade-legacy-failback.sh FBUP_CASE=object-only`，任务配 `local_retention_days=1`、`bucket_retention_days=30`。修复后先在 A 上 `FLUSH BINARY LOGS` 定一个封存边界，等边界之前的每一段都 SEALED 且 UPLOADED、唯一没封存的是边界之后新开的那段。这些段（含 A 第一段）由产品自己的拆分保留删掉：脚本只把修改时间调到 2 天前，再 `FLUSH BINARY LOGS` 触发保留，本地文件删掉，对象和目录行留下，记 `RETENTION_REMOVED`；脚本不 mv、不删这些文件。`/replay` 必须把每个被删段标成 `bucket`。读 `/replay` 和 `/replay/archive` 期间用 `mc admin trace` 计数，每个被删段的对象键至少一次 MinIO `GetObject`。归档成员与删前的文件逐字节一致，被删段排在归档最前。归档读完、还原之后各扫一遍 md5：`data_dir`（递归，含 `.cache` 等点目录）、服务端 `TMPDIR`、`/tmp` 下比删除更新的文件，出现任何一个被删段的同字节副本就失败；扫描不覆盖这些目录以外的路径，那种藏起来的副本靠 `GetObject` 计数抓。只用这些 bucket 成员、宿主机 MySQL 8.0 `mysqlbinlog` 还原进清空的新库，`CHECKSUM TABLE` 和 `gtid_executed` 必须等于 A 在边界时的值，不依赖 open 尾段。open 尾段例外：边界之后写的事务只在本地（边界后封存的空段和 open 段），叠加还原后等于 A 的最终校验和，这一步读的是本地文件，不算对象存储的证明（丢盘时的 RPO 见 [#530](https://github.com/Fanduzi/BinlogServer/issues/530)）。`e2e-split-retention` 另外证明按保留期删掉本地文件后 download/replay/archive 仍从对象取回 | MET | — |
| D3 | 给一个时间点或 GTID，能查出覆盖它的文件和对象（`/coverage`），并一步生成还原计划和预检（`/restore-plan`） | `e2e-coverage` 每个 PR 跑 `smoke-coverage.sh`：窗口内、窗口外、断点上的时间和 GTID 各 10 例，答案与 mysqlbinlog 的独立解析一致（分段名和停止偏移）；5 个时间点按 `/restore-plan` 的 `--stop-datetime` 回放，`CHECKSUM TABLE` 与源库当时一致；全备 GTID 有洞或与将要应用的事务重叠时 `full_backup_gtid` 预检为 red。`limit` 不截断前缀。集群模式三个接口 501 由 `TestTaskAPI_CoverageAndRestorePlan` 覆盖（ADR 0011 PR-4 之前不按单节点目录回答） | MET | — |
| D4 | 各支持版本上都能拉流并且能还原 | `unit-and-e2e` 每个 PR 的 quick 档：`smoke.sh` 在 mysql57、mysql80、mysql84、percona57、percona80 上拉流，写入后 checkpoint 的 file/pos 必须前进；`smoke-version-restore.sh` 把 mysql57 和 mysql84 在任务启动之后写入的库表（含一次 `FLUSH BINARY LOGS`）经 `mysqlbinlog` 回放到同版本空实例，`CHECKSUM TABLE` 和行文本与源库一致；`smoke-standalone-restart` 仍在 MySQL 8.0 和 MariaDB 11.4 上做还原 | MET | — |

## E. 升级、迁移与回滚

| # | 要求 | 验证 | 状态 | 阻塞 |
|---|---|---|---|---|
| E1 | 每个 schema 迁移都能 up/down 往返；新二进制拒绝太旧的 schema，旧二进制拒绝自己不认识的 schema | `smoke-task-desired-migration`（拿已发布的 v0.5.49/v0.5.52 实测）、`smoke-control-loop`（逐步 down 到 schema 3，每一步核对表和索引），都在 quick | MET | — |
| E2 | 从上一个发布版带数据原地升级，续传没有缺口，旧数据目录能被修复 | `e2e-upgrade-failback` 每个 PR 跑四档：`plain`、`object-only`、`object-gone` 从 v0.5.57 → 0a84917a → 当前构建（旧目录修复后续传、还原和 A 一致；`object-gone` 拒绝且目录和对象不变）；`plain` 从 v0.5.82 直接升到当前构建（`FBUP_SKIP_MID=1`、`FBUP_REQUIRE_REUSE=0`），带数据原地升级后续传、还原和 A 一致。#437 由 `TestOnlineMigrateUpWritesRetentionMarkerWithoutSecondEvent`（`go test ./...`）锁住：000010 列缺席时只写一条 `RETENTION_REMOVED`，列出现后的下一轮只补 `retention_removed_at`、不写第二条事件 | MET | — |
| E3 | cluster 滚动升级有栅栏：预检、`row_generation` 触发器、`MIXED_VERSION`；降级会拒绝不安全的状态 | ADR 0011 §5 的 000011 迁移，加 §4.2 第 12 行的 e2e | NOT MET | `smoke-rolling-upgrade.sh` 已实现四道栅栏（up 预检拒绝/落地、MIXED_VERSION+认领闸住、down 预检拒绝/降级后旧二进制回跑），等 CI 首跑验证 |

## F. 可观测与告警

| # | 要求 | 验证 | 状态 | 阻塞 |
|---|---|---|---|---|
| F1 | finding 指标（`binlog_server_finding_open` 等）总有输出；某个任务的读数失败时显式输出 0 并开 finding，不会直接丢掉这条序列 | `internal/api/metric_source_test.go`（S10/S11）、`metrics_findings` 单测 | MET | — |
| F2 | 每个任务都有 `binlog_server_task_healthy` 和 SLO 指标；仓库里随附推荐告警规则文件（用 promtool 测）和一个通用 webhook（ADR 0009 的决定 1） | `internal/api/metrics_health_test.go` 断言 `/metrics` 里的 `binlog_server_task_healthy`、progress age、upload lag、open CRITICAL；`internal/tasks/health_export_test.go` 断言非主人不输出；`internal/notify/webhook_test.go` 覆盖投递、500 后重试、400 不重试；`internal/tasks/health_notify_test.go` 覆盖失败后重试和稳态不重复，以及 shadow 不发。都在 `unit-and-e2e` 的 `go test ./...` 里。`deploy/prometheus/binlog-server-alerts.yml` 由 `promtool` job 每个 PR 和 push main 跑 `promtool test rules` | MET | — |
| F3 | `/metrics` 和 trace 的 e2e 每个 PR 都跑 | `e2e-observability`（`make e2e-observability`，即 `scripts/e2e/run-observability.sh`）在每个 PR 和 push main 都跑，`workflow_dispatch` 时只在选 `observability` 时跑。`/metrics`：`smoke-observability` 查 5 个核心指标和状态、checkpoint_age 的变化。trace：服务端开 OTLP/HTTP tracing（采样 1）导出到本地 `otlp-sink.py`，场景要求收到本次任务 start 请求的 API span（服务名、`POST /api/tasks/*path`、`http.target`），收不到就失败 | MET | — |
| F4 | 健康评估器在默认配置下生效，结论只有一条写入路径，CI 测的就是生产跑的路径 | `health.evaluator` 开关已删：`EvaluatorMode`/`ParseEvaluatorMode`/`EvaluatorConfig.Mode`/shadow 写入路径与 `EvaluatorStats.Shadowed` 一并移除，评估器恒按旧 `on` 语义运行；仍设置该键的配置在启动时得到一条 retired 警告并被忽略（滚动升级兼容）。e2e 脚本里的 `BINLOG_SERVER_HEALTH_EVALUATOR` 全部清掉，CI 跑的就是生产路径 | MET | ADR 0011 PR-1（#481）、PR-4（#524）已落地 |

## G. 安全

| # | 要求 | 验证 | 状态 | 阻塞 |
|---|---|---|---|---|
| G1 | PRODUCTION 模式下鉴权失败就拒绝启动；源密码和 standalone 任务记录加密存放；依赖做漏洞扫描 | `internal/app/http_server_test.go`、`smoke_test.go`、`internal/taskrecords` 单测、`smoke-standalone-restart`（密钥错误、缺失）、`vulncheck` job | MET | — |
| G2 | 到源库和 meta 的连接支持并校验 TLS，S3 默认走 TLS | `TestUploadUseSSLDefaultsToTLS`、`TestMetaTLSParseAndValidate`（`internal/config`）、`TestUseSSLTrueSendsTLSClientHello`（`internal/upload`）、`TestNormalize_DisabledIsDefault`、`TestNormalize_RejectsShape`、`TestApplyDSN_DisabledLeavesDSN`、`TestApplyDSN_PreferredAndRejectsExistingTLS`、`TestClientConfig_VerifyCASkipsHostname`、`TestClientConfig_KeyMaterialStaysOutOfErrors`、`TestClientConfig_LoadsClientCert`（`internal/mysqltls`）、`TestSourceTLSShapeAndUpdateKeepsOmittedBlock`、`TestTaskRecordKeepsSourceTLS`（`internal/tasks`），都在 `unit-and-e2e` 的 `go test ./...` 和 `race-and-vet` 里。真实 MySQL 和 MinIO（job `e2e-tls`，每个 PR 和 push main）：`TestLiveAppTLS`（`internal/app`）经 `POST /api/tasks` 创建 `source.tls` `verify-ca` 任务并 Start，真实 runner 的 binlog dump 在 `performance_schema.threads` 的 `CONNECTION_TYPE` 为 SSL/TLS；错误 CA 和错误主机名的任务为 `FAILED`，`last_error` 含 x509，没有明文 Binlog Dump。同一测试用 `./migrate --config` 的 `meta.tls` `verify-ca` 迁完库后 `App.Run` 打开 meta 池，该账号连接的 `CONNECTION_TYPE` 为 SSL/TLS；错误 CA 启动失败且错误含 `meta.tls` 和 x509。`TestLiveS3TLS`（`internal/upload`）对私有 CA 的 MinIO 用 `upload.tls.ca_file` 经 HTTPS 上传一个分段，不受信任的 CA 失败。`TestLiveMySQLTLS`（`internal/replication`）覆盖 `connectSource` 的模式，`preferred` 只在问候包没有 `CLIENT_SSL` 时退回明文 | MET | — |

## H. 性能与资源上界

| # | 要求 | 验证 | 状态 | 阻塞 |
|---|---|---|---|---|
| H1 | 在约定负载下（ADR 0009 §6：≥500 TPS、24h、注入各类故障）延迟有界，RSS/fd/goroutine 不会单调增长，用独立判定器验收 | `smoke-scale` 只能手动 opt-in，不在任何 job 里；`scripts/soak/` 不存在 | NOT MET | ADR 0009 阶段 6 |
| H2 | CI 里跑 `go test -race` | `race-and-vet` job 每个 PR 和 push main 跑 `go vet ./...` 和 `go test -race -count=1 ./...`，不再 `-skip`。原先在 -race 下偶发失败的 `TestFailurePathMatrixMetaOutage`、`TestOpenEpochENOENTAfterTakeoverDoesNotFail`（#490），`TestOpenReplayArchive_MissingOpenSegmentIsNotOnProcess`、`TestEvaluator_1500SegmentsNearLinear`（#504），`TestApp_ProductionAuthProtectsRealRoutes`（#439），`TestTaskRecords_StopAndDeleteApplyWhenSaveFails`（#526）都在这个 job 里跑 | MET | #386 没有点名测试，仍开着 |

## I. 运维文档与 runbook

| # | 要求 | 验证 | 状态 | 阻塞 |
|---|---|---|---|---|
| I1 | 每个配置项都有文档，文档里的版本号和 VERSION 一致 | `internal/config/config_doc_test.go` | MET | — |
| I2 | 还原、升级/降级、每种 CRITICAL finding 都有 runbook，并且 runbook 里的命令能执行 | `troubleshooting.md` §9「CRITICAL finding 处置手册」：注册表 25 个 CRITICAL kind 加上表外的 `MIXED_VERSION`/`META_LOCK_STALLED`/`RETRY_LOOP_FAILED`/`FINDINGS_PERSIST_FAILED`/`UPLOAD_LATE` 每条一个 `### KIND` 小节（含义/处置/关闭条件）带命令块；`internal/health/runbook_doc_test.go` 契约测试枚举 `DefaultRegistry()` 的 CRITICAL 集合断言逐条覆盖、并对该节全部命令块跑 `bash -n`（`go test ./...` 每 PR 跑，命令写坏即红）。还原回放见 troubleshooting §7.4 与 deployment「源库不可用回放」节，升级/降级见 upgrade.md 与 deployment §7/§8；beta-acceptance 的 runbook 与两个 CI 门禁勾项已同步为当前事实 | MET | — |

## J. 发布完整性

| # | 要求 | 验证 | 状态 | 阻塞 |
|---|---|---|---|---|
| J1 | 发布时必须有发布说明，4 个平台的包带 checksums，Linux 包过 glibc 兼容检查 | `release.yml`（v0.5.76 有 4 个包加 `checksums.txt`，我核对过 sha256） | MET | — |
| J2 | 发布出来的二进制报告正确的 version 和 commit | `scripts/check-release-archive-identity.sh` 解包 linux_amd64 包，跑 `binlog-server version`，要求 `Version:` 等于 goreleaser 构建的版本（打 tag 时也等于去掉 `v` 的 tag）、`Commit:` 等于 `GITHUB_SHA`。`release.yml` 在发布前和发布后（发布会重新构建）各跑一次；`e2e.yml` 的 `release-archive` job 每个 PR 用 snapshot 构建跑同一个检查 | MET | — |
| J3 | 打 tag 之前，这个 commit 已经过了全部 e2e（包括多节点 job） | `release.yml` 的 `ci` job 调用整个 `e2e.yml`（`on: workflow_call`），`release` job `needs: [ci]`：tag 所在 commit 上每个按 PR 跑的 job（含 `e2e-cluster-roles`、`e2e-worker-crash-recovery`、`e2e-meta-failover`）都通过才构建和发布。`internal/lintguard/release_gate_test.go`（`go test ./...`）锁住这层依赖；`release_walk_test.go` 要求 release 路径上（release、它依赖的 job、`e2e.yml` 的每个 job 及其 step）没有 `continue-on-error`，`if` 只能是 `allowedIfs` 表里写明理由的条件 | MET | — |

## 计数

共 35 项：**MET 30 / PARTIAL 2 / NOT MET 3**。

## 最卡生产级的 5 项

1. **C5，cluster 故障注入矩阵**：`smoke-fault-matrix.sh` 已建并随 `e2e-fault-matrix` job 每个 PR 运行；7 格实跑、5 格 COVERED、其余 SKIP 写明缺的注入原语（PR-2/PR-4 未覆盖的事务窗口、分区、共享存储）。达到 MET 需要补：节点→meta 分区原语、Acquire/评估读/claim 事务窗口注入点、共享存储 runner、PR-5 依赖格（row9/14）与 refusal-cap 调参格（row8）。
2. ~~A1，链不变量 cluster 判定~~（已 MET：#462 的「cluster 删封存段没有 finding」有单测（ADR 0011 PR-3）；GTID 内部空洞的豁免链路 `smoke-gtid-hole` 在 quick 档、删封存段 `smoke-cluster-roles` 末格都进了 CI。所有权变化时 finding 单调（C6）已随 PR-5 落地：`OWNER_CHANGED` 关闭路径删除，`pr5_property_test.go` 三条属性跑在生产 `Reconcile` 上。#471 不合并）。
3. ~~F4，评估器默认关~~（已 MET：`health.evaluator` 开关删除，评估器恒运行，CI 跑的就是生产路径）。
4. **A4 仍是 PARTIAL，可选还原演练还没有**：standalone 的周期巡检在 CI 里（运行中的工人把缺本地文件和损坏对象报成 finding）。集群仍不巡检（A4，等 ADR 0011 的主人）。真实 MinIO 由 `e2e-verify` 每个 PR 跑。覆盖查询和还原计划已由 `e2e-coverage` 每个 PR 对账（D3 MET）；按保留点做的可选演练还没有。
5. **H1，上线前提没验**：约定负载下的长跑验收还没有（H1，ADR 0009 阶段 6）。G2 已由 `e2e-tls` 的 `TestLiveAppTLS`（真实任务路径的 dump 与 meta 池 `CONNECTION_TYPE`）、`TestLiveS3TLS`（私有 CA 的 HTTPS 上传）和 `TestLiveMySQLTLS`，以及 `unit-and-e2e` / `race-and-vet` 里的 TLS 单测盖住。
