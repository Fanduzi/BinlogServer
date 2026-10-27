# Binlog Server v0.5.82

Release date: 2026-10-27

v0.5.82 ships health metrics, recommended alert rules and a generic webhook (#545, ADR 0013 `docs/adr/0013-health-alerts.md`). `/metrics` now exports `binlog_server_task_healthy{task_id,reason}` for every task this node evaluates, plus a sentinel series when there is none, and three SLO gauges: progress age, upload lag and open CRITICAL findings. The repository ships the recommended rules in `deploy/prometheus/binlog-server-alerts.yml`, tested by `promtool test rules` in a new `promtool` CI job on every PR. A new optional `notify.*` block posts one JSON batch on health changes and CRITICAL finding transitions. `health.evaluator` still defaults to `off` (F4), so per-task series and the webhook only appear once you set it to `on`. No new migration (schema up to 10, `minRequiredSchemaVersion` still 6), and the upgrade floor stays v0.5.51. Production-readiness F2 is now MET; the checklist stands at MET 21 / PARTIAL 6 / NOT MET 8.

## Upgrade Notes

- **No migration, new optional `notify.*` keys.** The highest schema is still 10 and `minRequiredSchemaVersion` is still 6. From v0.5.81, replace the binary. With no `notify:` block nothing is posted. The v0.5.81 and v0.5.80 notes still apply when upgrading from older releases, including the `upload.use_ssl` default change in v0.5.80.
- **With the default `health.evaluator: off`, `/metrics` gains one series.** That is the sentinel `binlog_server_task_healthy{task_id="",reason="ok"} 1`; there are no per-task series and no webhook posts. The sentinel keeps `binlog_server_task_healthy == 0` quiet. Do not alert on `absent()`: every node that has not turned the evaluator on would page. Process death is `up == 0`.
- **To page on task health, set `health.evaluator: on` and load `deploy/prometheus/binlog-server-alerts.yml`.** Scrape every node: a task is exported only by the node that evaluates it (the lease owner, the delegated chain owner, or the node holding `_health`), so two scrapes never count it twice. During a lease handoff a task's series can be missing for about the lease TTL (Tip QA saw about 10 s); the `for: 2m` on `BinlogTaskUnhealthy` covers it. `BinlogProcessDown` uses `up{job="binlog-server"}`, so match the label to your Prometheus `job_name`. The file also carries the nine rules that used to be inline in `docs/guide/admin/observability.md` (replication lag, source switchover, checkpoint, FAILED, workers, uploads) with the same thresholds; that page now points at the file.
- **Cluster nodes with `cluster.role: worker` expose no `/metrics` (#549).** They evaluate the tasks they own, but Prometheus cannot scrape those series. Until #549 is fixed, per-task health alerting in a control-plane plus worker topology needs `all-in-one` nodes.
- **Webhook (`notify.*`).** `notify.webhook_url` (`http` or `https` with a host; empty is off), `notify.webhook_token` (optional, sent as `X-BinlogServer-Token`), `notify.timeout_sec` (default 5, 1–60), `notify.attempts` (default 3, 1–8), `notify.backoff_ms` (default 200, 1–30000, doubled per retry). An out-of-range value or a bad URL is a startup error. Environment variables follow the existing rule (`BINLOG_SERVER_NOTIFY_WEBHOOK_URL`, `BINLOG_SERVER_NOTIFY_WEBHOOK_TOKEN`, ...). A plaintext `notify.webhook_token` in the config file gets the same startup warning as other secrets. The example configs do not show the block yet (#551); see `docs/guide/admin/configuration.md` §3.11.2.
- **What the webhook sends.** Only with `health.evaluator: on` (`shadow` records samples but does not post). The first HEALTHY seen for a task is a baseline and is not posted; after that a health state change, or a CRITICAL finding `OPEN`, `CLOSE` or `ESCALATE`, posts one batch. A steady state is not posted again. Transport errors, 5xx and 429 are retried with the same `X-BinlogServer-Delivery` (SHA-256 of the body); other 4xx are not retried. Redirects are not followed, so the token never reaches another host. A batch that fails every attempt stays in process memory and is sent with the next one. A restart drops that pending batch and can send one notification again.
- **Rollback.** v0.5.81 runs on the same schema and ignores the `notify.*` keys, so the binary can be rolled back. You lose the health metrics and the webhook; the rules in `binlog-server-alerts.yml` that use the new series stop firing.

## Added

- **ADR 0013: health metrics, recommended alerts and a generic webhook (#545).**
  - `binlog_server_task_healthy{task_id,reason}` (gauge) comes from the existing `HealthOf` result of the same pass; it does not evaluate health again and does not read `health.evaluator`. 1 only for HEALTHY with `reason="ok"`; 0 with `reason` in the closed set `critical`, `warning`, `unknown`, `unevaluated` or `unread`. One series per task. A task this node owns but has not evaluated yet is 0 with `unevaluated`.
  - `binlog_server_task_progress_age_seconds`: for desired RUN, seconds since the last source byte or local advance; 0 otherwise. `binlog_server_task_upload_lag_seconds`: age of the oldest sealed segment that is not `UPLOADED` with a matching checksum, inside the grace period too; 0 without object storage. `binlog_server_task_open_critical`: open CRITICAL findings after the pass. `binlog_server_task_last_verified_timestamp` is the existing verifier metric.
  - `deploy/prometheus/binlog-server-alerts.yml`: `BinlogTaskUnhealthy` (`== 0` for 2m), `BinlogTaskProgressAge` (> 45 s for 2m), `BinlogTaskUploadLag` (> 900 s for 5m), `BinlogTaskOpenCritical`, `BinlogFindingCritical`, `BinlogVerifyStale` (verified before and older than 48 h) and `BinlogProcessDown`, plus the nine earlier rules. `deploy/prometheus/binlog-server-alerts.test.yml` has promtool cases for all sixteen.
  - `internal/notify`: one JSON POST, body `{"version":1,"events":[...]}`. Health events carry `status` and `previous`; finding events carry `kind`, `severity`, `op`, `subject` and `detail` (at most 1024 characters).
  - Out of scope: Alertmanager formats, silencing, a `_notify` lease and the unused notification outbox from migration 000009.

## Fixes

- **META_UNREACHABLE streak (#545):** a findings read that started during a meta outage was recorded when its goroutine finished, so it could land after the first good answer and reopen the streak (OPEN then UPDATE instead of OPEN then CLOSE). A failure dated at or before the latest answer no longer reopens the streak, and a read applies its observation before waiters are released.

## CI

- New `promtool` job on every PR and push to main: installs promtool 2.55.1 and runs `promtool test rules deploy/prometheus/binlog-server-alerts.test.yml`.
- `scripts/e2e/run-server.sh` retries `go build` three times, so a transient module fetch error no longer fails `e2e-observability`.

## Known issues

- **#549:** `cluster.role: worker` serves no `/metrics`, so F2 series from those nodes cannot be scraped. Use `all-in-one` nodes for per-task health alerting until it is fixed.
- **#550 (P3):** test gaps from the #545 QA: a 3xx counted as delivered, clearing pending only on a transport error, batch order and a stable delivery id are not asserted; the nine earlier promtool cases are firing-only. The behaviour is correct today; these are missing guards, not known bugs.
- **#551 (P3):** `config.example.yaml` and `config.production.example.yaml` have no `notify:` block although the configuration guide documents it.
- **F4:** `health.evaluator` still defaults to `off`, so without `on` there are no per-task series and no webhook.
- **#541 (P3):** test gaps from the #536 QA (upload TLS classifier mutations). The behaviour is correct today; these are missing guards, not known bugs.
- **#540 (P3):** `TestStopStartUnderPollKeepsOpenEpoch` (`internal/replication`) fails now and then under heavy `-race` load. Test-only.
- **#386 (P3):** `internal/tasks` tests can be flaky under fault load. No test is named; it stays open.
- **#510:** a non-owner node can keep listing a closed `SEGMENT_MISSING` in `/api/health` and `/api/findings` (and return 503). Check the owner's `/api/health` before acting on a non-owner's 503.
- **#473 (P1):** with per-node storage, a takeover after the owner pauses past the TTL hits `SEGMENT_NOT_ON_WORKER` and writes FAILED / desired STOP. ADR 0011 PR-4 is in progress.
- **#462 (P1):** the cluster silent gap is covered by unit tests; full closure comes with ADR 0011 PR-5.
- **#513 (P2):** after a takeover, a resumed old owner can write FAILED onto the new owner's task row; backup stops until an operator Start.
- **#539:** task start/stop opens more connections to the source than needed.
- **#514**, **#512**, **#494**, **#500**, **#502**, **#503** (P3) are still open, as are **#487**, **#488**, **#451**, **#452**, **#455**, **#456**, **#446**, **#437** and **#438**. `/api/findings` has no read timeout yet (P2-7). The verifier does not run in cluster mode (A4 PARTIAL).

## Changes since v0.5.81

`git log --oneline v0.5.81..c7b649b` (6 commits), plus the commit of these notes, which v0.5.82 is tagged on:

| Commits | Change |
|---|---|
| `1e99e7d` | Install docs point at v0.5.81 |
| `e792a35`..`c7b649b` | PR #545: ADR 0013, `binlog_server_task_healthy` and SLO gauges, recommended alert rules tested by promtool in CI, generic JSON webhook; META_UNREACHABLE late-read fix; e2e build retry |

The server binary changes in `/metrics` (the new series), the new `notify.*` config and webhook, and the META_UNREACHABLE streak. The other #545 changes are the alert rules and their tests, CI and docs.

## QA summary

**PR #545 QA PASS (BinlogServerQA), round 2 at `c7b649b6`, 2026-10-27.** CI 18/18 on the head.
- Round 1 at `40b83d43` held live (MinIO, source and meta SIGSTOP drove the metrics and the webhook; 0 duplicate series across a lease handoff; token absent from `/metrics`, `/api/health` and logs) but failed on two unguarded claims: removing the redirect refusal and clearing the pending batch on a failed POST both passed the tests.
- Round 2 adds `TestWebhookDoesNotForwardTokenOnRedirect` and `TestWebhookKeepsUndeliveredAcrossFailedPost`; both mutants and their variants are killed. Every threshold, expression and `for` mutant of the nine earlier promtool rules goes red. The remaining survivors are filed as #550.

**Tip QA of `c7b649b6` PASS (BinlogServerQA), 2026-10-27.** Push CI 18/18.
- Plain and `-race` build and test: 0 failures. F2 metrics and the evaluator-off sentinel as documented; cluster all-in-one failover with 0 duplicate series and about 10 s gap.
- `promtool check rules` (16 rules) and `promtool test rules` SUCCESS; a live Prometheus took a shortened `BinlogTaskProgressAge` pending → firing → resolved on a source SIGSTOP.
- Webhook: no post for the HEALTHY baseline, the same delivery id retried under 500, a 302 not followed (0 hits on the target), the restart loss as documented.
- LATEST / FILE_POS / GTID tasks, PRODUCTION fail-closed, TLS upload with the default `use_ssl` (checksums match), the plain-HTTP hint, `SIGTERM` in 0.1 s with idle keep-alives, and a 6.5-minute soak with sealed segments `UPLOADED` and matched.

## Chinese Release Notes

Chinese version:

# Binlog Server v0.5.82 中文发布说明

> 对应版本：`v0.5.82`

发布日期：2026-10-27

v0.5.82 带来健康指标、推荐告警规则和一个通用 webhook（#545，ADR 0013 `docs/adr/0013-health-alerts.md`）。`/metrics` 现在对本节点评估的每个任务输出 `binlog_server_task_healthy{task_id,reason}`，没有任务时输出一条哨兵序列，另有三个 SLO gauge：progress age、upload lag、open CRITICAL finding 数。仓库随附推荐规则 `deploy/prometheus/binlog-server-alerts.yml`，新的 `promtool` CI job 每个 PR 跑 `promtool test rules`。新增可选的 `notify.*` 配置，在健康状态变化和 CRITICAL finding 转移时 POST 一批 JSON。`health.evaluator` 默认仍是 `off`（F4），所以要设成 `on` 才会有每任务序列和 webhook。没有新迁移（schema 最高 10，`minRequiredSchemaVersion` 仍为 6），升级地板仍是 v0.5.51。生产就绪 F2 变为 MET，清单为 MET 21 / PARTIAL 6 / NOT MET 8。

## 升级须知

- **没有迁移，新增可选的 `notify.*`。** schema 最高仍为 10，`minRequiredSchemaVersion` 仍为 6。从 v0.5.81 升级只需替换二进制。不写 `notify:` 就不发送。从更早版本升级，v0.5.81 和 v0.5.80 的说明仍然适用，包括 v0.5.80 里 `upload.use_ssl` 默认值的变化。
- **默认 `health.evaluator: off` 时，`/metrics` 只多一条序列。** 就是哨兵 `binlog_server_task_healthy{task_id="",reason="ok"} 1`；没有每任务序列，也不发 webhook。哨兵让 `binlog_server_task_healthy == 0` 不会误响。不要用 `absent()` 告警：每一台还没开评估器的节点都会分页。进程挂了看 `up == 0`。
- **要按任务健康分页，把 `health.evaluator` 设为 `on`，并加载 `deploy/prometheus/binlog-server-alerts.yml`。** 抓取每一个节点：一个任务只由正在评估它的节点输出（lease 主人、委派的链主人，或持有 `_health` 的节点），两次抓取不会把同一个任务算两次。lease 交接期间，任务的序列可能缺大约一个 lease TTL（Tip QA 测到约 10 秒），`BinlogTaskUnhealthy` 的 `for: 2m` 能盖住。`BinlogProcessDown` 用 `up{job="binlog-server"}`，标签要和 Prometheus 的 `job_name` 一致。文件里还有原先写在 `docs/guide/admin/observability.md` 里的 9 条规则（复制延迟、源切换、checkpoint、FAILED、worker、上传），阈值不变；那一页现在指向这个文件。
- **`cluster.role: worker` 的节点没有 `/metrics`（#549）。** 它们评估自己负责的任务，但 Prometheus 抓不到这些序列。#549 修好之前，控制面加 worker 的拓扑里要按任务告警，节点需用 `all-in-one`。
- **Webhook（`notify.*`）。** `notify.webhook_url`（带主机的 `http` 或 `https`，空就是关）、`notify.webhook_token`（可选，放在 `X-BinlogServer-Token`）、`notify.timeout_sec`（默认 5，1–60）、`notify.attempts`（默认 3，1–8）、`notify.backoff_ms`（默认 200，1–30000，每次重试翻倍）。取值越界或 URL 不合法时启动失败。环境变量沿用现有规则（`BINLOG_SERVER_NOTIFY_WEBHOOK_URL`、`BINLOG_SERVER_NOTIFY_WEBHOOK_TOKEN` 等）。配置文件里明文写 `notify.webhook_token` 会和其它密钥一样在启动时警告。示例配置里还没有这一段（#551），见 `docs/guide/admin/configuration.md` §3.11.2。
- **webhook 发什么。** 只在 `health.evaluator: on` 时发（`shadow` 记样本但不发）。任务第一次被看到是 HEALTHY 只记基线，不发；之后健康状态变化，或 CRITICAL finding 的 `OPEN`、`CLOSE`、`ESCALATE`，发一批。稳态不重发。传输错误、5xx、429 用同一个 `X-BinlogServer-Delivery`（正文 SHA-256）重试；其它 4xx 不重试。不跟随重定向，令牌不会被送到别的主机。所有尝试都失败的一批留在进程内存里，随下一批一起送。进程重启会丢掉这批待发内容，也可能再通知一次。
- **回滚。** v0.5.81 用同一个 schema，并忽略 `notify.*`，可以回滚二进制。回滚后没有健康指标和 webhook，`binlog-server-alerts.yml` 里依赖新序列的规则不再触发。

## 新增

- **ADR 0013：健康指标、推荐告警和通用 webhook（#545）。**
  - `binlog_server_task_healthy{task_id,reason}`（gauge）取自同一轮已有的 `HealthOf` 结论，不再算一遍健康，也不读 `health.evaluator`。只有 HEALTHY 是 1，`reason="ok"`；0 的 `reason` 是封闭枚举 `critical`、`warning`、`unknown`、`unevaluated`、`unread` 之一。每个任务一条序列。本节点负责、但还没评估过的任务是 0，`reason="unevaluated"`。
  - `binlog_server_task_progress_age_seconds`：desired RUN 时距最后一次源字节或本地推进的秒数，其它为 0。`binlog_server_task_upload_lag_seconds`：最老一条已封存、但还不是 `UPLOADED` 且校验和 match 的段的年龄，宽限期内也算；没开对象存储为 0。`binlog_server_task_open_critical`：这一轮之后仍开着的 CRITICAL finding 数。`binlog_server_task_last_verified_timestamp` 沿用已有巡检指标。
  - `deploy/prometheus/binlog-server-alerts.yml`：`BinlogTaskUnhealthy`（`== 0` 持续 2m）、`BinlogTaskProgressAge`（> 45 秒持续 2m）、`BinlogTaskUploadLag`（> 900 秒持续 5m）、`BinlogTaskOpenCritical`、`BinlogFindingCritical`、`BinlogVerifyStale`（巡检过且距今超过 48 小时）、`BinlogProcessDown`，加上原有 9 条。`deploy/prometheus/binlog-server-alerts.test.yml` 给 16 条都写了 promtool 用例。
  - `internal/notify`：一次 JSON POST，正文 `{"version":1,"events":[...]}`。健康事件带 `status` 和 `previous`；finding 事件带 `kind`、`severity`、`op`、`subject`、`detail`（最长 1024 字符）。
  - 不做：Alertmanager 格式、静音、`_notify` 租约，以及迁移 000009 里那张未使用的通知发件箱。

## 修复

- **META_UNREACHABLE 连续计数（#545）：** meta 中断期间开始的 findings 读取，要等 goroutine 结束才记下结果，可能落在第一次成功应答之后，把连续计数重新打开（OPEN 之后变成 UPDATE，而不是 OPEN 之后 CLOSE）。现在时间不晚于最近一次应答的失败不再重开计数，读取在放行等待者之前先记下观察结果。

## CI

- 新增 `promtool` job，每个 PR 和 push main 都跑：装 promtool 2.55.1，执行 `promtool test rules deploy/prometheus/binlog-server-alerts.test.yml`。
- `scripts/e2e/run-server.sh` 的 `go build` 最多重试三次，模块下载的偶发错误不再让 `e2e-observability` 失败。

## 已知问题

- **#549：** `cluster.role: worker` 不提供 `/metrics`，这些节点上的 F2 序列抓不到。修好之前，按任务健康告警请用 `all-in-one` 节点。
- **#550（P3）：** #545 QA 留下的测试缺口：3xx 被当成已送达、只在传输错误时清待发、批内顺序和投递 id 不变都没有断言；原有 9 条 promtool 用例只测触发。当前行为是对的，缺的是守护测试，不是已知缺陷。
- **#551（P3）：** `config.example.yaml` 和 `config.production.example.yaml` 没有 `notify:` 段，配置文档里有。
- **F4：** `health.evaluator` 默认仍是 `off`，不设 `on` 就没有每任务序列和 webhook。
- **#541（P3）：** #536 QA 留下的测试缺口（上传 TLS 分类的变异）。当前行为是对的，缺的是守护测试，不是已知缺陷。
- **#540（P3）：** `TestStopStartUnderPollKeepsOpenEpoch`（`internal/replication`）在高负载 `-race` 下偶发失败。只影响测试。
- **#386（P3）：** `internal/tasks` 的测试在故障负载下可能不稳定，没有点名测试，仍开着。
- **#510：** 非 owner 节点可能在 `SEGMENT_MISSING` 关闭后，仍在 `/api/health` 和 `/api/findings` 里列着它（并返回 503）。根据非 owner 的 503 行动之前，先看 owner 的 `/api/health`。
- **#473（P1）：** 每节点独立存储时，owner 暂停超过 TTL 后接管会遇到 `SEGMENT_NOT_ON_WORKER` 并写 FAILED / desired STOP。ADR 0011 PR-4 正在做。
- **#462（P1）：** 集群下的静默缺口有单测覆盖；完整关闭要等 ADR 0011 PR-5。
- **#513（P2）：** 接管后，恢复运行的旧 owner 可能把 FAILED 写到新 owner 的任务行上，备份停到运维 Start。
- **#539：** 任务 start/stop 对源库的连接扇出偏大。
- **#514**、**#512**、**#494**、**#500**、**#502**、**#503**（P3）仍未关闭，**#487**、**#488**、**#451**、**#452**、**#455**、**#456**、**#446**、**#437**、**#438** 也是。`/api/findings` 还没有读超时（P2-7）。集群模式不跑巡检（A4 PARTIAL）。

## 自 v0.5.81 以来的变更

`git log --oneline v0.5.81..c7b649b`（6 个提交），加上这份说明所在的提交（v0.5.82 打在这个提交上）：

| 提交 | 变更 |
|---|---|
| `1e99e7d` | 安装文档指向 v0.5.81 |
| `e792a35`..`c7b649b` | PR #545：ADR 0013，`binlog_server_task_healthy` 和 SLO gauge，CI 里用 promtool 测的推荐告警规则，通用 JSON webhook；META_UNREACHABLE 迟到读取的修复；e2e 构建重试 |

服务端二进制的变化在 `/metrics`（新序列）、新的 `notify.*` 配置和 webhook，以及 META_UNREACHABLE 的连续计数。#545 的其余改动是告警规则及其测试、CI 和文档。

## QA 摘要

**PR #545 QA PASS（BinlogServerQA），第 2 轮 `c7b649b6`，2026-10-27。** 头上 CI 18/18。
- 第 1 轮 `40b83d43` 的运行时行为成立（MinIO、源库、meta 的 SIGSTOP 都驱动了指标和 webhook；lease 交接时 0 条重复序列；`/metrics`、`/api/health` 和日志里没有令牌），但两项声明没有测试守护：去掉重定向拒绝、POST 失败时清掉待发批，测试都照样通过，判 FAIL。
- 第 2 轮加了 `TestWebhookDoesNotForwardTokenOnRedirect` 和 `TestWebhookKeepsUndeliveredAcrossFailedPost`，这两个变异及其变体都被杀掉。原有 9 条 promtool 规则的阈值、表达式、`for` 变异全部变红。剩下存活的变异记在 #550。

**main `c7b649b6` 的 Tip QA PASS（BinlogServerQA），2026-10-27。** push CI 18/18。
- 普通和 `-race` 构建与测试：0 失败。F2 指标和评估器 off 时的哨兵与文档一致；集群 all-in-one 故障切换 0 条重复序列，缺口约 10 秒。
- `promtool check rules`（16 条）和 `promtool test rules` SUCCESS；真实 Prometheus 上缩短阈值的 `BinlogTaskProgressAge` 在源库 SIGSTOP 时走完 pending → firing → resolved。
- Webhook：HEALTHY 基线不发，500 时用同一个投递 id 重试，302 不跟随（目标 0 次命中），重启丢失与文档一致。
- LATEST / FILE_POS / GTID 任务、PRODUCTION fail-closed、默认 `use_ssl` 的 TLS 上传（校验和一致）、明文 HTTP 提示、开着空闲 keep-alive 时 `SIGTERM` 0.1 秒退出，以及 6.5 分钟 soak，封存段 `UPLOADED` 且 match。
