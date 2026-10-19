# 集群所有权与故障转移：一台状态机

Status: Proposed（2026-10-19）。只有设计，没有运行时代码。等本记录被接受之后才按第 5 节改代码。

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

The target is one transition function. Desired `STOP` is written only by an operator Stop. A fact that is not true for the cluster is a yield or a finding, never `FAILED`. `chain_owner` moves to its own record and is written only after this node has read the chain directory. Refusals and their deadlines are rows, not maps. Findings stay open across an ownership change while the fact still holds; ownership changes who may close them, it does not close them. Every meta transaction that locks the lease row has a deadline inside the recovery budget, and a stalled lock is a critical finding and HTTP 503. Start and Stop are one transaction: the HTTP status and the row agree.

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
2. **只在一个节点上为真、或只对一个已经丢掉的 epoch 为真的事实，不是集群终态。** 它可以成为本节点的拒绝、一条 finding、一次让出。它不能是 `FAILED`，也不能取消另一个节点上还健康的 dump。
3. **一个 epoch 最多一个写者，并且写完就被栅栏挡住。** 数据面沿用 0009 §23 的租约时钟。任务行的观察态过渡用条件更新（第 3 节的 `row_generation`），不再用无条件 upsert 收尾。
4. **`chain_owner` 只有读到了链目录的节点能写。** 读到是指本节点 `data_dir` 下该任务目录 `Stat` 和 `ReadDir` 都成功，并且这次评估用的就是那份结果。目录不存在、读失败、链锁没拿到、提前返回，都不写。持有任务租约不是读到目录。
5. **finding 的开区间跨过所有权变化仍然单调。** 事实还在，finding 就还开着。换 owner 改的是谁有权关闭、以及归因，不是先关再开。不完整的观察既不开新的链 finding，也不关旧的。
6. **RTO 有公式。** `lease TTL + 锁等待上界 + 一轮认领间隔`。宽限只缩短本进程主动让出的时间，不增加对端的等待。worker 注册不在公式里。公式之外的等待必须已经是一条 CRITICAL finding，并且 `/api/health` 是 503。
7. **没有静默停备。** 期望 `RUN` 而没有 dump 在跑时，在一个 sweep 之内有一条 CRITICAL（已有的 `TASK_NO_PROGRESS`，或锁等待、尾巴无人能读）。最后一行 `HEALTHY` 在 `evaluated_at` 已经过期、或本轮因为锁没写上时，不能再被当成健康。
8. **每一次操作员调用是一个事务。** HTTP 4xx/5xx 时，行与调用前相比没有新的观察态。HTTP 2xx 时，行在调用返回前已经是这次调用所声明的态，并且这个态在认领查询里。不允许 400 加上一个没有 owner 的 `STARTING`。
9. **认领查询的并集等于「期望 `RUN` 且还没有一个活着的 owner」的全部合法观察态。** 不存在要靠下一轮补丁才能被认领的形状。
10. **拒绝认领的期限在 meta 里，用库时钟。** 重启、对端、操作员之外的第三个进程，看到的是同一份期限。

## 3. 目标模型

### 3.1 选哪一个

考虑过三条路，第 3.7 节写为什么不选。选定的是：

**一台纯函数做观察态过渡，租约和链主人分成两条记录，过渡用 `row_generation` 条件更新。** 谁都可以提出事件（runner、续租循环、认领循环、评估器、操作员 API）。落库的只有这台函数算出的下一条记录，而且 `UPDATE … WHERE row_generation = ?` 匹配不上就重读，不当成任务失败。

不把「集群里只留一个进程负责所有写入」当成目标。字节在哪台机器上，dump 就在哪台机器上。一个远程代写者会把没看见的目录写成集群事实，这正是 §30 之前代写清掉 `chain_owner`、以及 #471 上持有者 sweep 偷走 `chain_owner` 的结构。纯函数可以在每个进程里跑；权威是条件更新之后的那一行，不是某个进程的内存。

