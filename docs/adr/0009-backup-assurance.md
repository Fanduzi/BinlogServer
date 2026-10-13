# 备份保障：一条备份链、一个结论汇、零静默失败

Status: Proposed (2026-10-13)。只有设计，没有实现代码。Epic #326。

相关记录：[0002](0002-best-effort-upload-one-caller.md)（尽力上传一个调用方）、[0005](0005-single-task-authority-and-segment-catalog.md)（任务状态一个权威、段一个键）、[0006](0006-segment-ledger.md)（段账本）、[0007](0007-storage-health.md)（存储健康）、[0008](0008-run-phases.md)（一次运行五个阶段）。这些决定都保留；本记录在它们之上回答三个问题：**备份是不是完整的**、**出了问题有没有人知道**、**要用的时候能不能找到并还原**。

## 背景：为什么不能再一个一个修

审计基于 main `10c5b97`（v0.5.64）。过去关闭的 P0/P1 与「静默」类问题按根因归成六类，每一类都修过不止一次：

| 类 | 典型问题 | 共同根因 |
|---|---|---|
| A. 「到底存了什么」有多个答案 | v0.5.57 re-dump 静默丢事务（PR #293）、#213 LATEST 重新解析丢事务、#223 接管后同一文件封存两次（事务重复）、旧版回切 A→B→A（v0.5.58–60）、#175 checkpoint 抹掉 GTID、#198 | 写入路径、续传路径、读取路径各自推断；ADR 0006/0007 已把**本地目录**收成一个账本，但目录行与对象存储不在账本里 |
| B. 目录行 / 磁盘 / 对象存储三份不一致 | #172 上传中崩溃后 LOCAL_ONLY 不可见（P0）、#173 校验不符仍 UPLOADED 且本地被删（P0）、#209 删了文件目录行还在、#224 `.takeover-*` 残留、#149 | 没有一个不变量说「三处必须一致」，也没有任何东西定期去比 |
| C. 读接口悄悄给部分答案 | #305 `/replay` 默认只给最新 200 个段（P1）、#307 limit 被静默忽略、#322 轮转时 `/replay/archive` 500 | 读接口自己拼文件清单、自己截断；「覆盖了什么」不是一个有类型的结果 |
| D. 状态有多个写者 | #174、#179、#203、#214、#220 | ADR 0005 已解决权威问题；剩下的是**写失败只打日志**（见下方静默清单 S2） |
| E. 重试语义不清 | #178 只有 SOURCE_UNREACHABLE 会放弃（P1）、#153 UPLOAD_FAILED 无后台重试 | 失败没有一个统一出口，重试、放弃、告警各写各的 |
| F. 问题只在有人去看时才存在 | `storage_alert`、`/window` 的断点、`recovery_breaks` 都是**请求时现算**；Console「告警」页不包含它们 | 没有持久化的结论，没有推送，只有拉取 |

A–C 是「完整性模型不完整」，D–F 是「失败没有统一出口」。本记录分别给出一个模型。

## 决定

1. **备份链 (Backup Chain)** 是唯一回答「这个任务的备份从哪到哪、连续吗、每段在哪」的深模块。它把段账本（本地）、目录行（meta）、对象清单（对象存储）合成一个有类型的结果，并对下面七条不变量逐条给出判定。`/window`、`/replay`、PITR、存储健康、指标、校验巡检、覆盖查询、还原计划都只读它，不再自己拼清单或截断。
2. **结论汇 (Finding Sink)** 是失败的唯一出口：`Report(ctx, Finding)` 一次写入**持久化结论 + 事件 + 指标**。任何错误路径要么返回给一个最终会调用 `Report` 的调用方，要么自己调用 `Report`；`_ = err`、只打日志不上报都在 CI 里被拒绝。
3. **健康备份** 是一个有定义、可度量的状态（下文「心跳与 SLO」），由一个周期性评估器写入，而不是由 Console 从 `state` 字段猜。
4. 校验巡检、覆盖查询、还原计划都是备份链之上的薄消费者。

## 1. 完整性不变量

### 记号

一个任务的**链**是有序段序列 `S_1 … S_n`，排序键为（源身份在 `.source-chain` 中的序号, 源文件序号, epoch, 名字），即 `binlog.Ledger.Segments()` 的现有顺序扩展到目录行与对象。对每段：

