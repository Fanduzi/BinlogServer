# E2E 脚本说明

本目录用于本项目的端到端测试，覆盖：

- 基础拉流与 checkpoint 推进
- MySQL 8.0 压缩事务与 binlog 文件一致性校验
- orchestrator 拓扑发现行为
- semi-sync ACK/阻塞语义
- metadata MySQL failover（Percona57 主从 + ProxySQL + orchestrator）
- cluster 角色分离（control-plane + worker）与 worker heartbeat 在线/离线恢复
- 控制回路：Stop 过程中改源密码再 Start 只留一条 Binlog Dump；源可达时状态变成 STOPPED 则源上已经没有这条 dump；掐断到源的代理后 Stop 写成 STOPPED 并在 API 上留下 `pending_dump_cleanup`，代理恢复后标记清掉且 dumps 为 0；两个 worker 上把代理握成黑洞再 Stop 然后立刻 Start，源上始终至多一条 Binlog Dump，路径恢复后 pending 清掉且 dumps 为 1（`[dump-fence]`）；v0.5.45 留下的 RUNNING/`desired_run=STOP` 任务升级后继续跑；先 `down --steps 1` 从 schema 6 回到 schema 5，`uk_task_file_epoch` 回来且 `pending_dump_cleanup` 仍在；再 down 回到 schema 4 时这一列仍在，再 down 一次回到 schema 3 且这一列消失，当前二进制在 schema 3 上以退出码 1 结束，日志含 `schema version too old` 和 `./migrate up`

## 依赖

运行前请确保本机有：

- `docker`（含 `docker compose`）
- `curl`
- `jq`
- `go`

可选：

- `make`（用于快捷命令）

兼容性说明：

- `run-server.sh` 构建的临时 Linux 测试二进制默认关闭 CGO，用于贴近 release 产物的 `glibc 2.17` 兼容基线。
- `smoke-retry-upload` 和 `smoke-split-retention` 从 Quay 拉 MinIO / `mc`（`MINIO_IMAGE` / `MC_IMAGE` 可覆盖）。Docker Hub 的 `minio/minio` 与 `minio/mc` 已拒绝匿名拉取。Quay 返回 401 时两者都改用 GitHub release 的同版本 linux 二进制（`MINIO_RELEASE` / `MC_RELEASE`）。

## 推荐入口

优先使用统一入口脚本 `run-suite.sh`：

```bash
# 日常回归（默认）：smoke-least-privilege-migrate + smoke-task-desired-migration + smoke + compression + smoke-source-outage + smoke-unreachable-giveup + smoke-control-loop + smoke-fail-alert
smoke-version-restore + compression + smoke-source-outage + smoke-unreachable-giveup + smoke-control-loop + smoke-fail-alert
./scripts/e2e/run-suite.sh

# 全量回归：smoke + compression + orchestrator + semisync + meta-failover
./scripts/e2e/run-suite.sh --profile full

# 自定义场景
./scripts/e2e/run-suite.sh --scenarios smoke,compression
./scripts/e2e/run-suite.sh --scenarios orchestrator,semisync
./scripts/e2e/run-suite.sh --scenarios meta-failover
./scripts/e2e/run-suite.sh --scenarios meta-failover-override
./scripts/e2e/run-suite.sh --scenarios smoke-observability
./scripts/e2e/run-suite.sh --scenarios smoke-cluster-roles
./scripts/e2e/run-suite.sh --scenarios smoke-control-plane-failover
./scripts/e2e/run-suite.sh --scenarios smoke-worker-crash-recovery
./scripts/e2e/run-suite.sh --scenarios smoke-invalid-inputs
./scripts/e2e/run-suite.sh --scenarios smoke-retry-upload
./scripts/e2e/smoke-verify.sh
./scripts/e2e/run-suite.sh --scenarios smoke-source-outage
./scripts/e2e/run-suite.sh --scenarios smoke-scale
./scripts/e2e/run-suite.sh --scenarios smoke-task-desired-migration
```

也可用 `Makefile`：

```bash
make e2e-quick
make e2e-full
make e2e-observability
make e2e-scale
make e2e SCENARIOS=smoke,compression
make e2e-topology-check
```

## 脚本列表