0005 的分工保留：操作员只写期望和规格，控制回路是唯一把期望变成一次 dump 的地方。变的是回路里那些直接改 `state` 的函数（`markFailedLocked`、`yieldUnreachableTailLocked`、`failYieldedTail`）收成事件，由同一台函数接受或拒绝。

### 3.2 状态、事件、守卫

期望与观察继续分列。期望只有 `RUN` 和 `STOP`。

观察态沿用现有枚举，不在这一步发明 API 上的新单词：`CREATED`、`STARTING`、`RUNNING`、`LEASE_DEGRADED`、`RETRY_BACKOFF`、`FAILED`、`STOPPING`、`STOPPED`。`LEASE_DEGRADED` 在这台机器里的含义收紧为：**本 epoch 已经放开，期望仍是 `RUN`，还没有下一个 owner**。它是可以认领的，也是操作员 Start 可以接受的。今天 Start 拒绝它，是第 1.5 节的洞。

新增的持久事实不塞进 `state` 字符串：

| 记录 | 键 | 内容 | 谁写 |
| --- | --- | --- | --- |
| `task_chain` | `task_id` | `owner_node`、`verified_at`（库时钟）、`verified_epoch` | 仅 `ChainVerified`。与任务租约的持有者写入分开，避免再在 `task_health` 的同一条 `UPDATE` 里顺手改掉 |
| `task_node_refusal` | `(task_id, node_id)` | `refuse_until`（库时钟）、`reason` | `TailUnreadable`。认领在 `refuse_until` 之前跳过这个节点。操作员 Start 在**同一个事务**里删掉本任务的全部拒绝行 |

`backup_tasks` 加 `row_generation`（`BIGINT NOT NULL DEFAULT 0`）。每一次接受的过渡加一。现有的无条件 upsert 在切到这台函数的那个 PR 里删掉，不留两条写路径。

`task_health.chain_owner` 停止被持有者写入维护。读的人改读 `task_chain`。列先留着，由写 `task_chain` 的同一事务填上，避免评估器还有旧二进制时读到空；旧语义（「谁最后跑了一轮持有者评估」）作废。

事件与守卫：