- `name(S)`、`id(S)`：源身份；`seq(S)`：源文件序号；
- `G(S)`：段内事务的 GTID 集合；`P(S)`：段头 previous-GTIDs；
- `pos(S) = [start_pos, end_pos)`，`E(S)`：段内事件的 `end_log_pos` 序列；
- `loc(S) ⊆ {LOCAL, OBJECT}`：可读位置；`row(S)`：目录行（`binlog_files`，键 `(task_id, source_file, epoch)`）；
- `B`：任务起点集合（`start_gtid_set` 或 LATEST 解析结果，存在 checkpoint 种子里）；
- `C`：checkpoint（`gtid_set`, `file`, `pos`）；`T`：打开尾部（`Ledger.Tail()`）中已落盘的部分。

### 不变量

| # | 名称 | 定义 | 历史问题 |
|---|---|---|---|
| I1 | 段内单调 | 对每段 `S`，`E(S)` 严格递增；段内不出现指向自己或更早文件的 Rotate | v0.5.57 re-dump |
| I2 | 文件/位点连续 | 对同一源身份的相邻封存段 `S_i, S_{i+1}`：`S_i` 的最后一个事件是 `Rotate(name(S_{i+1}), 4)`，或二者 `seq` 相邻且 `S_{i+1}` 以 format description 开头；跨源身份时，切换点必须有 `SOURCE_SWITCHOVER` 记录的 `gtid_set` | #176、#205、#214 |
| I3 | GTID 无缺口 | 令 `Stored = ⋃ G(S_i)`（GTID 模式）。`Holes(B) = ∅`，即对每个 uuid(:tag)，`B ∪ Stored` 从 `B` 的上界到 `Stored` 的上界是一个连续区间；且 `P(S_{i+1}) ⊆ B ∪ ⋃_{j≤i} G(S_j) ∪ Switch_i`（`Switch_i` 是该切换点记录的集合） | #213、旧版回切、stored hole |
| I4 | 无重叠 | 对 `i ≠ j`：`G(S_i) ∩ G(S_j) = ∅`；同一 `(id, seq)` 至多一个封存段处于链上（其余是被替换的旧 epoch，必须在目录行中标明） | #223 |
| I5 | 检查点不超前 | `C.gtid_set ⊆ B ∪ Stored ∪ T`；`C.file/pos` 不超过 `T` 的截断点 | v0.5.57、#175、#198 |
| I6 | 三处一致 | 对每个封存段：`row(S)` 存在；`loc(S) ≠ ∅`；`LOCAL ∈ loc(S)` 当且仅当文件在磁盘上；`OBJECT ∈ loc(S)` 当且仅当对象存在且大小等于 `row.size`。每个目录行都对应链上一段或标明为已按保留期删除 | #172、#209、#224、#149 |
| I7 | 上传完整 | 配置了对象存储时：每个 `sealed_at` 早于 `now − upload_grace` 的段，`row.upload_state = UPLOADED` 且 `checksum = MATCH`；本地删除（保留期）只能删除满足 I7 的段 | #173、#153 |

**可恢复窗口** = 满足 I1–I4、I6 的链的最长后缀（GTID 模式下还要求 I3 在该后缀内成立），`[earliest_event_time, latest_durable_event_time]`。窗口内的每个时间点、每个 GTID 都有唯一的一组段覆盖。今天的 `RecoveryWindow` 已经在算这个，但它只读本地+目录行，并且只在请求时算。

### 在哪里保证（写入时），在哪里检查（读取与巡检时）

