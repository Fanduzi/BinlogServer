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
# 日常回归（默认）：smoke-task-desired-migration + smoke + compression + smoke-source-outage + smoke-unreachable-giveup + smoke-control-loop + smoke-fail-alert
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

- `up.sh`: 启动 4 个 source MySQL（`mysql57/mysql80/percona57/percona80`）和独立的 `meta-primary`，并等待可用。
- `run-server.sh`: 用 e2e 配置启动 `binlog-server`，并以 `CGO_ENABLED=0` 构建临时测试二进制，避免 Linux 测试环境绑定构建机 glibc。
- `down.sh`: 清理 e2e 容器与 volume。
- `smoke.sh`: 创建并启动 4 个基础任务，写入数据并查看 checkpoint。
- `smoke-compression.sh`: 验证压缩事务场景，主动 rotate 并对比源端与备份 binlog 的 md5。
- `smoke-orchestrator.sh`: 验证 orchestrator 拓扑里是否误纳入 binlog 拉流客户端。
- `smoke-semisync.sh`: 验证 `semi_sync=true` 时的 client 挂载与停任务后主库提交阻塞到 timeout。
- `setup-meta-replication.sh`: 初始化 meta-primary/meta-replica GTID 主从复制与 ProxySQL 监控账号。
- `lib-topology.sh`: 解析并校验统一的数据库 host、端口、共享凭据和 metadata DSN。
- `topology-contract-test.sh`: 无需 Docker，验证拓扑默认值、覆盖优先级、direct/failover 选择、端口校验及 Compose 兜底一致性。
- `smoke-meta-failover.sh`: 触发 orchestrator 切主，验证元数据库 failover 后 checkpoint 继续推进。
- `smoke-meta-failover-override.sh`: 用非默认 `E2E_API/E2E_ORC_API` 地址（localhost）覆盖并执行 failover 场景。
- `smoke-observability.sh`: 验证 `/metrics` 核心指标存在，且 `task_state_count` 与 `checkpoint_age_seconds` 随状态/时间变化。
- `smoke-cluster-roles.sh`: 启动 control-plane + worker 双进程，验证任务执行、worker 离线检测与恢复链路。
- `smoke-control-plane-failover.sh`: 验证 control-plane 崩溃/重启期间 worker 持续拉流，checkpoint 不中断推进。
- `smoke-worker-crash-recovery.sh`: 模拟 worker 在 OPEN 期间崩溃，验证新 worker 接管后一致性（checkpoint 推进、stale OPEN 清理、sealed 文件与 md5 校验）。
- `smoke-invalid-inputs.sh`: 验证任务 API 对非法输入返回 `400`（cluster_key/source/start/storage）。
- `smoke-retry-upload.sh`: 验证上传失败不阻断拉流；MinIO 恢复后，已封存的 `UPLOAD_FAILED` 由后台补传变成 `UPLOADED`，场景本身不调用 `/api/tasks/{id}/files/retry-upload`。open 分段不会变成 `UPLOADED`。补传成功的封存文件 `checksum` 为 `match`；同一 MinIO 上内容不同的对象为 `mismatch`，且不让上传调用方失败。桶不可用时，把已封存失败文件的 mtime 拨到保留期之外再 `FLUSH BINARY LOGS`，文件、目录行和复制都留着，并只记一条 `RETENTION_SKIPPED_NOT_UPLOADED`；桶恢复并上传成功后，下一次打开文件会把它清掉。
- `smoke-split-retention.sh`: 验证桶保留短于本地保留时创建任务返回 400；只配 `storage.retention_days` 的响应不含新字段，过期已上传文件仍同时删对象、目录行和本地文件；`local_retention_days` 短于 `bucket_retention_days` 时，介于两者之间的已上传文件只删本地，`location` 为 `bucket`，下载、`replay`、`stop_datetime` 和 `replay/archive` 仍读到对象。
- `smoke-recovery-window.sh`: MySQL 8.0 上 GTID 打开。建库建表后任务从当时的 `GTID_EXECUTED` 启动，三次插入并 `FLUSH BINARY LOGS`，停任务。`GET /api/tasks/{id}/window` 是连续的，`earliest`、`latest`、`gtid_set` 都有，`breaks` 为空。`latest` 之后一秒的 `stop_datetime` 得到非空回放命令；`earliest` 之前一秒是空命令。然后删掉中间一条封存分段的本地文件和 `binlog_files` 行。窗口变为不连续，`reason` 含 `missing source file`，`/metrics` 上 `binlog_server_recovery_breaks{task_id}` 至少为 1，并且仍有 `binlog_server_recovery_earliest_age_seconds`。同一较晚停止时间的回放路径不再包含被删的文件名。不在 quick profile 里。场景里关掉 `binlog_transaction_compression`，结束时恢复为 ON。不拉 MinIO。
- `smoke-gtid-pitr.sh`: MySQL 8.0 上 GTID 打开。先建库建表，任务从当时的 `GTID_EXECUTED` 启动，等这张表进入 checkpoint 后 `FLUSH BINARY LOGS`，让建表分段封存。然后在同一秒里插入三行、一条不带 WHERE 的 `DELETE`、再插入两行，下一秒再插入一行。`GET /api/tasks/{id}/replay?stop_gtid=<坏事务>` 在 `.open.e*` 上返回 `--stop-position`，`limit=1` 仍包含前面的封存分段。把这条命令管道到一台新的空 MySQL 8.0：三行好数据各一行，`DELETE` 没有执行，后面的行不在。再 `FLUSH` 封存这条 GTID 所在的文件，并在新文件里写一行。同一 `stop_gtid` 的 `--stop-position` 不变，路径变成封存名，新文件不在列表里。再用封存后的命令恢复一台空 MySQL 8.0，结果相同。无法解析、序号或 UUID 不在备份里、MariaDB 形状、非 mysql flavor、两个停止参数同时出现、开始时间晚于这条 GTID，都是文档里的纯文本 400。`segment not found on this process` 仍由单测覆盖。场景里关掉 `binlog_transaction_compression`，结束时恢复为 ON。不拉 MinIO。
- `smoke-gtid-executed.sh`: MySQL 8.0 上先写几条任务启动前的事务，记下更老的 GTID 集合，`FLUSH BINARY LOGS` 后再记下启动点，使保留的第一段已经落在更老集合之后。任务从启动点的 `GTID_EXECUTED` 开始。建库插入两行后 `mysqldump --set-gtid-purged=ON`，记下这时的已执行集合，再 `FLUSH BINARY LOGS` 封存这一段。之后把第 3、4 行的事件头写到分段头之后两分钟，停止事务和第 6 行再晚 5 秒，停止时刻取第 3、4 行的下一秒，这样 `mysqlbinlog --stop-datetime` 不会停在格式描述上。`start_gtid_set` 加 `stop_gtid` 的命令用 `--exclude-gtids`，封存的中点文件不在 `paths` 里。把返回的命令管道到一台先导入这份 dump 的 MySQL 8.0，行是 1 到 4，停止事务和更后的行不在。同一集合加 `stop_datetime` 得到同样的行。集合已经盖住停止点之前的每一条事务时，HTTP 200，`paths` 为空，`command` 为空，`note` 说明已经包含。更老的集合、无法解析的集合、和非 mysql flavor，是文档里的纯文本 400。桶上的分段仍由单测覆盖。场景里关掉 `binlog_transaction_compression`，结束时恢复为 ON。不拉 MinIO。
- `smoke-gtid-purge.sh`: MySQL 8.0 上以当前 `GTID_EXECUTED` 启动任务（RawMode 仍是服务端默认）。连续三次自动提交后，checkpoint 的 `gtid_set` 必须包含当时的 `GTID_EXECUTED`（按 `server_uuid` 的区间比较，并用 `GTID_SUBSET` 核对；UUID 里的数字不算区间）。停止后 `FLUSH BINARY LOGS` 再 `PURGE BINARY LOGS TO` 最新文件，确认 checkpoint 文件已不在源上，然后 Start。任务进入 `RUNNING`，不在 `RETRY_BACKOFF` 上循环 1236。再提交 marker 和另一行后，`gtid_set` 再次包含新的 `GTID_EXECUTED`，marker 出现在数据目录里。场景里关掉 `binlog_transaction_compression`，让 GTID 和 XID 作为独立事件出现；结束时恢复为 ON。
- `smoke-source-outage.sh`: 套件进程是 all-in-one（有 meta DSN，默认 `cluster.role`）。MySQL 8.0 上任务已经 `RUNNING` 并且 dump 已打开后，默认连续 5 次（`E2E_SOURCE_RESTARTS`）`docker compose stop mysql80`。每次大约 5 秒内（库内 5 次重连，每次约 1 秒）`GET /api/tasks/{id}` 离开 `RUNNING`，进入 `RETRY_BACKOFF`，`last_error` 以 `SOURCE_UNREACHABLE` 开头，且不能是 `FAILED` 或 `SEGMENT_NOT_ON_WORKER`。源库再启动后任务回到 `RUNNING`，`last_error` 为空。最后一次停库之前再开一条 `LATEST` 任务：它已经 `RUNNING`，但还没有复制过行。两条任务都进入 `RETRY_BACKOFF` 后，套件 `SIGSTOP` binlog-server（`E2E_SERVER_PID`，由 `run-suite.sh` 导出），再启动 mysql80，在进程停住时插入三行 gap marker，然后 `SIGCONT`。这三行和恢复后的 marker，以及各自的 `GTID_NEXT`，在第二条任务的分段里各出现一次，checkpoint 仍没有 `gtid_set`。最后一次恢复后再写一行，主任务 checkpoint 的 file/pos 与 `SHOW MASTER STATUS` 对齐。`LATEST` 不写 `gtid_set`（文件中部的事件不是完整已执行集合）。停任务后用套件里的 Percona 8.0 `mysqlbinlog --verify-binlog-checksum` 读备份分段（`mysql:8.0` 镜像不带这个客户端）。每一次宕机前的 marker、恢复后的 marker，以及对应的 `GTID_NEXT`，各出现一次。解码结果里不能有 `end_log_pos 0` 的 Rotate（源库重启后 sender 补的 artificial rotate 不进本地文件）。场景里关掉 `binlog_transaction_compression`，结束时恢复为 ON，并重新启动 mysql80。不要求任务变成 `FAILED`：恢复前只有几次宕机，源恢复后 runner ready 会把连续失败清零。
- `smoke-task-desired-migration.sh`: 在独立元数据库上执行 migration `000003`、`000004`、`000005` 和 `000006`。`goto 2` 后按状态写入 `backup_tasks`，`goto 4` 后 `schema_migrations` 为 version 4、dirty 0，`SHOW COLUMNS` 含 desired-run 列和 `pending_dump_cleanup`，`desired_run` 无 NULL，RUN/STOP 回填与修订号、计数器 0 符合状态映射。不列出新列的旧 `INSERT` 仍能写入，`pending_dump_cleanup` 是空串，重复 upsert 不改已回填的 `desired_run`。schema 4 上写入四条 `binlog_files`（含空 `source_file`、NULL `source_file`、`end_pos` 0 和 epoch>0）。`goto 5` 后 version 5、dirty 0，`source_file` 无空值且等于 `file_name`，行数和 `start_pos`/`end_pos` 不变，`uk_task_file_epoch` 与 `uk_task_source_epoch` 都在。相同 `(task_id, source_file, epoch)` 再插入不同 `file_name` 得到重复键。当前二进制在 schema 5 上以退出码 1 结束，日志含 `schema version too old` 和 `./migrate up`。`migrate up` 到 version 6 后 `SHOW INDEX` 有 `uk_task_source_epoch`、没有 `uk_task_file_epoch`，`file_name` 列还在，行数不变，相同 `(task_id, source_file, epoch)` 仍是重复键。当前二进制在 schema 6 上通过 `/healthz`。`down --steps 1` 回到 version 5，`uk_task_file_epoch` 回来，行数不变。再 `down --steps 1` 回到 version 4，`binlog_files` 行数不变，`uk_task_source_epoch` 消失，空值约束恢复。同一二进制在 schema 4 上以退出码 1 结束，日志含 `schema version too old` 和 `./migrate up`。再 down 只丢掉 `pending_dump_cleanup` 回到 version 3，然后回到 version 2，行数不变，schema 2 同样拒绝启动。MySQL 8 上重复 schema 4 种子和 `000005`。v0.5.49 二进制在 schema 5 上通过 `/healthz`。`migrate up` 到 schema 6 后用 `BINLOG_TEST_META_DSN` 跑 `TestIssue189_SealedEpochsOnMySQL`（封存、上传），`GET /api/tasks/task-1/files` 列出同一源文件的 epoch 0 和 epoch 1，且 `upload_state` 为 `UPLOADED`。v0.5.52 在 schema 6 上以退出码 1 结束，日志含 `missing index` 和 `uk_task_file_epoch`。MySQL 8 上 `down --steps 1` 回到 schema 5 且行数不变，再 down 回到 schema 4，当前二进制拒绝 schema 4。
- `smoke-epoch-segments.sh`: 同一源文件名先有一条已上传的封存分段，再由 standalone 启动打开 `.open.e1`。目录里两条都还在。`GET /files` 各显示一行。`GET /replay` 和带 `start_datetime`/`stop_datetime` 的恢复命令都包含封存路径和 `.open.e1`。旧对象键 `e2e/legacy/mysql-bin.000176` 还在封存行上。后半段把元数据库退回 schema 1，写入 v0.5.33 那种 epoch 0 的打开行和封存行，再 `migrate up`，确认打开行的 epoch 已从路径回填，然后启动任务并在源上 `FLUSH BINARY LOGS` 完成一次 rotate。回放命令里有封存后的路径，没有已经消失的 `.open.e1`。
- `smoke-unreachable-giveup.sh`: 套件进程是 all-in-one（有 meta）。任务指向 `127.0.0.1:1`。认领循环大约每 2 秒跑一次。`RETRY_BACKOFF` 且 `SOURCE_UNREACHABLE` 的 runner 错误至少 4 条时 `SIGTERM` 套件进程，再用同一 meta 和 data dir 拉起。至少 7 条时再 `kill -9` 一次。两次重启都在同一次连续失败里。`backup_tasks.retry_attempt` 在重启后接着涨，不会回到 1。仍是 `RETRY_BACKOFF` 时 API 上的 `epoch` 与重启前相同。`FAILED` 时这类 runner 错误一共 10 条。`last_error` 以 `SOURCE_UNREACHABLE:` 开头，租约放开（`epoch` 为 0、没有 owner）。
- `smoke-fail-alert.sh`: 套件进程是 all-in-one。放在 quick 最后，因为中间会 `kill` `meta-primary` 一次，结束时把它拉起来。三条 MySQL 8.0 `LATEST` 任务：封存冲突在第一次 rotate 就 `FAILED`（`SEALED_FILE_EXISTS`，一条 `TASK_FAILED`，没有 `TASK_RETRY_BACKOFF`，租约放开，`desired_run=STOP` 且 `failed_spec_revision=spec_revision`），删掉冲突文件后 Start 回到 `RUNNING` 且 `spec_revision` 大于失败时的修订号、计数器为 0。文件位点且 checkpoint 没有 `gtid_set` 时，停任务、`FLUSH` 再 `PURGE` 掉 checkpoint 文件，Start 后第一次就是 `FAILED` 而不是 `RETRY_BACKOFF`，`last_error` 含 1236、purged、binlog，一条 `TASK_FAILED`，租约放开。第三条任务在已有 checkpoint 后 `kill` 元数据库，日志里出现这条任务的 `runner error` 再启动元数据库；状态回到 `RUNNING`，不调用 Start，API 和日志都不出现 `FAILED`，`TASK_FAILED` 为 0，之后 checkpoint 继续推进。
- `smoke-scale.sh`: 可选的 1000 控制面任务/100 实时流规模证据；复用单个 MySQL fixture（不把它当作数百个独立集群），按 100 条 batch 创建、校验分页/聚合、受控启动流，先写 priming marker 再快照每条 checkpoint，第二个 marker 后验证每条流推进及其 checkpoint 精确文件，并写入 JSON 报告。
- `run-suite.sh`: 统一编排入口（自动 `up -> 启动服务 -> 跑场景 -> down`）。

