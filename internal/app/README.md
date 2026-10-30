# internal/app Module

## Files
- `app.go`: 应用主流程与运行时装配。`mode: cluster` 时给调度器 `WithClusterMode`，覆盖查询和还原计划返回 501，不按本节点目录回答。打开 meta 池之前按 `meta.tls` 改写 DSN（ADR 0012）；`disabled` 时 DSN 不变。Restore 之前先对齐遗留 `desired_run`。调度器经 `clusterClaimOptsForRun` 拿到 `cluster.storage`、拒绝退避上限，以及实现了 `ClusterGate` 的 meta。worker 启动、认领任务之前，删掉 data_dir 下每个任务目录里崩溃留下的 `.takeover-*`；本进程正在写的那份不删。同一个上传客户端既删对象，也在接管时把封存 `UPLOADED` 对象读回来。配置了上传且本进程是 worker 时，启动已封存 `UPLOAD_FAILED` 的后台补传。worker（含单机和 all-in-one）同时启动残留 Binlog Dump 的 KILL 重试，任何任务状态都会试。集群模式打开 dump fence；schema 还没有 `pending_dump_cleanup` 列时启动日志写明要先跑迁移 `000004`。 关停有界且有序：取消 runCtx → `WaitRuns`（10s，runner 都从 runCtx 派生）→ 等后台 goroutine（HTTP 优雅关闭 5s、评估器、复核器、存活探测、补传、记录重试等，5s）→ 再试一次欠下的记录保存 → 结束 finding sink 并等它最后一次 flush（5s）。HTTP 关停先关掉空闲 keep-alive 和还没发出请求的连接；关停开始之后才变成 StateNew 或 StateIdle 的连接由同一个 ConnState 钩子关掉。在途请求留在 5s 预算里。`shutdown_http_test.go` 用一条裸连接加一个在途慢请求守住这件事，并在快照之前的空隙里拨号：shutting 必须已经置上，而且 ConnState 要在快照之前关掉这条连接（钩子里的读超时即失败），这条连接不能拖住 Shutdown。Run 返回后本次运行启动的任何东西都不再写 data_dir。创建 runner 时把调度器作为 `WithChainGate` 交给复制（轮转与健康评估共用 `HoldChain`，#441）。
- `app.go` / `startVerifier`（ADR 0009 阶段 3）: Restore 之后、对外服务之前在后台跑巡检。standalone 恒允许；集群 `Allows` 接 `scheduler.VerifyAllows`——只有任务租约持有者巡检（ADR 0011 per_node），未持有者跳过：不读目录、不开不关 finding、不写该任务的 `.verify.json` 进度。`*upload.S3Uploader` 必须实现 `tasks.ObjectHeadOpener`（编译期变量）。配了上传时启动对桶做一次 HEAD（`ProbeStorage`），2 秒超时（端点黑洞时监听最多晚这 2 秒）：桶在算通；鉴权失败、网络失败、桶不在、客户端没有 HEAD 都打 zap warn，detail 带桶名和端点、不带密钥。普通探针失败写进 `/api/health` 的 `detail`，`status` 仍是 ok，HTTP 仍是 200。TLS 传输失败在探针上直接返回该错误（不是 2 秒超时；正文已经是 TLS 时，超时不会盖住它），把 status 写成 degraded，HTTP 503；之后校验和匹配的上传或补传成功，或探针文本被替换，这条 reason 清掉。打开 meta 池之前按 `meta.tls` 改写 DSN，裸 x509 前缀为 `meta.tls:`（`metadb.example` 这种主机名也会加上）。`upload.tls.ca_file` 传给对象客户端。`tls_live_test.go` 的 `TestLiveAppTLS` 走真实任务和 meta 池。`TestLiveAppTLSPreferredNoAbort` 在没有 TLS 的 MySQL 上，`max_connect_errors=2`，4 个 preferred 任务加一次强制重试，`Aborted_connects` 和 `SUM_CONNECT_ERRORS` 不变。没有 `ProbeStorage` 的客户端改 HEAD 哨兵 key，key 不在算通。不因此拒绝启动，对象存储可以暂时不可用，本地捕获照常跑。探针只在启动时做一次。非 TLS 的 detail 留到被替换。TLS 的 detail 在后来的上传或补传成功时清掉。桶 HEAD 看不出「能 ListBucket、不能 GetObject」。`upload.bucket` 传进工人，对象没查完的原因写任务 id、对象 key 和这个桶名。日志里的周期和预算不写 `verify.drill_dsn`。Restore 之后先 `EvaluateClusterNow`，把任务记录的清除写入健康存储，再监听，避免重启后仍返回上一进程的 `TASK_RECORDS_FAILED`。`verifier_run_test.go` 从 `Run` 断言第一轮和 ticker 第二轮的 finding，锁住集群空转，并用假客户端覆盖鉴权错误、桶不在、网络错误和不能 HEAD 的客户端。`TestStartVerifier_PassesUploadBucket` 断言 `upload.bucket` 进了工人。
- `app.go` / `startHealthEvaluator`（ADR 0009 P2-3）: 评估器始终装配（`health.evaluator` 开关已移除）：有 meta 时写元数据库（000009），否则写 `{data_dir}/.health.json`；集群模式的 fence 是任务 lease（node = worker id），否则用持久化的 `{data_dir}/.node_id`；实例 id 每次启动随机生成。`healthEvaluatorIdentity`（#424 r3）：集群 worker 用 worker id；control-plane 节点用 `cluster.worker_id`，没配时用持久化的 `{data_dir}/.node_id`，并设 `ControlPlaneOnly`（不持有任务 lease，代评一切），所以 control-plane 节点也持有 `_health`、日志里 node 不再为空。多个 control-plane 节点共享 `data_dir` 时应各自配置 `cluster.worker_id`。`health.stale_after_sec`（0 = 5×sweep，再取 ADR 0011 的下限）传给 `EvaluatorConfig.StaleAfter`（§28）。集群 meta 走 `OpenClusterMeta`，会话超时上界是 `min(5s, lease_ttl/2)`。`health.node_gone_after_sec`（0 = max(3×sweep, lease_ttl+2×sweep)）传给 `EvaluatorConfig.NodeGoneAfter`（§30）。`notify.webhook_url` 非空时做成 `notify.Webhook` 交给评估器；空 URL 不发送。
- `app.go` / `standaloneTaskRecords`（ADR 0010）: 没有 `meta_dsn` 且 `standalone.task_records=on`（默认）时装配 `{data_dir}/.tasks.enc`，密钥取 `BINLOG_SERVER_TASK_KEY` 或 `standalone.task_key_file`（上一把密钥用于轮换）；Restore 和认领之后调用 `ResumeRecordedTasks`。off 或设了 `meta_dsn` 而文件存在时只记日志。
- `datadir_lock_unix.go` / `datadir_lock_other.go`: standalone 启动时对 `{data_dir}/.tasks.lock` 取 flock（写入 pid），取不到报错退出；非 unix 为空操作。
- `task_records_test.go`: 同一 data_dir 第二个 standalone 进程启动失败、密钥环境变量加载后被 unset；standalone 进程重启后任务自动恢复；`standalone.task_records=off` 时重启不恢复任何任务。
- `health_evaluator_test.go`: standalone 下评估器持久化 node id。`TestHealthEvaluatorIdentity`：control-plane 无 worker_id 时拿到稳定非空 id。
- `uploaded_takeover_test.go`: 走 `Run` 的启动接线。checkpoint 只在已上传对象里、本地文件不在时，不报 `SEGMENT_NOT_ON_WORKER`。worker 启动会删掉崩溃留下的 `.takeover-*`，其它名字留下。
- `tracing.go`: tracing provider 初始化与生命周期管理。
- `tracing_test.go`: OTLP HTTP 默认 traces 路径兼容性回归测试。
- `http_server_test.go`: HTTP 超时、PRODUCTION 以及非 loopback listen 的 auth fail-closed 校验测试。
- `smoke_test.go`: 应用层烟测（HTTP 创建任务需带 `source.password`），包括 auth 到真实路由、未带凭证可打开 Console、ClaimRunnableTasks 恢复、以及认领循环回归。`waitRunExit` 等的是 runner、后台和 finding 三段关停预算之和再加一秒。HTTP 关停跑在后台组里，已经含在后台那一段，不再另加 5s。`TestApp_StartAndServeHealth` 和 `TestApp_ProductionAuthProtectsRealRoutes` 用 2s、自己的 HTTP client 和 `t.TempDir()` 数据目录：关停前先关掉空闲 keep-alive 和还没发出请求的连接，空闲关停是毫秒级，多出来的几秒必须失败。超时的那次运行拿着的锁在它自己的目录里，不会挡住下一次。
- `restart_recovery_test.go`: standalone 重启后安全的持久化 active task 自动恢复、metadata/source 冲突任务保持停止的回归测试。
- `source_guard_test.go`: 从 `meta_dsn` 到任务 API 的 metadata/source 隔离装配回归测试。