| # | 写入时保证 | 读取/巡检时检查 |
|---|---|---|
| I1 | `run_events.go` `handleRotate` 拒绝倒退（`STREAM_REGRESSION`）；`run_dump.go` 只从落盘位点重连 | `binlog.Ledger.Damage`（`segmentDamage`） |
| I2 | `run_segment.go` `rotate`：先 seal 行 → 下一文件 checkpoint → put → open；`source_switch.go` 记录切换集合 | `window.go` `missingIndexBreak`；**新增**：备份链对每对相邻段判定 |
| I3 | `run_start.go` 续传起点只用 `Ledger.ResumeGTID`；1236 回退只用已落盘集合 | `Ledger.Holes`、`storedHoleIn`、`window.go` `gtidHole`/`switchHole`/`interiorHoleBreak` |
| I4 | 接管与续传前用账本判断「此 GTID 已存」 | **新增**：备份链判定（今天只靠 #223 的修复防止，没有检查） |
| I5 | `gtid_checkpoint.go` 只把落盘 GTID 写进 checkpoint | `Ledger.Damage(checkpoint, …)` 的 `Missing` |
| I6 | `sealed_upload.go` 唯一上传调用方；`retention release` 先记行再删 | **新增**：巡检器（第 3 节）比对目录行、磁盘、对象 HEAD |
| I7 | 保留期 `release` 拒绝删除未验证段（`unverifiedSealedUpload`） | 今天只有 `binlog_server_upload_failures_total` 一个计数；**新增**：按段年龄的 `upload_lag` 判定 |

备份链模块的接口（设计，不是实现）：

```go
// internal/assurance（新包）；只依赖 binlog.Ledger、tasks 目录行读者、对象清单读者三个接口
func ChainOf(task Task, ledger *binlog.Ledger, catalog CatalogReader, objects ObjectLister) *Chain
func (c *Chain) Segments() []ChainSegment            // 带 loc、row、G、P、pos
func (c *Chain) Check(now time.Time) []Violation     // I1–I7，每条带不变量编号、段、证据、首个违反点
func (c *Chain) Window() RecoveryWindow              // 由 Violation 推出，替换 window.go 里的独立推导
func (c *Chain) Cover(q CoverQuery) (Coverage, error) // 第 4 节
func (c *Chain) Plan(target RestoreTarget) (RestorePlan, error) // 第 5 节
```

`Violation` 的种类是封闭枚举（`I1_REGRESSION`、`I2_INDEX_GAP`、`I3_GTID_HOLE`、`I4_OVERLAP`、`I5_CHECKPOINT_AHEAD`、`I6_MISSING_LOCAL`、`I6_MISSING_OBJECT`、`I6_ORPHAN_ROW`、`I6_SIZE_MISMATCH`、`I7_UPLOAD_LATE`、`I7_UNVERIFIED`）；调用方按种类判断，不再看 `detail` 前缀（ADR 0007 已对存储健康做过同样的事）。`StorageHealth` 成为 `Check` 结果的一个视图：损坏 = I1，旧版回切与已存缺口 = I3 的两个子类。

对象清单读者只做 `ListObjects(prefix)` 与 `HeadObject`；段内容需要时用已有的 `binlog.ReadSegmentContents`。备份链每次问答新建，不做跨请求缓存以外的状态（段内容缓存沿用账本的 size+mtime 规则；对象清单按巡检周期缓存）。

## 2. 失败可见性：零静默

### 模型

每条失败路径的终点是三件事，缺一不可：

1. **持久化状态**：`task_findings` 表（迁移 `000007`）中的一行，键 `(task_id, kind, subject)`，带 `first_seen`、`last_seen`、`count`、`severity`（`CRITICAL`/`WARNING`）、`detail`、`resolved_at`。同一结论重复上报只更新 `last_seen/count`（幂等）；条件消失时由**同一个评估者**写 `resolved_at`。Standalone 模式存在内存任务结构里，并写入目录下 `.findings` 文件，进程重启不丢。
2. **告警**：Console「告警」页 = 未解决的 `CRITICAL/WARNING` 结论 ∪ 不健康的任务（第 2.2 节），不再是前端对 `state` 的过滤。可选通知出口见「需要决定的事」。
3. **指标**：`binlog_server_finding_open{task_id,kind,severity}`（gauge）和 `binlog_server_findings_reported_total{kind}`（counter）。

`Finding` 种类是封闭枚举，覆盖：七条不变量违反、`TRANSITION_PERSIST_FAILED`、`EVENT_PERSIST_FAILED`、`UPLOAD_LATE`、`UPLOAD_UNVERIFIED`、`RETENTION_PURGE_FAILED`、`VERIFY_FAILED`、`DUMP_STALLED`、`RETRY_LOOP_FAILED`、`DUMP_CLEANUP_PENDING`、`METRIC_SOURCE_ERROR`、`TASK_UNHEALTHY`。

