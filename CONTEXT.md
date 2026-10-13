# Binlog Server

集中式 MySQL binlog 备份。一个任务对应一台源库。集群里同一时刻只能有一个 Worker 跑同一个任务。

## Language

**任务 (Task)**:
一台源 MySQL 的一份备份配置与运行状态。
_Avoid_: job, backup job

**任务所有权 (Ownership)**:
此时此刻谁可以跑这个任务。包括：占上、续上、放开、捡没人跑的、开机把属于自己的接上、丢了就停、封文件前再问一次。
_Avoid_: Worker 注册, 心跳, 调度策略

**租约 (Lease)**:
任务所有权的凭证。能不能跑，只看租约。有过期时间，要续。新主人来时 Epoch +1。任务上的主人、epoch 是给界面和「请开始」用的抄本，必须和租约一起写。
_Avoid_: lock, session；任务行和租约各信各的

**认领 (Claim)**:
Worker 去捡该自己跑的任务。开机和平时是同一件事：没人要的 STARTING、自己名下还占着的、别人租约已过期的。查「该谁跑」和占租约是一次动作。不要去动别人还活着的任务。
_Avoid_: 抢占未过期的任务；把开机恢复写成另一套 Stop+Start；名单和占锁分开两处

**恢复 (Restore)**:
认领在进程启动时的那一次。
_Avoid_: 停掉别人还活着的任务

**Fail-safe Stop**:
租约丢了，或宽限期内续不上，强制停，避免两台一起写。

**FAILED 释放**:
任务因永久错误停掉、已经不在拉 binlog 时，立刻放开租约。暂时连不上、正在重试时（RETRY_BACKOFF）继续占着。

**Worker**:
真正拉 binlog 的进程。
_Avoid_: 把 Worker 注册当成任务租约

**控制面 (Control Plane)**:
下发启动/停止，不拉 binlog。启动只写成 STARTING（请开始），不占租约。占租约、认领、开机接上，都在 Worker。
_Avoid_: 控制面指定主人

**执行器 (Runner)**:
已经拿到所有权之后，去拉流、落盘、推进位点。封文件前再问一次：还是我吗。
_Avoid_: 执行器自己占租约

**尽力上传 (Best-effort Upload)**:
已封文件送到对象存储。失败不打断拉流。首次上传和事后重试是同一件事。
_Avoid_: 执行器里传一次、调度器里再传一次

**任务观测**:
当前过滤下，任务页、汇总、按源计数是同一次物化。
_Avoid_: 分页走一套、汇总再扫全表或另查 COUNT；用这一页的汇正当成全库人数

**集群观测**:
全库的所有权抄本：有多少任务、多少在跑、谁占着。
_Avoid_: 用任务观测的过滤结果冒充集群人数；用进程里还没和 store 对齐的名单当集群真相

**源身份**:
一台源是 host 加 port。回环地址的不同写法是同一台源。
_Avoid_: 同一界面里 lookup 当一台、过滤当两台

**段账本 (Segment Ledger)**:
一个任务目录此刻真正落盘了什么：按 binlog 顺序的分段、每段的 GTID 区间和 previous-GTIDs 头、已存 GTID 集合、集合里的空洞、第一个损坏分段及恢复集合、可续传的打开尾部（完整事务和末尾半截事务）。`binlog.LedgerAt(dir)` 每次问答读一次目录。GTID 区间只有一种类型 `binlog.GTIDSet`。
_Avoid_: 调用方自己 `ReadDir` 再扫分段；自己把 previous-GTIDs 和事务并成「已存集合」；再写一份区间类型

**存储健康 (Storage Health)**:
一个任务目录能不能放心回放的唯一结论：正常、损坏（Start 拒绝）、旧版回切（Start 修复）、已存缺口（只读），按这个顺序第一个命中的胜出，带同一个 `StorageProblem` 和截断分段。`Scheduler.storageHealth` 每次问答建一个段账本。见 ADR 0007。
_Avoid_: 调用方自己串检测器；用 `detail` 前缀倒推问题类别；为同一请求多次列目录

**一次运行 (Run)**:
`MySQLRunner.run` 的一次复制尝试，分五个阶段：续传起点（`resolveRunStart`）、连接/握手（`openFirstDump`）、分段追加与 rotate/封存（`openSegment`）、事件处理（`handleEvent`）、重连与错误分类（`pump`/`readEvent`/`streamEnded`）。见 ADR 0008。
_Avoid_: 在阶段之外改游标、起点或 syncer；为半同步再写一个拉流循环；新增跨阶段的闭包共享局部变量

**备份链 (Backup Chain)**:
一个任务的全部段按顺序排成的链，合并本地账本、目录行和对象存储三处，每段带可读位置。完整性不变量 I1–I7 和可恢复窗口都在链上判定。见 ADR 0009（提议中）。
_Avoid_: 读接口自己拼文件清单或自己截断；只看本地目录就说「完整」

**结论 (Finding)**:
一条持久化的失败或风险记录，有种类、首次/最近出现时间和解决时间。每条失败路径的终点都是一条结论加事件加指标。见 ADR 0009（提议中）。
_Avoid_: 只打日志；`_ = err`；用前端过滤 `state` 当告警

**健康备份 (Healthy Backup)**:
任务在跑、最近收到过事件或源心跳、checkpoint 在推进、没有未解决的严重结论、上传不落后、最近巡检成功。由评估器周期写入。见 ADR 0009（提议中）。
_Avoid_: 把 RUNNING 当作健康；把「没有指标」当作「没有问题」
