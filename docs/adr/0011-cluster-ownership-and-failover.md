# 集群所有权与故障转移：一台状态机

Status: Proposed（2026-10-19；同日第四轮设计评审后修订）。只有设计，没有运行时代码。等本记录被接受之后才按第 5 节改代码。

相关记录：[0001](0001-task-ownership.md)（所有权只有一扇门；worker 注册不是任务所有权）、[0005](0005-single-task-authority-and-segment-catalog.md)（期望与观察分开，meta 是权威，控制回路是唯一启动者）、[0009](0009-backup-assurance.md) 及其设计说明 `docs/design/0009-phase2-health-evaluator.md` 的 §28–§31.6（健康、让出、链委派、封存段缺失）。这些决定里「meta 是权威、操作员写期望、段按 `(task_id, source_file, epoch)` 标识、失败必须留下 finding」都保留。本记录替换的是 §28 之后那一串补丁所依赖的写法：每修一个故障转移缺陷，就在调度器、评估器和健康写入上再加一条特殊路径。

[0010](0010-standalone-task-records.md) 不变。standalone 仍是一个进程，内存仍是运行时权威。下面的状态机在 standalone 上对着内存跑，不新开第二份库。

## English summary

Cluster failover has been patched one incident at a time (ADR 0009 §28 through §31.6, issues #449 #457 #462 #463 #473 #476, PR #471). Each patch is locally plausible and the next QA round finds a new bug, because the system has no single state machine. Task phase, desired run, lease, epoch, chain owner, health and findings are written by the owner, the peer, the claim loop, the control loop, the renew loop, the evaluator and the operator API. Several of those writers treat a fact that is true only on one node, or only for a lost epoch, as a cluster-wide terminal row (`FAILED` and desired `STOP`). The guards that were added to stop this (yield, tail backoff, “do not steal `chain_owner`”) live in process memory, so a restart forgets them, and they are not the same function the next writer consults.

Four failures on the current #471 build, plus #476, are the same hole:

1. A chain finding closes with `OWNER_CHANGED` and opens again with `EVAL` about 100 ms later across a takeover. Close-then-open is what `Reconcile` does on purpose. A finding that is still true must not close.
2. After more than two yields, a node that cannot read the chain directory writes itself into `chain_owner`, then writes `FAILED` / desired `STOP`. The yield guard is “`chain_owner` is someone else”. A holder health write sets `chain_owner` to the writer even when the directory was not read. Once the guard flips, `markFailedLocked` runs, and that function always sets desired `STOP`.
3. `POST /start` on the yielded-then-`FAILED` row returns 400 and leaves `STARTING` with an empty owner permanently. Start persists `STARTING` before it knows the follow-up start will be accepted, and it keeps the old `run_id`. The claim predicate then rejects that same row on every tick.
4. The tail-yield backoff is a map in the scheduler. A restart claims again immediately.
5. #476: an owner stopped while it holds `SELECT … FROM task_leases … FOR UPDATE` blocks every peer with no deadline tied to the lease TTL. The task stays `RUNNING`, the last health row stays `HEALTHY`, `/api/health` stays 200, and no dump is running.

The target is one transition function. Desired `STOP` is written only by an operator Stop or operator Delete. A fact that is not true on every node is a yield, a retry, or a finding. `FAILED` is only the closed terminal allowlist (source authentication, `log_bin` off). Every runner event updates on `row_generation` and `epoch` together, and the same transaction checks the lease row; a newer epoch drops the event instead of reapplying it on the new owner's row. `chain_owner` moves to its own record and is written only after this node has read the chain directory. A chain owner returns from `FAILED` / `CHAIN_OWNER_GONE` to `LEASE_DEGRADED` only after a `ChainVerified` newer than `failed_at`, not on the heartbeat alone. An expired `STOPPING` lease is finalized by any node (`StopExpired`). `DumpReady` deletes every refusal for that task. Only a `per_node` tail refusal is open-ended; every other refusal retries on a 600 second cap, so `epoch` grows at a bounded rate and a disk blip does not stick. Refusals and `tail_unreadable_since` are rows, not maps. The tail finding stays open across claim and Start until a `DumpReady` after that timestamp. The lease-row lock is bounded by a timeout that exists on that server. The default is server-side only, because `cluster.side_kill` defaults to `off`: MySQL 8 uses `wait_timeout` and the net read/write timeouts; MariaDB uses `idle_transaction_timeout` plus those backstops. The published RTO uses that server-side bound. `auto` is opt-in and only for a direct connection; it is unsafe behind any proxy that parses SQL. When it is on, the canary and the real kill are both `CALL binlog_server_kill_trx(trx_id)`. The procedure finds the thread by `trx_id`, checks that it still holds the row lock on `task_leases` or `meta_canary`, and kills inside the server (`mysql.rds_kill` on RDS). The client never sends `KILL`. A reused connection has a new `trx_id`, so the call does nothing. If the procedure cannot be created, `side_kill` stays off. A stalled lock is a critical finding on `_cluster`, written without locking `task_leases`, and HTTP 503. RTO runs from the last successful Renew (or the Acquire, if none) to the new owner's `DumpReady`. Start, Stop, and Delete are one transaction: the HTTP status and the row agree. A rolling upgrade rejects every old `backup_tasks` update that does not bump `row_generation`. The migration prechecks for live registrations and long transactions before it takes a metadata lock.

PR #471 should not be merged. The one behavior worth keeping (a missing sealed segment on a directory this node actually read opens `SEGMENT_MISSING`) is re-landed by itself in a later, smaller change. The yield, backoff and “fail when the chain owner is me” paths in that branch are deleted rather than fixed in place.

## 1. 现状

对照的代码是 main，以及尚未合并的 `fix/462-sealed-segment-missing`（PR #471，含 §31 与 §31.6）。#471 的路径在下面单独标出。main 上已经落地的是 §28、§29、§29.10、§29.11、§30。

### 1.1 角色

| 角色 | 在哪 | 它以为自己在决定什么 |
| --- | --- | --- |
| 操作员 API | `POST /api/tasks/{id}/start` 与 `stop`，`internal/api/handlers_tasks.go` | Start/Stop 的 HTTP 结果就是任务的下一态。除「记录文件写失败」返回 503 以外，其它错误一律 400 |
| 控制回路 | `writeStartIntent`、`decideConverge`、`convergeTask`（`internal/tasks/control_loop.go`） | 期望 `RUN`/`STOP` 对上观察态之后，该启动、该停，还是什么都不做。0005 把这台回路定成唯一启动者 |
| 认领循环 | `ClaimRunnableTasks`（`internal/tasks/scheduler_lifecycle.go`），约 2 秒一次 | 没人要的 `STARTING`、租约已过期的活动行、以及本 worker 名下还没有本地 run 的行，都该拉起来 |
| 租约续期 | `renewLeaseLoop`（`internal/tasks/scheduler_cluster_lease.go`） | 续不上就降级；宽限用尽、无人接手、续期 panic 只结束本 epoch |
| 本 epoch 的 owner | `runTask` / `failRunLocked` / `markFailedLocked` | runner 的错误要么重试，要么把行写成终态 |
| 对端 | 同一套认领与 `startTask` | 租约过期就可以 Acquire。它不读对方的磁盘，只读 meta 里的目录行和路径 |
| 评估器 | `Evaluator.evaluate`（`internal/tasks/evaluator.go`） | 谁握着任务租约谁评全量；`_health` 持有者代写无主任务；`chain_owner` 等于自己时委派评 `chain.local` |
| 健康存储 | `ApplyTransitions`（`internal/meta/health_store.go`） | 一次事务里改 finding、写 `task_health`，并锁 `task_leases` |
| 元数据库 | `backup_tasks`、`task_leases`、`task_health`、`task_findings`、`worker_registrations`、`worker_heartbeats` | 0005 说它是期望和观察的权威。实际上下面每个写者都在写它，而且有的写者不带栅栏 |

0001 的那句话仍然成立，但门后面已经不是一个写者：占上、续上、放开之外，失败、让出、代写、委派都在改同一行。

### 1.2 谁写哪一列

任务行的普通落库是 `persistTaskLocked` → `UpsertTask`。这条按 `spec_revision` 防旧写覆盖新规格，**不**按 epoch 栅栏，也**不**按「我还持有租约」栅栏。带租约栅栏的任务行写入只有停机收尾（`persistTaskFencedLocked`：`failSafeStopLocked`、`writeStoppedLocked`）。`FAILED` 走的是不带栅栏的 upsert。

| 写者 | 条件 | `state` | `desired_run` | owner / epoch | 栅栏 |
| --- | --- | --- | --- | --- | --- |
| `writeStartIntent` | 观察态是 `CREATED`/`STOPPED`/`FAILED`/`RETRY_BACKOFF`/`STOPPING`。`LEASE_DEGRADED`、`RUNNING`、`STARTING` 直接拒绝 | `STARTING` | `RUN`，`spec_revision+1`，清 `last_error` 和两个计数 | 只有控制面（没有 runner）清 owner、epoch、`run_id`。有 runner 的进程保留内存里的 owner 和 `run_id` | 无。先落库再改内存（`commitTransitionLocked`） |
| `StopTask` | `RUNNING`/`RETRY_BACKOFF`/`STARTING`/`LEASE_DEGRADED` | `STOPPING` | `STOP` | 不在 API 进程里取消别人的 dump | 无 |
| `markFailedLocked` | runner 认定不可恢复，且行还不是 `FAILED` | `FAILED` | **`STOP`**，`failed_spec_revision = spec_revision` | 清空 owner、epoch、`run_id` | 无。随后 `Release` 租约 |
| `markRetryBackoffLocked` / `markLeaseDegradedLocked` / `markRunnerReadyLocked` | 重试、续租失败、dump ready | 对应观察态 | 不动（保持 `RUN`） | 不动 | 无 |
| `yieldForResumeLocked` | 宽限用尽、租约丢失且没有新主人、续租 panic（§29） | 内存留在 `LEASE_DEGRADED` | 保持 `RUN` | `LeaseClock.Lose`，等 run 退出再 `Release` | 不把 `STOPPED` 写出去 |
| `yieldFencedEpochLocked` | 没有新主人的 `WRITE_FENCED`（§29.10，#454） | 内存留在 `LEASE_DEGRADED` 或 `RETRY_BACKOFF` | 保持 `RUN` | 同上一行 | 无落库的 `STOPPED` |
| `yieldLostLeaseLocked` | 行上已经是别的 owner 或更大的 epoch | **只改本进程内存** | 不动共享行 | 本地把 owner 清掉 | 不写库 |
| `ReconcileLegacyDesiredRun` | 每轮认领，`spec_revision = 0` 的旧行 | 不动 | 按观察态回填 `RUN` 或 `STOP` | 不动 | 无。会把旧 `FAILED` 写成 `STOP` |
| #471 `yieldUnreachableTailLocked` | 集群、评估器 on、本节点握着租约、错误是 `SEGMENT_NOT_ON_WORKER`，且 `chain_owner` 非空、不是自己（读失败也让出） | `LEASE_DEGRADED` | 保持 `RUN` | 行上 owner 清空、epoch 写成 0。`run_id` **留着** | 让出前用 `PreserveChainOwner` 写一条 finding |
| #471 `failYieldedTail` | 代写轮上看见让出仍在，且链主人心跳早于 `node_gone_after` | `FAILED` | **保持 `RUN`**（故意不走 `markFailedLocked`） | 不动，所以 owner 仍空、`run_id` 仍在 | 无 |
| #471 上 `chain_owner` 等于自己时的 `SEGMENT_NOT_ON_WORKER` | `yieldsUnreachableTail` 返回 false，落到 `failRunLocked` | `FAILED` | **`STOP`**（`markFailedLocked`） | 清空 | 无 |

租约行（`internal/meta/sql/lease.sql`）：

| 操作 | 规则 |
| --- | --- |
| `Acquire` | `lease_expire_at <= NOW(6)` 才换 owner。owner 变了 epoch 才加一。同一 `worker_id` 在过期后续租 epoch 不变。没有 `FOR UPDATE` |
| `Renew` / `Release` | 必须 owner 与 epoch 都对上。`Release` 把 owner 写成空、过期时间写成现在，**epoch 留在行上** |
| `Lose` | 不是 SQL。`LeaseClock.Lose` 只活在本进程 |
| 健康写入与数据面栅栏 | `SELECT … FROM task_leases WHERE task_id=? FOR UPDATE`（`health_store.go` 的 `applyOnce`，`fenced_exec.go` 的 `leaseRowForUpdate`）。锁住之后才改 `task_health` 或 checkpoint / 目录行 |

`task_health`（迁移 `000009`，写入在 `applyOnce`）：

| 列 | 谁写 | 条件 |
| --- | --- | --- |
| `chain_owner` | 集群持有者那一次 `ApplyTransitions` | `FenceCluster` 且不是 `Proxy`、不是 `Delegated` 时写成 `fence.Node`。**不要求这次真的读到了目录**。代写、委派、`PreserveChainOwner`（仅 #471 的尾巴上报）保持原值 |
| `owner` / `owner_epoch` | 同一次 UPDATE | main 上写成栅栏的节点和任务 epoch。无主行的代写因此会写成 `_health` 持有者。#471 改成跟任务行，行上没有 owner 就是空 / 0 |
| `fence_*` / `generation` / `evaluated_at` / `status` | 每一次成功的 apply | generation 由这次写入加一。`evaluated_at` 用库时钟 |
| finding 行 | 同一次事务里的 `Reconcile` 结果 | 见 1.5 |

认领看得到的观察态（`listTasksWithExpiredLeaseSQL`、`listStartingUnownedTaskSQL`、`isClaimableActiveState`）：

- 过期租约：`RUNNING`、`LEASE_DEGRADED`、`RETRY_BACKOFF`、`STOPPING`，外加 `STOPPED` 且 desired `RUN` 且 owner 为空（§29.10 的补丁）。
- 空 owner 的 `STARTING`：`ListStartingUnownedTasks`，不看 `run_id`。
- `FAILED` 不认领。desired `STOP` 的活动行不认领。
- 不在这张表里的形状（`STOPPED` + desired `RUN` + owner 非空、以及任何写者新造出来的组合）没有人拉。#454 就是这么停住的。

### 1.3 散落的标志，以及只活在内存里的状态

这些都不在 meta 里。进程一重启就没了；对端永远看不见。

| 名字 | 在哪 | 干什么 | 重启之后 |
| --- | --- | --- | --- |
| `LeaseClock` | 本进程 | 这个 epoch 已经 `Lose`，数据面写入会被 `WRITE_FENCED` | 新进程不认为丢过 |
| `yielded` / `yieldRelease` | `Scheduler` | 本进程已经放弃这一轮，run 退出时补 `Release` | 靠下一轮认领再释放；释放本身可以重试（`pendingRelease`） |
| `pendingRelease` | 同上 | meta 不可达时记下的释放 | 丢。认领循环只重试本进程还记得的 |
| `unpublished` / `persisted` generation | 同上 | 落库在飞时不要用旧行覆盖内存 | 丢。行上以最后一次成功 upsert 为准 |
| `tailYieldUntil` | **仅 #471** | `SEGMENT_NOT_ON_WORKER` 让出之后，本进程在 `health.tail_yield_backoff_sec`（缺省 60 秒）内不再认领。操作员 Start 删这一个键 | **丢。重启立刻可以再认领再让出** |
| 评估器的 `delegated` / `fenced` / `unsaved` | 本进程 | 这一轮委派被拒、健康写失败时的内存账 | 丢 |
| `worker_registrations` | meta，但是另一把租约 | TTL 取 `cluster.lease_ttl_sec`（`effectiveWorkerRegistrationTTL`）。SIGSTOP 不释放。同一 `worker_id` 的新进程在它过期之前起不来 | 还在。认领 SQL **不**读这张表 |
| `data_dir` 是否共享 | **没有这个事实** | 代码用 `pathUnderDataDir` 比较绝对路径。读不到或路径不在本节点 `data_dir` 下就是 `SEGMENT_NOT_ON_WORKER`。共享挂载和各节点一份盘，走的是同一条错误 | — |

宽限（`lease_grace_sec`，缺省 30）只在**本进程的续租循环**里计时。owner 被 `SIGSTOP` 时这个循环一起停，宽限不会在对端生效。对端能用的只有 `lease_expire_at`。

### 1.4 历史问题，以及缺的那条不变量

每一行都是「当时的补丁让症状对上了，下一条不变量仍然没有」。

| 问题 | 发生了什么 | 缺的不变量 |
| --- | --- | --- |
| #203（0005，已修） | 认领循环每 2 秒把还在 `RETRY_BACKOFF` 的任务再启动一次，计数归零，epoch 一直涨 | 重试预算在行上；已经归自己的运行不会被再启动 |
| #450 / §29 | meta 中断超过宽限，行变成 `STOPPED`、desired `STOP`、没有 owner。`TASK_NO_PROGRESS` 因为 desired 是 `STOP` 不开，`HEALTH_STALE` 因为健康行还在刷新也不开 | **只有操作员写 desired `STOP`**。没有主人且期望仍是 `RUN` 时，必须有人能认领，并且有一条 CRITICAL |
| #454 / §29.10 | 宽限已经 `Lose` 之后，堵在 meta 上的 append 返回 `WRITE_FENCED`。内存收成 `STOPPED`，迟到的再发布把这行写出去。认领查询都匹配不到 | 没有新主人的栅栏拒绝必须落在认领看得到的观察态上。**不存在认领查询覆盖不到的合法形状** |
| #457 / §29.11 | 对端接管并删掉旧 `.open.eN`。旧 owner 的 `stat` 得到 `ENOENT`，`markFailedLocked` 写成 `FAILED` / desired `STOP`，新 owner 取消自己还健康的 dump | **只对一个已经丢掉的 epoch 为真的事实，不能写成集群终态** |
| #449 / §28 | 健康行不再刷新时没有信号 | 停滞必须是一条 finding。这一条是 WARNING，不让 `/api/health` 变 503。#476 说明「WARNING 且最后一行仍是 HEALTHY」盖不住锁死 |
| #421 起的 `SEGMENT_NOT_ON_WORKER` | 对端磁盘上没有前一个 owner 的尾巴，任务 `FAILED` / desired `STOP` | 同上，而且当时把「本节点读不到」当成了「集群失败」 |
| #462 | 集群里 `chainFactsInto` 丢掉 `I6_MISSING_LOCAL`。删掉一个封存段没有 finding，改坏一个字节却有 `SEGMENT_UNREADABLE` | **在已经读到的目录上，缺段必须是 finding**。这条 continue 是委派做完之前的防护，§30 之后读目录的人已经是持有者或被委派的链主人，继续丢就是静默 |
| #463 | 每节点一份盘，`lease_ttl` 60、宽限 30，对端约 143 秒才接管，和 n1 的 worker 注册过期对齐，而不是和 TTL 对齐 | **RTO 由 TTL 加上写明的超时构成**。注册行不是任务所有权（0001），但 SIGSTOP 既不释放注册、也不结束它正握着的事务，现场就多出一段没有上界的等待 |
| #473 | 每节点存储，owner `SIGSTOP` 超过 TTL。没有目录的对端认领，`SEGMENT_NOT_ON_WORKER` 写成 `FAILED` / desired `STOP`，原主人 `SIGCONT` 之后被「别的 epoch 把行写成 FAILED」取消。链还在原主人磁盘上 | **只在一个节点上为真的事实不是集群终态**。只有操作员写 desired `STOP`。和 #457 是同一个洞，对象从「旧 epoch 的文件」换成了「本节点的目录」 |
| #476 | owner 停在持有 `task_leases` 行锁的事务里。对端的 `FOR UPDATE` 排上队。数分钟内行仍是 `RUNNING`，`task_health` 仍是 `HEALTHY`，`/api/health` 200，源上 dump 线程是 0 | **锁等待有上界，而且上界算进 RTO**。评估器拿不到锁时不能继续展示上一行 `HEALTHY`。停滞是 CRITICAL，`/api/health` 503 |
| #471 QA（1） | 接管时一条 finding 以 `OWNER_CHANGED` 关闭，约 100 ms 后以 `EVAL` 再打开 | **finding 在事实还在时不关闭**。换 owner 只改「谁有权关」，不制造一次关、一次开 |
| #471 QA（2） | 让出超过两次之后，读不到链目录的节点把自己写成 `chain_owner`，接着 `FAILED` / desired `STOP` | **`chain_owner` 只有读到了目录的节点能写**。让出的守卫不能是「这一列目前是不是我」——因为写这一列的人就是接下来要被守卫挡回去的人 |
| #471 QA（3） | 对让出之后的 `FAILED` 行 `POST /start` 返回 400，行停在 `STARTING`、owner 为空，不再有人接 | **一次操作员调用要么把行推进它声明的下一态，要么行保持原样**。空 owner 且期望 `RUN` 的 `STARTING` 必须能被认领 |
| #471 QA（4） | 退避在 `tailYieldUntil`。重启的进程没有这张表，立刻再认领 | **拒绝认领的期限在 meta 里，用库时钟** |

### 1.5 这四条 QA 在代码里怎么走通

**（1）关了又开。** `Registry.Reconcile`（`internal/health/reconcile.go`）在 `Facts.ChainOwnerChanged` 时，把 scope 只有链目录的打开 finding 以 `OWNER_CHANGED` 关闭，**即使这一轮目录不完整**。同一轮里新目录仍然要这个 key，就再以 `EVAL` `OPEN`。同一 key 的 `CLOSE` 排在 `OPEN` 前面，两次写入两个 generation。`TestOwnerChangedReopensTheSameKey` 把这个顺序定成了规格。§31.6 只补了一个洞：`SEGMENT_UNREADABLE` 在 `chain.local` 还没评完、或 subject 还在 `AwaitRehash` 里时不关。其它链 finding 仍按 §30 在不完整目录上关闭。`AwaitRehash` 只在 `chainFactsInto` 走到末尾才填，链锁、checkpoint、目录、链顺序任何一处提前返回，名单是空的，补丁不生效。QA 看到的约 100 ms，就是这两次 apply。

**（2）让出两次之后把自己写成链主人。** 让出守卫是 `yieldsUnreachableTail`：`chain_owner` 非空且不是本节点才让出；是本节点就落到 `failRunLocked` → `markFailedLocked`（desired `STOP`）。`chain_owner` 的赋值在 `applyOnce`：只要这次是集群持有者写入，就设成 `fence.Node`，不看 `chainFactsInto` 有没有返回目录。对端 Acquire 之后、runner 返回 `SEGMENT_NOT_ON_WORKER` 之前，它就是持有者。评估器在这个窗口里跑一轮，目录 `Stat` 失败、`chainFactsInto` 返回 nil，健康事务照样提交，`chain_owner` 变成这个没有文件的节点。下一次（或同一次，取决于扫到的时机）守卫翻转。代码里没有「两次」这个数；QA 是在两次让出都还看见旧 `chain_owner` 之后，第三次撞上了这个窗口。`PreserveChainOwner` 只盖住 `reportUnreachableTail` 那一次写，盖不住持有者的普通 sweep。这正是 §31 D3 写过要禁止的事。

**（3）400，行却是 `STARTING` 且永远没有 owner。** 让出把行收成 `LEASE_DEGRADED`、owner 空、epoch 0，`run_id` 从上一轮抄下来，没有清。`failYieldedTail` 再把它写成 `FAILED`、desired 仍是 `RUN`，`run_id` 仍在。`operatorStartState` 接受 `FAILED`，不接受 `LEASE_DEGRADED`：

- 还停在 `LEASE_DEGRADED` 时，Start 在落库前返回 `cannot start from state LEASE_DEGRADED`，400，行不动。操作员拉不起来一次让出。
- 已经是上述 `FAILED` 时，`writeStartIntent` 先把 `STARTING` 落库。有 runner 的进程**不**清 `run_id`。随后 `StartTask` 调用 `convergeTask` → `startTask`。此时观察态已经是 `STARTING`，`canClaimDispatched` 要求 `run_id` 为空，不满足；过期接管和空闲续跑也不认这行。`startTask` 返回 `cannot start from state STARTING`。API 把这个错误写成 400。行已经是 `STARTING`、owner 为空、`run_id` 非空。
- `ClaimStartingTasks` 会把这行装进内存再走同一条 `startTask`，每轮得到同一个错误就 `continue`。没有别的查询认领它。

`commitTransitionLocked` 的「先落库」只保证写失败时内存不变。它不保证同一次 HTTP 调用里后面的 `startTask` 失败时把行退回去。400 和行上的 `STARTING` 就是这么同时出现的。

**（4）退避在内存里。** `tailYieldBlocked` 读 `tailYieldUntil`。重启没有这个 map。§31.6.4 把「不落库」写成了本步不做。重启因此是绕过退避的办法。

**（#476）行锁没有上界。** `applyOnce` 和 `FencedExec` 都对 `task_leases` 做 `FOR UPDATE`。应用侧的租约调用有约 2 秒的上下文超时，栅栏写有约 5 秒，评估器 sweep 有自己的 meta 超时；健康事务在 1213/1205 上最多重试 50 次。这些超时都在**还在跑的进程**里。owner 被 `SIGSTOP` 在事务中间时，它的会话停在 Sleep，事务保持 `RUNNING`，上下文超时不会触发。对端的 `Acquire`（同一行的 `UPDATE`）和 `FOR UPDATE` 都等这把锁。默认的 `innodb_lock_wait_timeout` 不由本进程设置。锁释放之前，任务行不会被对端改掉，评估器也写不进新的健康，于是上一行 `HEALTHY` 一直被 `/api/health` 读走。dump 线程已经不在跑。这是静默停备，RTO 等于 owner 停多久。

## 2. 必须成立的不变量

下面每一条都能用第 1 节的某一行当反例。目标模型以这些为检查条件，而不是再为每一行加一个 `if`。

1. **只有操作员写 desired `STOP`。** 写者是 `StopTask`。`FAILED`、让出、宽限、`WRITE_FENCED`、`ENOENT`、`SEGMENT_NOT_ON_WORKER`、链主人心跳消失，都不写。操作员确认放弃丢失尾巴（0009 §31 D6，本记录仍不实现那个 API）算操作员动作，等那个 API 存在时再写。
2. **只在一个节点上为真、或只对一个已经丢掉的 epoch 为真的事实，不是集群终态。** 它可以成为本节点的拒绝、一条 finding、一次让出。它不能是 `FAILED`，也不能取消另一个节点上还健康的 dump，也不能把任务永久留在 `LEASE_DEGRADED`。`refuse_until = NULL` 只用于 `per_node` 上的 `TailUnreadable`。`shared` 上的 `TailUnreadable` 和全部 `EpochLost` 都会到期再试。
3. **一个 epoch 最多一个写者，并且写完就被栅栏挡住。** 数据面沿用 0009 §23 的租约时钟。会改任务行的事件都在同一事务里核对租约 epoch（第 3.2 节）。runner 事件的条件更新同时带 `row_generation` 和 `epoch`；行上 epoch 已经往前走就丢弃事件、停掉本地 run，不重放到新主人的行上。不再用无条件 upsert 收尾。
4. **`chain_owner` 只有读到了链目录的节点能写。** 读到是指本节点 `data_dir` 下该任务目录 `Stat` 和 `ReadDir` 都成功，并且这次评估用的就是那份结果。目录不存在、读失败、链锁没拿到、提前返回，都不写。持有任务租约不是读到目录。
5. **finding 的开区间跨过所有权变化仍然单调。** 事实还在，finding 就还开着。换 owner 改的是谁有权关闭、以及归因，不是先关再开。不完整的观察既不开新的链 finding，也不关旧的。
6. **RTO 有公式，并且起止点写死。** 起点是最后一次成功续租的库时钟（`task_leases.renewed_at`，即那次续租语句里的 `NOW(6)`）；本 epoch 还没有成功续租时，起点是 `Acquire` 提交的库时钟。终点是新 owner 的 `DumpReady` 提交（新 epoch 上的观察态 `RUNNING`）。上界是 `lease_ttl + max(idle_kill, lock_wait) + claim_interval + start_to_ready`。`idle_kill` 是这一 flavor 真正武装的会话杀上限（MySQL 8 的 `wait_timeout`，MariaDB 的 `idle_transaction_timeout`），`lock_wait` 是 `innodb_lock_wait_timeout`，`start_to_ready` 是「`STARTING` 已提交、目录可读、源接受 dump」到 `DumpReady` 提交的预算，写进发布说明。宽限不进公式。另一个 `worker_id` 的注册 TTL 不进跨节点公式。同一 `node_id` 在 `per_node` 上重启要不要把注册 TTL 算进它自己的 RTO，留到 PR-2（第 6 节 F10）。公式之外、并且是 **owner 已经不在** 的等待，必须已经是一条 CRITICAL finding，并且 `/api/health` 是 503。owner 还活着、dump 却不往前走的上界是配置的 `no_progress_after` 再加一轮 sweep（缺省 120 + 60 = 180 秒），与今天相同，不另用这条 RTO 去夹。
7. **没有静默停备，也不把健康集群打成 503。** 期望 `RUN`、没有活 owner、并且不在合法的 `RETRY_BACKOFF` 窗口里时，一个 sweep 之内有一条 CRITICAL（已有的 `TASK_NO_PROGRESS`，或锁等待、尾巴无人能读）。合法窗口是：预算未用尽，且库时钟上的退避期限未到。期限一过仍没有 `DumpReady`，下一个 sweep 开 CRITICAL。有活 owner 时，进展停滞用配置的 `no_progress_after`（缺省 120 秒），不夹到 RTO；安静的源在这个窗口内不升 CRITICAL。没有活 owner 的那一支用 sweep，不用被夹短的 `no_progress_after`。`stale_after` 的有效值是 `max(2×sweep + lock_wait, min(S, RTO 上界))`，其中 S 在配置为 0 时是 5×sweep，否则是配置值。缺省 sweep 60 秒、`lock_wait` 5 秒、S = 300 秒、RTO = 32 秒时，有效值是 `max(125, 32) = 125` 秒，大于一轮 sweep，健康集群的 `evaluated_at` 不会老过它，`/api/health` 保持 200。`evaluated_at` 老于这个有效值，或本轮因为锁没写上时，最后一行 `HEALTHY` 不能再被当成健康。不把 `sweep <= RTO/2` 当成启动失败：缺省 sweep 60 秒大于缺省 RTO 的一半，那条校验会拒绝今天的配置；上面的下限代替它。
8. **每一次操作员调用是一个事务，而且 2xx 声明的态走得完。** HTTP 4xx/5xx 时，行与调用前相比没有新的观察态。HTTP 2xx 时，行在调用返回前已经是这次调用所声明的态。声明成 `STARTING` 且期望 `RUN` 的，在第 9 条的运行认领集合里。声明成 `STOPPING` 且期望 `STOP` 的，在第 9 条的停止收尾集合里：持有者 `StopConfirmed`，或者租约过期后任一节点 `StopExpired`。`pending_delete` 置位时 Start 是 409，不是「已经在跑」的 204。不允许 400 加上一个没有 owner 的 `STARTING`。不允许 204 的 Start 落在一行即将被删掉的 `RUNNING` 上。
9. **两个并集都盖住，没有死角。** 运行认领集合是：期望 `RUN`、`pending_delete` 未置位、没有活着的 owner，且观察态是空 owner 的 `STARTING`、`LEASE_DEGRADED`、租约已过期的 `RUNNING`、期限已到的 `RETRY_BACKOFF`，或 `STOPPED` 且 owner 为空。`FAILED` 不在这个集合里。离开 `FAILED` 的事件只有 `OperatorStart` 和满足第 3.2 节守卫的 `ChainOwnerBack`。停止收尾集合是：期望 `STOP`、观察态 `STOPPING`、租约为空或已过期。任一节点用 `StopExpired` 把它收成 `STOPPED`，`pending_delete` 置位则同一事务删除。今天过期的 `STOPPING` 能被认领（第 1.2 节），这个集合把它留在模型里。活租约还在 TTL 内的行由持有者自己收尾，不在这两个集合里，也不是死角。除此之外，不存在要靠下一轮补丁才能被接住的形状。
10. **拒绝认领的期限在 meta 里，用库时钟。** 重启、对端、操作员之外的第三个进程，看到的是同一份期限。

## 3. 目标模型

### 3.1 选哪一个

考虑过三条路，第 3.7 节写为什么不选。选定的是：

**一台纯函数做观察态过渡，租约和链主人分成两条记录，过渡用 `row_generation` 与 `epoch` 一起做条件更新。** 谁都可以提出事件（runner、续租循环、认领循环、评估器、操作员 API）。落库的只有这台函数算出的下一条记录。`row_generation` 对不上、但租约 epoch 仍是提出者的，才重读再提，不当成任务失败。行上 epoch 已经更大，事件丢弃，停掉本地 run，不写到新主人的行上。

不把「集群里只留一个进程负责所有写入」当成目标。字节在哪台机器上，dump 就在哪台机器上。一个远程代写者会把没看见的目录写成集群事实，这正是 §30 之前代写清掉 `chain_owner`、以及 #471 上持有者 sweep 偷走 `chain_owner` 的结构。纯函数可以在每个进程里跑；权威是条件更新之后的那一行，不是某个进程的内存。

0005 的分工保留：操作员只写期望和规格，控制回路是唯一把期望变成一次 dump 的地方。变的是回路里那些直接改 `state` 的函数（`markFailedLocked`、`yieldUnreachableTailLocked`、`failYieldedTail`）收成事件，由同一台函数接受或拒绝。

### 3.2 状态、事件、守卫

期望与观察继续分列。期望只有 `RUN` 和 `STOP`。

观察态沿用现有枚举，不在这一步发明 API 上的新单词：`CREATED`、`STARTING`、`RUNNING`、`LEASE_DEGRADED`、`RETRY_BACKOFF`、`FAILED`、`STOPPING`、`STOPPED`。`LEASE_DEGRADED` 在这台机器里的含义收紧为：**本 epoch 已经放开，期望仍是 `RUN`，还没有下一个 owner**。它是可以认领的，也是操作员 Start 可以接受的。今天 Start 拒绝它，是第 1.5 节的洞。

新增的持久事实不塞进 `state` 字符串：

| 记录 | 键 | 内容 | 谁写 |
| --- | --- | --- | --- |
| `task_chain` | `task_id` | `owner_node`、`verified_at`（库时钟，允许 NULL）、`verified_epoch` | 已验证的值仅 `ChainVerified` 写。`verified_at` 为 NULL 表示回填的未验证候选，见第 5 节。`TailUnreadable` 里「本节点是链主人」只在 `verified_at` 非空时成立 |
| `task_node_refusal` | `(task_id, node_id)` | `refuse_until`（库时钟；仅 `per_node` 的 `TailUnreadable` 允许 NULL）、`reason`、`attempts` | `TailUnreadable` 与 `EpochLost`。认领在 `refuse_until` 未到时跳过这个节点；NULL 也跳过。`DumpReady` 在同一事务里删掉本任务的全部拒绝行。操作员 Start 也删。拒绝行与让出写在同一事务里这一条的实现核对留到 PR-4（第 6 节 F12） |
| `backup_tasks.failed_reason` | `task_id` | 空，或终态允许名单里的码，或 `CHAIN_OWNER_GONE`，或迁移写入的 `LEGACY_UNKNOWN` | 进入 `FAILED` 的那次过渡。离开 `FAILED` 时清空。两种 `FAILED` 靠这一列分开，不靠 desired |
| `backup_tasks.failed_at` | `task_id` | 库时钟，或空 | 进入 `FAILED` 时写下。`ChainOwnerBack` 要求此后有一次新的 `ChainVerified`。离开 `FAILED` 时清空 |
| `backup_tasks.tail_unreadable_since` | `task_id` | 库时钟，或空 | `TailUnreadable` 在它还空的时候写下，重复让出不往前拨。只有 `DumpReady` 清空。`OperatorStart`、进入 `STARTING`、清 `last_error` 都不动它 |
| `backup_tasks.pending_delete` | `task_id` | 布尔 | `OperatorDelete` 在仍有活租约时置位。`StopConfirmed` 在同一事务里删行并清掉 |

`backup_tasks` 加 `row_generation`（`BIGINT NOT NULL DEFAULT 0`）。每一次接受的过渡加一。现有的无条件 upsert 在切到这台函数的那个 PR 里删掉，不留两条写路径。

`task_health.chain_owner` 停止被持有者写入维护。读的人改读 `task_chain`。列先留着，由写 `task_chain` 的同一事务填上，避免评估器还有旧二进制时读到空；旧语义（「谁最后跑了一轮持有者评估」）作废。`chain_owner` 不再是 `ChainOwnerGone` 的权威。

**每一笔事件都核对 epoch。** runner 事件（`DumpReady`、`RetryableError`、`TerminalError`、`TailUnreadable`、`EpochLost`、`StopConfirmed`）的条件更新是 `UPDATE … WHERE row_generation = ? AND epoch = ?`，并且同一事务 `SELECT task_leases FOR UPDATE`，`owner_worker_id` 与 epoch 都要等于事件里的值。行上 epoch 更大：丢弃事件，停掉本地 run，任务行一个字都不改。只是 `row_generation` 落后、epoch 仍是自己的，才重读再提。决定当时读不到租约行，就不得假定「租约还是我的」，走 `EpochLost`。操作员事件不携带旧 owner 的 epoch；它们在同一把租约锁下看 epoch 是否还活着，活着就不写（下面三行 `OperatorStart`）。

`node_gone_after` 配置为 0 时，有效值取 `max(3×sweep, lease_ttl + 2×sweep)`，不拒绝启动。缺省 sweep 60 秒、TTL 15 秒时这是 `max(180, 135) = 180` 秒，与今天的 3×sweep 相同。sweep 5 秒、TTL 60 秒（harness 和现场）时这是 `max(15, 70) = 70` 秒，升级后不改配置也起得来，并且第一次让出不会在 TTL 之内触发 `ChainOwnerGone`。只有把 `node_gone_after` 显式写成小于 `lease_ttl + 2×sweep` 的正数，才拒绝以 cluster 模式启动。

`health.refusal_retry_cap_sec` 缺省 600，负数拒绝启动。它是 `shared` 上 `TailUnreadable` 和全部 `EpochLost` 的重试上限，不是 `per_node` 那条 NULL 拒绝的期限。

事件与守卫：

| 事件 | 谁可以提出 | 守卫 | 结果 |
| --- | --- | --- | --- |
| `OperatorStart`（推进） | 操作员 API。一个事务，锁 `task_leases` | 观察态是 `CREATED`、`STOPPED`、`FAILED`、`RETRY_BACKOFF`、`LEASE_DEGRADED`。租约为空或已过期，于是本事务里 `Acquire` | 期望 `RUN`，`spec_revision+1`，计数清零，`last_error` 空，`run_id` 空，观察态 `STARTING`，owner 与 epoch 是这次 `Acquire` 的值。删掉本任务全部 `task_node_refusal`。**不清** `tail_unreadable_since`。返回 204。`Acquire` 和写入 `STARTING` 同一笔提交。锁下看到期望已经是 `STOP` 则整笔回滚。本地收敛失败是 finding，不再把 HTTP 改成 400 |
| `OperatorStart`（幂等） | 操作员 API | 观察态已经是 `STARTING` 或 `RUNNING`，这次请求不改规格，且 `pending_delete` 未置位 | 204。不写行，不加 `spec_revision`，不动 epoch |
| `OperatorStart`（拒绝） | 操作员 API | `STOPPING`；`pending_delete` 已置位（含仍是 `RUNNING` 的窗口，以及改规格）；其它不在推进名单里的态；`STARTING`/`RUNNING` 但请求要改规格；或者另一个 worker 还握着未过期的租约 | 409，正文是当前观察态；租约被别人握着或 `pending_delete` 置位时正文同时写明。行不变，不清 owner，不清 epoch。不留下一份要空等一个 TTL 的 `STARTING` |
| `OperatorStop` | 操作员 API，一个事务 | 活动态，含 `LEASE_DEGRADED`、`STARTING`、`RUNNING`、`RETRY_BACKOFF`。已经是 `STOPPING` 且规格未变则走幂等 | 期望 `STOP`，观察态 `STOPPING`，`spec_revision+1`。幂等时 204，不加 revision。真正的 `STOPPED` 等 `StopConfirmed`，持有者若已经不在则等 `StopExpired` |
| `StopConfirmed` | 握着 dump 的 worker，`Close` 已返回 | epoch 与租约行一致 | 观察态 `STOPPED`，释放租约，owner 清空。`pending_delete` 置位时，同一事务删除任务行、`task_chain`、拒绝行和 finding。epoch 对不上则丢弃事件 |
| `StopExpired` | 认领循环或 `_health` 持有者，任一节点 | 期望 `STOP`，观察态 `STOPPING`，租约为空或已过期 | 观察态 `STOPPED`，owner 清空，过期租约释放。`pending_delete` 置位时同一事务删除任务行、`task_chain`、拒绝行、finding 和租约。持有者 `kill -9` 或一直 `SIGSTOP` 过了 TTL 之后，不需要那个进程再发 `StopConfirmed` |
| `OperatorDelete` | 操作员 API。一个事务，锁 `task_leases` | 无，或有未过期的 owner | 本事务把期望写成 `STOP`，观察态写成 `STOPPING`，`pending_delete` 置位。租约未过期时 owner 与 epoch 不动，204，行是 `STOPPING` 而不是原来的 `RUNNING`。租约已空或已过期时，同一事务接着做 `StopExpired` 的删除，204，行已经不在。不释放自己不持有的活租约，不留下「epoch 已加、观察态还是旧的」 |
| `LeaseExpired` | 认领循环，租约行已经过期 | 期望 `RUN`；`pending_delete` 未置位；本节点没有未过期或 `refuse_until` 为 NULL 的拒绝行；观察态在第 2 节第 9 条的运行认领集合里。期望 `STOP` 的过期 `STOPPING` 不走这一行，走 `StopExpired` | 与 `Acquire` 同一事务进入 `STARTING`，epoch 按现有规则加一。锁下期望已经是 `STOP` 则整笔回滚，不加 epoch |
| `DumpReady` | 本 epoch 的 runner | 上面的 epoch 栅栏通过 | `RUNNING`，计数清零，清空 `tail_unreadable_since` 和 `failed_reason`。同一事务 `DELETE` 本任务全部 `task_node_refusal`，`attempts` 随之消失。`per_node` 上让出过的对端因此可以参加下一次接管，不必再等一次操作员 Start |
| `RetryableError` | 本 epoch 的 runner | 在 0005 的重试允许名单上，预算未用尽，并且决定时 meta 确认租约仍是自己 | `RETRY_BACKOFF`，同一 epoch，预算列加一。本节点的栅栏写撞上 1205、以及本会话被 idle kill 之后的 1927/2013 且 `lease_expire_at` 仍在将来，走这一行，不走 `FAILED` |
| `TerminalError` | 本 epoch 的 runner | **只有**终态允许名单：源认证失败，`log_bin` 关闭。两者在任何节点上都会失败。名单是闭的，加一条要改本记录 | `FAILED`，`failed_reason` 为该码，`failed_spec_revision = spec_revision`，释放租约。**期望保持 `RUN`**。认领不拉 `FAILED`。下一次 `OperatorStart` 是新的 spec |
| `TailUnreadable` | 本 epoch 的 runner，错误是 `SEGMENT_NOT_ON_WORKER` | `task_chain` 上不存在「`owner_node` 是本节点且 `verified_at` 非空」。未验证的回填行不算链主人。本节点这次没有 `ChainVerified`。epoch 栅栏通过 | 释放本 epoch。观察态 `LEASE_DEGRADED`，期望 `RUN`，owner 空，**`run_id` 空**。`tail_unreadable_since` 还空才写成现在的库时钟。`per_node`：`refuse_until` 为 NULL，本节点在 `DumpReady` 或操作员 Start 删掉该行之前不得再 `Acquire`，同一条未读尾巴上每个这样的节点至多贡献一个 epoch。`shared`：不写 NULL，按 `EpochLost` 的同一条退避写会到期的 `refuse_until`，挂载闪断之后还能再接管。尾巴 finding 的事实是 `tail_unreadable_since`，不是 `last_error` |
| `ChainVerified` | 评估器，且仅当第 2 节第 4 条的读目录成功 | 本节点是任务租约持有者，或已经是 `task_chain.owner_node` 且租约已释放（今天的委派） | 把 `owner_node` 写成本节点，`verified_at` 写成库时钟，`verified_epoch` 写成这次读到的 epoch。覆盖回填留下的 NULL。不改变任务观察态。最新封存段必须与 catalog 一致这一条留到 PR-5（第 6 节 F7），本事件的守卫先仍是第 4 条 |
| `ChainOwnerGone` | `_health` 持有者的代写轮 | `task_chain.owner_node` 非空（`verified_at` 为 NULL 的回填行也用来看这个节点的心跳），该节点心跳早于有效 `node_gone_after`，观察态是 `LEASE_DEGRADED`，且 `tail_unreadable_since` 非空 | 观察态 `FAILED`，`failed_reason = CHAIN_OWNER_GONE`，`failed_at` 写成库时钟，期望保持 `RUN`，finding 保持打开。这不是 desired `STOP`。自动出口是 `ChainOwnerBack`，不是操作员 Start |
| `ChainOwnerBack` | 链主人节点自己 | 观察态 `FAILED`，`failed_reason = CHAIN_OWNER_GONE`，期望 `RUN`，本节点是 `task_chain.owner_node`，并且心跳恢复之后本节点做过一次 `ChainVerified`：`verified_at` 非空且 `verified_at > failed_at`。只有心跳、目录没读到，守卫不通过 | 观察态 `LEASE_DEGRADED`，`failed_reason` 与 `failed_at` 清空，可以被认领。不需要操作员 Start，不加 `spec_revision`。`tail_unreadable_since` 留到 `DumpReady`。目录或盘已经不在时行留在 `FAILED`，CRITICAL 保持打开，epoch 不加。心跳还在、目录还在时，这就是今天 `SIGCONT` 之后自己续跑的那条路 |
| `EpochLost` | 续租循环；`WRITE_FENCED` 且行上没有更新的 epoch；本 epoch 的 open 文件 `ENOENT`（`per_node` 与 `shared` 都算，即使本地以为还握着租约）；`ENOSPC` / `EROFS`；不在终态允许名单、也不在重试允许名单上的 runner 错误；决定时读不到租约行 | epoch 栅栏：读得到租约且 epoch 已经更大，则只丢弃，不再写拒绝行。期望是 `STOP` 时不走让出 | 期望是 `STOP`：结果与 `StopConfirmed` 相同（`STOPPED`，`pending_delete` 则删除）。期望不是 `STOP`：释放本 epoch，观察态 `LEASE_DEGRADED`，期望 `RUN`，owner 空，`run_id` 空。拒绝行的 `attempts` 加一，`refuse_until = now + min(refusal_retry_cap_sec, claim_interval × 2^(attempts-1))`。缺省从 2 秒起按 2 的幂增加，不超过 600 秒；到顶之后每次仍是 `now + 600` 秒，不写 NULL。磁盘满或只读的节点在这一期限到了之后还会再 `Acquire`。唯一能读的节点只是短暂磁盘满时，恢复之后最坏再等约一个 `refusal_retry_cap_sec`（缺省 600 秒）才回到 `RUNNING`；这 600 秒加在第 3.4 节的 RTO 公式之外，整段等待是 CRITICAL，`/api/health` 是 503。WARNING。同一节点连续 3 次且中间没有 `DumpReady`，这条升为 CRITICAL。不写 `FAILED`，不写 desired `STOP`。epoch 按这个上限以有界速率增长，不永久停在 `LEASE_DEGRADED` |
| `MetaLockTimeout` | 任何一次锁等待超时，或旁路连接看见持有者超过 idle 上界 | 无。写入不得再去锁 `task_leases` | 不改任务观察态。finding 写在 `_cluster`。见 3.4 |

`FAILED` 不再附带 desired `STOP`。0005 用 desired `STOP` 阻止控制回路把失败的任务再拉起来；同一件事用「认领不拉 `FAILED`，直到 `spec_revision` 大于 `failed_spec_revision`」已经表达了。两套开关（desired `STOP` 和 `failed_spec_revision`）让 #450 那类补丁必须记得把哪一个改回去。目标只留规格这一套，再用 `failed_reason` 区分「允许名单上的终态」和「链主人暂时不在」。操作员 Stop 仍然把期望写成 `STOP`。`LEGACY_UNKNOWN` 与允许名单上的 `FAILED` 靠 `OperatorStart` 离开；`CHAIN_OWNER_GONE` 靠满足 `verified_at > failed_at` 的 `ChainOwnerBack`，或靠 `OperatorStart` 离开。终态允许名单和 `LEGACY_UNKNOWN` 进入 `FAILED` 时写 `failed_spec_revision = spec_revision`。`Acquire` 只在 `spec_revision > failed_spec_revision` 时拉这行；两者相等时认领拒绝。离开的唯一操作是 `OperatorStart`：它把 `spec_revision` 加一，并清空 `failed_reason`、`failed_at` 和 `failed_spec_revision`。`CHAIN_OWNER_GONE` 不写 `failed_spec_revision`，认领不拉它，离开靠 `ChainOwnerBack` 或 `OperatorStart`。

未分类的 runner 错误不是 `TerminalError`。本记录自己会引入的 1927/2013（idle kill 之后）和 owner 自己的栅栏写撞上的 1205，在租约仍有效时是 `RetryableError`；磁盘满、`EROFS`、以及本 epoch 的 open 文件 `ENOENT`（#458 在 5 秒内写成了 `FAILED` / desired `STOP`）是 `EpochLost`。不变量 2 不允许把这些写成集群终态。

一次让出的 `run_id` 清空，是为了第 1.5 节的卡死：`STARTING` 且 owner 为空必须满足 `canClaimDispatched`，否则认领查询和 `startTask` 互相矛盾。`OperatorStart` 的推进路径改为同一事务 `Acquire`，因此成功的 Start 不再留下空 owner。

### 3.3 存储模式

今天没有「盘是共享的」这一事实，所以每节点一份盘和共享 `data_dir` 在接管时走同一条 `SEGMENT_NOT_ON_WORKER`。目标把模式写成配置 `cluster.storage`：`shared` 或 `per_node`。缺省在迁移完成前按 `per_node` 处理接管失败（让出，不写终态）。这是安全的一边：共享盘上不该出现 `SEGMENT_NOT_ON_WORKER`，出现了就当成像盘不可读一样让出并告警，而不是停掉备份。确认挂载之后再配 `shared`。

| 模式 | 接管时尾巴不在本节点 | 谁写 `task_chain` | 链 finding |
| --- | --- | --- | --- |
| `per_node` | `TailUnreadable`，不写 `FAILED` | 读到自己 `data_dir` 下该目录的节点 | 只有 `task_chain.owner_node` 能开、能关。别的节点的空目录既不开 `SEGMENT_MISSING`，也不以 `OWNER_CHANGED` 关掉已有的 |
| `shared` | `TailUnreadable` 仍让出并开 CRITICAL。共享盘上出现这条，说明挂载或路径错了，不是「换一台接着跑」 | 当前持有者，且第 2 节第 4 条的读必须成功 | 持有者读到的那一份就是集群的那一份 |

不在这一步做「`_health` 持有者直接读别人的目录」（§30 D9 已经拒绝）。共享与否由配置声明，不由某次路径比较推断。uploads 开或关不改变接管走哪一个事件：还没封存的 OPEN 尾巴是 `LOCAL_ONLY`。把 `cluster.storage` 登记进 meta、不一致则拒绝认领，留到 PR-4（第 6 节 F11）。D6 仍不做。

### 3.4 锁、超时、RTO

`idle_transaction_timeout` 只有 MariaDB 有。MySQL 8.0 没有这个变量，设不上，也杀不掉 #476 里那个停在 Sleep、事务仍是 `RUNNING`、`rows_locked = 1` 的会话。两种库各用自己真有的机制，e2e 分开证明。上界都是 `min(5, lease_ttl_sec / 2)` 秒，至少 1 秒。任何一个本 flavor 必需的 `SET SESSION` 失败，拒绝以 cluster 模式启动。

MySQL 8.0：

- `wait_timeout` 设为该上界。它杀掉处于 Sleep 的连接，包括事务还开着、但连接已经 Sleep 的那种。#476 的现场就是这个形状：进程 `SIGSTOP`，会话 Sleep，事务 `RUNNING`。
- `net_read_timeout` 与 `net_write_timeout` 设为同一上界，覆盖还停在语句中途、还没进入 Sleep 的连接。
- `innodb_lock_wait_timeout` 设为同一上界。它约束的是**等锁的人**，不是握锁的人。
- 每一条会锁 `task_leases` 的调用带上相同时限的客户端上下文。超时后调用方关掉自己的连接。被 `SIGSTOP` 的进程跑不到这个超时，所以它不是 #476 的证明，只是还在跑的进程的上界。
- 旁路过程的规则见下面一节。握锁的进程自己是停的，杀它的只能是已经在等这把锁的节点，而且客户端不发 `KILL`。

MariaDB：

- `idle_transaction_timeout` 设为该上界，作为「事务已开且变成空闲」的第一刀。
- 其余与 MySQL 8.0 相同：`wait_timeout`、`net_read_timeout`、`net_write_timeout`、`innodb_lock_wait_timeout`、客户端上下文，以及同一套旁路过程。缺省仍是 `side_kill=off`。

连接池的 `ConnMaxIdleTime` 只作用于还在池里、没有被借出的连接。#476 的连接已经被借出并停在事务里，它杀不掉。`wait_timeout` 是 5 秒时，把 `ConnMaxIdleTime` 配得短于 5 秒，当作池卫生，不把它写进 RTO 的证明。

**旁路杀会话。** 配置 `cluster.side_kill` 是 `auto` 或 `off`，缺省 `off`。缺省不演练，也不调用过程。`auto` 是显式打开，只用于到 meta 的直连。任何会解析 SQL 的代理（ProxySQL、MaxScale，以及同类）后面打开 `auto` 都不安全：代理会自己处理 `KILL`，也会改写或丢掉 `CALL`。发布说明写明这条不能开在这种代理后面。

客户端不发 `KILL`。演练和真杀都只 `CALL binlog_server_kill_trx(p_trx_id)`。过程在服务器内做完这四步，否则什么都不做：

1. 按 `trx_id` 在 `information_schema.innodb_trx` 里找到仍在的线程。
2. 确认该事务仍握着 `task_leases` 或 `meta_canary` 的行锁。锁检查读不出来，也什么都不做。
3. 同一调用里杀掉该线程。RDS 上这一步是过程内部的 `mysql.rds_kill`，不是客户端发的 `KILL`。
4. 连接被多路复用转给别的客户端之后，新事务的 `trx_id` 已经不同。按旧 `trx_id` 的这次 `CALL` 是 no-op，不会杀到新会话。

谁可以发真杀：已经在等这把 `task_leases` 锁、并且已经提出 `MetaLockTimeout` 的节点。等锁的那条连接不发 `CALL`。另一条不执行 `SELECT … FROM task_leases FOR UPDATE` 的连接，读 `innodb_trx` 与锁等待视图。目标必须在两次探测里是同一个 `trx_id`，两次间隔至少 `idle_kill`，两次都是 Sleep，并且两次都握着该任务的 `task_leases` 行锁。然后把这个 `trx_id` 传进过程。过程在服务器里再查一次锁。`trx_id` 变了就不杀。`trx_id` 不复用：看到的 id 一旦离开 `innodb_trx`，下一次出现的同号不算续上。

`CONNECTION_ID()` 和 `performance_schema.threads` 是在同一个后端上算的，经过代理也相等，不用它判断能不能杀。`auto` 下能开 `side_kill` 的唯一证据是端到端演练，启动时做一次，之后每个 sweep 再做一次。`meta_canary` 每个 `node_id` 一行，本节点只锁自己的行，节点之间的演练不互相挡：

1. 连接 A 执行 `SELECT … FROM meta_canary WHERE node_id = ? FOR UPDATE`。这行不是 `task_leases`。节点在演练前 `INSERT` 自己的那一行。
2. 连接 B 从 `information_schema.innodb_trx` 读出 A 的 `trx_id`，不读线程 id 去发 `KILL`。
3. 连接 B 执行 `CALL binlog_server_kill_trx(该 trx_id)`。
4. 只有 A 的会话掉了，并且 B 随即能锁上同一行，这次演练才通过，`side_kill` 为 on。A 还在、B 锁不上、过程不存在或 `CALL` 失败，演练失败，`side_kill` 为 off，直到下一次 sweep 通过。

过程是 `SQL SECURITY DEFINER`，`DEFINER` 是跑迁移的那个账号，与触发器相同。调用方不需要 `SUPER`。建过程走与 `CREATE TRIGGER` 同一条预检：`log_bin` 开着时，账号要有 `SUPER` 或 `log_bin_trust_function_creators = 1`。过程建不上，即使配置写成 `auto`，`side_kill` 也保持 off。`DEFINER` 在服务器里执行 `KILL` 仍要有 `CONNECTION_ADMIN` 或 `SUPER`；RDS 上要能执行 `mysql.rds_kill`。没有这些，演练失败，开关保持 off。

每次成功的演练会在服务器错误日志留下一条 `Aborted connection`。打开 `auto` 的部署把这条加进 DBA 告警白名单。缺省 `off` 不演练，不产生这条日志。

缺省 `side_kill` 为 off。RTO 公式的形状不变，`idle_kill` 只是服务器超时：MySQL 8 是 `wait_timeout`（以及同一上界的 `net_read_timeout`、`net_write_timeout`），MariaDB 是 `idle_transaction_timeout`。旁路过程不进缺省公式。健康详情写 `side_kill=off`。#476 那种已经 Sleep、事务仍开着的会话，仍由这些超时在上界内拆掉。服务器既不把它当成 Sleep、也不把它当成网络读写真的卡住时（冻结在语句执行中，又没有打开 `auto`），这个上界不再覆盖它；发布说明把这一条写在 RTO 旁边。`auto` 且演练已通过时，过程可以在同一 `idle_kill` 上界内更早拆掉锁，数字上界不因此变短，也不写进缺省 RTO。

事务内部不做磁盘 IO、不等待源库。0009 §23 的写边界保持「先锁租约行，再写这一小段 meta」。评估器把目录读完，再开这一个短事务提交结果。健康事务不再为 1205/1213 重试 50 次。

`MetaLockTimeout` 的 finding 主题是 `META_LOCK_STALLED`（只用这一个名字，不再用 `META_UNREACHABLE` 兼着）。它写在任务 `_cluster` 上。写入者是观察到超时或调用了 `binlog_server_kill_trx` 的那个节点，走一条**不**调用 `applyOnce`、**不**锁 `task_leases` 的路径。这条写入自己失败时，该节点的 `/api/health` 503 用进程内「已经观察到锁超时」来判断，不依赖 finding 行先落库。关闭者就是这同一个节点：它连续两次在 `lock_wait` 之内拿到先前卡住的那把锁之后，关掉 finding，并清掉进程内的 503 原因。第二次必须是同一个节点，并且落在它上一次拿到这把锁之后的 `lock_wait` 之内；别的节点拿锁不算，超过 `lock_wait` 则从第一次重新计。观察者不能从仍握着锁的持有者手里把锁拿走。单次锁等待要不要先经过 Confirm 才打开 finding、才让 503，留给 PR-2（第 6 节 F12）；本记录不把第一次 1205 规定成一次页面。

`/api/health` 在下面任一成立时是 503，即使最后一行 `task_health.status` 还是 `HEALTHY`：

- 有未关闭的 CRITICAL finding
- 期望 `RUN` 的任务，`evaluated_at` 老于有效 `stale_after`（第 2 节第 7 条。缺省是 125 秒，大于 60 秒的 sweep），或本轮因为锁没写上

第二条把 #449 的 WARNING 在「行已经不可信」时升成降级。只是慢、但锁得上、数据还在涨、且年龄仍在有效 `stale_after` 之内的任务，仍保持今天的 WARNING，不 503。缺省配置下健康集群每轮 sweep 都能在 125 秒这条线之内刷新，不会每分钟有一半时间 503。

RTO 公式，写进发布说明和 e2e 的断言。起止点用库时钟，不写进日志让人猜：

```
start = 最后一次成功 Renew 的 NOW(6)（task_leases.renewed_at）
        本 epoch 没有成功 Renew 时，start = Acquire 提交的 NOW(6)
end   = 新 owner 的 DumpReady 提交（该 epoch 的观察态 RUNNING）
RTO  <= lease_ttl + max(idle_kill, lock_wait) + claim_interval + start_to_ready
```

缺省 `cluster.side_kill` 是 `off`，所以上面这条就是发布的缺省公式，里面没有旁路杀会话。`idle_kill` 在 MySQL 8 上是 `wait_timeout`，在 MariaDB 上是 `idle_transaction_timeout`。`lock_wait` 是 `innodb_lock_wait_timeout`。`start_to_ready` 的起止是 `STARTING` 提交到 `DumpReady` 提交。缺省数字是 10 秒，写进发布说明，e2e 用这个数字断言。缺省量级：TTL 15 秒，`idle_kill` 与 `lock_wait` 都是 5 秒，认领间隔 2 秒，`start_to_ready` 10 秒，RTO 上界 32 秒。现场 TTL 60 秒时，上界是 60 + 5 + 2 + 10 = 77 秒。健康详情写 `side_kill=off`。宽限不进公式。唯一能读的节点短暂磁盘满时，还要在这条公式之外再加上约一个 `refusal_retry_cap_sec`（缺省 600 秒），整段是 CRITICAL 和 503。跨节点恢复不等另一个 `worker_id` 的注册过期。同一 `worker_id` 被 `SIGSTOP` 之后，新进程要等注册过期才能占用这个 id——这是 0001 的会话所有权，保留；它不得挡住**另一个** `worker_id` 的任务认领。#463 的 143 秒在这套公式下是失败，不是可以接受的上界。同一 `node_id`、同一 `data_dir` 在 `per_node` 上被 `kill -9` 之后的上界见第 6 节 F10，不塞进这条跨节点公式。

### 3.5 finding 从状态机派生，而不是从一次扫目录的差集派生

`Reconcile` 今天是「上一份打开集合」对「这一轮想要的集合」做差，再对链 finding 插入 `OWNER_CHANGED` 这个例外。例外就是关了又开的来源。

目标：一条 finding 的开关是状态机事实的函数，同一把键在事实不变时没有转移。

| 事实 | 打开 | 关闭 | 所有权变化时 |
| --- | --- | --- | --- |
| `task_chain` 上的目录违反了 I1–I7 中仍然启用的那一条 | 读到目录的节点，按现有 Confirm（连续两轮）打开 | 只有当前 `task_chain.owner_node`，而且这一轮是它自己 `ChainVerified` 的结果，该键不再被派生。关的原因是 `EVAL`。没有第二个关闭者 | 不关闭。还没有 `ChainVerified` 的新节点不能关 |
| `desired = RUN` 且 `tail_unreadable_since` 非空，并且该时刻之后没有 `DumpReady` 提交 | `TailUnreadable` 写下这一列时打开 CRITICAL，subject 是 `SEGMENT_NOT_ON_WORKER` | 只有一次 `DumpReady` 提交在这个时刻之后，并清空这一列。关闭者就是这次 `DumpReady`，不看 `last_error`，也不看观察态是不是还停在 `LEASE_DEGRADED` | 保持打开。对端认领进入 `STARTING`、`OperatorStart` 清掉 `last_error`，都不关。无人 `DumpReady` 的这段时间 `/api/health` 503 |
| 期望 `RUN`，没有活 owner，且不在第 2 节第 7 条的合法 `RETRY_BACKOFF` 窗口里，超过一个 sweep | 已有的 `TASK_NO_PROGRESS` | 有了 owner 并且 dump 有进展，或进入一个未到期的合法退避 | 无关 |
| 锁等待超时 | `META_LOCK_STALLED`，写在 `_cluster`，路径不锁 `task_leases`。单次超时的 Confirm 见第 6 节 F12 | 观察到它的节点，连续两次在 `lock_wait` 内拿到那把锁。观察者死后改由 `_health` 持有者关，见第 6 节 N6 | 无关。不经过 `applyOnce` |
| 封存段在**已验证的**目录上缺失 | `SEGMENT_MISSING`（#462，不再丢弃 I6） | 文件回到**同一份**已验证目录，`EVAL` | 不因为新节点还没读到目录就关。rehash 未完成时的 catalog-only 校验和按不完整读处理，这一条留到 PR-5（第 6 节 F7） |

`OWNER_CHANGED` 这个关闭原因删掉。需要知道「链换节点了」时，记一条事件，不作为 finding 的开关。`AwaitRehash`、`PreserveChainOwner`、`noteOwnerChanged` 在不完整目录上照样关 finding 的那条路径，随这个 PR 一起删，不保留兼容分支。

同一轮里对同一把键先 `CLOSE` 再 `OPEN` 视为模型错误，属性测试直接失败。

### 3.6 操作员 API

`StartTask` 不再「先 `writeStartIntent` 再 `convergeTask`，两步的错误都变成 400」。

1. 一个事务里跑 `OperatorStart`、`OperatorStop` 或 `OperatorDelete`（3.2）。Start 的推进与认领一样，锁 `task_leases`，`Acquire` 和写入 `STARTING` 一起提交；锁下期望已经是 `STOP` 则回滚。
2. 返回码以 3.2 的三行 `OperatorStart` 为准，不另做一套。已经是 `STARTING` 或 `RUNNING`、规格没变、且 `pending_delete` 未置位：204，不加 revision。`STOPPING`、`pending_delete` 已置位、改规格的重复 Start、以及未过期租约还在别人手里：409，行不变。epoch 栅栏把 runner 事件丢掉时，HTTP 不参与，行也不变。
3. 事务提交之后才在本进程收敛。收敛的错误不再改写已经返回的状态码；若响应还没写，收敛失败也不把已经提交的 `STARTING` 说成 400。调用方看到推进的 204 时，行上已经是这次 `Acquire` 的 owner 和 `STARTING`。
4. `LEASE_DEGRADED` 且期望 `RUN`、`pending_delete` 未置位，是合法的 Start。它清拒绝行、清 `run_id`、进入 `STARTING`。这是操作员明确要求再试，不是认领循环偷偷违反退避。它不清 `tail_unreadable_since`。

`per_node` 上把链目录拷到一台曾经让出的对端之后，该对端的 `refuse_until` 仍是 NULL，认领不会用它。只拷目录不清这行。操作员必须再 Start 一次：Start 删掉本任务全部拒绝行，对端才能 `Acquire`。本记录不另加「本地目录已经可读就删掉自己的 NULL 拒绝」事件。清掉它的是这次 Start，或另一节点的 `DumpReady`。

Stop 已经是「先落库 `STOPPING`」。保持为单事务。失败不写 `STOPPING`。认领事务进行中收到 Stop：认领在同一把锁下看见期望 `STOP`，回滚 `Acquire`。删除落在接管中途：`OperatorDelete` 把观察态写成 `STOPPING` 并置 `pending_delete`。持有者还在就等 `StopConfirmed`；持有者已死、租约过期后，任一节点 `StopExpired` 删行。期望 `STOP` 时的 `EpochLost` 与 `StopConfirmed` 同一结果。

### 3.7 考虑过的方案

**继续在 #471 上补第 5 个守卫。** 例如「持有者 sweep 在目录没读到时也带上 `PreserveChainOwner`」「Start 之前清 `run_id`」「退避写进表」。每一条都能让当前 QA 变绿，并且和 §29.10、§29.11、§31.6 的写法相同：在出问题的那个函数里再加一个条件。下一个窗口（锁、注册、重启、第三次让出）不在这个条件里。不选。

**集群只留一个写者进程，其它节点把事件寄给它。** 写者若不是链所在的节点，就得代写它没读过的目录，§30 已经证明代写会改掉不该改的列。dump 也无法挪到没有字节的机器上。不选。纯函数加上条件更新得到「逻辑上一个写者」，而不要求「物理上一个进程」。

**租约和链主人继续放在同一条 `task_health` UPDATE 里，只用注释禁止误写。** #471 的 `PreserveChainOwner` 就是这个方案。普通持有者写入不看那个布尔值。分开两条记录之后，偷写要专门再写一条 SQL，审查看得见。选分开。

**用 generation 代替状态机，谁后写谁赢。** `row_generation` 只解决丢失更新，不解决「这次写入的内容是不是合法过渡」，也不挡住旧 epoch。#457 的 `FAILED` 可以带着新 generation 覆盖健康的 owner；旧 owner 在 `SIGCONT` 之后用仍然匹配的 generation 把事件套回新行，是同一类。generation 和 epoch 都只是过渡函数的比较条件，不是模型本身。

**把退避放在租约行的 `lease_expire_at` 上，让 Acquire 自然失败。** 过期时间表达的是「谁现在握着」，不是「这个节点到什么时候不许再试」。一个节点的拒绝会挡住其它还没试过的节点。拒绝按 `(task_id, node_id)` 记。不选把期限塞进租约过期时间。

**共享盘由 `_health` 持有者跨节点读目录。** §30 D9 已拒绝。本记录用 `cluster.storage` 声明，不新增跨节点读。

## 4. 验证

实现期间不靠「再跑一轮手工 QA 看会不会冒新症状」当门槛。两道门都是自动的：模型必须接受第 1 节的轨迹，故障注入必须在 e2e 里失败于回归。

### 4.1 模型

过渡函数放在独立包里，不导入调度器、不打开数据库。状态是一个结构体：期望、观察态、`row_generation`、`failed_reason`、`failed_at`、`tail_unreadable_since`、`pending_delete`、租约 owner/epoch/过期、`task_chain`（含 `verified_at` 是否为空）、拒绝表（含 `attempts` 与 `refuse_until` 是否为空）、finding 开集、各节点能否读目录、哪把 meta 锁被谁握着。事件是 3.2 的枚举。

检查分安全性与活性。安全性看每一段前缀。活性只在下面三条公平性假设成立时要求有界步数；这三条是模型的前提，不是被证明的性质：

1. 没有被停住的节点最终会走出一步（认领循环、评估器或 runner）。
2. `task_leases` 上的行锁在 `idle_kill` 内释放。缺省证明是服务器的会话超时（`side_kill` 为 `off`）。`auto` 且演练已通过时，过程可以更早拆掉同一把锁，不改变这个上界。
3. 读得到链目录、拒绝期限已过、且没有被停住的节点，会在一个 `claim_interval` 内被调度到。

- **安全**（Go 的随机序列，固定种子可重放）：从 `CREATED` 出发，任意交错事件，每一段前缀都满足不变量 1–5、8、10，以及 6、7、9 的安全半边。特别是：非操作员 Stop/Delete 之后 desired 不是 `STOP`；`FAILED` 只出现在终态允许名单或 `CHAIN_OWNER_GONE` 上，不会出现在「本节点读不到目录、且 `task_chain` 是别人」的前缀里；未分类错误、`ENOENT`、`ENOSPC`、`EROFS` 的前缀不是 `FAILED`；同一 finding 键不会在事实未变时先关后开；`tail_unreadable_since` 在 `DumpReady` 之前不被 Start 或 `STARTING` 清掉；`OperatorStart` 的 409 分支不改变状态；runner 事件在更新的 epoch 上不写入；拒绝期限只来自库时钟那一列；认领不拉 `FAILED`。
- **活性**（同一台函数，步数有上界。一步是一个事件或一个超时）：
  - (a) 前提是：期望 `RUN`，至少一个能读目录的节点没有被停住。拒绝行不放进前提。停住的链主人也不从前提里拿掉。公平步图的每个底强连通分量要么含 `RUNNING`，要么是单个吸收的、操作员表里的 `FAILED`（`SOURCE_AUTH`、`LOG_BIN_OFF`、`LEGACY_UNKNOWN`、`CHAIN_OWNER_GONE`）。非平凡的底（退避环）就是违例。搜索状态的键不存绝对 `now`，也不存 epoch 和 `row_generation`。每个计时器记成 `deadline−now`：零值单独一位。`Step` 的每条时间比较在同一张表里，写明字段、常数和比较符，测试核对这张表盖住源码里的比较。常数 c，以及比较是 `>` 或 `>=` 时的 c−1ns 和 c+1ns，都是单独的边界，键里保持精确；只有严格早于这些边界的差值才收成同一个饱和区。还在未来的差值保持精确，因为下一跳是这些未来差值里的最小值，收成一档会让后继不再由当前键决定。`attempts` 超过 21 收成 22。`tail_unreadable_since` 只记有无，`verified_at` 相对 `failed_at` 只记先后。退避间隔顶到 cap 之后，新期限总是 now 加同一个常数，键会重复，底强连通分量合上。合不上、状态数顶到上限，是活性失败，不是跳过。随机配置全部在上限内搜完，穷尽比例是 1，超过上限的种子数是 0。同一抽象键上，可走步和下一步键必须相同：测试拿只差时间平移、或只差已经饱和的过去的两份具体状态核对。`FairTerminal` 只是这张表的声明，和底分量双向对账：声明了的终态在公平步下吸收，吸收的非 `RUNNING` 状态都已声明。已经 `RUNNING` 是终态。`FAILED` 只有在公平调度走不出去时才是终态。允许名单上的 `SOURCE_AUTH`、`LOG_BIN_OFF`、`LEGACY_UNKNOWN` 在 `InClaimSet` 为假时才算，也就是 `spec_revision` 还没有超过 `failed_spec_revision`；规格往前走之后认领可以把行拉回 `RUNNING`。`CHAIN_OWNER_GONE` 认领不拉。它在链主人停住、不在节点表里、或者还活着但读不到目录（`CanRead` 为假）时是终态。finding 的 subject 就是 `CHAIN_OWNER_GONE`，和其它 `FAILED` 一样。链主人活着而且读得到目录时，公平搜索用该节点的 `CanRead` 作为 `ChainVerified` 的 `ReadOK`，通过后再 `ChainOwnerBack`，这条路径不是终态。操作员 `Start` 仍可以离开任何 `FAILED`。`per_node` 的 NULL 拒绝是唯一写成永久形状的拒绝，只由 `TailUnreadable` 在 `per_node` 上写入。它记下的是当时读不到目录。`Acquire` 按该节点当前的 `CanRead` 重判：目录恢复后这行不再挡住认领，行本身留到别人的 `DumpReady` 或操作员 Start 才删。`EpochLost`、`shared` 的 `TailUnreadable` 都写有期限的拒绝，不是永久的。`shared` 的 `TailUnreadable` 和全部 `EpochLost` 在 `refusal_retry_cap_sec` 内到期，能读的节点会再次可调度。步数上界 K 按公平搜索里的事件步计，不按 wall-clock 的 600 秒计。下一跳期限只有一个函数 `NextDeadline`：`Step` 会反应的计时器（租约到期、`idle_kill`、`RetryUntil`、拒绝退避、停住链主人的 sweep = `HeartbeatAt+node_gone+1ns`、`FailedAt+1ms`）取最小值。窗口 W 是同一组的最大值。没有认领者但 sweep 还在前面时，搜索跳到这一期限，而不是报卡住。唯一的时间步是把时钟推进到 `NextDeadline`（`fairDeadlines` 的最小值）。搜索不单独跳到 `ExpireAt`、`RetryUntil` 或某一条拒绝的 `Until`：这些期限若在 `fairDeadlines` 里，就是 `NextDeadline` 本身。读者期限之后、又严格早于窗口里已有终点的非读者拒绝，只有在那个时刻认领集已经开着时才进这组：`Acquire` 会从拒绝变成应用，跳过它会让 `Step` 在下一跳之前变掉。租约还活着、或还在合法退避里，认领照旧被拒绝，那个时刻不是一跳，也不把 W 拉长。读不到目录的节点按 `RefusalDelay` 再认领：`delay = min(cap, claim_interval × 2^(attempts-1))`，`attempts` 从 1 起；指数大于 20、`claim_interval > cap>>shift`、乘积非正、或 `delay` 超过 `refusal_retry_cap_sec` 时，`delay` 取 cap，`attempts` 停在 21，不再靠溢出把期限算短。一个现在就能认领、之前没有失败过的节点，在读者还要等 H 的这段里，若 `delay` 一直没顶到 cap，能排进去的认领次数是 `floor(log2(H/claim_interval + 1))+1`。顶到 cap 之后间隔变成 cap，次数必须按 `RefusalDelay` 逐次累加，不能再用这个对数式：H = 3600 秒、`claim_interval` = 2 秒、cap = 600 秒时对数式得 11，逐次累加得 14；H = 600 秒、同样的缺省时两者都是 9。这是单个新节点的认领次数，不是 K。K 是公平步数的上界，不是最长调度的长度。估大是安全的，只有估小是错。读者取活着的、能读、没被停住、没有 NULL 拒绝的 owner，否则按节点顺序取第一个这样的节点。读者期限 H 是这些节点里还没到期的拒绝期限的最大值，没有则为 0。非读者窗口 W 是公平搜索在认领结束前能把时钟推到的最远绝对期限减去 now。几段等待取最大值，不相加。放进这个最大值的，是 `Step` 上公平搜索会跳过去的每一处：能读节点的拒绝期限（H）；行锁在 `Lock.Since+idle_kill` 被前导清掉（`Since` 为零则期限已在过去）；停住的活 owner 等到 `ExpireAt`，这种 owner 不走让出；活着、没停住、又不是读者的 owner，让出被拒绝时等到同一个 `ExpireAt`；`RETRY_BACKOFF` 即使租约已经先到期也跳到 `RetryUntil`，这个点可以落在 `ExpireAt` 之后，到期前 `Acquire` 仍然拒绝；`CHAIN_OWNER_GONE` 在 `VerifiedAt` 尚未晚于 `FailedAt` 时跳到 `FailedAt+1ms`，`ChainOwnerBack` 就在这一刻。不放进 W：`StopExpired`、`ChainOwnerBack`、`DumpReady`、`Acquire` 都在当前时刻；搜索刷新没停住节点的心跳，不等这些节点的 `node_gone`。停住的链主人是例外：观察态是 `LEASE_DEGRADED` 且 `tail_unreadable_since` 已写下时，公平搜索跳到 `HeartbeatAt` 加有效 `node_gone` 再加 1ns，走 sweep 的 `ChainOwnerGone`，这一步算进 K，终点是上面的终态。已验证链主人读不到目录、但行还不是这个形状时，K 先算主人的 `EpochLost` 写下 `tail_unreadable_since`，再算紧跟着的 `ChainOwnerGone`。`UnownedSince+Sweep` 和 `no_progress_after` 只服务 sweep，不是通向 RUNNING 的公平步；`lock_wait` 只约束第二次拿锁关掉 `META_LOCK_STALLED`，活性走 `idle_kill` 上的 `MetaLockTimeout`；`StartToReady` 和 `Renew` 不是这次故障切换的跳步。一次浪费新写的 `refuse_until`，以及 `takeLease` 写下的 `ExpireAt`，是窗口内部的跳点，不再把 W 往外推。每个非读者（读不了、没被停住、不是这个读者）：已经是 NULL 拒绝，或下一次可认领晚于 W，计 0。晚于 H、但不超过某次强制跳步的，按 W 计。`per_node` 上还不是已验证链主人的至多 1 次，因为 `TailUnreadable` 写成 NULL、不到期；已验证链主人不走这一支，它的让出是 `EpochLost`，会到期。其余情况把该行已有的 `attempts` 记为 a，下一次间隔是 `RefusalDelay(a+1)`。n 是从 a+1 起逐次调用 `RefusalDelay`，直到返回值 ≥ min(cap, W) 的次数，含这一次。q 在 W=0 或 cap=0 时为 0，否则是 `floor(W/cap)+1`，即间隔至少为 cap 时长度 W 的窗口里至多能放进多少次。按 cap 分开的认领可能已经算进 n，所以 n+q 是多算的上界，不是紧的。落在 (now, now+W] 的已有拒绝期限各算一个待跳点，端点 W 算在内。W>0 时每次无用认领至多制造一个新期限，每个非读者至多一个待跳点，窗口终点再算一次，跳步 ≤ 无用认领总数 + 待跳点 + 1；W=0 时没有跳步。有读者时再加 `Acquire` 与 `DumpReady`。行锁、链校验、链主人回来、停住的 owner 等到 TTL、活着但读不了的 owner 让出、`StopExpired`、合法重试窗口，每项至多再加一步。最长公平调度 ≤ K。观察步数等于 K 的夹具用这个等式卡住少算一步的上界。唯一能读的节点短暂磁盘满时，恢复后最坏就是这一次 600 秒，整段是 CRITICAL，`/api/health` 是 503，并且这 600 秒加在第 3.4 节的缺省 RTO 之外。前提因此不会因为拒绝行永不到期而变成空集。
  - (b) 不存在把任务永久停在 `LEASE_DEGRADED` 的认领循环。`per_node` 上 `tail_unreadable_since` 还在时，每个读不到目录的节点至多 `Acquire` 一次（`refuse_until` 为 NULL），直到某一次 `DumpReady` 删掉这些行；这一段 epoch 至多增加 N，搜索把第二次认领剪掉。`shared` 的 `TailUnreadable` 和 `EpochLost` 到顶之后按 `refusal_retry_cap_sec` 再试，速率有界，不写 NULL，搜索不剪掉到期后的再认领。链主人还在节点表里、心跳可以是新鲜的，但 `CanRead` 为假，并且观察态已是 `LEASE_DEGRADED`、`tail_unreadable_since` 已写下时，`ChainOwnerGone` 立刻成立，不等 `node_gone`。这是进入 `FAILED` / `CHAIN_OWNER_GONE` 的 `Step` 过渡。其它节点的 `Acquire` 被拒绝，不能悄悄接管这条链。链主人自己的 `EpochLost` 会把还空着的 `tail_unreadable_since` 补上，退避因此有出口，不会停在 `LEASE_DEGRADED`。已经是 `FAILED` 之后，心跳还在但目录读失败时，`ChainVerified` 的 `ReadOK` 为假，`ChainOwnerBack` 不发生，行留在 `FAILED` / `CHAIN_OWNER_GONE`。`InClaimSet` 为假。离开靠链主人自己把目录读回来（环境把 `CanRead` 设回真，然后 `ReadOK` 为真的 `ChainVerified`，再 `ChainOwnerBack`），或者靠操作员 `Start`。停住的链主人不在公平图里恢复，恢复是环境变化。K 含这 N 次无用认领，并且含 (a) 里按退避能排进读者期限的每一次再认领，以及这些认领之间的跳步：一次无用认领在搜索里算一步（`Acquire` 加上把它放开的 `TailUnreadable`，或该节点已是已验证链主人时的 `EpochLost`）。起点包含 `FAILED` / `CHAIN_OWNER_GONE`。活性用三条公平性假设下的有界搜索，随机前缀只验安全性。上界和搜索写在 `internal/ownership/bound.go`、`observe_test.go`（第 6 节 N9）。
  - (c) `DumpReady` 之后，尾巴 finding 在一个 sweep 步内关闭。
- **轨迹**（表驱动，QA 场景写成事件序列，断言终态）：

| 轨迹 | 事件 | 终态 |
| --- | --- | --- |
| §29 宽限 | owner 续租超时，没有新主人 | `LEASE_DEGRADED`，desired `RUN`，可被 `LeaseExpired` 拉起，epoch+1，没有第二条 dump |
| §29.10 | 让出之后 `WRITE_FENCED` | 仍是可认领的观察态，不是「`STOPPED` + desired `RUN` + 空 owner」以外的死角；`STOPPED`+`RUN`+空 owner 若出现也必须可认领 |
| §29.11 | 新 epoch 已在跑，旧 epoch `ENOENT` | 共享行仍是新 owner 的 `RUNNING`。旧节点只有本地收尾 |
| #462 | `ChainVerified` 的目录缺一个非 `UPLOADED` 封存段 | `SEGMENT_MISSING` 打开。未 `ChainVerified` 的节点不能开、也不能关 |
| #473 | `per_node`，owner 停住，对端 `TailUnreadable`，然后对端进程「重启」（内存清空，meta 还在） | 始终 desired `RUN`，`task_chain` 仍是原主人。对端的 `refuse_until` 是 NULL，重启后仍不能 Acquire。原主人 `DumpReady` 同一事务删掉全部拒绝行，finding 关闭。下一次接管对端还能再试 |
| #471 QA（2） | 对端在 `TailUnreadable` 之前插入一次「目录读取失败的评估」 | `task_chain` 不变。随后仍是让出，不是 `FAILED` |
| #471 QA（3） | 让出或 `ChainOwnerGone` 之后 `OperatorStart` | 成功则 `STARTING`、`run_id` 空、拒绝行已删，204，`tail_unreadable_since` 仍在。`STOPPING` 上 Start 是 409，行与调用前相同 |
| #471 QA（1） | `ChainVerified` 从节点 A 换成节点 B，违反仍然在 | 目录 finding 始终开着，关闭次数是 0。B 尚未 `ChainVerified` 时同样不关。尾巴 finding 只在 `DumpReady` 时关 |
| #476 | owner 握着租约行锁，对端收到 `MetaLockTimeout` | 观察态可以还是 `RUNNING`，finding 写在 `_cluster` 且写入不锁 `task_leases`。锁的持有步数有上界（超时事件在 `idle_kill` 步内出现，不是 TTL） |
| #463 | 另一个 `worker_id` 的 `LeaseExpired`，原节点注册未过期 | 认领成功，不等注册 |
| #457 重放 | 新 epoch 已提交，旧 owner 带着旧 epoch 再提 `DumpReady` 或 `TerminalError`，`row_generation` 恰好仍能对上 | 事件丢弃，新行仍是新 owner 的 `RUNNING`，desired 不是 `STOP` |
| 链主人回来 | `ChainOwnerGone` 之后心跳恢复，并且 `verified_at > failed_at` 的 `ChainVerified` 已经提交，没有 `OperatorStart` | `ChainOwnerBack` 把它收成 `LEASE_DEGRADED`，随后本节点 `LeaseExpired` + `DumpReady` |
| 链主人只剩心跳 | `CHAIN_OWNER_GONE` 之后心跳恢复，目录读失败，没有新的 `ChainVerified` | 仍是 `FAILED`。不进入 `LEASE_DEGRADED`，epoch 不加。CRITICAL 保持打开 |
| 操作员竞态 | 认领事务中期望变成 `STOP`；`STOPPING` 上 Start；重复 Start 且规格未变；有活租约时 Delete；`pending_delete` 上再 Start；持有者 `kill -9` 后租约过期 | 认领回滚且 epoch 不加；Start 为 409 且行不变；重复 Start 为 204 且 `spec_revision` 不变；Delete 把观察态写成 `STOPPING` 并置 `pending_delete`；此后的 Start 仍是 409；`StopExpired` 在租约过期后把行收成 `STOPPED` 或删掉 |

这包测试在第 5 节的第一个实现 PR 里就进 `go test`。生产代码还没接上时，它锁的是规格。接线的 PR 只许把调度器改成调用这台函数，不许在调度器里复制一份守卫。

PlusCal 不作为 CI 的依赖。过渡函数的注释用同一套变量名（`phase`、`desired`、`chain`、`refusal`、`findings`），使愿意用 TLC 的人可以照着译，而不在仓库里维护第二份会漂的规格。

### 4.2 故障注入，作为 e2e 门槛

每一格是一次真节点（两节点 meta，评估器 on）必须出现的结果。`SIGSTOP` 打在该阶段的进程上，停超过 TTL，再 `SIGCONT`。断言用 meta 行、`/api/health`、finding、源库 `SHOW PROCESSLIST` 的 dump 线程数，不用日志。

| 阶段 | `SIGSTOP` | `kill -9` | 网络分区（只断该节点到 meta） | 重启落在退避期内 |
| --- | --- | --- | --- | --- |
| `Acquire` 已提交、dump 未开 | 对端在 RTO 内接手或让出；不得 `FAILED`+desired `STOP` | 同左 | 同左，且分区期间 503 | 不适用 |
| dump 已开，事务未开始 | 对端在 RTO 内接手，恰好一条 dump | 同左，open 段不丢已落盘事件 | 原 epoch 让出，desired `RUN`，503 | 不适用 |
| 正握着 `task_leases` 的 `FOR UPDATE` | **RTO 内**锁消失（服务器杀掉空闲事务），对端继续；期间 503，不得保持 200+`HEALTHY` | 连接随进程消失，对端在 TTL 内接手 | 同锁那一格 | 不适用 |
| `TailUnreadable` 已提交 | `per_node`：这条尾巴还在时该节点不再 Acquire，直到别人 `DumpReady` 删掉拒绝行。`shared`：按 `refusal_retry_cap_sec` 再试。另一节点若能读目录则跑起来 | 拒绝行还在 | 503，行仍是 `LEASE_DEGRADED` | **拒绝行还在，不立刻再认领。原主人 `DumpReady` 后拒绝行被删** |
| 评估器正读目录，尚未 `ChainVerified` | `task_chain` 不变 | 同左 | 同左 | 不适用 |
| `OperatorStart` 事务已开未提交 | 提交或回滚二选一；不得留下没有 owner 的 `STARTING` 且 HTTP 已经 400 | 同左 | 调用返回 503，行不变 | Start 仍是单事务成功 |

另外四格，不放进上面的相位表：

| 场景 | 断言 |
| --- | --- |
| 连续让出，对端始终没有目录 | 每个没有目录的节点至多让出一次。`task_chain` 仍是最后一次 `ChainVerified` 的节点；epoch 的增量不超过节点数；观察态不是 desired `STOP`；CRITICAL 一直开着；`/api/health` 503 |
| 把任务目录拷到对端再 Start | 新节点 `ChainVerified` 之后 finding 按事实关闭或保持，关闭次数对「事实未变」的键是 0；dump 一条 |
| `cluster.storage=shared`，拔掉一侧挂载 | 让出 + CRITICAL，不写 desired `STOP`。挂载回来之后原节点或对端在 RTO 内回到 `RUNNING` |
| `cluster.storage=per_node`，只 `SIGSTOP` owner | 与 #473 相反：原主人 `SIGCONT` 之后以更大 epoch 继续，源上始终最多一条 dump，GTID 不重叠 |

设计评审补上的故障行。每一行都是发布档里的一格，断言用 meta 行、`/api/health`、finding 和源上的 dump 线程数。

| # | 场景 | 断言 |
| --- | --- | --- |
| 1 | MySQL 8.0 的 meta，事务内 `SIGSTOP` | 不依赖 `idle_transaction_timeout`。缺省 `side_kill=off`，由 `wait_timeout` 在 `idle_kill` 内拆掉会话。`auto` 且演练已通过时，由 `binlog_server_kill_trx` 在同一上界内拆掉，客户端不发 `KILL`。对端的 `FOR UPDATE` 继续；等待期间 503，不是 200 加 `HEALTHY`。RTO 的终点是新 owner 的 `DumpReady` |
| 2 | 停住的是 `_health` 持有者而不是任务 owner，并且它握着 `task_leases` 行锁 | owner 自己的写入撞上 1205 时重试，任务保持这个 owner 的 epoch，不写 `FAILED` |
| 3 | owner 在事务里停顿超过 `idle_kill`、不到 TTL | 会话被杀，租约行的 `lease_expire_at` 仍在将来。同一个 owner 在新连接上重试，不发生对端接管，不写 `FAILED` |
| 4 | `ChainVerified` 之后停 2×TTL 再写 | 写入带着的 epoch 与 `task_leases` 不符，条件更新拒绝。行留在新 owner 上，旧事件丢弃 |
| 5 | `Acquire` 已执行、`STARTING` 还没写时进程被停住 | 这两步是同一事务。行要么仍是旧观察态加旧 epoch，要么是新 epoch 上的 `STARTING`。不出现「epoch 已加、观察态还是旧的」 |
| 6 | owner 停住超过 `node_gone_after` 之后 `SIGCONT`，目录还在 | 先有 `verified_at > failed_at` 的 `ChainVerified`，然后 `ChainOwnerBack` 把 `FAILED` / `CHAIN_OWNER_GONE` 收成 `LEASE_DEGRADED`，没有操作员 Start。同一节点到达 `DumpReady`。只有心跳、没有这次 `ChainVerified` 时行留在 `FAILED` |
| 7 | `per_node`，唯一能读目录的节点 `kill -9` 后以同一 `node_id`、同一 `data_dir` 重启 | 期望保持 `RUN`，不出现 `FAILED` + desired `STOP`。该节点重新成为 owner。注册 TTL 算不算进这一格的数字上界由 PR-2 定（第 6 节 F10）。另一个 `worker_id` 的跨节点恢复仍不等这条注册 |
| 8 | 本节点握着租约时，自己的 open 文件 `ENOENT`。`per_node` 一行，`shared` 一行 | 两行都不是 `FAILED`，也不是 desired `STOP`。决定时向 meta 核对租约；核对到了仍走 `EpochLost`。拒绝按指数拉开，到顶后每 `refusal_retry_cap_sec`（缺省 600 秒）再试，不写 NULL。核对本身失败同样是 `EpochLost` |
| 9 | 对端磁盘上有一份旧目录 | 最新封存段与 catalog 不一致时不得提交 `ChainVerified`，不得成为 `task_chain.owner_node`。比对规则在 PR-5（第 6 节 F7）；这一格是该 PR 的验收 |
| 10 | 三个节点，只有一个有目录 | 没有目录的两个节点各拒绝一次；这一圈 epoch 至多增加 2；有目录的节点到达 `RUNNING`。不出现无界的认领/让出循环（第 4.1 节活性 (b)） |
| 11 | 认领中途 Stop；`STOPPING` 上 Start；规格不变的重复 Start；接管中删除；持有者随后 `kill -9` | 认领回滚，epoch 不加。Start 返回 409，行不变。重复 Start 返回 204，`spec_revision` 不变。删除把观察态写成 `STOPPING` 并置 `pending_delete`，活租约不动。此后 Start 仍是 409。租约过期后 `StopExpired` 删行，不等已死的持有者 |
| 12 | 旧二进制与新二进制同时对着一套已迁移的 meta | 触发器只拒绝 `backup_tasks` 上不把 `row_generation` 加一的 `UPDATE`。续租在 `task_leases`，目录行在 `binlog_files`，dump 线程不走这张表，这三样照常。响声是 `_cluster` 上的 `MIXED_VERSION`。`SIGSTOP` 到注册过期再 `SIGCONT` 的旧节点能绕过预检，回来后继续 dump。升级顺序仍是先停掉全部旧节点，再 `migrate up` |
| 13 | meta 切主，或库时钟偏差大于 TTL 减续租间隔 | 比较用同一条语句里的 `NOW(6)`。偏差已经大于 TTL − renew 时允许提前接管。旧 epoch 的写入被栅栏拒绝。源上至多一条 dump，不得停在 200 加 `HEALTHY` |
| 14 | 损坏的副本，校验和还只来自 catalog，rehash 没跑完 | 这一轮是不完整读。不得关闭 `SEGMENT_UNREADABLE`。规则在 PR-5（第 6 节 F7）；这一格是该 PR 的验收 |
| 15 | owner 磁盘满或 `EROFS` | `EpochLost`，不是 `TerminalError`。期望保持 `RUN`。拒绝指数退避，到顶后每 `refusal_retry_cap_sec` 再 `Acquire`，磁盘恢复后可以 `DumpReady` 并清掉全部拒绝行。不永久停在 `LEASE_DEGRADED`。唯一能读的节点只是短暂磁盘满时，恢复后最坏再等约 600 秒，整段 CRITICAL，`/api/health` 503，这 600 秒加在缺省 RTO 之外。WARNING，连续 3 次升 CRITICAL。写得出的另一个节点可以接管 |
| 16 | 宽限用尽之后、TTL 之前的 45 秒（#450 / #454） | 恢复之后观察态在第 2 节第 9 条的并集里，下一任 owner 的 epoch 加一。不是 desired `STOP`，也不是认领查询匹配不到的形状 |
| 17 | uploads 开启与关闭各跑一遍接管 | 两次的事件相同。还没封存的尾巴是 `LOCAL_ONLY`，接管不因为 uploads 开着就换一条过渡。D6 仍不做。`cluster.storage` 登记进 meta 留到 PR-4（第 6 节 F11） |

这张表进 e2e 的发布档，不进每次 PR 的 `unit-and-e2e` quick（quick 跑不了多节点 `SIGSTOP`）。接线的那个 PR 至少带两格：MySQL 8.0 上锁中间 `SIGSTOP`（第 1 行，#476）、让出后重启（#471 QA 4）。MariaDB 上的同一格 `SIGSTOP` 用 `idle_transaction_timeout` 再跑一遍。其余格在同一张表里列成该 PR 的测试计划，没跑的格子写明没跑，不把没跑写成通过。

### 4.3 第二轮走查里的 GAP 行

下面每行对应评审走查里标成 GAP 的场景。走法按本节修订后的第 3 节，不再按 `861802d`。

| 走查 | 修订后的走法 | 结果 |
| --- | --- | --- |
| S2 的 epoch 每 60 秒加一 | `per_node` 上 `tail_unreadable_since` 还在时，每个没有目录的节点只 `Acquire` 一次，`refuse_until` 为 NULL。epoch 的增量不超过节点数。原主人 `DumpReady` 同一事务删掉这些行，下一轮接管还能用它们 | 关上。不再无上限，也不会在续跑之后把对端永久排除 |
| S3b 心跳在、目录不在 | `ChainOwnerBack` 要求 `verified_at > failed_at`。目录读不到就没有这次 `ChainVerified`，行留在 `FAILED`，CRITICAL 开着，认领不拉 `FAILED`，epoch 不加 | 关上 |
| S15b `pending_delete` 上再 Start | Delete 已把观察态写成 `STOPPING`。`pending_delete` 上的 Start 是 409，行不变 | 关上 |
| S16 持有者 `kill -9` 之后的 Stop 或 Delete | 行是 `STOPPING` / `STOP`。租约过期后任一节点 `StopExpired` 写成 `STOPPED`，`pending_delete` 则同一事务删除。不等已死的 `StopConfirmed`。Start 在 `STOPPING` 和 `pending_delete` 上都是 409 | 关上 |
| S17 缺省 sweep 60 秒的健康集群 | 有效 `stale_after` = 125 秒，大于一轮 sweep。`evaluated_at` 在这条线之内，`/api/health` 保持 200 | 关上 |
| S18 sweep 5 秒、TTL 60 秒、`node_gone` 未设置 | 有效 `node_gone_after` = `max(15, 70)` = 70 秒。集群启动。不在第一次让出时触发 `ChainOwnerGone` | 关上 |
| S19 迁移时还有一个旧节点 | 预检看见未过期注册，迁移在 DDL 之前失败。触发器只挡 `backup_tasks` 的 `UPDATE`。dump、`task_leases` 上的续租、`binlog_files` 不受影响。注册过期之后 `SIGCONT` 回来的旧节点继续 dump，新节点看到旧协议注册就打开 `MIXED_VERSION` 并且不认领 | 关上。措辞不再把续租和 dump 算进触发器 |
| S20 迁移时有停住的长事务 | 预检看见 `innodb_trx` 里超过 `idle_kill` 的事务，迁移失败，不排到元数据锁后面。DDL 前 `SET SESSION lock_wait_timeout = 10`，锁真的出现也在 10 秒内失败返回 | 关上 |
| S21 `per_node` 上回填 `task_chain` | `INSERT IGNORE`，`verified_at` 为 NULL。这行只给 `ChainOwnerGone` 看心跳。`TailUnreadable` 不把未验证的 `owner_node` 当成链主人，没有目录的节点仍让出。重跑迁移不插第二行 | 关上 |
| S23 meta 在 ProxySQL、MaxScale 或 RDS 后面 | 客户端不发 `KILL`。演练和真杀都是 `CALL binlog_server_kill_trx(trx_id)`。过程在服务器内按 `trx_id` 找线程，确认它仍握着 `task_leases` 或 `meta_canary` 的行锁，再在同一调用里杀掉；RDS 在过程里调 `mysql.rds_kill`。连接转给别人之后 `trx_id` 已变，`CALL` 是 no-op。`meta_canary` 每个 `node_id` 一行。`cluster.side_kill` 缺省 `off`，不演练，缺省 RTO 只用服务器超时。`auto` 只用于直连，写明在会解析 SQL 的代理后面不安全。过程建不上则开关保持 off。演练成功会在错误日志留下 `Aborted connection` | 关上 |

## 5. 迁移

每个 PR 自己能发布，自己有失败的测试在先。旧路径在引入新路径的同一个 PR 里删掉，不留「先双写一个版本」。不改 `.github/workflows`。standalone 不读这些表。

滚动升级要有栅栏。今天 `minRequiredSchemaVersion` 是 6，已发布的二进制只拒绝比 6 更旧的 schema。改列名只让重启后的心跳写失败。会话变量会在重连、`COM_RESET_CONNECTION` 或代理后面丢掉，不拿它当栅栏。PR-4 的迁移（下一号是 `000011`）按这个顺序做。每一步先查 `information_schema`，列或表已经在就跳过，所以一次失败的 up 可以重跑。MySQL 8 没有 `ADD COLUMN IF NOT EXISTS`。

1. **预检，失败则一行 DDL 都不做。** 下面任一成立，迁移直接退出：`worker_registrations` 里还有 `lease_expire_at > NOW(6)` 的行；`information_schema.innodb_trx` 里有起始时间早于 `idle_kill` 的事务；`log_bin` 开着，而当前账号既没有 `SUPER`，也没有 `log_bin_trust_function_creators = 1`。最后一条同时盖住 `CREATE TRIGGER` 和 `CREATE PROCEDURE binlog_server_kill_trx`：两者在开了 binlog 的主从或 RDS 上都会要这个权限，不能等 `ALTER` 做完一半才失败。过程最终没建出来时，`side_kill` 保持 off，不把集群卡在没有过程的 `auto` 上。这样 #476 那种停住的事务不会把 `ALTER` 堵在元数据锁上，后面的查询也不会排到 `ALTER` 后面。
2. **DDL 前** `SET SESSION lock_wait_timeout = 10`。预检之后仍出现的锁，10 秒内失败返回，不把集群挂住。
3. 新二进制把 `minRequiredSchemaVersion` 抬到 11。迁移没跑完，新进程不起。这一条只管新二进制。
4. 同一迁移把 `worker_heartbeats.version` 改名为 `build_version`（数据原样拷过去）。这只让**重启**的旧进程在启动校验里失败，与 `000006` 删掉仍被旧清单点名的索引是同一类。它停不住已经跑着的 dump，下一步的触发器也停不住。
5. **触发器只盖 `backup_tasks` 的 `UPDATE`，不看 desired 有没有变成 `STOP`，也不读会话变量。** 新写入把 `row_generation` 设成旧值加一。旧二进制的任务行 upsert 不碰这一列，`NEW.row_generation` 等于 `OLD.row_generation`，触发器 `SIGNAL`，这一笔任务行更新失败。`markFailedLocked` 和 `ReconcileLegacyDesiredRun` 写的是这张表，所以它们写不进 desired `STOP`。dump 线程、`task_leases` 上的续租、`binlog_files` 上的目录行都不经过这个触发器，照常成功。`SIGSTOP` 到注册过期再 `SIGCONT` 的旧节点绕得过预检，回来后继续 dump。新节点能发出的响声是 `_cluster` 上的 CRITICAL `MIXED_VERSION`，并且不认领。触发器带明确的 `DEFINER`（跑迁移的那个账号）。这个账号以后被删掉，`backup_tasks` 上的更新会全部失败；升级说明把它写成不能删。

```sql
CREATE TRIGGER backup_tasks_bu_generation
BEFORE UPDATE ON backup_tasks
FOR EACH ROW
BEGIN
  IF NEW.row_generation <> OLD.row_generation + 1 THEN
    SIGNAL SQLSTATE '45000'
      SET MESSAGE_TEXT = 'backup_tasks update must bump row_generation';
  END IF;
END;
```

6. `worker_registrations` 增加 `cluster_protocol_version`（缺省 0，新二进制写 11）。预检已经要求迁移时没有活注册。迁移之后若又有协议低于 11 且未过期的注册，新节点不认领，并在 `_cluster` 上打开 CRITICAL `MIXED_VERSION`。这是旧节点还在 dump 时集群上能看见的信号。同一迁移建 `meta_canary`（主键 `node_id`，每个节点一行）和过程 `binlog_server_kill_trx(p_trx_id)`。过程是 `SQL SECURITY DEFINER`，`DEFINER` 与触发器相同，升级说明写明这个账号不能删。演练锁自己的 `meta_canary` 行，不锁 `task_leases`。升级说明写明：先停掉全部旧节点，再 `migrate up`，再启动新节点。手写的 `UPDATE backup_tasks`（例如 `scripts/e2e/smoke-epoch-segments.sh`，以及升级说明里的 SQL）会被这个触发器拒绝，清单和改法在 PR-4（第 6 节 P2-3），不在本记录里逐条改脚本。

存量行的迁移，同一笔 `000011`，并且这笔 `UPDATE` 自己也要把 `row_generation` 加一，否则过不了上面的触发器。触发器在这批回填之后再建，回填就不必绕过它；顺序是回填，然后 `CREATE TRIGGER`。

```sql
UPDATE backup_tasks
SET desired_run = 'RUN',
    failed_reason = 'LEGACY_UNKNOWN',
    row_generation = row_generation + 1
WHERE state = 'FAILED'
  AND desired_run = 'STOP';
```

操作员 Stop 落到 `STOPPING` 或 `STOPPED`，不落到 `FAILED`，所以上面的谓词不会改写一次成功的操作员 Stop。它会改写「Stop 过程中被系统写成 `FAILED` + desired `STOP`」的行：升级之后这行的期望是 `RUN`，`failed_reason` 是 `LEGACY_UNKNOWN`，Console 上像是还能跑。升级说明要写这一句，操作员若本来是要停，升级后补一次 Stop。`FAILED` 且 desired 已经是 `RUN` 的行（#471 的 `failYieldedTail`）不动，`failed_reason` 留空，不标成 `CHAIN_OWNER_GONE`。

`task_chain` 回填只作未验证的心跳候选。`task_health.chain_owner` 在目录没读到时也会被写上（第 1.2 节），不能当成已经 `ChainVerified`。`task_chain.task_id` 是主键，`INSERT IGNORE` 让迁移重跑不插第二行；已经由真的 `ChainVerified` 写过的行保持原样：

```sql
INSERT IGNORE INTO task_chain (task_id, owner_node, verified_at, verified_epoch)
SELECT task_id, chain_owner, NULL, owner_epoch
FROM task_health
WHERE chain_owner <> '';
```

`verified_at` 为 NULL。`ChainOwnerGone` 可以用 `owner_node` 看心跳。`TailUnreadable` 的「本节点是链主人」不成立，没有目录的节点照样让出。下一次真的 `ChainVerified` 才把 `verified_at` 写成库时钟。

**降级。** `migrate down` 同样先做预检：还有 `cluster_protocol_version >= 11` 且未过期的注册，就拒绝，不在新进程还连着的时候拆栅栏。然后 `SET SESSION lock_wait_timeout = 10`，再按这个顺序：

1. `DROP PROCEDURE binlog_server_kill_trx`。旧二进制不调用它。
2. `DROP TRIGGER backup_tasks_bu_generation`。触发器还在时，旧二进制对 `backup_tasks` 的更新失败。
3. 把 `build_version` 改回 `version`，旧进程的启动校验才认这列。
4. 先处理 `pending_delete`，再删列。还有 `pending_delete` 置位、并且 `task_leases` 未过期的行，down 拒绝，列先留着。等持有者 `StopConfirmed`，或等租约过期后再跑一次 down。租约已空或已过期、且 `pending_delete` 置位的行，在删列之前删掉：任务行、`task_chain`、拒绝行、finding、租约。观察态已经是 `STOPPED` 的也删。列一删，旧二进制不认识这个意图，行会永远留成 `STOPPED`。
5. 删掉这次加的列和表：`backup_tasks` 上的 `row_generation`、`failed_reason`、`failed_at`、`tail_unreadable_since`、`pending_delete`，`worker_registrations.cluster_protocol_version`，以及 `task_chain`、`task_node_refusal`、`meta_canary`。留下它们的话，MySQL 8 没有 `ADD COLUMN IF NOT EXISTS`，再 `migrate up` 会在重复列上失败。不把 `LEGACY_UNKNOWN` 改回 desired `STOP`：分不清哪些是升级时翻转的，哪些是升级之后操作员又 Start 过的。需要停的任务由操作员在降级前 Stop。
6. 降级说明：先停掉全部新节点，再 `migrate down`，再启动旧节点。新二进制的 `minRequiredSchemaVersion` 是 11，schema 回到 10 之后它不起。活租约还在的 `pending_delete` 行会让这一步拒绝，等租约过期后重跑。

不使用 `@binlog_server_protocol` 或任何会话变量。重连和代理都会把它丢掉，操作员 Stop 不能靠它。

`ReconcileLegacyDesiredRun` 今天的 `CASE` 把不在 `RUN` 名单里的状态（含 `FAILED`）写成 `STOP`（`internal/meta/mysql_store.go` 的 `reconcileLegacyDesiredRunSQL`，且只碰 `spec_revision = 0`）。PR-4 把 `FAILED` 放进 `RUN` 那一臂，删掉「`FAILED` → `STOP`」这个分支。函数本身还留着，给 `spec_revision = 0` 的历史 `RUNNING` 行回填 desired `RUN`。`spec_revision > 0` 的存量 `FAILED` + `STOP` 靠上面的 `UPDATE`，不靠这个函数。

**PR #471 不合并。** 它把 §31 的「缺段要有 finding」和一套新的让出、内存退避、`failYieldedTail`、`PreserveChainOwner` 绑在一起。后一半就是第 1.5 节的四条失败。缺段的那一半在下面的 PR-B 里按「只有 `ChainVerified` 才派生 I6」重做，不把 #471 的分支 rebase 进来。#471 在本记录被接受后关闭，说明由本记录取代。

| 顺序 | PR 做什么 | 删掉什么 | 关上 |
| --- | --- | --- | --- |
| 0 | 本记录 | 无代码 | 不关 issue。接受之后才有下面几行 |
| 1 | 纯过渡函数、第 4.1 节的属性和轨迹，含目录消失时 `ChainOwnerBack` 不发生、`StopExpired`、以及 epoch 有上限。活性的检查方法（有界搜索，K 含 N 次无用认领，第 6 节 N9）在这个 PR 里写定。不接到调度器 | 无 | 无。规格先红后绿 |
| 2 | 按 flavor 设置会话杀：MySQL 8 的 `wait_timeout`、`net_read_timeout`、`net_write_timeout`，MariaDB 的 `idle_transaction_timeout`，两边都设 `innodb_lock_wait_timeout`。`cluster.side_kill` 缺省 `off`，缺省 RTO 只用这些服务器超时。`auto` 只用于直连，发布说明写明在会解析 SQL 的代理后面不安全。打开时演练和真杀都是 `CALL binlog_server_kill_trx`，客户端不发 `KILL`；`meta_canary` 每个 `node_id` 一行；过程不存在或演练失败则保持 off。发布说明把演练的 `Aborted connection` 列进告警白名单。`SET SESSION` 失败则拒绝 cluster 启动。`META_LOCK_STALLED` 写到 `_cluster`。有效 `stale_after` 用第 2 节第 7 条的下限，不夹到一轮 sweep 以下；有活 owner 的 `no_progress_after` 保持配置值。e2e 在 MySQL 8.0 和 MariaDB 上各打一格「握着 `FOR UPDATE` 时 `SIGSTOP`」，并打缺省 sweep 下 `/api/health` 保持 200。认领路径加一条断言：不读 `worker_registrations`。发布说明写上带起止点的缺省 RTO 公式和 `start_to_ready` 的 10 秒。同一 `node_id` 的 `per_node` 重启上界（F10）、单次锁超时的 Confirm（F12 的打开侧）、以及观察者死后由 `_health` 持有者关闭 `META_LOCK_STALLED`（N6）在这个 PR 里定。必做（第 6 节 P2-7、P2-8、P2-9）：会话超时要落在每一条池连接和每一次重连上，用 DSN 系统变量或连接钩子，并在一条新开的池连接上做 e2e，否则静默退回 28800 秒；ProxySQL / MaxScale 可能吞掉会话 `SET`，租约事务里回读 `@@session.wait_timeout`，不一致就报 degraded，或者把这些上界写成后端全局值；过程的 `DEFINER` 还要 `PROCESS`，MySQL 8 还要能读 `performance_schema.data_locks`，`EXECUTE` 只授给 binlog-server 账号；MariaDB 没有 `data_locks`，锁视图只显示卷进等待的锁，`auto` 演练要有第三条连接先等着，或者写明 `auto` 只支持 MySQL；`mysql.rds_kill` 点名 RDS MySQL、RDS MariaDB、Aurora MySQL；锁检查和 `KILL` 之间的 TOCTOU 是服务器内微秒级，可以接受 | 健康事务「1205 重试 50 次」这条把上界乘开的循环，改成超时就按 3.4 报告 | **#476、#463** |
| 3 | 已 `ChainVerified` 的目录不再丢弃 `I6_MISSING_LOCAL`，打开 `SEGMENT_MISSING`。未读到目录不写 `task_chain` / `chain_owner`。finding 还没改成单调之前，这条 PR **禁止**在不完整观察上关闭已打开的 `SEGMENT_MISSING`（先不关，留给 PR-5，避免再引入 100 ms 开合） | `chainFactsInto` 里集群模式对 I6 的 `continue` | **#462** |
| 4 | 接上过渡函数。`task_chain`（`INSERT IGNORE` 回填且 `verified_at` 为 NULL）、`task_node_refusal`、`row_generation`、`failed_reason`、`failed_at`、`tail_unreadable_since`、`pending_delete`。runner 事件的 epoch 栅栏，`ChainOwnerBack` 要求 `verified_at > failed_at`，`per_node` 的 `TailUnreadable` 每个节点一次且 `DumpReady` 删掉全部拒绝行，`shared` 与 `EpochLost` 到顶后按 `refusal_retry_cap_sec`（缺省 600 秒）再试。预检同时盖住 `CREATE TRIGGER` 和 `CREATE PROCEDURE binlog_server_kill_trx` 所需的 `SUPER` 或 `log_bin_trust_function_creators`；过程没建出来则 `side_kill` 保持 off。down 先删过程，再删掉租约已过期的 `pending_delete` 行，活租约还在则拒绝 down，然后删掉新列。`StopExpired`，`OperatorDelete` 写入 `STOPPING`，`pending_delete` 上 Start 为 409。`node_gone_after` 为 0 时取 `max(3×sweep, lease_ttl + 2×sweep)`。迁移 `000011`：预检、`lock_wait_timeout=10`、抬 `minRequiredSchemaVersion`、改名 `worker_heartbeats.version`、`row_generation` 触发器、`cluster_protocol_version`、`FAILED`+`STOP` 的存量 `UPDATE`、down 的步骤。`Release` 之后同一 `worker_id` 再 `Acquire` 必须把 epoch 加一，比较用同一条语句的 `NOW(6)`（第 6 节 F9）。拒绝行与让出同一事务（F12 的写入侧）。`cluster.storage` 登记进 meta（F11）。终态允许名单在进 `FAILED` 前先按可重试处理至少 2 分钟（N7）。只有 API 或没有目录的节点上 Start 写空 owner 的 `STARTING`（N8）。e2e：让出后重启、拷贝目录后 Start、`per_node` 的 `SIGSTOP`/`SIGCONT`（含目录还在时自己恢复，以及只有心跳时留在 `FAILED`）、持有者 `kill -9` 后的 Stop 与 Delete、第 4.2 节的竞态行与混跑行。手写的 `UPDATE backup_tasks` 必须把 `row_generation` 加一（第 6 节 P2-3）。迟到的 `EpochLost` 或 `StopConfirmed` 更新 0 行是 no-op，不是错误；租约行已经不在，就是 epoch 已丢；`per_node` 删除留下 tombstone，交给已有的 leftovers（P2-5） | `yieldUnreachableTailLocked`、`failYieldedTail`、`tailYieldUntil`、`yieldsUnreachableTail`、`reportUnreachableTail` 的 `PreserveChainOwner` 特殊写入、`markFailedLocked` 里对 desired `STOP` 的赋值、`ReconcileLegacyDesiredRun` 里 `FAILED` → `STOP` 的那一臂。`startTask` 里「`STARTING` 但 `run_id` 非空就拒绝」不再能碰到，因为写 `STARTING` 的唯一入口会清 `run_id` | **#473**，以及 #471 上尚未单独开 issue 的 QA（2）（3）（4） |
| 5 | finding 改成 3.5 的派生，尾巴事实用 `tail_unreadable_since`。同一键先关后开的属性测试接上生产的 `Reconcile`。`ChainVerified` 要求最新封存段与 catalog 一致；rehash 完成前的 catalog-only 校验和算不完整读，不关 `SEGMENT_UNREADABLE`（第 6 节 F7） | `OWNER_CHANGED` 关闭、`AwaitRehash` 作为「这次要不要关」的条件、`noteOwnerChanged` 在目录不完整时仍然置位并往下关的路径 | #471 QA（1）。#462 的 finding 从此在接管时不会闪 |

**PR-2 的期限（上表第 2 行，实现后补）。** 每一条拿锁的语句各自有客户端期限：`innodb_lock_wait_timeout` 再加上 2 秒，从这条语句开始时算起。健康插入、租约 `SELECT … FOR UPDATE`、健康行的 `FOR UPDATE` 和健康 `UPDATE` 都是这样。事务里更早的预持有或插入用掉的时间，不会把后面那条语句的期限压到服务器计时器下面。InnoDB 大约每秒才检查一次锁等待，1205 最多晚大约 1 秒，所以 2 秒的余量让服务器的 1205 通常先返回。若这条语句自己的客户端期限先到，并且它已经跑了至少 `innodb_lock_wait_timeout`，也按锁等待分类，和服务器 1205 一样，并记下这次是按已经过去的时间分类的。外层上下文取消，或者语句在 `innodb_lock_wait_timeout` 之前就被取消，不是锁等待。整次健康写入仍留一个外层期限，只做兜底，盖住这四条语句。

从池里等一条连接，和等一条语句，分开计。连接等到超时，只有在池已满（在用数等于上限）并且至少有一条正在进行的元数据写入已经确认在等锁、或者已经超过它自己的语句期限时，才算卡住，原因是 pool starvation behind a lock wait。池只是慢、没有任何锁证据时，仍是未确认的期限，不算卡住，这样普通的池拥塞不会被报成锁等待。

PR-2 和 PR-3 互不依赖，可以并行。PR-4 依赖 PR-1。PR-5 依赖 PR-3 的「未验证不关」那条临时禁令，并在同一 PR 里把禁令换成正式的派生规则，不让禁令多活一个版本。

0005 第 5 步的「失败则 desired `STOP`」在 PR-4 被本记录修正。0005 正文不在这个设计 PR 里改；PR-4 改代码时在 0005 的失败策略一节加一段指向本记录的修正，避免两份 ADR 对着干。

## 6. 留到实现 PR 的项

第一轮留下的 F7、F9–F12 仍不在本修订里展开。第二轮的 N6–N8 同样只定落点。N9 的检查方法已在 PR-1 写定。F14 的 epoch 上限和第 4.3 节的 GAP 行已经在本修订里关上。批准意见 [PR #477 的评论](https://github.com/Fanduzi/BinlogServer/pull/477#issuecomment-6280039941) 里还没做的实现备注记为 P2-7、P2-8、P2-9。

| 编号 | 留什么 | 落在 |
| --- | --- | --- |
| F7 | `ChainVerified` 不能只凭 `Stat` + `ReadDir`。最新封存段要与 catalog 一致，否则一份旧目录也能写成链主人。删掉 `AwaitRehash` 之后由这条补上：rehash 完成前只有 catalog 校验和，算不完整读，不得关闭 `SEGMENT_UNREADABLE` | PR-5 |
| F9 | `Release` 之后，同一个 `worker_id` 再 `Acquire` 必须把 epoch 加一。今天 `lease.sql` 在 owner 没变时不加。所有比较用同一条语句里的 `NOW(6)`。否则排在释放后面的 `WRITE_FENCED` 追加还能穿过栅栏 | PR-4，改租约 SQL 的那一笔，先于把调度器接上过渡函数 |
| F10 | `per_node` 上只有本节点能读链时，同节点重启的 RTO 要计入注册 TTL，或者允许同一 `node_id`、同一 `data_dir` 的干净重启接管自己已经过期的注册。二选一由这个 PR 定。跨节点公式仍不含另一个 `worker_id` 的注册 | PR-2 |
| F11 | `cluster.storage` 是每个进程自己的配置，各节点可以不一致。把它登记进 meta，不一致则拒绝认领。uploads 不改变接管、OPEN 尾巴是 `LOCAL_ONLY` 已写在第 3.3 节。D6 仍推迟 | PR-4 |
| F12 | 拒绝行与让出同一事务提交。单次 `MetaLockTimeout` 先经过 Confirm，再开 finding、再让 503，避免正常的行争用把健康接口打闪。原先 `TailUnreadable` 结果里「不开也关」那句笔误，已随尾巴事实改成 `tail_unreadable_since` 一起删掉 | 打开侧的 Confirm 在 PR-2；同一事务的拒绝行在 PR-4 |
| F14 | 第一轮说的「epoch 每 60 秒加一、没有上限」：`per_node` 的 `TailUnreadable` 每个节点一次，`DumpReady` 删掉拒绝行；其余拒绝到顶后按 600 秒再试，速率有界，不永久停住 | 本修订。不再留给实现 PR |
| N6 | `META_LOCK_STALLED` 的观察者死后 finding 不关，全集群一直 503。subject 用任务 id，`_cluster` 的 Reconcile 不得关掉不是它派生的这一条。观察者心跳早于 `node_gone_after` 之后，由 `_health` 持有者在连续两次拿到锁之后关闭 | PR-2 |
| N7 | 源改密码，或 VIP 切到 `log_bin` 关闭的从库，会把 `FAILED` 钉死，而 desired 仍是 `RUN`。进终态允许名单之前先按 `RetryableError` 重试至少 2 分钟。Console 显示 `failed_reason`。`OperatorStart` 清空 `failed_reason` 已在第 3.2 节写明 | PR-4 |
| N8 | 推进的 `OperatorStart` 在接住 HTTP 的那个进程上 `Acquire`。只有 API、或没有目录的节点会因此空握一个 TTL。这种进程改为写 owner 为空的 `STARTING`，交给有目录的节点认领 | PR-4 |
| N9 | 第 4.1 节活性 (a) 的 K 是上界，不是最长调度的长度。估大可以，估小是错。K 仍含 (b) 的 N 次无用认领。非读者窗口 W = max(读者拒绝期限, 行锁 Since+idle_kill, 停住或非读者持有者的 ExpireAt, RetryUntil, 链 FailedAt+1ms) − now。不计入的跳步是没停住节点的 node_gone、sweep、lock_wait、StartToReady，理由与第 4.1 节 (a) 相同。停住链主人的有效 node_gone 算进 W，随后的 `ChainOwnerGone` 算一步。已验证链主人读不到目录、但行还不是 `LEASE_DEGRADED` 且 `tail_unreadable_since` 已写下时，K 仍计入随后这次 `ChainOwnerGone`。sweep 到期之后可以先走其它已经启用的公平步，读者期限不截到 sweep。终点可以是终态而不是 `RUNNING`。终态由公平步图的底强连通分量导出，再和 `FairTerminal` 双向对账，与第 4.1 节 (a) 相同。唯一的时间步是把时钟推进到 `NextDeadline`，窗口 W 用同一组计时器。读者期限之后、又严格早于已有终点的非读者拒绝，仅当那个时刻认领集已经开着时算进最小值，不把 W 拉长。随机配置里包含目录恢复（`CanRead` 从假翻成真）这一环境步。每个非读者：NULL 拒绝计 0；下一次可认领晚于 W 计 0；`per_node` 上还不是已验证链主人的至多 1；其余（含 `per_node` 的已验证链主人，让出是会到期的 `EpochLost`）从已有 attempts 的下一次 `RefusalDelay` 数到间隔 ≥ min(cap, W)，再加 W>0 时的 `floor(W/cap)+1`。cap 用该状态的 `refusal_retry_cap_sec`。W>0 时跳步 ≤ 这些认领 + 已经落在 (now, now+W] 的拒绝点 + 1，再加 `Acquire`、`DumpReady` 和强制步。搜索只调用 `Step` 和 `Check`，不与上界函数共用辅助函数。20 万状态内能搜完的夹具要求观察步数 ≤ K；观察步数等于 K 的夹具用这个等式卡住少算的上界。超过上限的配置用偏向无用认领的带种子公平游走，已知长调度用 `Step` 重放，都要求 ≤ K。另有至少 300 个带种子的随机配置（cap、期限、attempts、非缺省 idle_kill 与 TTL、行锁、停住 owner、重试、链主人、1–4 个节点、shared 与 per_node，几种强制时钟可以同时在）。穷尽搜索要求观察步数 ≤ K。状态键用 `deadline−now`，只有严格早于每条比较的边界（含 c±1ns）才饱和，边界本身保持精确，epoch 和 `row_generation` 不在键里，所以顶满的退避会重复状态并形成底强连通分量，而不是把绝对时间一直往前推到上限。状态数超过上限是活性失败，不是跳过。单个种子超过时间预算也是失败，输出里带种子号。随机配置必须全部在上限内搜完，穷尽比例是 1，超过上限的种子数是 0。输出按种类计底强连通分量的个数。其中一部分配置观察步数等于 K。起点包含 `FAILED` / `CHAIN_OWNER_GONE`。随机前缀只验安全性 | PR-1。上界在 `internal/ownership/bound.go`，搜索在 `internal/ownership/observe_test.go` |
| P2-3 | 触发器会拒绝一切不把 `row_generation` 加一的 `UPDATE backup_tasks`。PR-4 列出并改掉仓库里的手写更新，包括 `scripts/e2e/smoke-epoch-segments.sh` 和升级说明里的 SQL | PR-4 |
| P2-5 | `StopExpired` 之后，旧持有者迟到的 `EpochLost` 或 `StopConfirmed` 若更新 0 行，就是 no-op，不是错误。租约行已经不在，也算 epoch 已丢。`per_node` 上删除任务之后，目录由谁清理要有主人：删除时留 tombstone，交给已有的 leftovers 机制，不在本记录里新做一套清理器。纯模型里，迟到的 0 行事件是 `DecisionDiscarded`，`per_node` 删除带 `IntentTombstone` | PR-4 |
| P2-7 | meta 的会话超时必须落在每一条池连接和每一次重连上，用 DSN 系统变量参数或连接钩子。e2e 在一条新开的池连接上复现握着 `FOR UPDATE` 时被服务器踢掉。只 `SET` 一次的话，其余连接仍是 28800 秒，第 3.4 节的 RTO 不成立 | PR-2，必做 |
| P2-8 | ProxySQL / MaxScale 可能吞掉 `SET wait_timeout`，也不会因此让「`SET SESSION` 失败则拒绝启动」生效。租约事务里回读 `@@session.wait_timeout`，和配置的上界不一致就报 degraded；或者发布说明要求这些上界是后端的全局值 | PR-2，必做 |
| P2-9 | `binlog_server_kill_trx` 的 `DEFINER` 除了 `CONNECTION_ADMIN` 或 `SUPER`，还要 `PROCESS`（才能看到别的账号在 `innodb_trx` 里的行）。MySQL 8 还要能读 `performance_schema.data_locks`，缺了会 fail closed。`EXECUTE` 只授给 binlog-server 账号，不给其他账号库级 `GRANT ALL`。MariaDB 没有 `data_locks`，InnoDB 锁视图只显示卷进等待的锁，canary 里第二条连接并不在等，演练会失败，`auto` 开不起来；要么让第三条连接先等着再 `CALL`，要么写明 `auto` 只支持 MySQL。`mysql.rds_kill` 点名 RDS MySQL、RDS MariaDB、Aurora MySQL。过程里看锁和 `KILL` 之间的 TOCTOU 是服务器内微秒级，最坏杀到的是已经连续两次探测都 Sleep 的线程，可以接受 | PR-2 写演练和 `auto`；PR-4 建过程时把权限和 `GRANT EXECUTE` 写进升级说明 |

## 本记录不做

- §31 D6 的「放弃丢失的 `LOCAL_ONLY` 尾巴、从最后一条已上传段续跑」API。`ChainOwnerGone` 把行留在 `FAILED`、期望 `RUN`、`failed_reason = CHAIN_OWNER_GONE`、finding 打开。链主人在 `failed_at` 之后重新 `ChainVerified` 时，`ChainOwnerBack` 回到 `LEASE_DEGRADED`。只有心跳、目录不在，行留在 `FAILED`。操作员仍可以用 Start 离开这行。D6 的 API 不在本记录里。
- 把 `_health` 持有者派去读其它节点的磁盘。
- standalone 的记录文件、段键、上传账本。
- 新的指标名。已有的 `binlog_server_finding_open` 和 `/api/health` 够用。
- 在本 PR 里改任何 Go 代码、迁移或 workflow。