结论汇自己的写入失败怎么办：内存中保留一个有上限的待写队列，失败本身计入 `binlog_server_finding_sink_errors_total`，并让 `/healthz` 返回 degraded。这是唯一允许「只有指标」的路径，因为它就是出口本身。

### 2.1 静默路径审计（main `10c5b97`）

下面每一条都是「失败发生了，但没有同时留下持久状态、告警和指标」：

| # | 位置 | 现在的行为 | 后果 |
|---|---|---|---|
| S1 | `internal/tasks/scheduler.go:637-640` | `eventLane.write` 中 `_ = store.AppendEvent(...)`，注释写明「事件落库失败不阻断主流程」 | 任务事件（含 `TASK_RUNNER_ERROR`、`SOURCE_SWITCHOVER`、`STORAGE_REPAIRED`）丢失；`carriedSourceFailures` 依赖事件，重试预算因此变小或归零 |
| S2 | `internal/tasks/scheduler_transitions.go:125-128` `logTransitionPersistError`；调用于 `scheduler_lifecycle.go:1005,1108,1134,1157,1192,1220` 与 `scheduler_cluster_lease.go:21,46,55,63,73` | `FAILED`、`RETRY_BACKOFF`、`STOPPING`、`LEASE_DEGRADED` 的落库失败只 `log.Printf` | 进程内是 FAILED，`backup_tasks` 仍是 RUNNING；其他节点和 Console 看到「正常」——最危险的一条 |
| S3 | `internal/tasks/scheduler_lifecycle.go:964`、`:932`；`scheduler_dump_cleanup.go:237,280,458` | STOPPING/STOPPED 落库失败、`pending_dump_cleanup` 写失败只打日志 | 源库上残留 Binlog Dump 线程无人知道 |
| S4 | `internal/replication/mysql_runner.go:1931-1933` | 打开尾部截断失败只打日志，把 `Truncate` 置为 false 继续 | 半截事务留在段里继续追加；I1/I3 可能被破坏而续传照常 |
| S5 | `internal/replication/mysql_runner.go:2437-2440` | 保留期把未验证上传记成 UPLOAD_FAILED 失败，只打日志 | 未验证对象继续被当作已上传 |
| S6 | `internal/replication/mysql_runner.go:2608` | `_ = writer.AppendEvent(... RETENTION_REMOVED ...)` | 删除本地文件没有审计记录，窗口缩短无从解释 |
| S7 | `internal/tasks/scheduler_retry_upload.go:80-83,95-97,108-110` | 后台补传整轮失败（同步任务失败、计数失败、单任务失败）只打日志；`retryLastTS` 只在有结果时更新（`:301`） | 补传循环本身停摆时，`upload_retry_last_ts` 停在旧值，没有任何告警规则会注意 |
| S8 | `internal/tasks/scheduler_retry_upload.go:187,192` | `_ = s.markRetryUploadFailure(...)` | 空 `file_path`/`object_key` 的目录行失败记录本身可能丢 |
| S9 | `internal/tasks/sealed_upload.go:73-75`（`default` 分支，注释 `:45`） | 上传器无法读回对象时记 `UPLOADED` + 空 checksum | 看起来是绿色的「已上传」，其实从未验证 |
| S10 | `internal/api/metrics_prometheus.go:149-152,166-169,182,233-236` | 单任务取进度/checkpoint/窗口出错时 `continue`，该任务的序列直接消失；心跳列表出错时整个 `worker_online` 消失 | 告警规则 `> 阈值` 对缺失序列永远不触发——「没有数据」被当成「没问题」 |
| S11 | `internal/api/handlers_tasks.go:263,1191` | `progress, ok, _ := GetReplicationProgress(...)` | 进度读取失败时列表显示为无延迟 |
| S12 | `internal/replication/mysql_runner.go:1190-1202` `syncerConfig` | 设置了 `HeartbeatPeriod` 15s，但没有读超时；没有「多久没收到任何事件或心跳就算卡住」的判定 | 半开 TCP 连接上 dump 永远等待，任务保持 RUNNING；`AtTip` 停留在最后一次的值，延迟指标为 0 |
| S13 | `internal/tasks/scheduler_lifecycle.go:1236-1239` | 读源失败计数的事件失败时回退到内存，新进程内存为空，计数归零 | 重试预算静默重置（ADR 0005 已记录） |
| S14 | `console/src/features/tasks/filter.ts:28-31` `isAlertRow` | 告警页只看 `ABNORMAL`/`DELAYED`/`FAILED`/`RETRY_BACKOFF` | `storage_alert`、UPLOAD_FAILED、`recovery_breaks > 0`、checkpoint 停滞都**不在告警页** |
| S15 | `internal/tasks/scheduler_task_ops.go:611` | 列出磁盘上的备份失败只打日志 | 删除任务时遗留目录不可见 |