| 事件 | 谁可以提出 | 守卫 | 结果 |
| --- | --- | --- | --- |
| `OperatorStart` | 操作员 API，一个事务 | 观察态是 `CREATED`、`STOPPED`、`FAILED`、`RETRY_BACKOFF`、`LEASE_DEGRADED`、`STOPPING`（后两个仅当期望仍是 `RUN` 或正在停）。其它态拒绝，**不写** | 期望 `RUN`，`spec_revision+1`，计数清零，`last_error` 空，`run_id` 空，owner 空，epoch 0，观察态 `STARTING`，删掉本任务全部 `task_node_refusal`。返回 204。本地若能跑，204 之后再收敛；收敛失败是 finding，不再把 HTTP 改成 400 |
| `OperatorStop` | 操作员 API，一个事务 | 活动态，含 `LEASE_DEGRADED` | 期望 `STOP`，观察态 `STOPPING`，`spec_revision+1`。真正的 `STOPPED` 仍等握着 dump 的那边确认 `Close` 之后再写（0005） |
| `LeaseExpired` | 认领循环，租约行已经过期 | 期望 `RUN`；本节点没有未过期的拒绝行；观察态在第 2 节第 9 条的并集里 | `Acquire`，epoch 按现有规则加一，进入 `STARTING` |
| `DumpReady` | 本 epoch 的 runner | 仍持有该 epoch | `RUNNING`，计数清零 |
| `RetryableError` | 本 epoch 的 runner | 在 0005 的允许名单上，预算未用尽 | `RETRY_BACKOFF`，同一 epoch，预算列加一 |
| `TerminalError` | 本 epoch 的 runner | 错误在任何节点上都会失败（认证、`log_bin` 关闭、未分类的 runner 错误、本 epoch 仍持有租约时本节点目录里缺了**自己的** open 文件） | `FAILED`，`failed_spec_revision = spec_revision`，释放租约。**期望保持 `RUN`**。认领不拉 `FAILED`。下一次 `OperatorStart` 是新的 spec |
| `TailUnreadable` | 本 epoch 的 runner，错误是 `SEGMENT_NOT_ON_WORKER` | `task_chain.owner_node` 不是本节点（空也算不是：读不清就不写终态）。本节点这次没有 `ChainVerified` | 释放本 epoch。观察态 `LEASE_DEGRADED`，期望 `RUN`，owner 空，**`run_id` 空**。写 `task_node_refusal`，期限是库时钟加退避（缺省 60 秒，配置项保留 `health.tail_yield_backoff_sec`）。CRITICAL finding 的 subject 是 `SEGMENT_NOT_ON_WORKER`，在有人重新跑起来并清掉 `last_error` 之前不开也关 |
| `ChainVerified` | 评估器，且仅当第 2 节第 4 条的读目录成功 | 本节点是任务租约持有者，或已经是 `task_chain.owner_node` 且租约已释放（今天的委派） | 写 `task_chain`。不改变任务观察态 |
| `ChainOwnerGone` | `_health` 持有者的代写轮 | `task_chain` 非空，该节点心跳早于 `node_gone_after`，且行还停在 `LEASE_DEGRADED`、`last_error` 是尾巴不可读 | 观察态 `FAILED`，期望保持 `RUN`，finding 保持打开。这是「没有剩下的节点能读这条尾巴」，不是 desired `STOP`。操作员 Start 或未来的放弃尾巴 API 才能离开 |
| `EpochLost` | 续租循环、`WRITE_FENCED` 且行上没有更新的 epoch、本 epoch 的 open 文件 `ENOENT` 且租约已经不是自己的（§29、§29.10、§29.11） | 期望不是 `STOP` | 与 `TailUnreadable` 同一条让出，但不写 `task_node_refusal`（这台机器还能读目录）。不写 `FAILED` |
| `MetaLockTimeout` | 任何拿 `task_leases` 锁的调用，超时返回 | 无 | 不改任务观察态（改不了）。开 CRITICAL，`/api/health` 503。见 3.4 |
| `OperatorStart` 碰到不合法的态 | API | 例如已经 `RUNNING` | 409，正文是当前观察态。行不变 |

`FAILED` 不再附带 desired `STOP`。0005 用 desired `STOP` 阻止控制回路把失败的任务再拉起来；同一件事用「认领不拉 `FAILED`，直到 `spec_revision` 大于 `failed_spec_revision`」已经表达了。两套开关（desired `STOP` 和 `failed_spec_revision`）让 #450 那类补丁必须记得把哪一个改回去。目标只留规格这一套。操作员 Stop 仍然把期望写成 `STOP`，失败的任务可以被 Stop，也可以被 Start。

一次让出的 `run_id` 清空，是为了第 1.5 节的卡死：`STARTING` 且 owner 为空必须满足 `canClaimDispatched`，否则认领查询和 `startTask` 互相矛盾。

### 3.3 存储模式

今天没有「盘是共享的」这一事实，所以每节点一份盘和共享 `data_dir` 在接管时走同一条 `SEGMENT_NOT_ON_WORKER`。目标把模式写成配置 `cluster.storage`：`shared` 或 `per_node`。缺省在迁移完成前按 `per_node` 处理接管失败（让出，不写终态）。这是安全的一边：共享盘上不该出现 `SEGMENT_NOT_ON_WORKER`，出现了就当成像盘不可读一样让出并告警，而不是停掉备份。确认挂载之后再配 `shared`。