- `up.sh`: 启动 5 个 source MySQL（`mysql57/mysql80/mysql84/percona57/percona80`）和独立的 `meta-primary`，并等待可用。`mysql84` 是 MySQL 8.4 LTS。
- `run-server.sh`: 用 e2e 配置启动 `binlog-server`，并以 `CGO_ENABLED=0` 构建临时测试二进制，避免 Linux 测试环境绑定构建机 glibc。模块 zip 拉取失败时最多构建 3 次，成功才 exec。
- `down.sh`: 清理 e2e 容器与 volume。
- `smoke.sh`: 创建并启动 5 个基础任务（MySQL 5.7、8.0、8.4 LTS、Percona 5.7、Percona 8.0），等 LATEST 的 checkpoint 落盘后再写入，并要求 checkpoint 的 file/pos 离开写入前的值。
- `smoke-version-restore.sh`: MySQL 5.7 和 MySQL 8.4 LTS。任务从当时的尖端启动并追上之后，建库建表、插入、更新、删除、`FLUSH BINARY LOGS`、再插入。停任务后把 `GET /replay` 的全部分段用同系列 `mysqlbinlog`（官方 `mysql` 镜像没有这个客户端，5.7 用 Percona Server 5.7，8.4 用 Percona Server 8.4）回放到一台同版本空实例。`CHECKSUM TABLE` 和按主键排序的行文本必须与源库一致。在 quick 档（CI `unit-and-e2e`）里运行。
- `smoke-compression.sh`: 验证压缩事务场景，主动 rotate 并对比源端与备份 binlog 的 md5。
- `smoke-orchestrator.sh`: 验证 orchestrator 拓扑里是否误纳入 binlog 拉流客户端。
- `smoke-semisync.sh`: 验证 `semi_sync=true` 时的 client 挂载与停任务后主库提交阻塞到 timeout。
- `setup-meta-replication.sh`: 初始化 meta-primary/meta-replica GTID 主从复制与 ProxySQL 监控账号。
- `lib-topology.sh`: 解析并校验统一的数据库 host、端口、共享凭据和 metadata DSN。
- `topology-contract-test.sh`: 无需 Docker，验证拓扑默认值、覆盖优先级、direct/failover 选择、端口校验及 Compose 兜底一致性。
- `smoke-meta-failover.sh`: 触发 orchestrator 切主，验证元数据库 failover 后 checkpoint 继续推进。
- `smoke-meta-failover-override.sh`: 用非默认 `E2E_API/E2E_ORC_API` 地址（localhost）覆盖并执行 failover 场景。
- `smoke-observability.sh`: 验证 `/metrics` 核心指标存在，且 `task_state_count` 与 `checkpoint_age_seconds` 随状态/时间变化；设了 `E2E_OTLP_SINK_DIR` 时还要求收到 API span（服务名、`POST /api/tasks/*path`、本任务 start 的 `http.target`）。
- `run-observability.sh`: `make e2e-observability` 和 CI `e2e-observability` 的入口。先起 `otlp-sink.py`（默认端口 14318，`E2E_OTLP_PORT` 可改），用 `BINLOG_SERVER_TRACING_*` 打开 OTLP/HTTP tracing（采样 1），再跑 `smoke-observability`。
- `otlp-sink.py`: 只给测试用的 OTLP/HTTP 收集器，把每次 `/v1/traces` 导出原样写成一个文件。
- `smoke-cluster-roles.sh`: 启动 control-plane + worker 双进程，验证任务执行、worker 离线检测与恢复链路。
- `smoke-control-plane-failover.sh`: 验证 control-plane 崩溃/重启期间 worker 持续拉流，checkpoint 不中断推进。
- `smoke-worker-crash-recovery.sh`: 模拟 worker 在 OPEN 期间崩溃，验证新 worker 接管后一致性（checkpoint 推进、stale OPEN 清理、sealed 文件与 md5 校验）。
- `smoke-invalid-inputs.sh`: 验证任务 API 对非法输入返回 `400`（cluster_key/source/start/storage）。
- `smoke-verify.sh`: ADR 0009 阶段 3。自己起 MinIO（Quay 镜像，拉不到就用 GitHub release 二进制），跑 `TestVerifyAgainstMinIO`：三个封存段上传后，删一个对象、改一个对象的一个字节、改本地一个字节，下一轮三条都报出来。不连接源库，不在 quick 或 full 里；`e2e.yml` 的 `e2e-verify` job 每个 PR 跑它（测试被跳过也算失败）。直接 `./scripts/e2e/smoke-verify.sh`，或 `run-suite.sh --scenarios smoke-verify`。
- `smoke-retry-upload.sh`: 验证上传失败不阻断拉流；MinIO 恢复后，已封存的 `UPLOAD_FAILED` 由后台补传变成 `UPLOADED`，场景本身不调用 `/api/tasks/{id}/files/retry-upload`。open 分段不会变成 `UPLOADED`。补传成功的封存文件 `checksum` 为 `match`；同一 MinIO 上内容不同的对象为 `mismatch`，且不让上传调用方失败。桶不可用时，把已封存失败文件的 mtime 拨到保留期之外再 `FLUSH BINARY LOGS`，文件、目录行和复制都留着，并只记一条 `RETENTION_SKIPPED_NOT_UPLOADED`；桶恢复并上传成功后，下一次打开文件会把它清掉。
- `smoke-split-retention.sh`: 验证桶保留短于本地保留时创建任务返回 400；只配 `storage.retention_days` 的响应不含新字段，过期已上传文件仍同时删对象、目录行和本地文件；`local_retention_days` 短于 `bucket_retention_days` 时，介于两者之间的已上传文件只删本地，`location` 为 `bucket`，下载、`replay`、`stop_datetime` 和 `replay/archive` 仍读到对象。
- `smoke-recovery-window.sh`: MySQL 8.0 上 GTID 打开。建库建表后任务从当时的 `GTID_EXECUTED` 启动，三次插入并 `FLUSH BINARY LOGS`，停任务。`GET /api/tasks/{id}/window` 是连续的，`earliest`、`latest`、`gtid_set` 都有，`breaks` 为空。`latest` 之后一秒的 `stop_datetime` 得到非空回放命令；`earliest` 之前一秒是空命令。然后删掉中间一条封存分段的本地文件和 `binlog_files` 行。窗口变为不连续，`reason` 含 `missing source file`，`/metrics` 上 `binlog_server_recovery_breaks{task_id}` 至少为 1，并且仍有 `binlog_server_recovery_earliest_age_seconds`。同一较晚停止时间的回放路径不再包含被删的文件名。不在 quick profile 里。场景里关掉 `binlog_transaction_compression`，结束时恢复为 ON。不拉 MinIO。
- `smoke-gtid-pitr.sh`: MySQL 8.0 上 GTID 打开。先建库建表，任务从当时的 `GTID_EXECUTED` 启动，等这张表进入 checkpoint 后 `FLUSH BINARY LOGS`，让建表分段封存。然后在同一秒里插入三行、一条不带 WHERE 的 `DELETE`、再插入两行，下一秒再插入一行。`GET /api/tasks/{id}/replay?stop_gtid=<坏事务>` 在 `.open.e*` 上返回 `--stop-position`，`limit=1` 仍包含前面的封存分段。把这条命令管道到一台新的空 MySQL 8.0：三行好数据各一行，`DELETE` 没有执行，后面的行不在。再 `FLUSH` 封存这条 GTID 所在的文件，并在新文件里写一行。同一 `stop_gtid` 的 `--stop-position` 不变，路径变成封存名，新文件不在列表里。再用封存后的命令恢复一台空 MySQL 8.0，结果相同。无法解析、序号或 UUID 不在备份里、MariaDB 形状、非 mysql flavor、两个停止参数同时出现、开始时间晚于这条 GTID，都是文档里的纯文本 400。`segment not found on this process` 仍由单测覆盖。场景里关掉 `binlog_transaction_compression`，结束时恢复为 ON。不拉 MinIO。空恢复实例只在 TCP 登录成功，并且 `RESET MASTER`（8.4 起为 `RESET BINARY LOGS AND GTIDS`）之后 `gtid_executed` 为空时才接收回放。官方镜像会先拉起一个只监听 socket 的临时初始化进程，root 密码生效后随即退出；对 socket 做一次 `SELECT 1` 再立刻 `RESET` 会撞上关闭，两条语句都返回 2002。
- `smoke-coverage.sh`: MySQL 8.0 上 GTID 打开，压缩关掉。建库建表之后再写五条，任务从当时的 `GTID_EXECUTED` 启动。五个分段各四条 `SET TIMESTAMP` 插入，时间取源库时钟 120 秒之后，避免 `mysqlbinlog --stop-datetime` 停在更早的格式描述上。每段都 `FLUSH BINARY LOGS` 后等封存（停任务不会把最后一段收成 SEALED）。末尾多出来的 OPEN 文件可以留下。用 Percona 8.0 的 `mysqlbinlog`（`TZ=UTC`）独立解析每个封存文件：窗口内、窗口外（早于最早复制事件，或晚于最晚复制事件）、断点上的时间和事务 GTID（`uuid:seq`，不含 mysqlbinlog 在文件末尾补的 `AUTOMATIC`）各 10 例，`GET /coverage` 的 `covered`、原因、分段名和停止偏移必须一致，`limit=1` 不截断。五个分段各自最后一个复制事件的时间走 `POST /restore-plan`，按 `--stop-datetime` 回放到一台空 MySQL 8.0（先建同结构的空表），`CHECKSUM TABLE` 与源库在该分段写完时的值一致。全备 GTID 换成另一个 UUID 时预检 red；换成计划将要应用的集合时也是 red。不拉 MinIO。结束时把压缩恢复为 ON。
- `smoke-gtid-executed.sh`: MySQL 8.0 上先写几条任务启动前的事务，记下更老的 GTID 集合，`FLUSH BINARY LOGS` 后再记下启动点，使保留的第一段已经落在更老集合之后。任务从启动点的 `GTID_EXECUTED` 开始。建库插入两行后 `mysqldump --set-gtid-purged=ON`，记下这时的已执行集合，再 `FLUSH BINARY LOGS` 封存这一段。之后把第 3、4 行的事件头写到分段头之后两分钟，停止事务和第 6 行再晚 5 秒，停止时刻取第 3、4 行的下一秒，这样 `mysqlbinlog --stop-datetime` 不会停在格式描述上。`start_gtid_set` 加 `stop_gtid` 的命令用 `--exclude-gtids`，封存的中点文件不在 `paths` 里。把返回的命令管道到一台先导入这份 dump 的 MySQL 8.0，行是 1 到 4，停止事务和更后的行不在。同一集合加 `stop_datetime` 得到同样的行。集合已经盖住停止点之前的每一条事务时，HTTP 200，`paths` 为空，`command` 为空，`note` 说明已经包含。更老的集合、无法解析的集合、和非 mysql flavor，是文档里的纯文本 400。桶上的分段仍由单测覆盖。场景里关掉 `binlog_transaction_compression`，结束时恢复为 ON。不拉 MinIO。空恢复实例只在 TCP 登录成功，并且 `RESET MASTER`（8.4 起为 `RESET BINARY LOGS AND GTIDS`）之后 `gtid_executed` 为空时才接收回放。官方镜像会先拉起一个只监听 socket 的临时初始化进程，root 密码生效后随即退出；对 socket 做一次 `SELECT 1` 再立刻 `RESET` 会撞上关闭，两条语句都返回 2002。
- `smoke-source-switchover.sh`: 两台 MySQL 8.0 组成 GTID 复制，任务连的是宿主机上的一个 TCP 代理。先在旧主库写入，GTID 任务从建表前的 `GTID_EXECUTED` 启动，`LATEST` 任务从当时的尖端启动。提升副本、把代理指到新主库、再写入。GTID 任务继续，旧分段封存且不含切换后的行。新主库的分段文件名以新 `server_uuid` 开头；其中一份含切换后的行。新主库更早的 binlog（例如初始化时的时区数据）可以另成一份，但不包含切换后的行，也不会把旧主库的文件覆盖掉。GTID 重拉回到更早文件时不另开第二份。`SOURCE_SWITCHOVER` 事件写出两台身份。`GET /window` 连续。`stop_datetime` 和 `stop_gtid` 的 `paths` 都是旧主库文件在前、新主库文件在后。把 `stop_datetime` 的命令回放到一台空 MySQL 8.0，行和 `CHECKSUM TABLE` 与新主库一致。`stop_gtid` 停在最后一行之前，那一行不在恢复库里。`LATEST` 任务变成 `FAILED`，`last_error` 以 `SOURCE_SWITCHOVER` 开头，点名两台身份，并说明没有 GTID、要新建任务。`GET /api/tasks/{id}` 的 `source_chain.outcome` 在 GTID 任务上是 `continued`、`current` 是新身份；在 `LATEST` 上是 `stopped`、`reason` 是 `no_gtid`、`current` 仍是旧身份。`GET /api/tasks/{id}/files` 的 `source_identity` 与文件名前缀一致，无前缀的文件属于旧身份。`binlog_server_source_switchovers{task_id,outcome}` 在继续的任务上 `continued` 至少为 1、`stopped` 为 0，在停下的任务上相反。嵌入的 `/ui/` 包里有 `task-source-chain`。再把代理指回旧主库并写入只有旧主库才有的行：GTID 任务 `FAILED`，错误说明新主库缺了备份里已有的事务，已有分段的字节不变。这时 `outcome` 是 `stopped`，`reason` 是 `missing_transactions`，最新切换是从新身份到旧身份，`current` 仍是新身份，`.source-chain` 仍是旧身份在前、新身份在后，两个 `outcome` 都至少为 1。之前停下的 `LATEST` 任务再 Start 两次：原始 `SOURCE_SWITCHOVER` 事件变成 3 条，`source_chain.switches` 仍是 1 条，`stopped` 仍是 1。然后按 DBA 的正常回切：旧主库以新主库为源 `AUTO_POSITION=1` 追平，停复制，代理指回旧主库，再 Start GTID 任务：任务 `RUNNING`，`source_chain.servers` 是旧、新、旧三段，第三段 `prefix` 是 `{旧身份}~2` 且为 current，`outcome` 是 `continued`，之前那次停止 `resolved`，`.source-chain` 三行，`continued` 为 2、`stopped` 为 0。回切后写入的行只在 `{旧身份}~2.` 文件里，这些文件的 `source_server` 是 3。`GET /window` 连续。`stop_datetime` 的 `paths` 顺序是无前缀、新身份、`{旧身份}~2`，回放到空 MySQL 后行和校验和与旧主库一致。再 Stop/Start 两次：没有新的 `SOURCE_SWITCHOVER`，链仍三行，没有 `SEALED_FILE_EXISTS`。最后 Start `LATEST` 任务：`RUNNING`，`outcome` 是 `resumed`，`stopped` 回到 0。再普通 Stop 这条 `LATEST` 任务：`STOPPED`，`outcome` 仍是 `resumed`，那次停止仍 `resolved`，`stopped` 仍是 0。不在 quick profile 里。不拉 MinIO。代理连上后端后清掉 `create_connection` 的 10 秒超时（`settimeout(None)`），否则空闲的 binlog dump 每 10 秒被切断。脚本用 `set -Eeuo pipefail` 和 ERR trap，失败时打印退出码、行号、命令和函数栈（#392）；mysql 辅助函数不再丢弃 stderr，只过滤命令行密码警告。
- `upgrade-legacy-failback.sh`: 升级路径复现（QA 6c88ad5b P1），不在 `run-suite.sh` 的任何 profile 里；`e2e.yml` 的 `e2e-upgrade-failback` job 每个 PR 跑矩阵：`plain`、`object-only`、`object-gone` 从 `v0.5.57`，以及 `plain` 从 `v0.5.82`（`FBUP_SKIP_MID=1`、`FBUP_REQUIRE_REUSE=0`），手动触发 `e2e.yml` 时选 `e2e_profile=upgrade-failback` 只跑这一个 job（`0a84917a` 是 PR #293 的合并 commit，不在任何分支上，job 先单独 fetch；宿主机要有 MySQL 8.0 的 `mysqlbinlog` 和 PyMySQL）。宿主机网络上起元数据 MySQL、两台 GTID MySQL 8.0（A、B）、MinIO 和一个可切换后端的 TCP 代理。默认从 `FBUP_OLD_REF`（`v0.5.57`）、`FBUP_MID_REF`（`0a84917a`，`FBUP_SKIP_MID=1` 时不编）和当前检出各编一个 binlog-server（`FBUP_OLD_BIN`、`FBUP_MID_BIN`、`FBUP_NEW_BIN` 可直接给二进制）。schema 11 把 `worker_heartbeats.version` 改名为 `build_version`，所以编号小于 11 的迁移先给 OLD 和 MID；两者退出并且 `worker_registrations` 没有未过期租约之后，当前检出的 migrate 才执行 000011，v0.5.57 启动时仍能看到 `version` 列。`FBUP_REQUIRE_REUSE` 默认 `1`：要求旧版本复用 A 第一段的文件名；`0` 时没有这份复用仍按 plain 断言续传和校验和（从 `v0.5.82` 直接升级）。在 A 上写 mysqldump 种子，GTID 任务从种子集合启动；A 写 200 个事务（INSERT/UPDATE/DELETE），提升 B、代理指向 B 写 200，A 以 B 为源追平后再提升 A、代理指回 A 写 200（旧版本在这里复用 A 第一段的文件名）。旧版本重启一次，再换 MID、再换 NEW，每一步打印目录、`.source-chain` 和 `binlog_files` 行。NEW 上打印 `storage_alert`、`/files`、`/replay`、`/window`、Start 结果和 `STORAGE_REPAIRED` 事件，等后台补传把修复标记的行传完，再用宿主机上的 MySQL 8.0 `mysqlbinlog`（`FBUP_MYSQLBINLOG`，MariaDB 的不行）把 `/replay` 回放到一台从种子恢复的空 MySQL，比较 `CHECKSUM TABLE`。`FBUP_EXPECT=repaired`（默认）断言任务 `RUNNING`、链是 A,B,A、没有 `storage_alert` 和回放 `warning`、校验和与 A 一致；`report` 只打印（用旧的 NEW 复现问题时用）；`refused` 断言 `FAILED`、`storage_alert`、回放 `warning`，以及拒绝前后目录行和 MinIO 对象都不变。`FBUP_CASE=plain`（默认）保持旧版本留下的目录；`object-only` 在换 NEW 前删掉本地第一段那份文件（MinIO 里的对象还在），断言 Start 取回后修复、文件 md5 与原来一致、`/window` 连续、恢复校验和一致（QA bbc2c88b）；换 NEW 前删第一段用的是 mv（模拟本地那份丢了）。随后是 D2：任务创建时配了 `local_retention_days=1`、`bucket_retention_days=30`；先在 A 上 `FLUSH BINARY LOGS` 定封存边界，等边界之前每一段都 SEALED 且 UPLOADED、唯一没封存的是边界后新开的段，记下 A 在边界的 `CHECKSUM TABLE` 和 GTID；把这些段的修改时间调到 2 天前再 `FLUSH BINARY LOGS`，由产品的拆分保留删掉本地文件（对象和目录行留下，打印 `RETENTION_REMOVED` 数；产品留下文件时等补传核对后重试，三次都留下就失败），脚本自己不 mv、不删；之后在 A 上再写 30 个事务，只落在本地 open 段。接着后台跑 `mc admin trace --json`，要求 `/replay` 把每个被删段标成 `bucket`，下载 `/replay/archive`，成员与删前的拷贝逐字节一致、被删段排在最前、第一段 md5 不变；归档读完和还原之后各扫一遍 `data_dir`（递归，含 `.cache` 等点目录）、服务端 `TMPDIR`（脚本给每个服务端设成 `$FBUP_WORK/tmp`）和 `/tmp` 下比删除更新的文件，有被删段的同字节副本就失败（只扫这三处）；只用 bucket 成员还原进清空的还原库，`CHECKSUM TABLE` 和 `gtid_executed` 必须等于 A 在边界的值；再叠加边界之后的本地成员（空的封存段和 open 段），必须等于 A 的最终校验和（这一步读本地文件）；最后要求每个被删段的对象键至少有一次 MinIO `GetObject`；`object-gone` 再删掉那个对象，默认期望 `refused`。每种情况都断言 NEW 起来 20 秒（超过一次后台补传）后、Start 之前，目录行和 MinIO 对象没变。对象用 MinIO 容器里自带的 `mc` 列出和删除（临时别名，不写 mc 配置）。`FBUP_MODE=cluster`（默认）或 `standalone`（单机带元数据库：重启后 MemoryLease 从 epoch 1 开始，NEW 必须在修复后的 `.sealed.eN` 之上续写，否则 open 行覆盖封存行、`/replay` 静默缺一段，QA #404）。`FBUP_KEEP=1` 保留工作目录。
- `smoke-gtid-purge.sh`: MySQL 8.0 上以当前 `GTID_EXECUTED` 启动任务（RawMode 仍是服务端默认）。连续三次自动提交后，checkpoint 的 `gtid_set` 必须包含当时的 `GTID_EXECUTED`（按 `server_uuid` 的区间比较，并用 `GTID_SUBSET` 核对；UUID 里的数字不算区间）。停止后 `FLUSH BINARY LOGS` 再 `PURGE BINARY LOGS TO` 最新文件，确认 checkpoint 文件已不在源上，然后 Start。任务进入 `RUNNING`，不在 `RETRY_BACKOFF` 上循环 1236。再提交 marker 和另一行后，`gtid_set` 再次包含新的 `GTID_EXECUTED`，marker 出现在数据目录里。场景里关掉 `binlog_transaction_compression`，让 GTID 和 XID 作为独立事件出现；结束时恢复为 ON。
- `smoke-gtid-redump.sh`: MySQL 8.0 上同时跑一个 GTID 任务和一个 LATEST 任务。写入过程中多次 `KILL` 本场景专用复制账号上的 Binlog Dump，并在中间 `FLUSH BINARY LOGS`。还会在 4 个 3000 行的大事务流过时连续 `KILL` dump（重连常落在事务中间）；在一个大事务写入时 `kill -9` 套件进程，停机期间继续写入，再用同一数据目录和元数据库重启（需要 run-suite 传入的 `E2E_SERVER_PID`、`E2E_SERVER_LOG`、`BINLOG_SERVER_META_DSN`，场景结束时停掉自己启动的进程）；最后用 `GTID_NEXT` 先提交 `max+3` 再由自动 GTID 补上 `max+1`、`max+2`，并提交一条另一个 UUID 的事务。两条任务全程不能出现 `FAILED`、`STREAM_REGRESSION` 或 `STORAGE_INCONSISTENT`，结束时都是 `RUNNING`。服务端日志里该 GTID 任务的 `open tail` 对账行作为证据打印出来。GTID 任务 checkpoint 相对启动时 `GTID_EXECUTED` 多出来的事务必须是 `/window` 的子集，`/window` 必须 `continuous: true` 且是源 `GTID_EXECUTED` 的子集。两条任务各回放一次：先丢掉这张库并重置 GTID，再用套件里的 Percona 8.0 `mysqlbinlog` 导入一台不占用宿主网络的临时 MySQL 8.0。行校验和与源表一致；`mysqlbinlog` 或导入失败都让场景失败。quick 档会跑这个场景。同样关掉再恢复 `binlog_transaction_compression`。
- `smoke-source-outage.sh`: 套件进程是 all-in-one（有 meta DSN，默认 `cluster.role`）。MySQL 8.0 上任务已经 `RUNNING` 并且 dump 已打开后，默认连续 5 次（`E2E_SOURCE_RESTARTS`）`docker compose stop mysql80`。每次大约 5 秒内（库内 5 次重连，每次约 1 秒）`GET /api/tasks/{id}` 离开 `RUNNING`，进入 `RETRY_BACKOFF`，`last_error` 以 `SOURCE_UNREACHABLE` 开头，且不能是 `FAILED` 或 `SEGMENT_NOT_ON_WORKER`。源库再启动后任务回到 `RUNNING`，`last_error` 为空。最后一次停库之前再开一条 `LATEST` 任务：它已经 `RUNNING`，但还没有复制过行。两条任务都进入 `RETRY_BACKOFF` 后，套件 `SIGSTOP` binlog-server（`E2E_SERVER_PID`，由 `run-suite.sh` 导出），再启动 mysql80，在进程停住时插入三行 gap marker，然后 `SIGCONT`。这三行和恢复后的 marker，以及各自的 `GTID_NEXT`，在第二条任务的分段里各出现一次，checkpoint 仍没有 `gtid_set`。最后一次恢复后再写一行，主任务 checkpoint 的 file/pos 与 `SHOW MASTER STATUS` 对齐。`LATEST` 不写 `gtid_set`（文件中部的事件不是完整已执行集合）。停任务后用套件里的 Percona 8.0 `mysqlbinlog --verify-binlog-checksum` 读备份分段（`mysql:8.0` 镜像不带这个客户端）。每一次宕机前的 marker、恢复后的 marker，以及对应的 `GTID_NEXT`，各出现一次。解码结果里不能有 `end_log_pos 0` 的 Rotate（源库重启后 sender 补的 artificial rotate 不进本地文件）。场景里关掉 `binlog_transaction_compression`，结束时恢复为 ON，并重新启动 mysql80。不要求任务变成 `FAILED`：恢复前只有几次宕机，源恢复后 runner ready 会把连续失败清零。
- `smoke-task-desired-migration.sh`: 在独立元数据库上执行 migration `000003`、`000004`、`000005` 和 `000006`。`goto 2` 后按状态写入 `backup_tasks`，`goto 4` 后 `schema_migrations` 为 version 4、dirty 0，`SHOW COLUMNS` 含 desired-run 列和 `pending_dump_cleanup`，`desired_run` 无 NULL，RUN/STOP 回填与修订号、计数器 0 符合状态映射。不列出新列的旧 `INSERT` 仍能写入，`pending_dump_cleanup` 是空串，重复 upsert 不改已回填的 `desired_run`。schema 4 上写入四条 `binlog_files`（含空 `source_file`、NULL `source_file`、`end_pos` 0 和 epoch>0）。`goto 5` 后 version 5、dirty 0，`source_file` 无空值且等于 `file_name`，行数和 `start_pos`/`end_pos` 不变，`uk_task_file_epoch` 与 `uk_task_source_epoch` 都在。相同 `(task_id, source_file, epoch)` 再插入不同 `file_name` 得到重复键。当前二进制在 schema 5 上以退出码 1 结束，日志含 `schema version too old` 和 `./migrate up`。`migrate up` 到 version 6 后 `SHOW INDEX` 有 `uk_task_source_epoch`、没有 `uk_task_file_epoch`，`file_name` 列还在，行数不变，相同 `(task_id, source_file, epoch)` 仍是重复键。当前二进制在 schema 11 上通过 `/healthz`（先回到 schema 6 做 000006 的目录断言，再 `migrate up`）。`down --steps 1` 回到 version 5，`uk_task_file_epoch` 回来，行数不变。再 `down --steps 1` 回到 version 4，`binlog_files` 行数不变，`uk_task_source_epoch` 消失，空值约束恢复。同一二进制在 schema 4 上以退出码 1 结束，日志含 `schema version too old` 和 `./migrate up`。再 down 只丢掉 `pending_dump_cleanup` 回到 version 3，然后回到 version 2，行数不变，schema 2 同样拒绝启动。MySQL 8 上重复 schema 4 种子和 `000005`。v0.5.49 二进制在 schema 5 上通过 `/healthz`。`migrate up` 到 schema 6 后用 `BINLOG_TEST_META_DSN` 跑 `TestIssue189_SealedEpochsOnMySQL`（封存、上传），`GET /api/tasks/task-1/files` 列出同一源文件的 epoch 0 和 epoch 1，且 `upload_state` 为 `UPLOADED`。v0.5.52 在 schema 6 上以退出码 1 结束，日志含 `missing index` 和 `uk_task_file_epoch`。MySQL 8 上 `down --steps 1` 回到 schema 5 且行数不变，再 down 回到 schema 4，当前二进制拒绝 schema 4。
- `smoke-standalone-restart.sh`: ADR 0010。不用 compose，自己 `docker run` 起 MySQL 8.0 和 MariaDB 11.4 源库及空的恢复库，standalone 服务监听 `127.0.0.1:18090`，带 `task-key generate` 生成的密钥文件（评估器恒运行）。建一个 LATEST 任务并启动，写数据、`FLUSH BINARY LOGS`，`kill -9` 服务，停机期间继续写，再启动：不调用 adopt 也不调用 start，任务自己回到 RUNNING，没有 `TASK_UNADOPTED` 和 `TASK_RECORDS_FAILED`。再写一批，等 `GET /files` 的 `end_pos` 追上源库 `SHOW MASTER STATUS`，用 `GET /replay` 的全部路径经 mysqlbinlog（Percona 8.0 镜像）或 mariadb-binlog（`--skip-gtid-strict-mode`）回放到恢复库，`CHECKSUM TABLE` 与源库一致。检查 `.tasks.enc` 权限是 0600，文件和服务日志里都没有源库密码和密钥。仅 MySQL 8.0：错误密钥、缺少密钥、损坏文件各起一次服务，分别出现 `TASK_RECORDS_FAILED` 的 `wrong_key`、`key_missing`、`corrupt`，建任务被拒绝，文件哈希不变；最后换回正确密钥，任务自动恢复且告警关闭。随后在无密钥的情况下建任务并运行，加密钥重启后出现 `TASK_UNADOPTED`、`/api/health` 503，用 `scripts/adopt-leftovers.sh --map --start` 批量 adopt 后任务 RUNNING、追上源库、health 200（tip QA b4c52f4 P1）。在 `run-suite.sh` 的 quick 档（CI `unit-and-e2e`）里运行，也可 `--scenarios smoke-standalone-restart` 单跑。
- `smoke-standalone-full-disk.sh`: #395 QA P2。30MB tmpfs 作 standalone data_dir（需要免密 sudo 挂载，否则跳过），不需要 MySQL。建一个任务后用 dd 填满磁盘：再建任务返回 503 `TASK_RECORDS_UNAVAILABLE`，`GET /api/findings` 从内存给出 `TASK_RECORDS_FAILED/write` 和 `FINDINGS_PERSIST_FAILED`，`GET /api/health` 返回 503 degraded；删掉填充文件后两者关闭、health 回到 ok。在 quick 档运行。
- `smoke-least-privilege-migrate.sh`: ADR 0011 PR-4。在 compose 的 meta-primary（Percona 5.7）和 mysql80 上，root 建一个只有该库 `ALL` 的账号。两台都要求 `log_bin` 开着且 `log_bin_trust_function_creators` 为 0，`SHOW GRANTS` 只有该库的 `ALL` 和 `USAGE`，该账号读 `information_schema.innodb_trx` 失败。`./migrate up` 的 stderr 含跳过 `PROCESS` 预检的警告，version 11 且不脏，没有触发器，也没有 `binlog_server_kill_trx`。`down --steps 1` 回到 version 10 并丢掉 `row_generation`，再 `up` 仍警告，已经在 11 上的再一次 `up` 不警告。在 quick 档运行。
- `smoke-session-kill.sh`: ADR 0011 PR-2。用 compose 里的 MySQL 8.0 和自己 `docker run` 的 MariaDB 11.4。持有者的 DSN 不带会话变量，连接经 `OpenClusterMeta`。`SIGSTOP` 之后对端先拿不到 `task_leases` 的 `FOR UPDATE`，会话超时之内才拿到。脚本导出两台库的 root DSN，跑 `TestSessionKillDropsSleepingForUpdate`、`TestQA484DeadlineMargin`、`TestQA484PoolPressure` 和 `TestRejectedSetRefusesStartup`，两个 flavor 都必须 PASS，SKIP 算失败。再各建一个只有该库权限、没有 `PROCESS` 的账号：先跑 `TestMetaLockStallWithoutProcess` 确认 1205，再用 `--plain` 持有者 `SIGSTOP`，由本节点的评估器按 2 秒 sweep 自己打开 `META_LOCK_STALLED`，`GET /api/health` 为 503。最后在只有一条已停止任务的元数据库上起评估器 on、不设置 `health.sweep_interval_sec`（默认 60 秒）的集群进程，连续请求 `GET /api/health` 超过一轮 sweep，每次都是 200 且 `status=ok`，响应里没有 `HEALTH_STALE` 或 `META_LOCK_STALLED`。在 quick 档运行，也可 `make e2e-session-kill`。
- `hold_for_update.go`: `--plain` 不走武装池，把 `wait_timeout` 设长后一直握着 `task_leases`，给没有 `PROCESS` 的评估器格子用。缺省路径仍走 `OpenClusterMeta`。
- `smoke-sigstop-takeover.sh`: ADR 0011 PR-4（production-readiness C3，#473）。`per_node` 存储下 control-plane + 两个 worker，各自独立 `data_dir`。任务在 worker1 上 RUNNING 且目录有真实链之后 `kill -STOP` worker1 超过 `lease_ttl`：worker2 认领出更大的 epoch，但链尾巴在 worker1 盘上读不到，worker2 必须让出落成 `LEASE_DEGRADED`——全程每个采样点断言 state 不是 `FAILED`、`desired_run` 不是 `STOP`（#473 回归目标）、源库 `Binlog Dump` 线程至多一条。随后 `SIGCONT` worker1：它发现 epoch 已过期、优雅让出旧 dump 并用更大 epoch 重新认领回 RUNNING（它的尾巴在本机可读），断言进程没有死。写数据确认链继续推进后干净 stop。CI 的 `e2e-sigstop-takeover` job 每个 PR 都跑。
- `smoke-epoch-segments.sh`: 同一源文件名先有一条已上传的封存分段，再由 standalone 启动打开 `.open.e1`。目录里两条都还在。`GET /files` 各显示一行。`GET /replay` 和带 `start_datetime`/`stop_datetime` 的恢复命令都包含封存路径和 `.open.e1`。旧对象键 `e2e/legacy/mysql-bin.000176` 还在封存行上。后半段把元数据库退回 schema 1，写入 v0.5.33 那种 epoch 0 的打开行和封存行，再 `migrate up`，确认打开行的 epoch 已从路径回填，然后启动任务并在源上 `FLUSH BINARY LOGS` 完成一次 rotate。回放命令里有封存后的路径，没有已经消失的 `.open.e1`。
- `smoke-unreachable-giveup.sh`: 套件进程是 all-in-one（有 meta）。任务指向 `127.0.0.1:1`。认领循环大约每 2 秒跑一次。`RETRY_BACKOFF` 且 `SOURCE_UNREACHABLE` 的 runner 错误至少 4 条时 `SIGTERM` 套件进程，再用同一 meta 和 data dir 拉起。至少 7 条时再 `kill -9` 一次。两次重启都在同一次连续失败里。`backup_tasks.retry_attempt` 在重启后接着涨，不会回到 1。仍是 `RETRY_BACKOFF` 时 API 上的 `epoch` 与重启前相同。第 10 条这类 runner 错误写成 `FAILED`，`last_error` 仍以 `SOURCE_UNREACHABLE:` 开头，`consecutive_source_failures` 为 10，租约放开（epoch 0，owner 空）。`RETRY_BACKOFF` 上 `epoch` 不变。
- `smoke-fail-alert.sh`: 套件进程是 all-in-one。放在 quick 最后，因为中间会 `kill` `meta-primary` 一次，结束时把它拉起来。三条 MySQL 8.0 `LATEST` 任务：封存冲突在第一次 rotate 就让出为 `LEASE_DEGRADED`（`SEALED_FILE_EXISTS`，一条 `TASK_LEASE_DEGRADED`，没有 `TASK_FAILED` 也没有 `TASK_RETRY_BACKOFF`，期望保持 `RUN`），删掉冲突文件后回到 `RUNNING`、计数器为 0。文件位点且 checkpoint 没有 `gtid_set` 时，停任务、`FLUSH` 再 `PURGE` 掉 checkpoint 文件，Start 后第一次就是 `LEASE_DEGRADED` 而不是 `RETRY_BACKOFF`，让出 detail 含 1236、purged、binlog。第三条任务在已有 checkpoint 后 `kill` 元数据库，日志里出现这条任务的 `runner error` 再启动元数据库；状态回到 `RUNNING`，不调用 Start，API 和日志都不出现 `FAILED`，`TASK_FAILED` 为 0，之后 checkpoint 继续推进。
- `smoke-scale.sh`: 可选的 1000 控制面任务/100 实时流规模证据；复用单个 MySQL fixture（不把它当作数百个独立集群），按 100 条 batch 创建、校验分页/聚合、受控启动流，先写 priming marker 再快照每条 checkpoint，第二个 marker 后验证每条流推进及其 checkpoint 精确文件，并写入 JSON 报告。
- `run-suite.sh`: 统一编排入口（自动 `up -> 启动服务 -> 跑场景 -> down`）。