S12 和 S14 不是「错误被吞掉」，而是「失败没有被定义」：系统根本不知道它该报警。这正是需要健康定义的原因。

### 2.2 「正在健康备份」心跳与 SLO

每个 `desired_run = true` 的任务，评估器每 `assess_interval`（默认 30s）计算一次，写入 `task_health`（随 `task_findings` 一起在 `000007`）：

```
healthy(t) ⇔ state ∈ {RUNNING}
           ∧ progress_age(t) ≤ stall_after            -- 最后一个事件或源心跳距今；默认 3 × 15s = 45s
           ∧ checkpoint_age(t) ≤ checkpoint_after     -- 非 at-tip 时 checkpoint 必须推进；默认 120s
           ∧ open_findings(t, CRITICAL) = ∅           -- 含 I1–I6 违反
           ∧ upload_lag(t) ≤ upload_grace             -- 最老未 UPLOADED+MATCH 的封存段年龄；默认 15min
           ∧ verify_age(t) ≤ verify_every × 2         -- 最近一次巡检成功；默认 24h × 2
```

- `progress_age` 由 runner 在**收到任何包（包括 heartbeat）**时更新，与 `AtTip` 分开；超过 `stall_after` 时 runner 主动断开重连（读超时 = `stall_after`），并上报 `DUMP_STALLED`。这解决 S12。
- 指标：`binlog_server_task_healthy{task_id}`（0/1，对每个 desired_run 任务**总是**输出，读不到时输出 0 并带 `reason`）、`binlog_server_task_progress_age_seconds`、`binlog_server_task_upload_lag_seconds`、`binlog_server_task_last_verified_timestamp`。
- **RPO**（定义）：本地 RPO = `progress_age`（at-tip 时）；对象存储 RPO = 本地 RPO + `upload_lag` + 打开段时长。
- **SLO**（建议默认）：每任务按分钟采样，30 天内 `healthy` 分钟占比 ≥ 99.9%；对象存储 RPO 的 p99 ≤ 20min。
- 指标的「缺失即不健康」规则：评估器对每个任务都写一行，所以 `absent()` 只在整个进程挂掉时出现，配套一条 `up == 0` 规则即可。

### 2.3 让静默不再被写回去

- CI 加 `errcheck`（排除 `Close` on read-only、`Remove` on temp 等白名单，白名单在仓库里列明）和一个自定义 `go vet` 分析器：在 `internal/{tasks,replication,binlog}` 中，`log.Printf` 的参数含 `err` 且同一个块里没有 `return` 或 `Report` 时报错。
- 每个 `Finding` 种类必须有一条测试：注入该失败 → 断言结论行、事件、指标三者都出现。种类枚举与测试表在同一文件里，新增种类没有测试就编译不过（表驱动 + `exhaustive` 检查）。

## 3. 周期性校验巡检与还原演练

**巡检器 (Verifier)** 是 worker 上的周期任务（只在持有租约的 worker 上跑该任务，复用 ADR 0001 的所有权），每 `verify_every`（默认 24h，分批平摊，每任务每轮上限 `verify_budget` 字节/秒）：