| 模式 | 接管时尾巴不在本节点 | 谁写 `task_chain` | 链 finding |
| --- | --- | --- | --- |
| `per_node` | `TailUnreadable`，不写 `FAILED` | 读到自己 `data_dir` 下该目录的节点 | 只有 `task_chain.owner_node` 能开、能关。别的节点的空目录既不开 `SEGMENT_MISSING`，也不以 `OWNER_CHANGED` 关掉已有的 |
| `shared` | `TailUnreadable` 仍让出并开 CRITICAL。共享盘上出现这条，说明挂载或路径错了，不是「换一台接着跑」 | 当前持有者，且第 2 节第 4 条的读必须成功 | 持有者读到的那一份就是集群的那一份 |

不在这一步做「`_health` 持有者直接读别人的目录」（§30 D9 已经拒绝）。共享与否由配置声明，不由某次路径比较推断。

### 3.4 锁、超时、RTO

每一条会锁 `task_leases` 的会话，在拿到连接之后、开始事务之前设置：

- `innodb_lock_wait_timeout` = `min(5, lease_ttl_sec / 2)` 秒，至少 1 秒
- MySQL 8.0 上再设 `idle_transaction_timeout` 为同一上界。事务在 `SIGSTOP` 里变空闲时，由服务器杀掉，而不是等进程被 `SIGCONT`

事务内部不做磁盘 IO、不等待源库。0009 §23 的写边界保持「先锁租约行，再写这一小段 meta」。评估器把目录读完，再开这一个短事务提交结果。

对端超时或收到锁等待错误，提出 `MetaLockTimeout`：CRITICAL finding（新的 subject 用现有 `META_UNREACHABLE` 或单独的 `META_LOCK_STALLED`，实现时二选一，不两个都留），`/api/health` 503。不重试 50 次把 RTO 乘开。

`/api/health` 在下面任一成立时是 503，即使最后一行 `task_health.status` 还是 `HEALTHY`：

- 有未关闭的 CRITICAL finding
- 期望 `RUN` 的任务，`evaluated_at` 老于 `stale_after`，或本轮因为锁没写上

第二条把 #449 的 WARNING 在「行已经不可信」时升成降级。只是慢、但锁得上、数据还在涨的任务，仍保持今天的 WARNING，不 503。

RTO 公式，写进发布说明和 e2e 的断言，而不是写进日志让人猜：

```
RTO <= lease_ttl + lock_wait_timeout + claim_interval
```

缺省量级：TTL 15 秒（配置缺省）或现场的 60 秒，锁等待不超过 TTL 的一半，认领间隔 2 秒。宽限不进公式。`worker_registrations` 不进公式，认领继续不读它。同一 `worker_id` 被 `SIGSTOP` 之后，新进程要等注册过期才能占用这个 id——这是 0001 的会话所有权，保留；它不得挡住**另一个** `worker_id` 的任务认领。#463 的 143 秒在这套公式下是失败，不是可以接受的上界。

### 3.5 finding 从状态机派生，而不是从一次扫目录的差集派生

`Reconcile` 今天是「上一份打开集合」对「这一轮想要的集合」做差，再对链 finding 插入 `OWNER_CHANGED` 这个例外。例外就是关了又开的来源。

目标：一条 finding 的开关是状态机事实的函数，同一把键在事实不变时没有转移。

