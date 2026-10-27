# Binlog Server v0.5.83

Release date: 2026-10-27

v0.5.83 adds MySQL 8.4 LTS as a supported source (#555) and proves in-place upgrade on every PR (#557). MySQL 8.4 removed `SHOW MASTER STATUS`, so v0.5.82 could not read the source position there: a LATEST start and the idle at-tip check failed with `ERROR 1064`. BinlogServer now chooses the binary-log status statement from `VERSION()`: `SHOW BINARY LOG STATUS` on MySQL 8.4 and newer, `SHOW MASTER STATUS` on older MySQL and on MariaDB. `mysql:8.4` is in the e2e matrix, and 5.7 and 8.4 backups are restored and compared by `CHECKSUM TABLE` in CI. `e2e-upgrade-failback` now runs four legs on every PR, including a plain in-place upgrade from v0.5.82. It also fixes #437: after an online `migrate up` to 000010, a row already removed by retention gets `retention_removed_at` on the next pass without a second `RETENTION_REMOVED` event. No new migration (schema up to 10, `minRequiredSchemaVersion` still 6), no new config key, and the upgrade floor stays v0.5.51. Production-readiness D4 and E2 are now MET; the checklist stands at MET 23 / PARTIAL 4 / NOT MET 8.

## Upgrade Notes

- **No migration, no new config keys.** The highest schema is still 10 and `minRequiredSchemaVersion` is still 6. From v0.5.82, replace the binary. The v0.5.82, v0.5.81 and v0.5.80 notes still apply when upgrading from older releases, including the `upload.use_ssl` default change in v0.5.80.
- **Supported sources are MySQL 5.7, 8.0 and 8.4 LTS, and MariaDB 10.3+ (11.4 is restored in CI).** On a MySQL 8.4 source, LATEST tasks now start and the at-tip check reads the source position. Nothing needs to change in task definitions. The source still needs `REPLICATION CLIENT` for the status statement.
- **Status statement choice.** The statement comes from `VERSION()`. If it fails, the other known names are tried, including MariaDB's `SHOW BINLOG STATUS`. The statement that returned a row is remembered per source `host:port`; a remembered statement that starts failing is dropped and the list is tried again.
- **Retention marker after an online `migrate up` (#437).** A process that wrote `RETENTION_REMOVED` while migration 000010 was not applied now stores `retention_removed_at` on the next pass after `migrate up`, without a restart and without a second event. A restart while a marker is still pending can still write one more event, as design 0009 accepts.
- **Rollback.** v0.5.82 runs on the same schema and config, so the binary can be rolled back. Tip QA upgraded from the v0.5.82 release tarball, rolled back to it and returned to this build with sealed files byte-identical and no transaction gap or duplicate. On a MySQL 8.4 source, v0.5.82 cannot start LATEST tasks or read the source position.

## Added

- **MySQL 8.4 LTS support (#555, checklist D4).**
  - `internal/replication/binlog_status.go` picks the status statement in one place from `VERSION()`: MySQL 8.4 and newer (including 9.x) use `SHOW BINARY LOG STATUS`; older MySQL, including 5.7 and 8.0, and every MariaDB version string use `SHOW MASTER STATUS`. Fallback list and per-`host:port` cache as above.
  - The binlog dump, `caching_sha2_password` authentication, GTID and row replay worked on 8.4 with the current client once the status statement was corrected. No other product change was needed.

## Fixes

- **#437 (#557):** after an online `migrate up` to 000010, rows the same process had already removed by retention were never marked, because the idempotency set recorded the key while the column was still missing. The key now stays pending, and the first pass after the column appears writes only the marker. Regression test `TestOnlineMigrateUpWritesRetentionMarkerWithoutSecondEvent`.

## CI

- `mysql:8.4` joins the e2e compose matrix. In the quick profile (job `unit-and-e2e`, every PR and push to main), `smoke.sh` requires the checkpoint to advance after a source write on MySQL 5.7, 8.0, 8.4 LTS, Percona 5.7 and Percona 8.0. `up.sh` fails if `mysql84` is not ready.
- New `smoke-version-restore.sh` in the quick profile: on MySQL 5.7 and 8.4 a LATEST task starts at the tip, the source creates, updates, deletes, rotates and inserts, and the backed-up files are applied with `mysqlbinlog` onto a fresh instance of the same major version. `CHECKSUM TABLE` and the ordered row text must match. `mysqlbinlog` comes from Percona Server 5.7 / 8.4 because the official `mysql` images do not ship it.
- `e2e-upgrade-failback` runs a four-leg matrix on every PR (`fail-fast: false`): `plain`, `object-only` and `object-gone` from v0.5.57 (through 0a84917a), and `plain` from v0.5.82 straight to the current build (`FBUP_SKIP_MID=1`, `FBUP_REQUIRE_REUSE=0`). A missing old ref fails the leg; there is no skip.

## Known issues

- **#559 (P3):** two standalone processes pointed at the same `meta_dsn` both start. One process's upload retry can ship the other's sealed segment by its absolute `file_path` to its own bucket and mark the row `UPLOADED`. This is a misconfiguration hazard, not a regression; give each standalone its own meta database.
- **#558 (P4):** with migration 000010 applied, a transient failure writing `retention_removed_at` adds one extra `RETENTION_REMOVED` event per failed pass. Behaviour is the same as v0.5.82.
- **#556 (P3):** `mysql84` is not locked into the quick e2e scenario list by a separate guard; deleting both the 8.4 smoke and the 8.4 restore line together would let quick pass without the 8.4 proof.
- **#549:** `cluster.role: worker` serves no `/metrics`, so F2 series from those nodes cannot be scraped. Use `all-in-one` nodes for per-task health alerting until it is fixed.
- **#550 (P3):** test gaps from the #545 QA (webhook 3xx, pending-clear, batch order and delivery id; firing-only promtool cases). The behaviour is correct today; these are missing guards, not known bugs.
- **#551 (P3):** `config.example.yaml` and `config.production.example.yaml` have no `notify:` block although the configuration guide documents it.
- **#440 (P3):** `e2e-source-switchover` can fail with exit 23 when `curl | awk` exits early under `pipefail`. Test-only.
- **Upgrade legs (P3, from the #557 QA):** `e2e-upgrade-failback` applies the new `migrations/` once before the old binary starts, so no leg runs `migrate up` between old and new. This release has no migration, so E2 holds; a release that adds one needs that leg. A no-op 000010 and an unreadable old checkpoint are caught by unit tests only.
- **F4:** `health.evaluator` still defaults to `off`, so without `on` there are no per-task series and no webhook.
- **#541**, **#540**, **#386** (P3), **#510**, **#473** (P1, ADR 0011 PR-4 in progress), **#462** (P1, full closure with ADR 0011 PR-5), **#513** (P2) and **#539** are unchanged from v0.5.82. **#514**, **#512**, **#494**, **#500**, **#502**, **#503** (P3) are still open, as are **#487**, **#488**, **#451**, **#452**, **#455**, **#456**, **#446** and **#438**. `/api/findings` has no read timeout yet (P2-7). The verifier does not run in cluster mode (A4 PARTIAL).

## Changes since v0.5.82

`git log --oneline v0.5.82..d58e660` (3 commits), plus the commit of these notes, which v0.5.83 is tagged on:

| Commits | Change |
|---|---|
| `0c05ad3` | Install docs point at v0.5.82 |
| `34d53f1` | PR #555: `SHOW BINARY LOG STATUS` on MySQL 8.4+, chosen by `VERSION()` with fallback and per-host cache; `mysql:8.4` in the e2e matrix; `smoke-version-restore.sh` for 5.7 and 8.4 |
| `d58e660` | PR #557: #437 retention marker catch-up after an online `migrate up`; four-leg `e2e-upgrade-failback` on every PR, including from v0.5.82 |

The server binary changes in how the source position is read (MySQL 8.4) and in the retention marker catch-up. The other changes are CI, e2e scripts and docs.

## QA summary

**PR #555 QA PASS (BinlogServerQA) at `34d53f1d`, 2026-10-27.** CI 18/18 on the head.
- Real `mysql:8.4.13`: `SHOW MASTER STATUS` returns 1064 and `SHOW BINARY LOG STATUS` works. LATEST, GTID and FILE_POS tasks reach RUNNING, the checkpoint follows `FLUSH BINARY LOGS` to the tip, and FILE_POS sealed files match the source byte for byte.
- Independent restores onto fresh `mysql:8.4` and `mysql:5.7` match `CHECKSUM TABLE` and rows. A cache poisoned with `SHOW MASTER STATUS` on 8.4 recovers through the fallback.
- A MASTER-only mutant fails the unit tests and turns an 8.4 LATEST task FAILED with `ERROR 1064`. Removing both 8.4 lines from the quick profile together would survive; filed as #556.

**PR #557 QA PASS (BinlogServerQA) at `d58e660e`, 2026-10-27.**
- #437: unit tests and a live online `migrate up` with fresh rows, `kill -9` and extra passes give exactly 1 `RETENTION_REMOVED` per row with the marker set; a restart while a marker is pending gives 2, as design 0009 accepts.
- The four upgrade legs pass on every PR with no silent skip. Invalid SQL in a migration, unreadable old segments and an unfetchable old ref all go red.
- In-place upgrade from the v0.5.82 release tarball under write load, `kill -9`, rollback to v0.5.82 and back: 30/30 sealed files byte-identical, GTID 1–20418 with no gap or duplicate, and a restore with matching `CHECKSUM TABLE`.

**Tip QA of `d58e660e` PASS (BinlogServerQA), 2026-10-27.** Push CI 21/21, including all four `e2e-upgrade-failback` legs.
- Plain and `-race` build and test: 19/19 packages, 0 failures, 0 data races.
- LATEST / FILE_POS / GTID on MySQL 5.7.44, 8.0.46, 8.4.13 and MariaDB 11.4.13: 12/12 RUNNING (8.4 LATEST start OK), checkpoints advance, the first full sealed file byte-identical to the source.
- F2 metrics, sentinel and webhook; TLS upload with the default `use_ssl`; the plain-HTTP hint; `SIGTERM` in 0.02 s with idle keep-alives; PRODUCTION fail-closed.
- Cluster failover with a shared `data_dir`: takeover in about 14 s, 0 duplicate file rows, 0 duplicate metric series.
- In-place upgrade from the v0.5.82 release under load; retention at schema 10 with exactly 1 event per file; a 13-minute soak with 75/75 full sealed files byte-identical and 76/76 objects matching; a restore round trip with matching `CHECKSUM TABLE`.

## Chinese Release Notes

Chinese version:

# Binlog Server v0.5.83 中文发布说明

> 对应版本：`v0.5.83`

发布日期：2026-10-27

v0.5.83 把 MySQL 8.4 LTS 列为支持的源库（#555），并在每个 PR 上验证原地升级（#557）。MySQL 8.4 去掉了 `SHOW MASTER STATUS`，v0.5.82 在 8.4 上读不到源库位点：LATEST 启动和空闲时的追平检查都以 `ERROR 1064` 失败。现在 BinlogServer 按 `VERSION()` 选择 binlog 状态语句：MySQL 8.4 及以上用 `SHOW BINARY LOG STATUS`，更早的 MySQL 和 MariaDB 用 `SHOW MASTER STATUS`。`mysql:8.4` 加进了 e2e 矩阵，CI 里会把 5.7 和 8.4 的备份恢复出来，用 `CHECKSUM TABLE` 比对。`e2e-upgrade-failback` 现在每个 PR 跑四档，其中一档从 v0.5.82 直接原地升级。另外修复 #437：在线 `migrate up` 到 000010 之后，已经被保留策略删掉的行在下一轮补上 `retention_removed_at`，不再写第二条 `RETENTION_REMOVED`。没有新迁移（schema 最高 10，`minRequiredSchemaVersion` 仍为 6），没有新配置项，升级地板仍是 v0.5.51。生产就绪 D4 和 E2 变为 MET，清单为 MET 23 / PARTIAL 4 / NOT MET 8。

## 升级须知

- **没有迁移，也没有新配置项。** schema 最高仍为 10，`minRequiredSchemaVersion` 仍为 6。从 v0.5.82 升级只需替换二进制。从更早版本升级，v0.5.82、v0.5.81 和 v0.5.80 的说明仍然适用，包括 v0.5.80 里 `upload.use_ssl` 默认值的变化。
- **支持的源库是 MySQL 5.7、8.0、8.4 LTS，以及 MariaDB 10.3+（CI 里恢复的是 11.4）。** 源库是 MySQL 8.4 时，LATEST 任务现在能启动，追平检查也能读到源库位点。任务定义不用改。状态语句仍需要 `REPLICATION CLIENT` 权限。
- **状态语句怎么选。** 语句由 `VERSION()` 决定。失败时依次尝试其它已知写法，包括 MariaDB 的 `SHOW BINLOG STATUS`。返回了结果的语句按源库 `host:port` 记住；记住的语句开始失败时会被丢掉，重新按列表尝试。
- **在线 `migrate up` 之后的保留标记（#437）。** 在 000010 还没应用时写过 `RETENTION_REMOVED` 的进程，`migrate up` 之后的下一轮会补上 `retention_removed_at`，不用重启，也不会写第二条事件。标记还没补上时重启，仍可能多写一条事件，这是设计 0009 接受的。
- **回滚。** v0.5.82 用同一个 schema 和配置，可以回滚二进制。Tip QA 从 v0.5.82 的发布包升级、回滚到 v0.5.82 再升回这一版，封存文件逐字节一致，事务没有缺口也没有重复。源库是 MySQL 8.4 时，v0.5.82 不能启动 LATEST 任务，也读不到源库位点。

## 新增

- **支持 MySQL 8.4 LTS（#555，清单 D4）。**
  - `internal/replication/binlog_status.go` 在一个地方按 `VERSION()` 选状态语句：MySQL 8.4 及以上（含 9.x）用 `SHOW BINARY LOG STATUS`；更早的 MySQL（含 5.7 和 8.0）和所有 MariaDB 版本串用 `SHOW MASTER STATUS`。回退列表和按 `host:port` 的缓存如上。
  - 状态语句改对之后，8.4 上的 binlog dump、`caching_sha2_password` 认证、GTID 和行事件回放用现有客户端都正常，不需要别的产品改动。

## 修复

- **#437（#557）：** 在线 `migrate up` 到 000010 之后，同一进程已经按保留策略删掉的行一直不会被标记，因为幂等集合在列还不存在时就记下了键。现在这把键留在 pending 里，列出现后的第一轮只写标记。回归测试 `TestOnlineMigrateUpWritesRetentionMarkerWithoutSecondEvent`。

## CI

- `mysql:8.4` 加入 e2e compose 矩阵。quick profile（`unit-and-e2e` job，每个 PR 和 push main）里，`smoke.sh` 要求 MySQL 5.7、8.0、8.4 LTS、Percona 5.7、Percona 8.0 上源库写入后 checkpoint 前进。`mysql84` 没就绪时 `up.sh` 直接失败。
- quick profile 新增 `smoke-version-restore.sh`：在 MySQL 5.7 和 8.4 上，LATEST 任务从当前位点开始，源库建表、更新、删除、轮转、再插入，备份下来的文件用 `mysqlbinlog` 应用到同一大版本的新实例上，`CHECKSUM TABLE` 和按序的行内容必须一致。官方 `mysql` 镜像不带 `mysqlbinlog`，客户端用 Percona Server 5.7 / 8.4。
- `e2e-upgrade-failback` 每个 PR 跑四档矩阵（`fail-fast: false`）：从 v0.5.57（经 0a84917a）的 `plain`、`object-only`、`object-gone`，以及从 v0.5.82 直接升到当前构建的 `plain`（`FBUP_SKIP_MID=1`、`FBUP_REQUIRE_REUSE=0`）。旧版本取不到时这一档失败，不会跳过。

## 已知问题

- **#559（P3）：** 两个 standalone 进程误配到同一个 `meta_dsn` 都能启动。一个进程的上传重试可能按绝对 `file_path` 把另一个进程的封存段传到自己的桶，并把这一行标成 `UPLOADED`。这是误配风险，不是回归；每个 standalone 请用单独的 meta 库。
- **#558（P4）：** 应用了 000010 之后，`retention_removed_at` 写入瞬时失败时，每失败一轮多写一条 `RETENTION_REMOVED`。行为与 v0.5.82 相同。
- **#556（P3）：** `mysql84` 没有单独的守护锁在 quick e2e 场景列表里；同时删掉 8.4 的 smoke 和 8.4 的恢复两行，quick 也能在没有 8.4 证明的情况下通过。
- **#549：** `cluster.role: worker` 不提供 `/metrics`，这些节点上的 F2 序列抓不到。修好之前，按任务健康告警请用 `all-in-one` 节点。
- **#550（P3）：** #545 QA 留下的测试缺口（webhook 3xx、清待发、批内顺序和投递 id；promtool 用例只测触发）。当前行为是对的，缺的是守护测试，不是已知缺陷。
- **#551（P3）：** `config.example.yaml` 和 `config.production.example.yaml` 没有 `notify:` 段，配置文档里有。
- **#440（P3）：** `e2e-source-switchover` 在 `pipefail` 下 `curl | awk` 提前退出时可能以 exit 23 失败。只影响测试。
- **升级档（P3，来自 #557 QA）：** `e2e-upgrade-failback` 在旧二进制启动之前就一次性应用新的 `migrations/`，所以没有一档在旧版和新版之间跑 `migrate up`。这一版没有迁移，E2 成立；以后带迁移的版本需要这一档。000010 改成空操作、旧 checkpoint 读不出来，这两种情况只有单测能抓到。
- **F4：** `health.evaluator` 默认仍是 `off`，不设 `on` 就没有每任务序列和 webhook。
- **#541**、**#540**、**#386**（P3）、**#510**、**#473**（P1，ADR 0011 PR-4 正在做）、**#462**（P1，完整关闭要等 ADR 0011 PR-5）、**#513**（P2）和 **#539** 与 v0.5.82 相同。**#514**、**#512**、**#494**、**#500**、**#502**、**#503**（P3）仍未关闭，**#487**、**#488**、**#451**、**#452**、**#455**、**#456**、**#446**、**#438** 也是。`/api/findings` 还没有读超时（P2-7）。集群模式不跑巡检（A4 PARTIAL）。

## 自 v0.5.82 以来的变更

`git log --oneline v0.5.82..d58e660`（3 个提交），加上这份说明所在的提交（v0.5.83 打在这个提交上）：

| 提交 | 变更 |
|---|---|
| `0c05ad3` | 安装文档指向 v0.5.82 |
| `34d53f1` | PR #555：MySQL 8.4+ 用 `SHOW BINARY LOG STATUS`，按 `VERSION()` 选择，带回退和按主机的缓存；`mysql:8.4` 进 e2e 矩阵；5.7 和 8.4 的 `smoke-version-restore.sh` |
| `d58e660` | PR #557：在线 `migrate up` 之后补保留标记（#437）；每个 PR 跑四档 `e2e-upgrade-failback`，包括从 v0.5.82 升级 |

服务端二进制的变化在读取源库位点的方式（MySQL 8.4）和保留标记的补写。其余改动是 CI、e2e 脚本和文档。

## QA 摘要

**PR #555 QA PASS（BinlogServerQA），`34d53f1d`，2026-10-27。** 头上 CI 18/18。
- 真实 `mysql:8.4.13`：`SHOW MASTER STATUS` 返回 1064，`SHOW BINARY LOG STATUS` 正常。LATEST、GTID、FILE_POS 任务都到 RUNNING，checkpoint 跟着 `FLUSH BINARY LOGS` 追到最新，FILE_POS 的封存文件与源库逐字节一致。
- 独立恢复到新的 `mysql:8.4` 和 `mysql:5.7`，`CHECKSUM TABLE` 和行内容一致。8.4 上缓存被强制成 `SHOW MASTER STATUS` 时能通过回退恢复。
- 只用 MASTER 的变异让单测失败，8.4 的 LATEST 任务以 `ERROR 1064` 进入 FAILED。同时从 quick 删掉两处 8.4 会存活，记为 #556。

**PR #557 QA PASS（BinlogServerQA），`d58e660e`，2026-10-27。**
- #437：单测和真实的在线 `migrate up`（含新行、`kill -9` 和多轮）下，每行恰好 1 条 `RETENTION_REMOVED` 且标记已写；标记待补时重启得到 2 条，这是设计 0009 接受的。
- 四个升级档每个 PR 都跑，没有静默跳过。迁移 SQL 写坏、旧段读不出、旧版本取不到，都会变红。
- 在写入负载下从 v0.5.82 发布包原地升级、`kill -9`、回滚到 v0.5.82 再升回：30/30 封存文件逐字节一致，GTID 1–20418 无缺口无重复，恢复后 `CHECKSUM TABLE` 一致。

**main `d58e660e` 的 Tip QA PASS（BinlogServerQA），2026-10-27。** push CI 21/21，包括 `e2e-upgrade-failback` 全部四档。
- 普通和 `-race` 构建与测试：19/19 个包，0 失败，0 数据竞争。
- MySQL 5.7.44、8.0.46、8.4.13 和 MariaDB 11.4.13 上的 LATEST / FILE_POS / GTID：12/12 RUNNING（8.4 LATEST 启动正常），checkpoint 前进，第一个完整封存文件与源库逐字节一致。
- F2 指标、哨兵和 webhook；默认 `use_ssl` 的 TLS 上传；明文 HTTP 提示；开着空闲 keep-alive 时 `SIGTERM` 0.02 秒退出；PRODUCTION fail-closed。
- 共享 `data_dir` 的集群故障切换：约 14 秒接管，0 条重复文件行，0 条重复指标序列。
- 在负载下从 v0.5.82 发布包原地升级；schema 10 下保留策略每个文件恰好 1 条事件；13 分钟 soak，75/75 个完整封存文件逐字节一致，76/76 个对象一致；恢复往返 `CHECKSUM TABLE` 一致。
