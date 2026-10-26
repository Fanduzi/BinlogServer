# Binlog Server v0.5.80

Release date: 2026-10-26

v0.5.80 ships TLS for the source, the meta DB and S3 (#536, ADR 0012 `docs/adr/0012-connection-tls.md`). Replication connections can require and verify TLS per task (`source.tls`), the meta connection can do the same (`meta.tls`, also honoured by `./migrate --config`), and the S3 client now uses HTTPS by default and can trust a private CA (`upload.tls.ca_file`). **`upload.use_ssl` now defaults to `true`: read the first upgrade note before you upgrade a deployment that talks to a plain-HTTP object store.** Upload TLS failures are classified, keep their original text, turn `/api/health` 503 with reason `UPLOAD_TLS`, and are counted by the new metric `binlog_server_upload_tls_failures_total`. Fixes #537. No new migration (schema up to 10, `minRequiredSchemaVersion` still 6), and the upgrade floor stays v0.5.51. Production-readiness G2 is now MET; the checklist stands at MET 20 / PARTIAL 6 / NOT MET 9.

## Upgrade Notes

- **Breaking: `upload.use_ssl` now defaults to `true`. Set `upload.use_ssl: false` before upgrading if your object store speaks plain HTTP.** A config that sets `upload.endpoint` and omits `upload.use_ssl` used to speak HTTP and now speaks HTTPS. Against a plain-HTTP store (MinIO, Ceph RGW without TLS) every upload and HEAD fails the TLS handshake:
  - `/api/health` returns 503 with reason `UPLOAD_TLS`, for example `UPLOAD_TLS: object storage probe: tls: first record does not look like a TLS handshake; set upload.use_ssl: false or fix the endpoint`. The hint never mentions `upload.tls.ca_file` for this case.
  - Replication keeps running. Sealed segments stay on local disk and are marked `UPLOAD_FAILED`; `binlog_server_upload_tls_failures_total` goes up.
  - Startup shows a single WARN (the object storage probe) with that text. There is no separate "default changed" log line, so check `/api/health` right after the restart.
  - Fix: add `upload.use_ssl: false` and restart. Tip QA upgraded v0.5.79 in place with a plain-HTTP MinIO and no `use_ssl` key, hit exactly this 503, then set `upload.use_ssl: false` and restarted: every pending segment was uploaded before the first poll, `/api/health` returned 200, and all 8 objects matched the local files by sha256.
  - Configs that already set `upload.use_ssl: true` or `false` do not change. The v0.5.79 `config.example.yaml` already sets `use_ssl: false`.
  - `upload.endpoint` is `host[:port]` with no scheme. A value containing `://` (for example `http://minio:9000`) is now a startup error that explains the new default.
- **`source.tls` and `meta.tls` default to `disabled`.** A config with no TLS keys and existing tasks keep dialing exactly as before, including a `tls=` you already put in `meta_dsn`. Modes, matching the MySQL client `--ssl-mode`: `disabled`, `preferred`, `required`, `verify-ca` (chain must lead to `ca`), `verify-identity` (chain plus hostname, `server_name` or the dial host). `cert`/`key` give a client certificate on `required` and the verify modes. With any mode other than `disabled`, `meta_dsn` must not contain `tls=` (startup refuses it; move it to `meta.tls`). Environment variables follow the existing rule (`BINLOG_SERVER_META_TLS_MODE`, `..._CA`, `..._CERT`, `..._KEY`, `..._SERVER_NAME`). `source.tls` lives on the task (`POST/PUT /api/tasks`); an update that omits it keeps the stored block, and `"tls": {"mode": "disabled"}` clears it. Source CA and key files are read by the worker when it dials, so in cluster mode they must exist on every worker.
- **`preferred` can be downgraded.** `preferred` trusts the `CLIENT_SSL` bit in the server's plaintext greeting. Anyone who can modify that packet can clear the bit and the connection continues in plaintext without an error. The meta driver's `tls=preferred` has the same property. Use `required`, `verify-ca` or `verify-identity` where that matters. On a source without TLS, `preferred` continues in plaintext on the same socket, so it does not add `Aborted_connects` or host-cache errors and cannot trip `max_connect_errors` (ERROR 1129).
- **Certificate failures are explicit.** A source certificate failure makes the task `FAILED` with the x509 text in `last_error`; no plaintext Binlog Dump is opened. A meta certificate failure stops startup with an error prefixed `meta.tls`.
- **`./migrate --config /path/config.yaml`** now loads `meta_dsn` and applies the same `meta.tls` as the server, so a meta DB behind a private CA no longer needs skip-verify to migrate (#537). `--dsn` and `META_DSN` still override the file's `meta_dsn`; the TLS block still comes from the file. Without `--config` the DSN is used unchanged. Dial errors carry the `meta.tls` prefix.
- **No migration.** This release adds no migration: the highest schema is still 10 and `minRequiredSchemaVersion` is still 6. From v0.5.79, replace the binary (after fixing `upload.use_ssl` as above). The v0.5.79 notes still apply when upgrading from v0.5.78 or earlier.
- **Rollback.** v0.5.79 runs on the same schema and ignores the new keys, so the binary can be rolled back. Before you do, know what you lose: v0.5.79 does not read `meta.tls`, `source.tls` or `upload.tls.ca_file`, so those connections go back to plaintext (or the system trust store), and with `upload.use_ssl` unset it speaks HTTP again. v0.5.79 also rewrites a task's stored source on its next state change without the `tls` block, so after rolling forward again, check each task's `source.tls` and set it again where it is gone. If you rely on HTTPS for uploads, write `upload.use_ssl: true` explicitly so both binaries agree.

## Added

- **ADR 0012: TLS for the source, the meta DB and S3 (#536).**
  - `source.tls` (per task) and `meta.tls` (process config) with the five modes above, optional client certificate, and `server_name` for `verify-identity`. Key material never appears in logs, errors or API responses; the API returns paths only.
  - `upload.tls.ca_file`: optional PEM CA for the object store. When set it is the only trust store for the object client; hostname checking stays on. Rejected with `upload.use_ssl: false`. A missing or empty file is a startup error naming the path.
  - Upload TLS errors keep the original text and get one hint by type: plain HTTP → `upload.use_ssl: false`; `x509.HostnameError` → hostname mismatch; `x509.UnknownAuthorityError` → `upload.tls.ca_file`; protocol-version mismatches and `remote error: tls:` alerts → no hint, still `UPLOAD_TLS`. The startup probe returns the TLS text immediately instead of a 2 s timeout. Each upload call keeps its own error, so concurrent failures do not swap text.
  - `UPLOAD_TLS` in `/api/health` reflects the current state: it clears only after a checksum-matched upload or retry (or a probe result that replaces the TLS text). A non-TLS upload failure or an `UPLOAD_RECORD_FAILED` does not clear it.
  - New metric `binlog_server_upload_tls_failures_total` (counter, no labels): upload attempts that failed the TLS handshake.
  - CI: job `e2e-tls` runs on every PR and push to main against real MySQL and MinIO with TLS (source `verify-ca`, `migrate --config` with `meta.tls`, private-CA MinIO upload, wrong CA and wrong hostname rejected).

## Fixes

- **#537:** `cmd/migrate` did not support `meta.tls`, so a meta DB behind a private CA could only be migrated with skip-verify. Fixed by `./migrate --config` (see the upgrade notes).
- **#516:** install docs point at v0.5.79 with its checksums. The next pin PR moves them to v0.5.80.

## Known issues

- **#541 (P3):** test gaps from the #536 QA. Removing the `remote error: tls:` match or the protocol-version strings, or the per-call error save on the real `UploadFile`, leaves the test suite green. The behaviour is correct today; these are missing guards, not known bugs.
- **#540 (P3):** `TestStopStartUnderPollKeepsOpenEpoch` (`internal/replication`) fails now and then under heavy `-race` load. Test-only.
- **#510:** a non-owner node can keep listing a closed `SEGMENT_MISSING` in `/api/health` and `/api/findings` (and return 503). Check the owner's `/api/health` before acting on a non-owner's 503.
- **#473 (P1):** with per-node storage, a takeover after the owner pauses past the TTL hits `SEGMENT_NOT_ON_WORKER` and writes FAILED / desired STOP. ADR 0011 PR-4 is in progress.
- **#462 (P1):** the cluster silent gap is covered by unit tests; full closure comes with ADR 0011 PR-5.
- **#513 (P2):** after a takeover, a resumed old owner can write FAILED onto the new owner's task row; backup stops until an operator Start.
- **#539:** task start/stop opens more connections to the source than needed (not TLS related).
- **#514**, **#512**, **#490**, **#494**, **#500**, **#502**, **#503**, **#504** (P3) are still open, as are **#487**, **#488**, **#451**, **#452**, **#455**, **#456**, **#446**, **#437** and **#438**. `/api/findings` has no read timeout yet (P2-7). The verifier does not run in cluster mode (A4 PARTIAL).

## Changes since v0.5.79

`git log --oneline v0.5.79..e04e949` (28 commits), plus the commit of these notes, which v0.5.80 is tagged on:

| Commits | Change |
|---|---|
| `2199464` | PR #516: install docs point at v0.5.79 |
| `3225aa7` | PR #521: CI checks the version and commit the release binary reports (J2) |
| `818b68b`..`d15dae7` | PR #522: a tag is built and published only after the tagged commit passes every e2e job (J3); cluster and observability e2e on every PR |
| `ea85163`..`3c14fa7` | PR #527: `go vet` and `go test -race` on every PR (H2), real-MinIO verifier scenario on every PR |
| `5e4dc7e`..`239542d` | PR #525: object-only restore e2e on every PR (D2); the open-segment RPO is documented (#530) |
| `23f096a`..`e04e949` | PR #536: ADR 0012, TLS for the source, the meta DB and S3 |

Only #536 changes the server binary. The other PRs change CI, tests and docs. v0.5.80 is the first release built under the J3 gate: the release workflow reruns every per-PR CI job on the tagged commit before it builds anything.

## QA summary

**PR #536 QA PASS (BinlogServerQA), round 5 at `e04e9496`, 2026-10-26.** CI 19/19 on the head.
- Upload TLS failure kept as `UPLOAD_TLS` / 503 through an `UPLOAD_RECORD_FAILED`; a TLS 1.0/1.1-only endpoint gives 503 `UPLOAD_TLS` within 0.2 s with the original text, counter 0→2; a recovered endpoint uploads the backlog and clears `UPLOAD_TLS`.
- Concurrent upload failures keep their own text (300 runs under `-race`, also with 8 and 16 CPU hogs, 0 failures).
- Earlier rounds still hold: plain-HTTP and hostname hints are correct; four `preferred` tasks against a non-TLS source with `max_connect_errors=2` leave `Aborted_connects` and host-cache errors at 0, no ERROR 1129.

**Tip QA of `e04e9496` PASS (BinlogServerQA), 2026-10-26.**
- go1.26.10: `go test ./...` and `go test -race ./...` pass; push CI on main 17/17.
- Upgrade from the published v0.5.79 with a plain-HTTP MinIO and no `use_ssl` key: immediate 503 `UPLOAD_TLS` with the `use_ssl: false` hint, replication kept running, segments kept locally as `UPLOAD_FAILED`; after `upload.use_ssl: false` and a restart all pending segments uploaded and 8/8 objects matched by sha256.
- `source.tls`: `required`, `verify-ca`, `verify-identity` run with the Binlog Dump on TLSv1.3; wrong CA or `server_name` gives FAILED with the x509 text; `required` against a non-TLS source fails; `preferred` runs plaintext on a non-TLS source and TLS on a TLS source.
- `meta.tls` `verify-ca`: the process's meta connection is TLSv1.3; `./migrate --config` migrates to version 10 with the right CA and fails with `meta.tls:`-prefixed x509 errors on a wrong CA or hostname.
- `upload.tls.ca_file` against a private-CA MinIO: uploads `UPLOADED`, health 200, TLS failure counter 0.
- LATEST, FILE_POS and GTID tasks together, restart resume: 8 marker rows appear exactly once per task; `/healthz`, `/api/health`, `/ui/`, `/swagger/`, `/metrics` all 200.

## Chinese Release Notes

Chinese version:

# Binlog Server v0.5.80 中文发布说明

> 对应版本：`v0.5.80`

发布日期：2026-10-26

v0.5.80 带上源库、meta 库和 S3 的 TLS（#536，ADR 0012 `docs/adr/0012-connection-tls.md`）。复制连接可以按任务要求并校验 TLS（`source.tls`），meta 连接同样可以（`meta.tls`，`./migrate --config` 也用它），S3 客户端默认改走 HTTPS，并可以信任私有 CA（`upload.tls.ca_file`）。**`upload.use_ssl` 默认改为 `true`：连明文 HTTP 对象存储的部署，升级前先看第一条升级须知。** 上传的 TLS 失败按类型分类、保留原文，让 `/api/health` 返回 503（reason `UPLOAD_TLS`），并计入新指标 `binlog_server_upload_tls_failures_total`。修复 #537。没有新迁移（schema 最高 10，`minRequiredSchemaVersion` 仍为 6），升级地板仍是 v0.5.51。生产就绪清单 G2 达标，清单现为 MET 20 / PARTIAL 6 / NOT MET 9。

## 升级须知

- **不兼容：`upload.use_ssl` 默认改为 `true`。对象存储走明文 HTTP 的，升级前写上 `upload.use_ssl: false`。** 配了 `upload.endpoint`、没写 `upload.use_ssl` 的配置，以前走 HTTP，现在走 HTTPS。对明文 HTTP 存储（MinIO、没开 TLS 的 Ceph RGW），每次上传和 HEAD 都会在 TLS 握手失败：
  - `/api/health` 返回 503，reason 为 `UPLOAD_TLS`，例如 `UPLOAD_TLS: object storage probe: tls: first record does not look like a TLS handshake; set upload.use_ssl: false or fix the endpoint`。这种情况的提示不会提 `upload.tls.ca_file`。
  - 复制照常运行。封存段留在本地磁盘，标为 `UPLOAD_FAILED`；`binlog_server_upload_tls_failures_total` 上涨。
  - 启动时只有一条 WARN（对象存储探针）带这段文字，没有单独的「默认值已改」日志，所以重启后马上看一下 `/api/health`。
  - 修法：加上 `upload.use_ssl: false` 再重启。Tip QA 用明文 MinIO、不写 `use_ssl` 的配置原地升级 v0.5.79，正好遇到这个 503；写上 `upload.use_ssl: false` 重启后，所有待传段在第一次轮询前就传完，`/api/health` 回到 200，8 个对象与本地文件 sha256 全部一致。
  - 已经写成 `upload.use_ssl: true` 或 `false` 的配置不变。v0.5.79 的 `config.example.yaml` 本来就写了 `use_ssl: false`。
  - `upload.endpoint` 是不带协议的 `host[:port]`。值里带 `://`（例如 `http://minio:9000`）现在启动失败，错误里说明了新的默认值。
- **`source.tls` 和 `meta.tls` 默认 `disabled`。** 不写 TLS 键的配置和已有任务照旧连接，`meta_dsn` 里已经写的 `tls=` 也照旧生效。模式与 MySQL 客户端 `--ssl-mode` 一致：`disabled`、`preferred`、`required`、`verify-ca`（证书链必须通到 `ca`）、`verify-identity`（证书链加主机名，取 `server_name`，为空则取拨号主机）。`required` 和两个 verify 模式可以用 `cert`/`key` 带客户端证书。模式不是 `disabled` 时，`meta_dsn` 不能再带 `tls=`（启动拒绝，改用 `meta.tls`）。环境变量沿用原规则（`BINLOG_SERVER_META_TLS_MODE`、`..._CA`、`..._CERT`、`..._KEY`、`..._SERVER_NAME`）。`source.tls` 在任务上（`POST/PUT /api/tasks`）；更新时不带它会保留已存的块，`"tls": {"mode": "disabled"}` 清掉它。源库的 CA 和密钥文件由 worker 在拨号时读取，集群模式下每个 worker 上都要有。
- **`preferred` 可以被降级。** `preferred` 信任服务端明文问候包里的 `CLIENT_SSL` 位。能改这个包的人把这一位清掉，连接就以明文继续，不报错。meta 驱动的 `tls=preferred` 也一样。需要防护的地方用 `required`、`verify-ca` 或 `verify-identity`。在没有 TLS 的源上，`preferred` 在同一条连接上继续明文，不增加 `Aborted_connects` 和 host cache 错误，不会触发 `max_connect_errors`（ERROR 1129）。
- **证书失败会明确报出。** 源库证书校验失败使任务 `FAILED`，`last_error` 带 x509 原文，不会打开明文 Binlog Dump。meta 证书失败使启动失败，错误以 `meta.tls` 开头。
- **`./migrate --config /path/config.yaml`** 现在读取 `meta_dsn` 并套用与服务端相同的 `meta.tls`，私有 CA 的 meta 库迁移不再需要 skip-verify（#537）。`--dsn` 和 `META_DSN` 仍覆盖文件里的 `meta_dsn`，TLS 块仍取自文件。不传 `--config` 时 DSN 原样使用。拨号错误带 `meta.tls` 前缀。
- **没有迁移。** schema 最高仍为 10，`minRequiredSchemaVersion` 仍为 6。从 v0.5.79 升级只需替换二进制（先按上面处理好 `upload.use_ssl`）。从 v0.5.78 或更早版本升级，v0.5.79 的说明仍然适用。
- **回滚。** v0.5.79 在同一个 schema 上运行，并忽略新配置项，所以可以回滚二进制。回滚前要知道会失去什么：v0.5.79 不读 `meta.tls`、`source.tls` 和 `upload.tls.ca_file`，这些连接会回到明文（或系统信任库）；没写 `upload.use_ssl` 时它又会走 HTTP。v0.5.79 还会在任务下一次状态变化时，把不带 `tls` 块的源配置写回存储，所以再次升级后要逐个检查任务的 `source.tls`，丢了就重新设置。依赖 HTTPS 上传的，显式写 `upload.use_ssl: true`，两个版本的行为才一致。

## 新增

- **ADR 0012：源库、meta 库和 S3 的 TLS（#536）。**
  - `source.tls`（按任务）和 `meta.tls`（进程配置），五种模式见上，可选客户端证书，`verify-identity` 可指定 `server_name`。密钥内容不会出现在日志、错误和 API 响应里，API 只返回路径。
  - `upload.tls.ca_file`：对象存储的可选 PEM CA。设置后它是对象客户端唯一的信任库，主机名仍然检查。与 `upload.use_ssl: false` 同时设置会被拒绝。文件不存在或为空时启动失败，错误写出路径。
  - 上传的 TLS 错误保留原文，按类型加一句提示：明文 HTTP → `upload.use_ssl: false`；`x509.HostnameError` → 主机名不匹配；`x509.UnknownAuthorityError` → `upload.tls.ca_file`；协议版本不匹配和 `remote error: tls:` 这类 alert → 不加提示，仍算 `UPLOAD_TLS`。启动探针直接返回 TLS 原文，不写成 2 秒超时。每次上传调用各自保存错误，并发失败不会串文本。
  - `/api/health` 的 `UPLOAD_TLS` 反映当前状态：只有校验和匹配的上传或补传成功（或探针结果替换了 TLS 文本）才清掉。非 TLS 的上传失败和 `UPLOAD_RECORD_FAILED` 都不清。
  - 新指标 `binlog_server_upload_tls_failures_total`（counter，无 label）：TLS 握手失败的上传次数。
  - CI：`e2e-tls` job 在每个 PR 和 push main 上对真实的 TLS MySQL 和 MinIO 运行（源库 `verify-ca`、带 `meta.tls` 的 `migrate --config`、私有 CA 的 MinIO 上传，错误 CA 和错误主机名被拒绝）。

## 修复

- **#537：** `cmd/migrate` 不支持 `meta.tls`，私有 CA 的 meta 库只能用 skip-verify 迁移。由 `./migrate --config` 修复（见升级须知）。
- **#516：** 安装文档指向 v0.5.79 和它的校验和。下一个固定版本的 PR 会把它们改到 v0.5.80。

## 已知问题

- **#541（P3）：** #536 QA 留下的测试缺口。去掉 `remote error: tls:` 匹配或协议版本字符串，或去掉真实 `UploadFile` 上按调用保存错误的逻辑，测试仍然全绿。当前行为是对的，缺的是守护测试，不是已知缺陷。
- **#540（P3）：** `TestStopStartUnderPollKeepsOpenEpoch`（`internal/replication`）在高负载 `-race` 下偶发失败。只影响测试。
- **#510：** 非 owner 节点可能在 `SEGMENT_MISSING` 关闭后，仍在 `/api/health` 和 `/api/findings` 里列着它（并返回 503）。根据非 owner 的 503 行动之前，先看 owner 的 `/api/health`。
- **#473（P1）：** 每节点独立存储时，owner 暂停超过 TTL 后接管会遇到 `SEGMENT_NOT_ON_WORKER` 并写 FAILED / desired STOP。ADR 0011 PR-4 正在做。
- **#462（P1）：** 集群下的静默缺口有单测覆盖；完整关闭要等 ADR 0011 PR-5。
- **#513（P2）：** 接管后，恢复运行的旧 owner 可能把 FAILED 写到新 owner 的任务行上，备份停到运维 Start。
- **#539：** 任务 start/stop 对源库的连接扇出偏大（与 TLS 无关）。
- **#514**、**#512**、**#490**、**#494**、**#500**、**#502**、**#503**、**#504**（P3）仍未关闭，**#487**、**#488**、**#451**、**#452**、**#455**、**#456**、**#446**、**#437**、**#438** 也是。`/api/findings` 还没有读超时（P2-7）。集群模式不跑巡检（A4 PARTIAL）。

## 自 v0.5.79 以来的变更

`git log --oneline v0.5.79..e04e949`（28 个提交），加上这份说明所在的提交（v0.5.80 打在这个提交上）：

| 提交 | 变更 |
|---|---|
| `2199464` | PR #516：安装文档指向 v0.5.79 |
| `3225aa7` | PR #521：CI 检查发布二进制报告的版本和提交（J2） |
| `818b68b`..`d15dae7` | PR #522：tag 所在提交通过全部 e2e job 后才构建和发布（J3）；集群和可观测性 e2e 每个 PR 都跑 |
| `ea85163`..`3c14fa7` | PR #527：每个 PR 跑 `go vet` 和 `go test -race`（H2），真实 MinIO 巡检场景每个 PR 都跑 |
| `5e4dc7e`..`239542d` | PR #525：只剩对象存储时的还原 e2e 每个 PR 都跑（D2）；open 段的 RPO 写进文档（#530） |
| `23f096a`..`e04e949` | PR #536：ADR 0012，源库、meta 库和 S3 的 TLS |

只有 #536 改了服务端二进制，其余 PR 改的是 CI、测试和文档。v0.5.80 是第一个在 J3 门禁下构建的版本：发布 workflow 在构建之前，先在 tag 所在提交上重跑每个 PR 的全部 CI job。

## QA 摘要

**PR #536 QA PASS（BinlogServerQA），第 5 轮 `e04e9496`，2026-10-26。** 头上 CI 19/19。
- 上传 TLS 失败经过一次 `UPLOAD_RECORD_FAILED` 仍保持 `UPLOAD_TLS` / 503；只支持 TLS 1.0/1.1 的端点 0.2 秒内返回 503 `UPLOAD_TLS` 并保留原文，计数 0→2；端点恢复后积压的段传完，`UPLOAD_TLS` 清掉。
- 并发上传失败各自保留文本（`-race` 下 300 次，另在 8 和 16 个 CPU 占满时，0 失败）。
- 之前几轮的结论仍成立：明文端点和主机名的提示正确；4 个 `preferred` 任务连 `max_connect_errors=2` 的无 TLS 源，`Aborted_connects` 和 host cache 错误都是 0，没有 ERROR 1129。

**main `e04e9496` 的 Tip QA PASS（BinlogServerQA），2026-10-26。**
- go1.26.10：`go test ./...` 和 `go test -race ./...` 通过；main 上 push CI 17/17。
- 用已发布的 v0.5.79、明文 MinIO、不写 `use_ssl` 升级：立即 503 `UPLOAD_TLS` 并提示 `use_ssl: false`，复制继续，段留在本地标为 `UPLOAD_FAILED`；写上 `upload.use_ssl: false` 重启后待传段全部上传，8/8 个对象 sha256 一致。
- `source.tls`：`required`、`verify-ca`、`verify-identity` 运行，Binlog Dump 为 TLSv1.3；CA 或 `server_name` 错误时 FAILED 并带 x509 原文；`required` 连无 TLS 源失败；`preferred` 对无 TLS 源走明文，对 TLS 源走 TLS。
- `meta.tls` `verify-ca`：进程的 meta 连接为 TLSv1.3；`./migrate --config` 用正确 CA 迁到 version 10，CA 或主机名错误时报带 `meta.tls:` 前缀的 x509 错误。
- `upload.tls.ca_file` 对私有 CA 的 MinIO：上传为 `UPLOADED`，health 200，TLS 失败计数 0。
- LATEST、FILE_POS、GTID 三个任务同时跑，重启续传：8 个标记行在每个任务里恰好出现一次；`/healthz`、`/api/health`、`/ui/`、`/swagger/`、`/metrics` 均 200。