说明：当前所有场景创建任务时均显式传入 `cluster_key`（创建/更新必填且全局唯一）。

## 常用环境变量

- `E2E_DATA_DIR`: e2e 数据目录（默认 `./tmp/e2e/data-suite-<timestamp>`）。
- `E2E_SERVER_LOG`: `run-suite.sh` 启动后端时的日志路径（默认 `/tmp/binlog-server-e2e-suite.log`）。
- `E2E_SOURCE_HOST` / `E2E_SOURCE_USER` / `E2E_SOURCE_PASS`: source 任务共享连接参数，默认 `127.0.0.1` / `repl` / `replpass`。
- `E2E_MYSQL57_PORT` / `E2E_MYSQL80_PORT` / `E2E_MYSQL84_PORT`: MySQL 5.7/8.0/8.4 宿主机端口，默认 `13306` / `13307` / `13310`。
- `E2E_PERCONA57_PORT` / `E2E_PERCONA80_PORT`: Percona 5.7/8.0 宿主机端口，默认 `13308` / `13309`。
- `E2E_META_HOST` / `E2E_META_USER` / `E2E_META_PASS` / `E2E_META_DB`: metadata 连接参数，默认 `127.0.0.1` / `meta` / `metapass` / `binlog_meta`。
- `E2E_META_PRIMARY_PORT` / `E2E_META_REPLICA_PORT`: metadata 主从宿主机端口，默认 `13316` / `13317`。
- `E2E_META_PROXYSQL_ADMIN_PORT` / `E2E_META_PROXYSQL_PORT`: ProxySQL 管理端口和 SQL 端口，默认 `6036` / `16036`。
- `E2E_META_DSN`: 完整 metadata DSN 覆盖值，优先级高于上述 metadata 分项；未设置时由具名的 `direct` 或 `failover` 拓扑生成。
- `SEMISYNC_TIMEOUT_MS`: `smoke-semisync.sh` 使用的半同步 timeout（默认 `7000`）。
- `E2E_WORKER_ID`: `smoke-cluster-roles.sh` 中 worker 进程上报的 worker_id（默认 `e2e-worker-1`）。
- `E2E_WORKER_OFFLINE_WAIT_SEC`: `smoke-cluster-roles.sh` 停 worker 后等待离线判定的秒数（默认 `20`）。
- `E2E_WORKER_HEALTH_ADDR`: `smoke-cluster-roles.sh` 中 worker health probe 地址（默认 `127.0.0.1:18081`）。
- `E2E_SCALE_CONTROL_TASKS` / `E2E_SCALE_LIVE_STREAMS`: scale 场景的控制任务/实时流数，默认 `1000` / `100`；实时流最多显式提高到 `300`。
- `E2E_SCALE_START_RATE`: 每秒启动实时流数（默认 `10`，最多 `25`，低于默认 API 限流）；`E2E_SCALE_REQUEST_DELAY_SEC` 控制 checkpoint 抓取节流；`E2E_SCALE_REPORT` 指定 JSON 证据输出路径。