| 事实 | 打开 | 关闭 | 所有权变化时 |
| --- | --- | --- | --- |
| `task_chain` 上的目录违反了 I1–I7 中仍然启用的那一条 | 读到目录的节点，按现有 Confirm（连续两轮）打开 | **同一个** `task_chain.owner_node` 再次读到目录，且该键不再被派生 | 不关闭。新 owner 只有在自己 `ChainVerified` 之后才能关。关的原因是 `EVAL`（事实没了），不是 `OWNER_CHANGED` |
| 观察态是 `LEASE_DEGRADED` 且 `last_error` 归类为 `SEGMENT_NOT_ON_WORKER`，或 `ChainOwnerGone` 之后的 `FAILED` 且期望仍是 `RUN` | `TailUnreadable` 当时就打开 CRITICAL `TASK_FAILED` / `SEGMENT_NOT_ON_WORKER` | 某个节点进入 `RUNNING` 且 `last_error` 已清 | 保持打开。无人持有租约的这段时间 `/api/health` 503 |
| 期望 `RUN`，没有活 owner，超过一个 sweep | 已有的 `TASK_NO_PROGRESS` | 有了 owner 并且 dump 有进展 | 无关 |
| 锁等待超时 | `MetaLockTimeout` | 下一次同键写入成功 | 无关 |
| 封存段在**已验证的**目录上缺失 | `SEGMENT_MISSING`（#462，不再丢弃 I6） | 文件回到**同一份**已验证目录，`EVAL` | 不因为新节点还没读到目录就关 |

`OWNER_CHANGED` 这个关闭原因删掉。需要知道「链换节点了」时，记一条事件，不作为 finding 的开关。`AwaitRehash`、`PreserveChainOwner`、`noteOwnerChanged` 在不完整目录上照样关 finding 的那条路径，随这个 PR 一起删，不保留兼容分支。

同一轮里对同一把键先 `CLOSE` 再 `OPEN` 视为模型错误，属性测试直接失败。

### 3.6 操作员 API

`StartTask` 不再「先 `writeStartIntent` 再 `convergeTask`，两步的错误都变成 400」。

1. 一个事务里跑 `OperatorStart` 或 `OperatorStop`（3.2）。
2. 条件更新匹配不到：重读。态已经被别人推进到本次要去的地方，返回 204。态不合法，返回 409，行是重读到的那一行。
3. 事务提交之后才在本进程收敛。收敛的错误不再改写已经返回的状态码；若响应还没写，收敛失败也不把已经提交的 `STARTING` 说成 400。调用方看到 204 就可以认领查询里看见这行。
4. `LEASE_DEGRADED` 且期望 `RUN` 是合法的 Start。它清拒绝行、清 `run_id`、进入 `STARTING`。这是操作员明确要求再试，不是认领循环偷偷违反退避。

Stop 已经是「先落库 `STOPPING`」。保持为单事务。失败不写 `STOPPING`。

### 3.7 考虑过的方案

**继续在 #471 上补第 5 个守卫。** 例如「持有者 sweep 在目录没读到时也带上 `PreserveChainOwner`」「Start 之前清 `run_id`」「退避写进表」。每一条都能让当前 QA 变绿，并且和 §29.10、§29.11、§31.6 的写法相同：在出问题的那个函数里再加一个条件。下一个窗口（锁、注册、重启、第三次让出）不在这个条件里。不选。

**集群只留一个写者进程，其它节点把事件寄给它。** 写者若不是链所在的节点，就得代写它没读过的目录，§30 已经证明代写会改掉不该改的列。dump 也无法挪到没有字节的机器上。不选。纯函数加上条件更新得到「逻辑上一个写者」，而不要求「物理上一个进程」。

**租约和链主人继续放在同一条 `task_health` UPDATE 里，只用注释禁止误写。** #471 的 `PreserveChainOwner` 就是这个方案。普通持有者写入不看那个布尔值。分开两条记录之后，偷写要专门再写一条 SQL，审查看得见。选分开。

**用 generation 代替状态机，谁后写谁赢。** `row_generation` 只解决丢失更新，不解决「这次写入的内容是不是合法过渡」。#457 的 `FAILED` 可以带着新 generation 覆盖健康的 owner。generation 是过渡函数的比较条件，不是模型本身。两样都要。

**把退避放在租约行的 `lease_expire_at` 上，让 Acquire 自然失败。** 过期时间表达的是「谁现在握着」，不是「这个节点到什么时候不许再试」。一个节点的拒绝会挡住其它还没试过的节点。拒绝按 `(task_id, node_id)` 记。不选把期限塞进租约过期时间。