1. **结构**：对每个本地封存段用 `binlog.ReadSegmentContents` 完整解析（magic、每事件 CRC32、`end_log_pos`、GTID 事件），结果与目录行比对（size、首尾位点、GTID 集合摘要）。失败 → `VERIFY_FAILED`（CRITICAL）。
2. **对象**：对每个 UPLOADED 段 `HEAD` 一次（存在、大小）；按 `verify_object_sample`（默认 5%，加上最近 24h 新上传的全部）下载并比对 SHA-256 与本地（本地已删则与目录行记录的 SHA-256 比对，因此上传时需要把 SHA-256 写进目录行）。缺失或不符 → `I6_MISSING_OBJECT`/`I6_SIZE_MISMATCH`/`VERIFY_FAILED`。这也会发现对象存储侧的生命周期策略误删。
3. **链**：对整条链跑 `Chain.Check`，把违反写成结论，消失的结论标记 resolved。
4. 每段记录 `last_verified_at`，进入健康定义的 `verify_age`。

**还原演练 (Restore Drill)**（可选，默认关闭）：配置一个**专用演练实例**（`verify.drill.dsn`），每 `drill_every`（默认 7 天）对窗口内随机一个时间点生成还原计划（第 5 节），用 `mysqlbinlog | mysql` 在演练实例上从 `B` 起回放到该点，比较演练实例的 `gtid_executed` 与计划预期；可选再比较用户提供的校验 SQL。演练绝不连接源库。结果写事件 `RESTORE_DRILL` 与指标 `binlog_server_restore_drill_last_success_timestamp`；失败为 CRITICAL 结论。

## 4. 可查找：给一个时间或 GTID，哪些文件和对象覆盖它

`Chain.Cover(q)`，`q` 是下列之一：`at=<datetime>`、`gtid=<uuid:n>`、`range=<from,to>`。结果：

```
Coverage {
  covered: bool                 // 是否在可恢复窗口内
  reason: string                // 不在时：早于 earliest / 晚于最新落盘 / 落在第 k 个断点
  segments: [{ name, source_identity, epoch, first_event_time, last_event_time, gtid_set,
               local_path?, object_key?, upload_state, checksum, last_verified_at }]
  start_from: { segment, pos }  // 需要从哪一段、哪个位点开始
  stop_at:    { segment, pos }  // 停在哪
  window: { earliest, latest, breaks[] }
}
```

- API：`GET /api/tasks/{id}/coverage?at=…|gtid=…`；跨任务：`GET /api/coverage?source=host:port&at=…`（同一源可能有多个任务、换主后的新任务）。
- 回答**从不截断**：#305 那类「只给最新 200 个」在这里不允许；分页只用于展示，结果对象总是完整的段列表引用。
- 时间索引：每段的首尾事件时间在封存时写进目录行（今天在 `pitr.go` 里请求时扫描 `EventSpan`），使覆盖查询不需要读文件。
- Console：任务详情增加「可恢复时间线」：窗口条、断点、每段位置（本地/对象/两者）、验证状态，点击时间点即显示 `Coverage`。

## 5. 还原便利：一步生成还原计划

`Chain.Plan(target)`，`target` 是时间点或 GTID（含「最新」）。`RestorePlan` 是**数据**而不是脚本文本：

```
RestorePlan {
  target, coverage,
  prerequisites: [ "全量备份的 gtid_purged 必须 ⊆ … 且 ⊇ …" ],   // 与用户提供的全量备份 GTID 对比
  prechecks:     [ { id, description, how, result? } ],
  steps:         [ { kind: download|verify|apply, files, command } ],
  bundle:        { url, sha256, size }                           // /replay/archive 的同一实现
}
```

- **预检**（服务器能做的自动做，其余列成用户要在目标库执行的 SQL）：窗口覆盖目标；所有段 `checksum = MATCH` 或本地在且 `last_verified_at` 足够新；全量备份的 `gtid_executed`（用户粘贴或填写）与起点 `B` 衔接（没有空洞也没有重叠）；目标实例 `gtid_mode`、`binlog_format`、`log_replica_updates`、`super_read_only`、版本兼容；`mysqlbinlog` 版本 ≥ 源版本。
- **命令**沿用 `FormatPITRCommand`/`FormatGTIDCommand`，统一由计划生成；包里附 `restore.sh`、`MANIFEST.json`（每文件 SHA-256）、`README`（中英）。
- API：`POST /api/tasks/{id}/restore-plan`（无副作用，只计算）。
- Console 向导：选目标（时间/GTID/最新）→ 粘贴全量备份的 GTID → 预检结果（红/黄/绿）→ 下载包或复制命令 → 还原后核对 SQL（`SELECT @@gtid_executed` 应包含计划的终点集合）。