## Exports
- `New(cfg)` / `Run(ctx)`: 应用生命周期入口。
- role/mode 装配逻辑：control-plane/worker/all-in-one。
- control-plane 与 worker-health HTTP server 均应用可配置超时（ReadHeader/Read/Write/Idle）。
- 通过 `config.meta.timeout.*` 注入 tasks/meta 的内部依赖调用超时（读/写/lease/上传）。
- 对 TCP `meta_dsn` 提取 host/port 并注入 tasks，同端点 source 在 create/update/start 边界被拒绝。
- API server 支持从 `config.API.Auth` 注入鉴权策略。`/ui/*` 不走这套中间件；`/api/*`、`/metrics`、`/swagger/*` 仍按配置保护。
- 非空 `PRODUCTION` 用标准布尔值解析；true 时 `EncryptionKey`（`--encryption-key`）为空则拒绝启动。control-plane 在 true 时仍强制 auth 已启用、同时保护 `/api/*` 和 `/metrics`，并复用 `config.ValidateAPIAuthConfig` 校验模式/已解析凭证；worker-only 不暴露该 API 且不套用 auth 约束，但仍拒绝空的 encryption key。
- control-plane `listen_addr` 非 loopback（含 `:8080`、`0.0.0.0:8080`）时同样强制 `api.auth.enabled` + `protect_api` + `protect_metrics`；`127.0.0.1`/`localhost`/`::1` 可保持未鉴权本地演示。`/healthz` 仍匿名。
- 创建 meta store 时把 `config.EncryptionKey` 注入，用于 `source_json` 源库密码加解密。
- tracing：默认关闭；启用时装配 HTTP 入站 span 与元数据存储调用 span；无路径的 OTLP HTTP endpoint 沿用 `/v1/traces` 默认路径。
- worker 启动时先删掉 data_dir 下每个任务目录里崩溃留下的 `.takeover-*`（日志带任务、文件名和大小；本进程正在写的不删），再与认领循环都走 `ClaimRunnableTasks`：先对齐遗留 `desired_run`，再没人要的 STARTING、过期租约、自己名下空闲的 active 任务。本进程正在跑的任务如果期望是 STOP，或共享行已经是 STOPPING 或 STOPPED，这一轮会取消拉流；STOPPED 要等 Close 返回后才写。租约已过期的 STOPPING 会收成 STOPPED。不对别人仍持有未过期租约的 RUNNING 做 Stop。认领循环在 standalone 同样每 2s 跑一次（不只集群）：LEASE_DEGRADED（磁盘写满这类 EpochLost）在自己的 dated refusal 到期后被拉回 RUNNING，不再需要人工 Start 或进程重启。
- 封文件前验租把 `LeaseManager` 直接交给 runner（`Verify`），不再经 App 适配。
- standalone worker 注入进程内 `MemoryLease`（worker_id=`standalone`），与集群走同一扇所有权门。
- 封文件后上传由 App 注入 `ApplySealedUpload`，执行器只 seal。同一个上传客户端也作为保留清理的对象删除器注入。该客户端能读对象时，接管用它把 checkpoint 已经落在其中的封存 `UPLOADED` 对象读回来。封存后的这次 put 使用 `meta.timeout.upload_sec`，与后台补传的 `withUploadTimeout` 是同一个秒数。不新增配置项。
- 同一个上传配置下，worker（含单机和 all-in-one）启动 `RunBackgroundUploadRetry`。桶恢复后，已封存的 `UPLOAD_FAILED` 不必调用补传 API 就会再传。control-plane-only 不跑这个循环。手动 `POST /api/tasks/{id}/files/retry-upload` 仍可用。不新增配置项。
- `config.DataDir` 注入 scheduler。`binlog_files` 没有该任务的行时，`GET /api/tasks/{id}/files` 扫描这个目录下的本地分段。

### Minimal Tracing Config Example
```yaml
tracing:
  enabled: true
  exporter: "otlp-http"
  endpoint: "http://127.0.0.1:4318/v1/traces"
  sample_ratio: 0.1
  service_name: "binlog-server"
```

## Dependencies
- Upstream: `cmd/binlog-server`。
- Downstream: `internal/config`, `internal/tasks`, `internal/replication`, `internal/meta`, `internal/api`。

## Update Rule
- 装配流程、运行模式、生命周期行为变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