说明：当前所有场景创建任务时均显式传入 `cluster_key`（创建/更新必填且全局唯一）。

## 常用环境变量

- `E2E_DATA_DIR`: e2e 数据目录（默认 `./tmp/e2e/data-suite-<timestamp>`）。
- `E2E_SERVER_LOG`: `run-suite.sh` 启动后端时的日志路径（默认 `/tmp/binlog-server-e2e-suite.log`）。
- `E2E_SOURCE_HOST` / `E2E_SOURCE_USER` / `E2E_SOURCE_PASS`: source 任务共享连接参数，默认 `127.0.0.1` / `repl` / `replpass`。
- `E2E_MYSQL57_PORT` / `E2E_MYSQL80_PORT`: MySQL 5.7/8.0 宿主机端口，默认 `13306` / `13307`。
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
4. 启动 worker2（复用同一数据目录）接管任务，确认 `epoch` 增长且 checkpoint 持续推进。
5. 停任务并到 `STOPPED` 后，校验旧 epoch `OPEN` 文件已清理；若仍有 `OPEN` 文件，必须全部属于 current epoch（允许存在，不计为异常）。
6. 仅对 sealed 文件集合做命名校验与唯一性校验（`mysql-bin.######`，无异常后缀）。
7. 抽样对比主库与备份 sealed 文件 md5（1-2 个文件）一致。

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
