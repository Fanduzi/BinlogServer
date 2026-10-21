# Binlog Server v0.5.77

Release date: 2026-10-21

v0.5.77 is a security and hardening release. It builds with Go 1.26.10 and `google.golang.org/grpc` v1.83.2, so GO-2026-6809 (`crypto/x509`) and GO-2026-6443 (grpc `http2Server.HandleStreams`) are gone in both source-mode and binary-mode govulncheck (#491, #495). It also ships ADR 0011 PR-2 (#492, from #484): cluster meta connections get server-side session timeouts, and a stalled metadata row lock opens CRITICAL `META_LOCK_STALLED` instead of hanging. Chain findings on non-shared storage are delegated to the task's last owner (#461), and the day-one traps found while dogfooding v0.5.76 are fixed (#472). No new migration (schema up to 10, `minRequiredSchemaVersion` still 6), and the upgrade floor stays v0.5.51. Two new optional config keys: `cluster.side_kill` and `health.node_gone_after_sec`.

## Upgrade Notes

- **Config values are validated more strictly at startup (#472).** Boolean keys accept only `true` and `false`; `yes`, `no`, `1` and anything else refuse to start, including environment overrides, with an error that names the key (#475). Non-numeric values for numeric keys refuse to start (#469). An unset `${BINLOG_SERVER_META_DSN}` placeholder in `meta_dsn` is now a startup error that names the variable (#464). Check your config with the new binary before the restart.
- **Cluster meta sessions are armed (#492).** In cluster mode every meta pool connection and reconnect sets `wait_timeout`, `net_read_timeout`, `net_write_timeout` and `innodb_lock_wait_timeout` (MariaDB also `idle_transaction_timeout`). The idle bound is `min(5s, lease_ttl/2)`, at least 1 s; with the defaults the lock wait is 4 s and each lock statement's client deadline is 6 s. If the server rejects a required `SET`, the cluster refuses to start. If a proxy (ProxySQL, MaxScale) swallows the `SET`, or the read-back fails, `/api/health` is degraded from startup but the process still starts. Standalone is not armed.
- **Meta account:** `GRANT ALL ON binlog_server_meta.*` is enough for stall detection; no `PROCESS` and no `performance_schema` grant are needed. Without the privileges to name the lock holder, the process logs one warning and `/api/health` shows `"lock_detail":"reduced"`. That is not a degraded reason.
- **New config key `cluster.side_kill`** (`off` or `auto`, default `off`). `auto` only for a direct meta connection, never behind a SQL-parsing proxy. The kill procedure is not installed by this release, so `auto` stays off in practice. Any other value refuses to start.
- **New config key `health.node_gone_after_sec`** (default `0` = 3 × sweep; negative refuses to start). It only matters with `health.evaluator: on` in a cluster on non-shared storage.
- **`GET /api/tasks` now returns `desired_run`** (#467). The v0.5.76 upgrade notes use this field to find tasks an older binary left `STOPPED` with desired `STOP`; v0.5.76 itself did not return it. On v0.5.77 that curl works as written, and the Console task list and detail show a Desired column.
- **Restore commands changed (#466, #474).** Generated restore commands and the restore docs use `set -o pipefail`, pass every file to one client, add `--verify-binlog-checksum` (both `mysqlbinlog` and `mariadb-binlog`) and `--skip-gtid-strict-mode` for `mariadb-binlog`. A corrupted file now makes the restore exit non-zero, and multi-file restores on MariaDB 11.4 no longer stop after the first file.
- Building from source requires Go 1.26.10+.
- No new migration. v0.5.76 runs on the same schema, so the binary can be rolled back. Before starting an older binary, remove `cluster.side_kill` and `health.node_gone_after_sec` if that binary rejects unknown keys. From v0.5.76, replace the binary; the v0.5.76 notes still apply when upgrading from v0.5.75 or earlier.

## Security

| ID | Component | Fix |
|---|---|---|
| GO-2026-6809 | Go standard library `crypto/x509` (go1.26.9) | Go 1.26.10 (#491) |
| GO-2026-6443 | `google.golang.org/grpc` v1.83.1 `http2Server.HandleStreams` | grpc v1.83.2 (#495, #493) |

GO-2026-6809 is reachable from `App.Run` through `http.Server.Serve`. GO-2026-6443 was not called in source mode, but scanners of the built binary or image (`govulncheck -mode=binary`, Trivy, Grype) reported it.

## Added

- **`META_LOCK_STALLED` (#492, ADR 0011 PR-2).** CRITICAL on task `_cluster`, subject the task id, written by the node that saw the wait. One lock wait is not a page: the finding and the `/api/health` 503 start on the second observation. A server 1205, a statement deadline that fires after it has run for at least `innodb_lock_wait_timeout`, or a pool checkout that times out while the pool is saturated behind a confirmed lock wait all count. A slow query or a busy pool without lock evidence does not. It closes when the same observer takes the lock twice within `lock_wait`; after the observer's heartbeat goes stale, the `_health` holder closes it the same way. The operator contract (RTO formula, same-node restart bound, session variables, grants) is in `docs/releases/adr-0011-pr2.md`.
- **Chain delegation to the last owner (#461, ADR 0009 §30).** On non-shared storage, chain findings (`chain.local`, `chain.objects`) of a task that is no longer running are kept by its last owner, bound to the last task lease epoch and node id (`task_health.chain_owner`, no migration). A delegated write from another node or an older epoch is fenced. When the task starts on another node, the old directory's chain findings close with `OWNER_CHANGED`. When the last owner's heartbeat is older than `health.node_gone_after_sec`, or it has no heartbeat row, health is UNKNOWN with detail `node gone` and chain findings stay open; a non-chain CRITICAL still makes it UNHEALTHY. Standalone is unchanged.

## Fixes

- **#464:** the shipped `config.example.yaml` did not load (a duplicate top-level `health` key). It loads again, starts in standalone mode with `meta_dsn` commented out, and every packaged example is loaded by the real loader in tests.
- **#465:** the Quick Start creates a mode-0600 standalone task key before the first start; `docs/security.md` (now in the release archive) covers generating, rotating and storing it.
- **#466, #474:** restore commands, as above.
- **#467:** `desired_run` in the task API and Console.
- **#468:** every key the loader reads is documented in `docs/guide/admin/configuration.md`, and the release version is pinned from `VERSION`.
- **#469, #475:** non-numeric and non-boolean config values are rejected at startup.
- The local release archive (`scripts/release-assets.sh`) now includes `scripts/adopt-leftovers.sh` and `docs/releases`, matching the GoReleaser archive.

## Known issues

- **#490 (P3):** `TestFailurePathMatrixMetaOutage` is flaky in `internal/tasks`, with and without `-race`, on loaded hosts. Test only.
- **#494 (P3):** `binlog_server_checkpoint_age_seconds` is emitted with an empty `task_id` label.
- **#487:** every `/metrics` scrape re-reads all sealed binlog files in full.
- **#488:** standalone without a meta DSN keeps uploaded files `LOCAL_ONLY` in the catalog.
- **#451, #452, #455 (P3)** and **#456 (P4)** from v0.5.76, and **#446, #437, #438**, are still open. `/api/findings` has no read timeout yet (P2-7).

## Changes since v0.5.76

`git log --oneline --first-parent v0.5.76..9ad91f9`, plus the commit of these notes, which v0.5.77 is tagged on:

| Commits | Change |
|---|---|
| `b4dd8a0`..`9250732` | PR #461: chain findings delegated to the last owner, `health.node_gone_after_sec` (ADR 0009 §30) |
| `ae670d3`..`5b90f75` | PR #472: day-one fixes from dogfooding v0.5.76 (#464–#469, #474, #475) |
| `8bf523e`..`1bbe012` | PR #477: ADR 0011 cluster ownership and failover state model (design) |
| `44e6458` | PR #479: production-readiness checklist (docs) |
| `52b89f1`..`71c7fbb` | early landing of the failover transition model, reverted |
| `6e903dc`..`4a97031` | PR #481: ADR 0011 PR-1, pure failover transition model and its tests (no runtime change) |
| `7e299b1`..`ea31aa5` | PR #482: ADR 0009 phase 3 verifier library (not wired into the worker yet) |
| `bab4161`, `ee2631c` | PR #483: verifier follow-ups from QA |
| `16187b6` | PR #486: C1 MET, ADR 0011 accepted (docs) |
| `46cc028` | PR #491: Go 1.26.10 (GO-2026-6809) |
| `af23493`..`853402f` | PR #492: ADR 0011 PR-2, cluster session timeouts and `META_LOCK_STALLED` (lands #484) |
| `9ad91f9` | PR #495: grpc v1.83.2 (GO-2026-6443, #493); `TestMigrateTo9FromEachVersion` follows the latest migration |

## QA summary

PR #484 QA PASS (BinlogServerQA), round 4 at `8e55aec7`, 2026-10-21: 45 of 47 mutants killed and the 2 that did not build have compiling forms that are killed; the elapsed-time classification shows live through a delay proxy on MySQL 8.0 and MariaDB 11.4; the deadline-margin check passed 8/8 per server under load. Round 3 under load (loadavg about 17 on 8 cores) gave 32/32 server 1205 classified as lock waits with 0 misses and no harmful false positive.

Tip QA 853402f PASS (BinlogServerQA), 2026-10-21, MySQL 8.0.36 and MariaDB 11.4.8: session timeouts are set on every pool connection and after a forced reconnect; a real `FOR UPDATE` stall opens `META_LOCK_STALLED` at about 37–39 s and clears about 2 s after release; without `PROCESS`, `lock_detail: reduced` and `side_kill` stays off; a LATEST task follows a rotation and seals the previous file; `-race` 17/17 packages; GO-2026-6809 absent in source and binary mode. That QA found #493, fixed in `9ad91f9`.

On `9ad91f9`, a go1.26.10 build: govulncheck v1.9.0 source and binary mode report no vulnerabilities affecting the code; a standalone start returns 200 on `/healthz`, `/api/health` and `/ui/`. CI passed on `9ad91f9`, including `vulncheck`.

## Chinese Release Notes

Chinese version:

# Binlog Server v0.5.77 中文发布说明

> 对应版本：`v0.5.77`

发布日期：2026-10-21

v0.5.77 是一次安全和加固版本。构建升级到 Go 1.26.10 和 `google.golang.org/grpc` v1.83.2，govulncheck 的源码模式和二进制模式都不再报 GO-2026-6809（`crypto/x509`）和 GO-2026-6443（grpc `http2Server.HandleStreams`）（#491、#495）。同时带上 ADR 0011 PR-2（#492，来自 #484）：集群的 meta 连接设置服务端会话超时，元数据行锁卡住时打开 CRITICAL `META_LOCK_STALLED`，不再一直挂着。非共享存储上的链 finding 委派给任务的最后一任 owner（#461），并修掉了 v0.5.76 试用时发现的上手问题（#472）。没有新迁移（schema 最高 10，`minRequiredSchemaVersion` 仍为 6），升级地板仍是 v0.5.51。新增两个可选配置项：`cluster.side_kill` 和 `health.node_gone_after_sec`。

## 升级须知

- **启动时配置校验更严格（#472）。** 布尔项只接受 `true` 和 `false`；`yes`、`no`、`1` 和其他值都拒绝启动，环境变量覆盖也一样，错误里写明是哪一项（#475）。数值项填了非数字会拒绝启动（#469）。`meta_dsn` 里的 `${BINLOG_SERVER_META_DSN}` 没有设置时，启动报错并写明变量名（#464）。重启前请先用新二进制检查配置。
- **集群 meta 会话会被武装（#492）。** 集群模式下每条 meta 池连接和每次重连都设置 `wait_timeout`、`net_read_timeout`、`net_write_timeout` 和 `innodb_lock_wait_timeout`（MariaDB 还有 `idle_transaction_timeout`）。空闲上界是 `min(5s, lease_ttl/2)`，至少 1 秒；缺省时锁等待 4 秒，每条拿锁语句的客户端期限 6 秒。服务器拒绝必需的 `SET` 时集群拒绝启动。代理（ProxySQL、MaxScale）吞掉 `SET`，或回读失败时，`/api/health` 从启动起就是 degraded，但进程照常启动。单机不武装。
- **meta 账号：** 卡锁检测只需要 `GRANT ALL ON binlog_server_meta.*`，不需要 `PROCESS`，也不需要 `performance_schema` 授权。没有点名持有者的权限时，进程打一条警告，`/api/health` 带 `"lock_detail":"reduced"`，这不算 degraded。
- **新配置项 `cluster.side_kill`**（`off` 或 `auto`，缺省 `off`）。`auto` 只用于到 meta 的直连，会解析 SQL 的代理后面不要开。本版本不安装 kill 过程，所以 `auto` 实际仍是关闭的。其他值拒绝启动。
- **新配置项 `health.node_gone_after_sec`**（缺省 `0` = 3 倍 sweep；负数拒绝启动）。只在集群、非共享存储、`health.evaluator: on` 时有意义。
- **`GET /api/tasks` 现在返回 `desired_run`**（#467）。v0.5.76 的升级须知用这个字段找旧二进制留下的 `STOPPED`、desired `STOP` 任务，但 v0.5.76 本身不返回它。v0.5.77 上那条 curl 可以照原样用，Console 任务列表和详情也有 Desired 列。
- **恢复命令有变化（#466、#474）。** 生成的恢复命令和恢复文档使用 `set -o pipefail`，把所有文件交给同一个客户端，加 `--verify-binlog-checksum`（`mysqlbinlog` 和 `mariadb-binlog` 都加），`mariadb-binlog` 另加 `--skip-gtid-strict-mode`。文件损坏时恢复以非零退出；MariaDB 11.4 上的多文件恢复不再在第一个文件后停下。
- 从源码构建需要 Go 1.26.10+。
- 没有新迁移。v0.5.76 用同一个 schema，可以回滚二进制。启动旧二进制前，如果它拒绝未知配置项，先删掉 `cluster.side_kill` 和 `health.node_gone_after_sec`。从 v0.5.76 升级直接替换二进制；从 v0.5.75 或更早升级时 v0.5.76 的说明仍然适用。

## 安全

| 编号 | 组件 | 修复 |
|---|---|---|
| GO-2026-6809 | Go 标准库 `crypto/x509`（go1.26.9） | Go 1.26.10（#491） |
| GO-2026-6443 | `google.golang.org/grpc` v1.83.1 `http2Server.HandleStreams` | grpc v1.83.2（#495、#493） |

GO-2026-6809 可以从 `App.Run` 经 `http.Server.Serve` 调到。GO-2026-6443 在源码模式下判定为未调用，但扫描构建出的二进制或镜像（`govulncheck -mode=binary`、Trivy、Grype）会报出来。

## 新增

- **`META_LOCK_STALLED`（#492，ADR 0011 PR-2）。** CRITICAL，写在任务 `_cluster` 上，subject 是任务 id，由看到等待的节点写入。一次锁等待不算告警：第二次观察到时才打开 finding 并让 `/api/health` 返回 503。服务器 1205、语句自己的期限在已经跑满 `innodb_lock_wait_timeout` 之后才到、池已满且有确认的锁等待时取连接超时，都算。慢查询、没有锁证据的繁忙池不算。同一观察者在 `lock_wait` 内连续两次拿到锁时关闭；观察者心跳过期后，由 `_health` 持有者用同样的方式关闭。运维约定（RTO 公式、同节点重启上界、会话变量、授权）见 `docs/releases/adr-0011-pr2.zh-CN.md`。
- **链 finding 委派给最后一任 owner（#461，ADR 0009 §30）。** 非共享存储上，不再运行的任务的链 finding（`chain.local`、`chain.objects`）由它最后一任 owner 继续维护，绑定最后一次任务 lease epoch 和节点 id（`task_health.chain_owner`，不迁移）。别的节点或旧 epoch 的委派写被栅栏拒绝。任务在另一个节点起来后，旧目录的链 finding 以 `OWNER_CHANGED` 关闭。最后一任 owner 的心跳早于 `health.node_gone_after_sec`，或者没有心跳行时，健康为 UNKNOWN，detail `node gone`，链 finding 保持打开；另有非链 CRITICAL 时仍是 UNHEALTHY。单机不变。

## 修复

- **#464：** 随包的 `config.example.yaml` 加载失败（顶层有两个 `health`）。现在能加载，`meta_dsn` 注释掉，以单机模式启动；每个随包示例都在测试里用真实加载器加载。
- **#465：** Quick Start 在第一次启动前生成 0600 的单机任务密钥；`docs/security.md`（现在随发布包提供）说明如何生成、轮换和保存。
- **#466、#474：** 恢复命令，见上。
- **#467：** 任务 API 和 Console 显示 `desired_run`。
- **#468：** 加载器读取的每个配置项都写进了 `docs/guide/admin/configuration.md`，发布版本号由 `VERSION` 固定。
- **#469、#475：** 启动时拒绝非数字和非布尔的配置值。
- 本地发布包（`scripts/release-assets.sh`）现在包含 `scripts/adopt-leftovers.sh` 和 `docs/releases`，与 GoReleaser 的发布包一致。

## 已知问题

- **#490（P3）：** `internal/tasks` 的 `TestFailurePathMatrixMetaOutage` 在高负载机器上不稳定，加不加 `-race` 都会出现。只影响测试。
- **#494（P3）：** `binlog_server_checkpoint_age_seconds` 的 `task_id` label 为空。
- **#487：** 每次抓取 `/metrics` 都会把所有已封存的 binlog 文件重新整个读一遍。
- **#488：** 单机不配 meta DSN 时，上传成功的文件在目录里仍是 `LOCAL_ONLY`。
- v0.5.76 起的 **#451、#452、#455（P3）** 和 **#456（P4）**，以及 **#446、#437、#438** 仍未关闭。`/api/findings` 还没有读超时（P2-7）。

## 自 v0.5.76 以来的变更

| 提交 | 变更 |
|---|---|
| `b4dd8a0`..`9250732` | PR #461：链 finding 委派给最后一任 owner，`health.node_gone_after_sec`（ADR 0009 §30） |
| `ae670d3`..`5b90f75` | PR #472：v0.5.76 试用中发现的上手问题（#464–#469、#474、#475） |
| `8bf523e`..`1bbe012` | PR #477：ADR 0011 集群归属与故障切换状态模型（设计） |
| `44e6458` | PR #479：生产就绪清单（文档） |
| `52b89f1`..`71c7fbb` | 故障切换转移模型的提前落地，已回退 |
| `6e903dc`..`4a97031` | PR #481：ADR 0011 PR-1，纯故障切换转移模型及其测试（运行时不变） |
| `7e299b1`..`ea31aa5` | PR #482：ADR 0009 第 3 阶段校验器库（还没接入 worker） |
| `bab4161`、`ee2631c` | PR #483：QA 之后的校验器跟进 |
| `16187b6` | PR #486：C1 达标，ADR 0011 通过（文档） |
| `46cc028` | PR #491：Go 1.26.10（GO-2026-6809） |
| `af23493`..`853402f` | PR #492：ADR 0011 PR-2，集群会话超时与 `META_LOCK_STALLED`（落地 #484） |
| `9ad91f9` | PR #495：grpc v1.83.2（GO-2026-6443，#493）；`TestMigrateTo9FromEachVersion` 跟随最新迁移 |

## QA 摘要

PR #484 QA PASS（BinlogServerQA），第 4 轮 `8e55aec7`，2026-10-21：47 个变异中 45 个被打死，另外 2 个编译不过，它们能编译的形式也被打死；经延迟代理在 MySQL 8.0 和 MariaDB 11.4 上实测到按已过去时间分类；高负载下 deadline 余量检查每个服务器 8/8。第 3 轮在高负载下（8 核 loadavg 约 17）服务器 1205 32/32 都分类为锁等待，0 漏报，没有有害误报。

Tip QA 853402f PASS（BinlogServerQA），2026-10-21，MySQL 8.0.36 和 MariaDB 11.4.8：每条池连接和强制重连后都设置了会话超时；真实的 `FOR UPDATE` 卡锁约 37–39 秒打开 `META_LOCK_STALLED`，释放后约 2 秒关闭；没有 `PROCESS` 时 `lock_detail: reduced`，`side_kill` 保持关闭；LATEST 任务跟上轮换并封存上一个文件；`-race` 17/17 个包通过；源码和二进制模式都没有 GO-2026-6809。这一轮 QA 发现了 #493，已在 `9ad91f9` 修复。

在 `9ad91f9` 上用 go1.26.10 构建：govulncheck v1.9.0 源码和二进制模式都没有影响代码的漏洞；单机启动后 `/healthz`、`/api/health`、`/ui/` 都返回 200。CI 在 `9ad91f9` 上通过，含 `vulncheck`。
