# Binlog Server v0.5.78

Release date: 2026-10-21

v0.5.78 wires in the ADR 0009 phase 3 verifier (#501, landing #485). An in-process worker re-reads local segments and checks uploaded objects on a schedule. A missing local file or a corrupt object now appears as a finding on the API, instead of waiting for a restore to find it. The verifier runs in standalone mode. In cluster mode it starts but skips every task until ADR 0011 can name the current owner, so production-readiness A4 stays PARTIAL. Release packaging is fixed as well: tarballs ship `docs/releases/` again (#505), and both binaries are built with `-w` only (#499, #505), which removes the GO-2026-5932 false positive from binary-mode scans. No new migration (schema up to 10, `minRequiredSchemaVersion` still 6), and the upgrade floor stays v0.5.51. New optional config keys: `verify.*` (see below).

## Upgrade Notes

- **The verifier is on in standalone and has no off switch.** It runs its first pass right after task records are restored at startup, then every `verify.every_sec` (default 86400, must be positive). Reads share one process-wide budget, `verify.budget_bytes_per_sec` (default 4 MiB/s; 0 also means 4 MiB/s, not unlimited). Each pass reads local segments and sends HEAD and sampled GET requests to the object store, so expect extra object-store requests. To lower the load, lower the budget, set `verify.pass_max_bytes`, or raise `verify.every_sec`.
- **A pre-existing problem can turn `/api/health` 503 after the upgrade.** The first pass may open CRITICAL `VERIFY_FAILED` (a local segment does not parse or does not match its catalog row) or `OBJECT_CORRUPT` (an object does not hash to the local file or the catalog digest). It may also open a missing-local finding. An open CRITICAL makes `/api/health` return 503, as other CRITICAL findings do. `LOCAL_UNREADABLE` and `UPLOAD_CLOCK_SKEW` are WARNING. `LOCAL_UNREADABLE` becomes CRITICAL on a second failure in a row when the local file is the only copy. Findings are on `GET /api/tasks/{id}/findings` and in the standalone `.findings.json`.
- **Cluster mode:** the worker starts, but every task is skipped (`tasks=0 skipped=N` in the pass log) and it opens no verifier findings. The startup storage probe below still runs.
- **Startup storage probe.** With an object store configured, startup sends one bucket HEAD with a 2 s timeout. With a blackholed endpoint, listening starts up to about 2 s later. A failure logs a warning and adds a `detail` entry to `GET /api/health` with the bucket and endpoint (never the keys). `status` stays `ok`, HTTP stays 200, and startup goes on. That detail stays until the process exits. The probe cannot see "can PUT but not GetObject".
- **3-strike object detail.** When an object HEAD fails during a pass, that pass logs `object_incomplete=true` and a warning with the task id, the object key and the bucket. After 3 such passes in a row (not configurable, so 3 days at the default period), `/api/health` gets a `detail` entry. `status` stays `ok`. A pass that completes the object range clears it, and so does a pass with no tasks. A pass where every task is skipped (for example, meta down) neither counts nor clears.
- **Restart caveat:** the 3-strike count lives only in memory. It is not written to `.verify.json`, so a restart starts again from 0. With the default `verify.every_sec` of 86400, a restart delays that detail by about 3 days. A restart also runs a pass at once.
- **Detection latency for same-size corruption.** An object corrupted without changing its size (`OBJECT_CORRUPT`) is found only when its segment's rotation slot is hashed. The worst case is `verify.rotation_periods` × `verify.every_sec` (20 × 1 day by default).
- **New files in `data_dir`.** Progress is kept in `{data_dir}/.verify.json`, replaced atomically. If it is lost, the next pass reads more but no finding is lost. Stale `.verify-*.tmp` files are removed at startup. Two processes sharing one `data_dir` remain unsupported.
- **Each pass logs one INFO line:** `verifier: pass tasks bytes findings_opened findings_open findings_closed cut_short object_incomplete skipped duration`. New metrics:
  - `binlog_server_verify_bytes_total`
  - `binlog_server_verify_pass_incomplete`
  - `binlog_server_verify_objects_unverifiable`
  - `binlog_server_verify_digest_backfill_pending`
  - `binlog_server_verify_objects_size_unverifiable`
  - `binlog_server_verify_deferred`
- **Release binaries are a little larger.** `binlog-server` keeps its symbol table (`-w` without `-s`, about 55.5 MB instead of 51.5 MB on linux/amd64), so `govulncheck -mode=binary`, Trivy and Grype no longer report GO-2026-5932 (x/crypto openpgp, not linked) (#499, #498).
- No new migration. v0.5.77 runs on the same schema, so the binary can be rolled back. Before starting v0.5.77, remove the `verify.*` keys if it rejects unknown keys. From v0.5.77, replace the binary; the v0.5.77 notes still apply when upgrading from v0.5.76 or earlier.

## New config keys (`verify`)

All are optional. Negative values refuse to start. See `docs/guide/admin/configuration.md` §3.11.1.

| Key | Default | Meaning |
|---|---|---|
| `verify.every_sec` | 86400 | Pass period in seconds. Must be positive |
| `verify.budget_bytes_per_sec` | 4194304 | Process-wide read budget. 0 uses the default |
| `verify.rotation_periods` | 20 | Rotation length in periods. 0 uses the default |
| `verify.recent_share_percent` | 25 | Largest share of a pass for the recent tier. 1–100; 0 refuses to start |
| `verify.pass_deadline_sec` | 0 | Pass deadline. 0 means `verify.every_sec` |
| `verify.pass_max_bytes` | 0 | Bytes read per pass at most. 0 means no cap |
| `verify.object_recent_sec` | 86400 | Recent-upload window. 0 uses the default (24 h) |
| `verify.drill_dsn` | empty | Restore-drill instance. Empty means off. This release loads it but never dials it; it is protected like a source password |
| `verify.drill_every_sec` | 604800 | Drill period |
| `verify.drill_check_sql` | empty | Optional check SQL after a drill |

## Added

- **Phase 3 verifier worker (#501, landing #485; ADR 0009 phase 3).** Each pass works from the catalog, the local `data_dir` and the object store:
  - local segments are read in full;
  - every uploaded object gets a HEAD;
  - SHA-256 is checked in budget order: forced re-checks, then the recent tier (not uploaded or uploaded within `verify.object_recent_sec`), then the rotation slot, then the least recently verified.
  
  Findings go through the existing outputs. A cancelled or deadline-cut pass closes only rows it read and saves its progress. Gone segments and tasks leave `.verify.json`. The restore drill stays off.

## Fixes

- **#505:** release tarballs had no `docs/releases/` (0 of 206 files), although the README lists it and the upgrade notes point to `docs/releases/adr-0011-pr2.md`. The archive glob now includes the top-level files.
- **#499 (#498):** release `binlog-server` binaries are built with `-w` only. **#505:** `migrate` is built with `-w` too (it had goreleaser's default `-s -w`).
- **#497:** install docs point at v0.5.77 with its checksums. The next pin PR moves them to v0.5.78.

## Known issues

- **#490 (P3):** `TestFailurePathMatrixMetaOutage` is flaky on loaded hosts (28 of 73 runs failed under load in the #485 QA). Test only.
- **#494 (P3):** `binlog_server_checkpoint_age_seconds` is emitted with an empty `task_id` label.
- **#500 (P3):** the stale-floor `LockWait`/`LeaseTTL` inputs and the `wait_timeout` reason have no test coverage on main (3 surviving mutants).
- **#502 (P3):** verifier test gaps. These behave correctly but are not pinned:
  - an all-skipped pass still prunes a deleted task from `.verify.json`;
  - the probe detail and the 3-strike detail show together;
  - a mixed pass clears the streak.
- **#503 (P3):** the mutation runner counts any `go test` failure with no test name as "killed build", so a broken environment reports 0 survivors. Tooling only.
- **#504 (P3):** `TestOpenReplayArchive_MissingOpenSegmentIsNotOnProcess` and `TestEvaluator_1500SegmentsUnder50ms` (under `-race`) are flaky on loaded hosts. Test only.
- **#487**, **#488**, **#451**, **#452**, **#455**, **#456**, **#446**, **#437** and **#438** are still open. `/api/findings` has no read timeout yet (P2-7). The verifier does not run in cluster mode (A4 PARTIAL).

## Changes since v0.5.77

`git log --oneline --first-parent v0.5.77..b28f6d8`, plus the commit of these notes, which v0.5.78 is tagged on:

| Commits | Change |
|---|---|
| `f63c886` | PR #497: install docs point at v0.5.77 |
| `c3315c2` | PR #499: keep the symbol table in release binaries (fixes #498) |
| `72c4b44`..`6eff61a` | PR #501: ADR 0009 phase 3 verifier worker (lands #485) |
| `b28f6d8` | PR #506: ship `docs/releases` in release archives, `migrate` with `-w` (fixes #505) |

## QA summary

**PR #485 QA PASS (BinlogServerQA), round 6 at `083c52fb`, 2026-10-21.**
- Mutation runner: 265 killed, 0 survived, 3 equivalent.
- Live with MinIO, a MySQL 8.0 source and MariaDB meta without `PROCESS`:
  - 3 strikes set the detail with status ok;
  - meta down keeps the streak (3 → 4 → 5);
  - deleting the last task clears the detail;
  - cluster health is 200 `ok` with the probe detail, `side_kill: off` and `lock_detail: reduced`;
  - no credentials appear in logs.

**Tip QA of `6eff61a4` PASS (BinlogServerQA), 2026-10-21.**
- Land fidelity: the code interdiff with the QA-passed head is empty.
- Startup removes stale `.verify-*.tmp` files; a symlink is unlinked and its target kept.
- A finished pass, and deleting every task, each clear the detail.
- The probe failure shows the bucket and endpoint.
- `/healthz`, `/api/health`, `/ui/` and Swagger return 200, and a LATEST task follows writes and a FLUSH.
- `govulncheck -mode=binary` on a `-w` build reports no vulnerabilities.

**Packaging proof for #506, which landed after the tip QA and changes only `.goreleaser.yml`.**
- `goreleaser check` passes.
- A snapshot release puts 257 entries in each of the 4 tarballs, including the 206 files under `docs/releases/` (`adr-0011-pr2.md` among them).
- `govulncheck -mode=binary` (v1.9.0) reports 0 affecting vulnerabilities for `binlog-server` and none for `migrate`.
- CI passed on `b28f6d8`.

## Chinese Release Notes

Chinese version:

# Binlog Server v0.5.78 中文发布说明

> 对应版本：`v0.5.78`

发布日期：2026-10-21

v0.5.78 接上了 ADR 0009 第 3 阶段的周期巡检（#501，落地 #485）。进程内的工人按周期重读本地段、检查已上传的对象。本地文件丢了或对象损坏，现在会作为 finding 出现在 API 上，不用等到还原时才发现。巡检在单机模式下运行。集群模式下它会启动，但在 ADR 0011 能确定当前主人之前跳过所有任务，所以生产清单 A4 仍是 PARTIAL。发布打包也修了：发布包重新带上 `docs/releases/`（#505），两个二进制都只用 `-w` 构建（#499、#505），二进制模式扫描不再误报 GO-2026-5932。没有新迁移（schema 最高 10，`minRequiredSchemaVersion` 仍为 6），升级地板仍是 v0.5.51。新增可选配置项 `verify.*`（见下）。

## 升级须知

- **单机下巡检默认开启，没有关闭开关。** 启动时恢复完任务记录后立刻跑第一轮，之后每 `verify.every_sec` 一轮（默认 86400，必须为正）。所有读取共用一把进程级预算 `verify.budget_bytes_per_sec`（默认 4 MiB/s；填 0 也是 4 MiB/s，不是不限速）。每一轮读本地段，并向对象存储发 HEAD 和抽样 GET，对象存储的请求会变多。想降低负载，可以调低预算、设置 `verify.pass_max_bytes`，或者调大 `verify.every_sec`。
- **升级后，原有的问题可能让 `/api/health` 变成 503。** 第一轮可能打开 CRITICAL `VERIFY_FAILED`（本地段解析失败，或与目录行不符）或 `OBJECT_CORRUPT`（对象的哈希与本地文件或目录摘要不符），也可能打开本地文件缺失的 finding。和其他 CRITICAL 一样，开着的 CRITICAL 让 `/api/health` 返回 503。`LOCAL_UNREADABLE` 和 `UPLOAD_CLOCK_SKEW` 是 WARNING。本地文件是唯一副本时，`LOCAL_UNREADABLE` 连续第二次失败会升为 CRITICAL。finding 可以在 `GET /api/tasks/{id}/findings` 和单机的 `.findings.json` 里看到。
- **集群模式：** 工人会启动，但每个任务都被跳过（这一轮日志写 `tasks=0 skipped=N`），不开任何巡检 finding。下面的启动存储探针照常运行。
- **启动存储探针。** 配了对象存储时，启动对桶做一次 HEAD，超时 2 秒。端点是黑洞时，开始监听会晚大约 2 秒。失败时打一条 warn，并在 `GET /api/health` 的 `detail` 里加一条，写桶名和端点，不写密钥。`status` 仍是 `ok`，HTTP 仍是 200，启动照常继续。这条 detail 留到进程退出。探针看不出「能 PUT、不能 GetObject」。
- **连续 3 轮的对象 detail。** 某一轮对象 HEAD 失败时，这一轮写 `object_incomplete=true`，并打一条 warn，带上任务 id、对象 key 和桶名。连续 3 轮这样（没有配置项，默认周期下是 3 天）后，`/api/health` 加一条 `detail`，`status` 仍是 `ok`。有一轮把对象范围查完就清掉这条；没有任务的一轮也会清掉。所有任务都被跳过的一轮（例如 meta 不可用）既不计数，也不清零。
- **重启注意：** 连续 3 轮的计数只在内存里，不写进 `.verify.json`，重启后从 0 重新数。默认 `verify.every_sec` 为 86400 时，重启会把这条 detail 推迟大约 3 天。重启后也会立刻跑一轮。
- **同尺寸损坏的检测延迟。** 对象损坏但大小不变时（`OBJECT_CORRUPT`），要等该段的轮换槽被哈希才发现，最坏是 `verify.rotation_periods` × `verify.every_sec`（默认 20 × 1 天）。
- **`data_dir` 里的新文件。** 进度写在 `{data_dir}/.verify.json`，原子替换。文件丢了，下一轮多读一些，finding 不会丢。启动时清掉残留的 `.verify-*.tmp`。两个进程共用一个 `data_dir` 仍不受支持。
- **每一轮打一行 INFO：** `verifier: pass tasks bytes findings_opened findings_open findings_closed cut_short object_incomplete skipped duration`。新增指标：
  - `binlog_server_verify_bytes_total`
  - `binlog_server_verify_pass_incomplete`
  - `binlog_server_verify_objects_unverifiable`
  - `binlog_server_verify_digest_backfill_pending`
  - `binlog_server_verify_objects_size_unverifiable`
  - `binlog_server_verify_deferred`
- **发布二进制稍大一些。** `binlog-server` 保留符号表（只用 `-w`、不用 `-s`；linux/amd64 上约 55.5 MB，原来 51.5 MB），所以 `govulncheck -mode=binary`、Trivy 和 Grype 不再报 GO-2026-5932（x/crypto openpgp，没有链接进来）（#499、#498）。
- 没有新迁移。v0.5.77 在同一个 schema 上运行，可以回滚二进制。启动 v0.5.77 之前，如果它拒绝未知配置项，先删掉 `verify.*`。从 v0.5.77 升级只需替换二进制；从 v0.5.76 或更早版本升级，v0.5.77 的说明仍然适用。

## 新配置项（`verify`）

都是可选项。负数拒绝启动。详见 `docs/guide/admin/configuration.md` 3.11.1 节。

| 参数 | 默认值 | 说明 |
|---|---|---|
| `verify.every_sec` | 86400 | 巡检周期，秒。必须为正 |
| `verify.budget_bytes_per_sec` | 4194304 | 进程级读预算。0 用默认值 |
| `verify.rotation_periods` | 20 | 轮换长度，按周期数。0 用默认值 |
| `verify.recent_share_percent` | 25 | 最近档最多占一轮的百分之几。1–100；0 拒绝启动 |
| `verify.pass_deadline_sec` | 0 | 一轮的截止时间。0 等于 `verify.every_sec` |
| `verify.pass_max_bytes` | 0 | 一轮最多读的字节数。0 不设上限 |
| `verify.object_recent_sec` | 86400 | 最近上传窗口。0 用默认值（24 小时） |
| `verify.drill_dsn` | 空 | 还原演练实例。空即关闭。这一版只加载、不拨号；按源密码的方式保护 |
| `verify.drill_every_sec` | 604800 | 演练周期 |
| `verify.drill_check_sql` | 空 | 演练后可选的校验 SQL |

## 新增

- **第 3 阶段巡检工人（#501，落地 #485；ADR 0009 第 3 阶段）。** 每一轮从目录、本地 `data_dir` 和对象存储出发：
  - 本地段完整读一遍；
  - 每个已上传对象做一次 HEAD；
  - SHA-256 在预算内按顺序比对：先强制重查，再最近档（未上传，或在 `verify.object_recent_sec` 内上传），再轮换槽，最后是最久没验过的。

  finding 走现有出口。被取消或被截止时间截断的一轮只关闭它读过的行，并保存进度。已不存在的段和任务会从 `.verify.json` 移除。还原演练仍关闭。

## 修复

- **#505：** 发布包里没有 `docs/releases/`（206 个文件一个都没有），可 README 列了这个目录，升级说明也让人去看 `docs/releases/adr-0011-pr2.md`。打包的 glob 现在包含这一层的文件。
- **#499（#498）：** 发布的 `binlog-server` 只用 `-w` 构建。**#505：** `migrate` 也改成 `-w`（之前用 goreleaser 默认的 `-s -w`）。
- **#497：** 安装文档指向 v0.5.77 和它的校验和。下一个固定版本的 PR 会把它们改到 v0.5.78。

## 已知问题

- **#490（P3）：** `TestFailurePathMatrixMetaOutage` 在高负载机器上不稳定（#485 的 QA 中高负载下 73 次失败 28 次）。只影响测试。
- **#494（P3）：** `binlog_server_checkpoint_age_seconds` 的 `task_id` label 为空。
- **#500（P3）：** stale floor 的 `LockWait`/`LeaseTTL` 输入和 `wait_timeout` reason 在 main 上没有测试覆盖（3 个变异存活）。
- **#502（P3）：** 巡检的测试缺口。下面这些行为正确，但没有测试固定：
  - 全部跳过的一轮仍会把已删除的任务从 `.verify.json` 清掉；
  - 探针 detail 和连续 3 轮的 detail 会同时出现；
  - 混合的一轮（一个任务完成、一个跳过）会清零计数。
- **#503（P3）：** 变异运行器把没有测试名的 `go test` 失败都算成「killed build」，环境坏了也报 0 个存活。只影响工具。
- **#504（P3）：** `TestOpenReplayArchive_MissingOpenSegmentIsNotOnProcess` 和 `TestEvaluator_1500SegmentsUnder50ms`（`-race` 下）在高负载机器上不稳定。只影响测试。
- **#487**、**#488**、**#451**、**#452**、**#455**、**#456**、**#446**、**#437**、**#438** 仍未关闭。`/api/findings` 还没有读超时（P2-7）。集群模式不跑巡检（A4 PARTIAL）。

## 自 v0.5.77 以来的变更

`git log --oneline --first-parent v0.5.77..b28f6d8`，加上这份说明所在的提交（v0.5.78 打在这个提交上）：

| 提交 | 变更 |
|---|---|
| `f63c886` | PR #497：安装文档指向 v0.5.77 |
| `c3315c2` | PR #499：发布二进制保留符号表（修复 #498） |
| `72c4b44`..`6eff61a` | PR #501：ADR 0009 第 3 阶段巡检工人（落地 #485） |
| `b28f6d8` | PR #506：发布包带上 `docs/releases`，`migrate` 用 `-w`（修复 #505） |

## QA 摘要

**PR #485 QA PASS（BinlogServerQA），第 6 轮 `083c52fb`，2026-10-21。**
- 变异运行器：打死 265 个，存活 0 个，等价 3 个。
- 实测环境为 MinIO、MySQL 8.0 源和没有 `PROCESS` 的 MariaDB meta：
  - 连续 3 轮后写入 detail，status 仍是 ok；
  - meta 不可用时保留计数（3 → 4 → 5）；
  - 删掉最后一个任务后 detail 清掉；
  - 集群的健康检查返回 200 `ok`，带探针 detail、`side_kill: off` 和 `lock_detail: reduced`；
  - 日志里没有凭据。

**main `6eff61a4` 的 Tip QA PASS（BinlogServerQA），2026-10-21。**
- 落地一致性：与 QA 通过的头相比，代码 interdiff 为空。
- 启动时清掉残留的 `.verify-*.tmp`；符号链接本身被删除，目标保留。
- 完成的一轮、删掉所有任务，都会清掉 detail。
- 探针失败时显示桶名和端点。
- `/healthz`、`/api/health`、`/ui/` 和 Swagger 都返回 200，LATEST 任务跟上写入和 FLUSH。
- 对 `-w` 构建做 `govulncheck -mode=binary`，没有漏洞。

**#506 的打包证明（在 Tip QA 之后落地，只改 `.goreleaser.yml`）。**
- `goreleaser check` 通过。
- snapshot 构建出的 4 个包每个都有 257 项，其中 `docs/releases/` 下 206 个文件，包括 `adr-0011-pr2.md`。
- `govulncheck -mode=binary`（v1.9.0）：`binlog-server` 没有影响代码的漏洞，`migrate` 没有漏洞。
- CI 在 `b28f6d8` 上通过。