**共享盘由 `_health` 持有者跨节点读目录。** §30 D9 已拒绝。本记录用 `cluster.storage` 声明，不新增跨节点读。

## 4. 验证

实现期间不靠「再跑一轮手工 QA 看会不会冒新症状」当门槛。两道门都是自动的：模型必须接受第 1 节的轨迹，故障注入必须在 e2e 里失败于回归。

### 4.1 模型

过渡函数放在独立包里，不导入调度器、不打开数据库。状态是一个结构体：期望、观察态、`row_generation`、租约 owner/epoch/过期、`task_chain`、拒绝表、finding 开集、各节点能否读目录、哪把 meta 锁被谁握着。事件是 3.2 的枚举。

检查用两种：

- **属性**（Go 的随机序列，固定种子可重放）：从 `CREATED` 出发，任意交错事件，每一段前缀都满足第 2 节全部 10 条。特别是：非 `OperatorStop` 之后 desired 不是 `STOP`；`FAILED` 不会出现在「本节点读不到目录、且 `task_chain` 是别人」的前缀里；同一 finding 键不会在事实未变时先关后开；`OperatorStart` 的失败分支不改变状态；拒绝期限只来自库时钟那一列。
- **轨迹**（表驱动，QA 场景写成事件序列，断言终态）：

| 轨迹 | 事件 | 终态 |
| --- | --- | --- |
| §29 宽限 | owner 续租超时，没有新主人 | `LEASE_DEGRADED`，desired `RUN`，可被 `LeaseExpired` 拉起，epoch+1，没有第二条 dump |
| §29.10 | 让出之后 `WRITE_FENCED` | 仍是可认领的观察态，不是「`STOPPED` + desired `RUN` + 空 owner」以外的死角；`STOPPED`+`RUN`+空 owner 若出现也必须可认领 |
| §29.11 | 新 epoch 已在跑，旧 epoch `ENOENT` | 共享行仍是新 owner 的 `RUNNING`。旧节点只有本地收尾 |
| #462 | `ChainVerified` 的目录缺一个非 `UPLOADED` 封存段 | `SEGMENT_MISSING` 打开。未 `ChainVerified` 的节点不能开、也不能关 |
| #473 | `per_node`，owner 停住，对端 `TailUnreadable`，重复超过两次，然后对端进程「重启」（内存清空，meta 还在） | 始终 desired `RUN`，`task_chain` 仍是原主人，重启后的拒绝行还在，未到期不能 Acquire。原主人 `DumpReady` 后 finding `EVAL` 关闭 |
| #471 QA（2） | 对端在 `TailUnreadable` 之前插入一次「目录读取失败的评估」 | `task_chain` 不变。随后仍是让出，不是 `FAILED` |
| #471 QA（3） | 让出或 `ChainOwnerGone` 之后 `OperatorStart` | 成功则 `STARTING`、`run_id` 空、拒绝行已删，204。若调用方构造一个不合法态，行与调用前相同 |
| #471 QA（1） | `ChainVerified` 从节点 A 换成节点 B，违反仍然在 | finding 始终开着，关闭次数是 0。B 尚未 `ChainVerified` 时同样不关 |
| #476 | owner 握着租约行锁，对端收到 `MetaLockTimeout` | 观察态可以还是 `RUNNING`，但 finding 是 CRITICAL，健康读是降级。锁的持有步数有上界（模型里的超时事件一定会在 TTL 步数内出现） |
| #463 | 另一个 `worker_id` 的 `LeaseExpired`，原节点注册未过期 | 认领成功，不等注册 |

这包测试在第 5 节的第一个实现 PR 里就进 `go test`。生产代码还没接上时，它锁的是规格。接线的 PR 只许把调度器改成调用这台函数，不许在调度器里复制一份守卫。