## 6. 长稳与故障注入

在现有 `scripts/e2e` 之上新增 `scripts/soak/`，一个编排器按时间表注入故障，结束后用**独立实现**（`mysqlbinlog` 解析 + 源库 `gtid_executed` + 全链还原后表校验和）判定，而不是用被测代码自己的 `/window`。

| 场景 | 注入方式 | 期望 |
|---|---|---|
| 网络闪断 | `tc netem` 丢包 30%、延迟 2s、5–120s 断开；iptables 单向 DROP（制造半开连接） | 重连；半开连接在 `stall_after` 内被发现（`DUMP_STALLED`）；无缺口 |
| 磁盘满 | 数据目录挂 loop 设备，写满；恢复后释放 | 任务 `FAILED` 或 `RETRY_BACKOFF` 且有 CRITICAL 结论；不产生半截段；释放后 Start 续传无缺口 |
| 对象存储故障 | MinIO 停机、返回 503、限速、写入后删除对象、篡改对象一字节 | 拉流不受影响；`upload_lag` 告警；保留期不删本地；巡检发现被删/被改对象 |
| 源库换主 | VIP 切到 GTID 一致的新主；切到缺事务的新主；切到已 purge 的新主 | 前者继续且链连续；后两者 `FAILED` + 结论，不混写 |
| kill -9 | worker 每 10–30min 随机 kill -9；meta DB kill -9；双 worker 互相接管 | 自动恢复或 STOPPED+接管；I1–I7 全部成立 |
| meta 故障 | meta MySQL 只读、断开 60s | S2 类写失败都有结论；恢复后状态一致 |
| 时钟 | worker 时钟跳变 ±5min | 保留期、健康判定不误删不误报 |

**验收**：持续 ≥ 24h，源写入负载 ≥ 500 TPS 混合 DML + DDL，binlog 轮转 ≤ 每 5min 一次；全部场景至少各触发 3 次；结束时：
1. 独立校验：源 `gtid_executed` 在起点之后的部分 = 链上 GTID 并集，无缺口、无重叠；
2. 窗口内随机 10 个时间点 + 最新点还原，表校验和 100% 一致；
3. 每次注入都在期望时间内出现对应结论/指标，恢复后结论 resolved；没有任何注入未产生结论（零静默的验收）；
4. `healthy` 分钟占比在非注入时段 ≥ 99.9%；
5. 进程 RSS、fd、goroutine 数无单调增长；磁盘只增长保留期允许的量。

## 消除各类反复问题的方式

| 类 | 本设计如何移除这一类 |
|---|---|
| A | 「存了什么」从账本（本地）扩展为备份链（本地+目录行+对象），I3–I5 是可执行的判定；所有读者只读链 |
| B | I6/I7 把三处一致写成不变量；巡检器定期比对；保留期只能删满足 I7 的段 |
| C | 覆盖查询与还原计划返回完整、有类型的结果；截断只能是 `Violation` 推出的窗口截断，并且带原因 |
| D | 状态写失败（S2/S3）进入结论汇，成为 CRITICAL；不再只有日志 |
| E | 每种失败的终点统一：重试由预算决定，放弃与告警都通过 `Report` |
| F | 评估器与巡检器**推**结论到持久表；告警页、指标、事件读同一份；「没有数据」= 不健康 |

## 分阶段实施（每阶段独立可验证）

