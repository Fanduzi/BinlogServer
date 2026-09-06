# Spec: 把「谁可以跑」和「怎么传上去」收拢

**Triage Label:** `ready-for-agent`  
**Status:** Approved  
**ADRs:** [0001](../../docs/adr/0001-task-ownership.md) · [0002](../../docs/adr/0002-best-effort-upload-one-caller.md) · [0003](../../docs/adr/0003-do-not-split-scheduler-first.md)  
**Glossary:** [CONTEXT.md](../../CONTEXT.md)

---

## Problem Statement

运维和排查的人面对的不是缺功能，是同一件事散在好几处：

1. 谁可以跑这个任务，写在调度器、执行器、启动恢复、认领循环里。过期接管、开机把别人正在跑的停掉、重启接不上，都是这处没合上。
2. 文件传上对象存储：执行器传一次，调度器再重试一次，钥匙（object key）只在一边算。
3. 界面分页走 SQL，汇总再扫全表，数字对不上。
4. 过期任务查询如果类型对不上，就当没有，静默漏掉接管。

## Solution

先加深，不先拆 Scheduler。

1. Worker 只留一处任务所有权：占、续、放、认领（开机和平时同一件事）、丢了就停、封文件前再问。控制面只写「请开始」。
2. 执行器只封文件。上传（第一次和重试）在一处。各家云 SDK 不动。
3. 分页、汇总、按源同一次读。
4. 要用的元数据查询写明白。禁止「对不上就当 0」。

测试走最高那扇门：所有权问「该我跑的跑起来」和「还是我吗」；上传问「把这份已封文件传上去」；观测问「这一页和汇总」。单机用「永远是我」，集群用租约表。不要为了测所有权去假造整条 binlog 流。

## User Stories

1. As a 集群 Worker，I want 开机时只接上该我跑的任务，so that 不会把别人还在跑的停掉。
2. As a 集群 Worker，I want 平时自动捡没人要的 STARTING 和租约已过期的任务，so that 控制面点启动之后有人真去拉。
3. As a 控制面操作者，I want 点启动只表示「请开始」，so that 没有执行器的进程不会占着租约跑不了。
4. As a Worker，I want 永久失败时立刻放开租约，so that 别人或重启后不必干等到 TTL。
5. As a Worker，I want 暂时连不上、正在重试时继续占着租约，so that 两台不会一起拉。
6. As a Worker，I want 租约丢了或宽限期内续不上就停，so that 不会双写。
7. As an 执行器，I want 封文件前再问一次还是不是我，so that 停命令和封文件之间的空隙不会写出别人的文件。
8. As a 单机部署，I want 走同一套「谁可以跑」的门（答案永远是自己），so that 不要维护两套 if。
9. As a 排查的人，I want 界面上的主人和能不能跑以租约为准，so that 任务行和租约对不上时不会误判。
10. As a 操作者，I want 上传失败不打断拉流，事后能重试，so that 对象存储抖一下不会丢备份进度。
11. As a 操作者，I want 第一次上传和点重试用同一套规则和同一把 object key，so that 重试不会传到另一个位置。
12. As a 看仪表盘的人，I want 分页总数和汇总数字来自同一次读取，so that 不会一页 10 条、汇总却是另一套全表。
13. As a 集群 Worker，I want 过期租约查询失败时被看见，so that 不会因为少实现一个查询就永远不去接管。

## Implementation Decisions

- 动手顺序：元数据查询写明白 → 任务所有权 → 尽力上传 → 任务观测。不要先拆 Scheduler。
- 任务所有权只存在于 Worker。控制面写 STARTING，清空任务行上的主人抄本，不写租约。
- 能不能跑只看租约。任务行上的 owner/epoch 是抄本，由所有权一起写。
- 认领：查名单和占锁是一次动作。开机多「自己名下还占着的」；平时多「别人租约过期的」。
- FAILED 立刻 Release。RETRY_BACKOFF 不 Release。
- 封文件前的「还是我吗」和占/续/放走同一处，不是 App 里再转一层。
- 单机 adapter：永远归本进程。测试用这个。集群 adapter：MySQL 租约表。
- Worker 注册和心跳不动。
- 尽力上传：封文件留执行器；上传收一处。不重开多云 ADR。FileUploader 仍是对各家云的那扇门。
- 任务观测：有 Store 走 SQL 分页和 COUNT；汇总、按源用同一过滤条件，不要再 ListTasks() 全表。
- 展开-收缩：新门先和旧路径并存，测绿再拆旧路径（App 里的 Stop+Start 恢复、leaseVerifierFromStore、静默 type-assert）。

## Testing Decisions

- 测能看见的行为，不测私有配方函数。
- 所有权测试：构造 Worker，不构造真实源库。断言：谁跑起来、谁没被停、租约在不在、封文件被拒绝。
- 上传测试：已封文件 → 成功/失败元数据；重试用同一 object key。
- 观测测试：同一过滤下，页 total 和汇总 total 一致。
- 已有先例：`lease_test.go`、`smoke_test.go` 的恢复/认领、`upload_retry_test.go`、`server_test.go` 分页。新测试取代「为测租约拼整个调度器」的那部分；旧测试在新门测绿后删掉，不要叠一层。

## Out of Scope

- 拆 Scheduler 成多个包
- 换云 SDK / 重开多云 ADR
- Worker 注册与心跳
- 界面改版
- `REBUILDING_FILE` 状态（未使用，这次不清理也不启用）

## Further Notes

词以 `CONTEXT.md` 为准。改代码从工单 01 开始，一次一张，做完再开下一张。