PlusCal 不作为 CI 的依赖。过渡函数的注释用同一套变量名（`phase`、`desired`、`chain`、`refusal`、`findings`），使愿意用 TLC 的人可以照着译，而不在仓库里维护第二份会漂的规格。

### 4.2 故障注入，作为 e2e 门槛

每一格是一次真节点（两节点 meta，评估器 on）必须出现的结果。`SIGSTOP` 打在该阶段的进程上，停超过 TTL，再 `SIGCONT`。断言用 meta 行、`/api/health`、finding、源库 `SHOW PROCESSLIST` 的 dump 线程数，不用日志。

| 阶段 | `SIGSTOP` | `kill -9` | 网络分区（只断该节点到 meta） | 重启落在退避期内 |
| --- | --- | --- | --- | --- |
| `Acquire` 已提交、dump 未开 | 对端在 RTO 内接手或让出；不得 `FAILED`+desired `STOP` | 同左 | 同左，且分区期间 503 | 不适用 |
| dump 已开，事务未开始 | 对端在 RTO 内接手，恰好一条 dump | 同左，open 段不丢已落盘事件 | 原 epoch 让出，desired `RUN`，503 | 不适用 |
| 正握着 `task_leases` 的 `FOR UPDATE` | **RTO 内**锁消失（服务器杀掉空闲事务），对端继续；期间 503，不得保持 200+`HEALTHY` | 连接随进程消失，对端在 TTL 内接手 | 同锁那一格 | 不适用 |
| `TailUnreadable` 已提交 | 该节点到期前不再 Acquire；另一节点若能读目录则跑起来 | 拒绝行还在 | 503，行仍是 `LEASE_DEGRADED` | **拒绝行还在，不立刻再认领** |
| 评估器正读目录，尚未 `ChainVerified` | `task_chain` 不变 | 同左 | 同左 | 不适用 |
| `OperatorStart` 事务已开未提交 | 提交或回滚二选一；不得留下没有 owner 的 `STARTING` 且 HTTP 已经 400 | 同左 | 调用返回 503，行不变 | Start 仍是单事务成功 |

另外四格，不放进上面的相位表：

| 场景 | 断言 |
| --- | --- |
| 连续 N 次让出（N>2），对端始终没有目录 | `task_chain` 始终是最后一次 `ChainVerified` 的节点；观察态不是 desired `STOP`；CRITICAL 一直开着；`/api/health` 503 |
| 把任务目录拷到对端再 Start | 新节点 `ChainVerified` 之后 finding 按事实关闭或保持，关闭次数对「事实未变」的键是 0；dump 一条 |
| `cluster.storage=shared`，拔掉一侧挂载 | 让出 + CRITICAL，不写 desired `STOP`。挂载回来之后原节点或对端在 RTO 内回到 `RUNNING` |
| `cluster.storage=per_node`，只 `SIGSTOP` owner | 与 #473 相反：原主人 `SIGCONT` 之后以更大 epoch 继续，源上始终最多一条 dump，GTID 不重叠 |

这张表进 e2e 的发布档，不进每次 PR 的 `unit-and-e2e` quick（quick 跑不了多节点 `SIGSTOP`）。接线的那个 PR 至少带两格：锁中间 `SIGSTOP`（#476）、让出后重启（#471 QA 4）。其余格在同一张表里列成该 PR 的测试计划，没跑的格子写明没跑，不把没跑写成通过。

## 5. 迁移

每个 PR 自己能发布，自己有失败的测试在先。旧路径在引入新路径的同一个 PR 里删掉，不留「先双写一个版本」。不改 `.github/workflows`。不抬 `minRequiredSchemaVersion`（新表、新列带默认值，和 0009 的可选迁移同一做法）。standalone 不读这些表。