`smoke-scale` 要求 `E2E_SERVER_PID` 指向存活的 suite server。它会在 disposable `mysql57` 与 `meta-primary` 上执行 `SET GLOBAL max_connections = LIVE_STREAMS + 20`，重新读取并在数据库未接受该容量时失败；因此只适合此隔离 E2E Compose 环境。

所有正式 E2E 入口都会加载 `lib-topology.sh`，必填值不能为空，端口必须处于 `1-65535`。Compose 保留同值兜底，仅用于直接执行 `docker compose ... ps/logs` 等排障命令；`make e2e-topology-check` 会阻止两处默认值漂移。

## smoke-cluster-roles 场景说明

该场景用于真实多进程验证 cluster 角色分离：

1. 启动 control-plane（仅 API/UI）与 worker（仅执行）两个独立进程。
2. 通过 control-plane API 创建并启动任务，确认任务进入 `RUNNING`。
3. 写入 source 数据并确认 checkpoint 推进，证明由 worker 执行。
4. 查询 `/api/workers`，确认 worker `online=true` 且存在 `last_seen_at`；并校验 worker health probe (`/healthz`,`/readyz`) 可用且不暴露 `/api/*`。
5. 停止 worker，等待超过在线阈值后确认 `online=false`。
6. 重启 worker，确认 `online=true` 恢复且任务继续推进。