| 阶段 | 内容 | QA 通过条件 |
|---|---|---|
| 0 结论汇与静默清零 | 迁移 `000007`（`task_findings`、`task_health`）；`Report` 实现；S1–S15 逐条改为上报；`errcheck` 与 vet 分析器进 CI；runner 读超时（S12） | 每个 S# 一个注入测试，断言结论行 + 事件 + 指标；meta 只读 60s 时 S2 产生 CRITICAL 且恢复后 resolved；半开连接 ≤ 60s 内报 `DUMP_STALLED` 并重连；现有 e2e quick 全绿 |
| 1 备份链 | `internal/assurance` 的 `Chain`、`Check`（I1–I7）、`Window`；`window.go`、`storage_health.go`、`/replay` 选文件改为读链；行为冻结的 golden（ADR 0006/0007 的 47 个目录用例）逐字节一致；目录行增加首尾事件时间与 SHA-256 | golden 一致；构造目录覆盖 7 种违反各 ≥ 2 例（含 #223 重叠、#209 孤行、#173 未验证）；`/window`、`/replay` 输出与 v0.5.64 一致 |
| 2 健康与告警 | 评估器、健康定义、SLO 指标；Console 告警页改读结论与健康；任务列表健康徽标 | 每个健康条件单独破坏一次，告警页与指标在 1 个评估周期内变化；`binlog_server_task_healthy` 对每个任务总有序列 |
| 3 巡检与演练 | Verifier（结构、对象、链）；限速；演练（默认关） | MinIO 中删一个对象、改一个字节、本地改一个字节，下一轮全部发现；演练在独立 MySQL 上 3 个时间点通过；限速下源延迟不升高 |
| 4 覆盖查询 | `/coverage` API、跨任务查询、Console 时间线 | 窗口内/外/断点上的时间与 GTID 各 10 例，结果与 `mysqlbinlog` 独立解析一致；无截断 |
| 5 还原计划 | `/restore-plan`、预检、包内 `MANIFEST.json`/`restore.sh`、Console 向导 | 按向导对 5 个时间点还原，校验和一致；全量备份 GTID 有空洞或重叠时预检为红 |
| 6 长稳 | `scripts/soak` 编排器与独立判定器；24h 跑 | 第 6 节验收 5 条全部满足；发现的问题按类回到本 ADR 的模型修，不打补丁 |

阶段 0 不依赖其他阶段，先做；1 是 2–5 的前提；6 的编排器可以与 1 并行开发，验收在 5 之后跑。每阶段一个或多个 PR，各自过 QA 才合并；阶段 1 是行为冻结的重构，与 ADR 0006–0008 同样以 golden 证明。

## 考虑过的方案

1. **继续逐个修 issue。** 已经证明不收敛：A 类修了五次、B 类修了五次。拒绝。
2. **只靠外部监控（Prometheus 规则写在现有指标上）。** 现有指标表达不了 I3/I4/I6，且 S10 让「缺失」等于「正常」。作为补充保留（提供推荐规则文件），不作为模型。
3. **把对象存储当唯一真相（每段一个 sidecar manifest 对象，不依赖 meta）。** 优点是 meta 丢失后仍能重建与查找；缺点是写入路径多一次对象写、一致性依赖对象存储语义。本设计把它降为可选：阶段 1 的目录行字段足以支撑覆盖查询；若 Adrian 希望 meta 丢失也能还原，阶段 4 增加 manifest 写入（见下）。
4. **全面事件溯源（目录行改为追加日志）。** 能天然得到审计，但要重写 ADR 0005 的目录与迁移所有读者，风险与收益不成比例。拒绝；`task_findings` + 事件已够审计。
5. **巡检全量下载对象。** 最强，但出网流量和费用随数据量线性增长。默认抽样 + 新上传全量 + 记录 SHA-256，可配置为全量。
6. **结论放进现有 `task_events`，不建新表。** 事件是追加的，没有「未解决」状态，告警页需要扫描并推断——这正是 F 类问题。拒绝。

## 需要 Adrian 决定的事

1. **告警通知出口**：只提供 Prometheus 指标 + 推荐告警规则（用户自己接 Alertmanager），还是内置 webhook（钉钉/飞书/Slack/邮件）？建议：先只做指标 + 通用 webhook 一种。
2. **默认阈值**：`stall_after` 45s、`upload_grace` 15min、`verify_every` 24h、SLO 99.9%、对象 RPO p99 20min 是否合适。
3. **还原演练**需要一个专用 MySQL 实例并由服务器写入它；是否接受这个能力（默认关闭）。
4. **巡检对象抽样比例**（出网费用）默认 5% + 新上传全量。
5. **迁移 `000007`** 会把 `minRequiredSchemaVersion` 抬到 7；是否接受在一个 v0.6.0 里发（建议用 v0.6.0 标示这一能力边界）。
6. 是否需要「meta 库丢失也能还原」（方案 3 的 sidecar manifest）。