**PR #471 不合并。** 它把 §31 的「缺段要有 finding」和一套新的让出、内存退避、`failYieldedTail`、`PreserveChainOwner` 绑在一起。后一半就是第 1.5 节的四条失败。缺段的那一半在下面的 PR-B 里按「只有 `ChainVerified` 才派生 I6」重做，不把 #471 的分支 rebase 进来。#471 在本记录被接受后关闭，说明由本记录取代。

| 顺序 | PR 做什么 | 删掉什么 | 关上 |
| --- | --- | --- | --- |
| 0 | 本记录 | 无代码 | 不关 issue。接受之后才有下面几行 |
| 1 | 纯过渡函数、第 4.1 节的属性和轨迹。不接到调度器 | 无 | 无。规格先红后绿 |
| 2 | meta 会话的锁等待上界、`idle_transaction_timeout`、`MetaLockTimeout` 的 CRITICAL 与 503。e2e 打「握着 `FOR UPDATE` 时 `SIGSTOP`」。认领路径加一条断言：不读 `worker_registrations`。发布说明写上 RTO 公式 | 健康事务「1205 重试 50 次」这条把上界乘开的循环，改成一次超时就报告 | **#476、#463** |
| 3 | 已 `ChainVerified` 的目录不再丢弃 `I6_MISSING_LOCAL`，打开 `SEGMENT_MISSING`。未读到目录不写 `task_chain` / `chain_owner`。finding 还没改成单调之前，这条 PR **禁止**在不完整观察上关闭已打开的 `SEGMENT_MISSING`（先不关，留给 PR-5，避免再引入 100 ms 开合） | `chainFactsInto` 里集群模式对 I6 的 `continue` | **#462** |
| 4 | 接上过渡函数。`task_chain`、`task_node_refusal`、`row_generation`。`TailUnreadable`、`OperatorStart` 单事务、`FAILED` 不写 desired `STOP`、`run_id` 在让出和 Start 时清空。e2e：N 次让出、退避期内重启、拷贝目录后 Start、`per_node` 的 `SIGSTOP`/`SIGCONT` | `yieldUnreachableTailLocked`、`failYieldedTail`、`tailYieldUntil`、`yieldsUnreachableTail`、`reportUnreachableTail` 的 `PreserveChainOwner` 特殊写入、`markFailedLocked` 里对 desired `STOP` 的赋值。`startTask` 里「`STARTING` 但 `run_id` 非空就拒绝」不再能碰到，因为写 `STARTING` 的唯一入口会清 `run_id` | **#473**，以及 #471 上尚未单独开 issue 的 QA（2）（3）（4） |
| 5 | finding 改成 3.5 的派生。同一键先关后开的属性测试接上生产的 `Reconcile` | `OWNER_CHANGED` 关闭、`AwaitRehash` 作为「这次要不要关」的条件、`noteOwnerChanged` 在目录不完整时仍然置位并往下关的路径 | #471 QA（1）。#462 的 finding 从此在接管时不会闪 |

PR-2 和 PR-3 互不依赖，可以并行。PR-4 依赖 PR-1。PR-5 依赖 PR-3 的「未验证不关」那条临时禁令，并在同一 PR 里把禁令换成正式的派生规则，不让禁令多活一个版本。

0005 第 5 步的「失败则 desired `STOP`」在 PR-4 被本记录修正。0005 正文不在这个设计 PR 里改；PR-4 改代码时在 0005 的失败策略一节加一段指向本记录的修正，避免两份 ADR 对着干。

## 本记录不做

- §31 D6 的「放弃丢失的 `LOCAL_ONLY` 尾巴、从最后一条已上传段续跑」API。`ChainOwnerGone` 只把行留在 `FAILED`、期望 `RUN`、finding 打开，等操作员 Start 或那个以后的 API。
- 把 `_health` 持有者派去读其它节点的磁盘。
- standalone 的记录文件、段键、上传账本。
- 新的指标名。已有的 `binlog_server_finding_open` 和 `/api/health` 够用。
- 在本 PR 里改任何 Go 代码、迁移或 workflow。