## smoke-observability 场景说明

该场景用于验证 observability 最小回归链路：

1. 创建并启动任务，确认任务状态进入 `RUNNING`。
2. 调用 `/metrics`，确认 5 个核心指标名都可见。
3. 验证 `task_state_count` 随任务状态变化（`CREATED -> RUNNING -> STOPPED`）。
4. 写入 source 数据并等待 checkpoint 生成，验证 `checkpoint_age_seconds` 两次抓取间递增。
5. 经 `run-observability.sh` 跑时（`make e2e-observability`、CI），等 `otlp-sink.py` 收到本次运行的 API span：服务名、`binlog_server/internal/api`、`POST /api/tasks/*path`、`http.target` 为本任务的 `/start`。

## smoke-control-plane-failover 场景说明

该场景用于验证 control-plane 故障不影响 worker 数据面：

1. 启动 control-plane + worker，创建并启动任务，确认 `RUNNING`。
2. 记录 checkpoint A，写入数据并推进到 checkpoint B（B > A）。
3. 停止 control-plane（worker 保持运行），再次写入数据。
4. 重启 control-plane，查询 checkpoint C（C > B），证明控面停机窗口内 worker 仍持续复制。
5. 验证 control-plane 恢复后 `/healthz` 与任务 API 可访问，且 `/api/workers` 仍展示 worker 在线状态。

## smoke-worker-crash-recovery 场景说明

该场景用于验证 worker 异常崩溃与接管后的 strict consistency：

1. 启动 control-plane + worker1，创建并启动任务到 `RUNNING`。
2. 写入 source 数据并触发 rotate。
3. 在检测到 `OPEN` 文件后强制 kill worker1（模拟崩溃）。
4. 启动 worker2（复用同一数据目录）接管任务，确认 `epoch` 增长且 checkpoint 持续推进；再写入并 rotate 一次，保证除第一个文件外至少有两个完整封存文件。
5. 停任务并到 `STOPPED` 后，校验旧 epoch `OPEN` 文件已清理；若仍有 `OPEN` 文件，必须全部属于 current epoch（允许存在，不计为异常）。
6. 仅对 sealed 文件集合做命名校验与唯一性校验（`mysql-bin.######`，无异常后缀）。
7. 对比主库与备份最新两个完整 sealed 文件的 md5 一致。任务的第一个文件不比：LATEST 从源文件中部开始，备份是 magic + dump 的 format description + 起点之后的源字节，本来就不等于源文件（不是数据损坏）。完整文件少于两个时失败。

## smoke-retry-upload 场景说明

该场景用于验证上传失败后的后台补传：

1. 从 Quay 启动 minio 与 bucket；Quay 不可用时改用 GitHub release 二进制。以 upload 配置启动 binlog-server。
2. 创建并启动任务。源库空闲停在 binlog 末尾时，先写一行，确认 checkpoint 已建立。这一行还在 open 分段里，随后停桶再 rotate 时才会封存。
3. 停止 minio，写入并 rotate，触发 `UPLOAD_FAILED` 文件记录。
4. 把这份已封存失败文件的 mtime 拨到 `retention_days` 之外，再 `FLUSH BINARY LOGS`。文件和目录行还在，任务保持 `RUNNING`，事件里有一条 `RETENTION_SKIPPED_NOT_UPLOADED`，`binlog_server_retention_blocked_files{task_id}` 至少为 1。再 rotate 一次，这条事件仍是一条。
5. 继续写入源库，确认 checkpoint 仍持续推进（best-effort 语义不变）。
6. 恢复 minio，不调用补传 API，等待后台重试把已封存的 `UPLOAD_FAILED` 变成 `UPLOADED`。服务日志里要有 `background upload retry`。`state=OPEN` 或路径里带 `.open.e` 的分段不能是 `UPLOADED`。
7. 再 `FLUSH BINARY LOGS`。刚才被拨旧的那份文件在上传成功后被清掉，gauge 变为 0。
8. 断言仍在的封存文件 `checksum` 为 `match`。
9. 对同一个 MinIO，把与封存文件等长但内容不同的对象写入后，`TestApplySealedUpload_MinIOChecksum` 断言 `checksum` 为 `mismatch` 且调用方不返回错误；匹配的上传（含大于 16MiB 的分片对象）仍为 `match`。
10. 再次写入并确认 checkpoint 继续推进。

## smoke-split-retention 场景说明

该场景证明本地保留和桶保留可以分开，并且只配 `retention_days` 的任务仍按原来的一次清理删掉对象、目录行和本地文件：

1. 从 Quay 启动 minio；Quay 不可用时改用 GitHub release 二进制。以 upload 和 `meta_dsn` 启动 binlog-server。
2. 创建桶保留短于本地保留的任务，HTTP 400，正文含 `shorter than local retention`。
3. 只带 `storage.retention_days` 的任务响应里没有 `local_retention_days` 和 `bucket_retention_days`。把它的一份已上传封存文件的 mtime 拨到 10 天前，再 rotate。本地文件、目录行和对象一起消失。
4. `local_retention_days=1`、`bucket_retention_days=30` 的任务里，mtime 早于本地、仍在桶保留内的 `UPLOADED` 文件只从磁盘删除。`location` 是 `bucket`，`GET /files/{name}` 的字节与删除前一致，对象还在。`replay` 的 `locations` 对这条路径是 `bucket`，`stop_datetime` 窗口和 `replay/archive` 仍包含它。mtime 早于桶保留的另一份则对象、目录行和本地文件一起消失。

## meta-failover 场景说明

`meta-failover` 场景会额外拉起：
- `meta-primary` / `meta-replica`（Percona57）
- `meta-proxysql`（meta DSN 固定入口）
- `orchestrator`（触发切主）

运行该场景时，`run-suite.sh` 会自动：
1. 执行 `setup-meta-replication.sh` 建立主从复制
2. 将 `BINLOG_SERVER_META_DSN` 覆盖到 `127.0.0.1:16036`（ProxySQL）
3. 执行 `smoke-meta-failover.sh` 或 `smoke-meta-failover-override.sh` 验证 failover 后恢复
4. 可通过 `E2E_API` / `E2E_ORC_API` 覆盖 binlog-server 与 orchestrator 地址。

## 排障建议

- 保留现场环境：

```bash
./scripts/e2e/run-suite.sh --profile full --keep-env
```

- 查看后端日志：

```bash
cat /tmp/binlog-server-e2e-suite.log
```

- 查看容器状态与日志：

```bash
docker compose -f deploy/e2e/docker-compose.yml ps
docker compose -f deploy/e2e/docker-compose.yml logs
```

- 排障后清理：

```bash
./scripts/e2e/down.sh
```

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
