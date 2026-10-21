# ADR 0009 阶段 3 设计：巡检器 (Verifier)

状态：设计（2026-10-20；第 2 轮按 QA 改为覆盖模型，见第 6 节；第 3 轮改为先免费、再按队列付费，见第 5 节）。上位决定：[ADR 0009](../adr/0009-backup-assurance.md) 第 3 节与「分阶段实施」第 3 行。前置是已落地的备份链（`internal/assurance` 的 `Chain` / `Check` / `Window`）和阶段 2 的健康评估器。本文只回答巡检器**查什么、怎么限速、结论叫什么、配置和迁移是什么、还原演练为什么默认关、以及它怎样挂到任务主人上**。不改 ADR 0009 的决定，只修订 ADR §3 第 2 条的 finding 表（对象的三种结论分开，见决定 3 与第 8 节）；和 ADR 原文或阶段 2 草图不一致的地方写在第 13 节，并给出理由。

集群所有权正在按 [ADR 0011](../adr/0011-cluster-ownership-and-failover.md) 重做。PR-1（纯过渡模型）还在 QA，PR-2 到 PR-5 会换掉今天的租约与 finding 单调性。本阶段**不读、不写、不封装**今天的 `task_leases`、认领循环或 failover。巡检器只通过第 10 节那条窄缝问「我是不是这个任务现在的主人」。那条缝的集群实现留给 ADR 0011 PR-2。

## 1. 要解决什么

阶段 1 的 `Chain.Check` 能在**有人来问**的时候判定 I1–I7。阶段 2 的评估器按 sweep 把链上已经算出来的违反写成 finding。两件事都不会做 ADR 0009 第 3 节要求的三件定期工作：

| 缺口 | 今天 | 后果 |
|---|---|---|
| 本地封存段没有被完整解析过 | 评估器复用账本的 size+mtime 缓存；复核器只重算 SHA-256，不看事件 CRC、位点和 GTID | 校验和没变、但事件结构坏了的文件可以一直绿 |
| 对象只在上传当时对过一次 ETag | 上传后桶里的对象被生命周期删掉或改掉一个字节，没有第二个人去 HEAD / 读 | #173 那类「看起来已上传」会在上传之后再次出现，而且本地保留删掉之后无本地可比 |
| 链上的违反没有一个固定的周期作者 | 评估器会派生它读到的违反，但对象侧 scope 还没有被标成「有人生产」（`health.Produced` 不含 `chain.objects` 和 `verify`） | 对象丢失不会变成一条会自己关掉的 finding |

还原演练（每 7 天在一台专用实例上放到一个时间点）默认仍关，等阶段 5 的还原计划。周期巡检的工人在 `internal/tasks/verifier.go`：缺本地文件和损坏对象由运行中的工人检出，经现有 API 报出，检查在 `go test ./...` 里。集群上 `Allows` 恒为 false，所以生产清单 A4 仍是 PARTIAL，等 ADR 0011 的主人（PR-2/PR-4）落地后再开。真实 MinIO 场景仍不在任何 job 里。

## 2. 这一份实现的边界

先做**不依赖主人是谁**的部分，因为主人的定义马上要换。

这一份做：

- 纯函数：给定目录行、本地封存段、对象存储的 HEAD/GET，返回本轮应当开着的 finding。
- 字节预算。
- 上传成功时，如果目录行还没有 SHA-256，把它补进**已经存在**的 `binlog_files.sha256`（迁移 `000008`）。不新建列。
- 用假对象存储的单测覆盖：删一个对象、改对象一个字节、改本地一个字节，下一轮都报出来；干净的链没有 finding；预算真的卡住读取。关闭、取消与截止时间、`last_verified_at`、强制重查、队列顺序、反复短截止时间下的进度上界各有单测，`internal/assurance/mutants/kill.go` 用它们打变异。

工人（PR-2，`internal/tasks/verifier.go`）已接上周期循环：每个 `verify.every_sec` 对任务调用 `Verify`，输入来自目录、本地 `data_dir` 和对象存储，finding 走现有出口（集群 `task_findings` 和 standalone 的 `.findings.json`）。`Report.VerifiedAt` 与 `Report.Read` 写在 `{data_dir}/.verify.json`，不占迁移。进程内一把字节预算，任务轮转开头。链传 `nil`（不把没跑的链写成 finding）。集群上 `Allows` 恒为 false，等 ADR 0011 PR-2 换成当前主人；standalone 不设 `Allows`，恒为允许。

仍不做：

- 把 `health.Produced` 里的 `chain.objects` / `verify` 改成 true。评估器不生产这些 kind。`TestHealthIgnoresScopesNotProduced` 锁着这两个 scope。注册表里 `VERIFY_FAILED`、`OBJECT_CORRUPT`、`UPLOAD_CLOCK_SKEW`、`LOCAL_UNREADABLE`、`DRILL_FAILED`、`DRILL_REFUSED` 是未托管项（`Derive` 为空），健康面是 `HealthSummary`：开着的 CRITICAL 让 `/api/health` 503。
- 还原演练的进程、DSN 和 MySQL。键可以加载，工人不拨号。
- 真实 MinIO 场景进 CI。场景脚本在 `scripts/e2e/smoke-verify.sh`，现有 workflow 不会选中它。要进 CI 必须改 workflow，本仓库的 token 改不了 workflow。改法写在第 14 节。A4 不靠这个场景：运行中的工人经 API 报出缺文件和坏对象的测试已经在 `go test ./...` 里。

## 3. 核心决定

1. **巡检是一次纯计算，不是一个新的写者。** 输入是这一刻的目录行、本地字节和对象字节，输出是 finding 集合和每个通过的段的 `last_verified_at`。同一份输入给出同一份输出。谁来周期调用它、谁把结果落库，是主人接缝后面的事。理由：阶段 2 已经有唯一的 finding 生命周期（`Reconcile`）。再做一个会写 `task_findings` 的循环，就会在 ADR 0011 换掉单调性之前多出一个写者。
2. **只有当前主人为这个任务跑巡检。** 纯函数自己不问。调用方先问第 10 节的缝；缝说不是，或者缝出错，就跳过这个任务，**既不开也不关** finding。理由：本地字节在主人那台机器上（ADR 0011 §3.3）。一台没有目录的机器跑结构检查，会把「文件不在我这儿」写成损坏。缝出错同样不能关 finding：关需要一份完整观察，观察失败不是「问题消失了」。这和 ADR 0011 §3.5「不完整的观察既不开也不关」是同一条。
3. **三条检查各报各的种类，不合成一个 `VERIFY_FAILED`。** 结构失败是 `VERIFY_FAILED`，对象不在是 `I6_MISSING_OBJECT`，对象大小不符是 `I6_OBJECT_SIZE_MISMATCH`，对象字节不符是 `OBJECT_CORRUPT`。理由：处置不同。对象没了要查桶的生命周期；字节不符是静默损坏；本地解析失败是这段备份不能读。ADR 原文把对象「不符」笼统写成这三个名字里的 `VERIFY_FAILED`。阶段 2 草图已经单列了 `OBJECT_CORRUPT`。沿用草图的名字，避免一条 finding 盖住两种事故。`I6_SIZE_MISMATCH` 不拿来用：那个名字在代码里是「旧段的本地大小和目录行不一致」，和对象大小不是一件事。
4. **先做免费的，再按队列花预算。** 每一轮先 stat 每个本地封存文件、HEAD 每个已上传对象（都不计费），再把要读内容的工作（本地整文件解析、对象正文哈希）排成一个队列，按「强制 → 最近（改动之后还没验过的未上传行和 24 小时内上传，有份额上限） → 轮换槽 → 最久没验过 → 末档（只花剩下的）」付费。预算或截止时间用完，剩下的记 `SKIPPED`，这一轮正常结束；只有调用方取消才中止。理由：第 2 轮先把所有本地文件整解析一遍再碰对象，QA 用 64 KiB/s、450ms 截止连跑 25 轮，每轮都停在同一个本地文件上，对象一次 HEAD 都没有（生产里 100 GiB 本地段在 4 MiB/s 下要 7 小时）。HEAD 便宜，能抓住误删和截断，不该排在付费工作后面；付费工作排序之后，每一轮从上一轮停下的地方之后接着走（第 5.3 节有上界）。最近档用 `last_verified_at` 判断新鲜度：一段改动（封存、上传）之后按内容验过一次就离开这一档，所以它的消耗有界，并按份额计入第 5.3 节的上界（QA r3 F1）。窗口取 24 小时，和 `verify.every` 的默认对齐。
5. **轮换长度按周期数配置，不按百分比。** 每个段名有一个固定的槽：`sha256(段名)` 的前 8 字节对 `N` 取模，`N = RotationPeriods`（默认 20）。第 `d` 个周期（`floor(unix 秒 / 周期)`，UTC 对齐）槽号等于 `d mod N` 的段进入轮换槽这一档。最坏间隔就是 `N × 周期`（`WorstGap`），不取整：周期从 24h 改成 48h，最坏间隔就是 20 × 48h = 960h（QA r2 在切换那天按小时采样量到 961h，即 20 个新周期加上采样粒度）。这是预算够用时的上界；预算不够时见第 5.3 节。理由：第 2 轮用百分比换算成 `ceil(100/percent)`，51–99 都变成 50%、30 变成 25%（QA r2 F5）。直接配周期数就没有换算。`RotationPeriods` 为负时关掉轮换和「最久没验过」这一档，只做强制和最近的工作（测试和应急用）。
6. **字节预算是整个进程一份，不是每个任务一份。** 默认 4 MiB/s，和现有 `health.rehash_bytes_per_sec` 同一个数。理由：ADR 写的是「每任务每轮上限」。100 个任务各 4 MiB/s 就是 400 MiB/s，QA 要求的「限速下源延迟不升高」做不到。一个任务可以吃满整个预算，多个任务分享它。复核器今天已经是全局限速；接上主人之后，巡检和复核器用同一只桶，不加倍。在那之前复核器不动，避免在所有权改写的同时改评估器的读盘行为。
7. **没有可比的东西时，记成「无法验证」，不算验证过。** 目录行 `sha256` 为空、本地文件又不在（或这一轮没能干净地解析它）时，没有东西可以和对象正文比：不下载正文，对象记 `UNVERIFIABLE`，`Report.UnverifiableNoDigest` 计一次，不开 finding，`last_verified_at` 不前进，已开的 `OBJECT_CORRUPT` 也不会因此关闭。行上的 `size_bytes` 为 0（未知）时，HEAD 不能比大小：对象的 `SizeChecked` 为 false，关不掉 `I6_OBJECT_SIZE_MISMATCH`；只有正文哈希和本地文件相符才算比过大小，否则计入 `Report.UnverifiableSize`。另一个计数 `Report.NoDigestLocal` 是「行上没有摘要、但本地文件还在」的封存段数：这些段现在还能从本地文件补摘要，保留期删掉之后就不能了。理由：把「旧行没有摘要」报成损坏，会在升级后的第一轮把保留期删掉的历史段全部打成 CRITICAL；但也不能把它们算成验证过。补摘要的任务和对应的 gauge 留给 PR-2（第 15 节第 9 条）。新上传的段由第 11 节在上传时补上摘要。
8. **还原演练默认关，而且代码路径上没有源库 DSN。** 只有配置了 `verify.drill_dsn` 才允许拨号，拨的是那台专用实例。源库地址不传进演练客户端。工人实现时再加一条：演练地址的 host 和 port 与该任务源地址相同就拒绝拨号，开 WARNING `DRILL_REFUSED`，不连接。理由：ADR 决定 3 是默认关。用两个开关（enabled 和 DSN）会漂。空 DSN 就是关。拒绝相同地址是为了防止把源 DSN 粘进演练配置之后，演练往生产库回放。
9. **不把 `chain.objects` 标成已生产。** 阶段 2 的 `TestHealthIgnoresScopesNotProduced` 要求这两个 scope 在巡检真正跑起来之前不要让健康变成 UNKNOWN。理由：纯函数还没有被周期调用。先翻这个布尔值，评估器会认为「该有对象结论却没有」，任务变成 UNKNOWN，而巡检并没有跑。

## 4. 结构检查

对任务目录里每个**封存**段，且目录行里有同名的封存行，做一次完整解析。名字用磁盘上的段名（链用的那个名字）。源文件名和磁盘名不一致时（`.sealed.eN`），调用方传入磁盘名。没有目录行的本地文件不在这里报：那是链的 `I6_MISSING_ROW`，而 adopt 不会回填行，阶段 2 故意不派生它。打开中的尾段不查，它还在被写。

解析用 `binlog.ReadSealedSegmentContents`（封存读法：magic、事件长度、CRC32；GTID 事件被解出来）。然后和目录行比：

| 比什么 | 行里的字段 | 不一致时 |
|---|---|---|
| 字节数 | `size_bytes` | `VERIFY_FAILED`，证据 `local=<n> row=<m>` |
| 整文件 SHA-256 | `sha256`（空则跳过） | `VERIFY_FAILED`，证据两边的摘要 |
| 首尾位点 | `start_pos` / `end_pos`（0 表示未知，跳过） | `VERIFY_FAILED` |
| GTID 集合文本 | 调用方可选传入；目录**没有**这一列 | 传入了且不一致才报 `VERIFY_FAILED` |

解析失败（CRC、截断、坏的事件长度）本身就是 `VERIFY_FAILED`，不再继续比摘要。一段只开一条结构 finding，按上面的顺序取第一条。严重度 CRITICAL。subject 是段名。大小不符时不解析，本地副本记 `STAT`；其余情况整文件读过，记 `PARSED`。

本地文件不在时，看行：

| 行 | 本地副本记 | finding |
|---|---|---|
| `UPLOADED` | `ABSENT`：对象是剩下的那份 | 无（对象检查接着看） |
| 带保留期标记（`RetentionRemoved`） | `ABSENT` | 无 |
| 其它（封存了、没上传、本地也没有） | `MISSING` | CRITICAL `I6_MISSING_LOCAL`：这段字节已经没有任何一份。名字和链的 I6 相同，同一段两边都看到时合并成一条 |

任务目录列不出来时（目录不在这台机器上、权限错），每段的本地副本都是 `SKIPPED`，不开 `I6_MISSING_LOCAL`，链也不查。列得出来但某个文件 stat 失败（不是不存在）时，那一段记 `ERROR`，链不查；解析途中读失败只让那一段记 `ERROR`。

大小比对是 stat 的结果，不花预算，每轮每段都做。整文件解析是队列里的工作（第 5.2 节），没排上或排上了没付到的，本地副本分别记 `STAT`（原因「这一轮没排内容检查」）或 `SKIPPED`（原因写预算或截止时间）。位点和解析、SHA-256 来自同一次读：读到的字节同时喂给解析器、哈希和一个流式的位点跟踪器（结果和 `binlog.EventSpan` 一致，有单测对拍），所以设了位点的段也只付一次钱（QA r2 指出第 2 轮会付两次）。

## 5. 一轮怎么走

### 5.1 免费的部分

对象存储没配（调用方没有传入对象读取器）时，这一轮**不判定**对象，每个已上传段的对象记 `SKIPPED`，也不关闭已有的对象 finding。关掉「存储被拆掉」是阶段 2 已经写过的 CONFIG 事实，要由工人显式给出，不能从「这次没传读取器」推断。

不是 `UPLOADED`（或没有 `object_key`）的行，对象记 `NOT_APPLICABLE`。对每个 `upload_state = UPLOADED` 且 `object_key` 非空的封存行，按行序：

1. 先看时钟。`uploaded_at` 比 now 晚超过 1 小时（`ClockSkewTolerance`）→ WARNING `UPLOAD_CLOCK_SKEW`，这一行**不算**最近。1 小时以内的未来时间算最近（小的时钟差不能让新上传漏掉）。这一步不做 I/O，在碰对象之前就记下（`Coverage.ClockChecked`）。`sealed_at` 同样处理，而且对每个封存行都看（不论是否已上传）：超前 1 小时以上也报 `UPLOAD_CLOCK_SKEW`（detail 写明是 `sealed_at`），这个时间不算改动，不把行钉在最近档里（QA r4 G5：`sealed_at` +3 天的行每小时一轮，96 小时读了 73 次）。1 小时以内的未来时间在过去之前也不算改动：这时读了，`last_verified_at` 还是早于它，每一轮都会再读一遍（QA r5 P3：10 分钟一轮，6 小时读了 7 次）；时间一过就是一次改动，读一次。最近窗口仍按 `uploaded_at` 算。`UPLOAD_CLOCK_SKEW` 说的是时间戳，不是字节：它不挡 `last_verified_at`，也不算「被 finding 点名」（第 6.4 节）。
2. HEAD。对象不在 → `I6_MISSING_OBJECT`，记 `MISSING`。其它错误（超时、503、凭据错）→ 这个对象记 `ERROR`，`Report.ObjectErr` 留下第一个错误，接着 HEAD 下一个。HEAD 到了 → `Found`；行的大小已知时比大小（`SizeChecked`），不符 → `I6_OBJECT_SIZE_MISMATCH`，正文不读。
3. 没有摘要可比（决定 7）→ `UNVERIFIABLE`，不排正文哈希。

HEAD 不计费。全部 HEAD 完、全部本地 stat 完之后，调用方给了链而且目录列得出来、没有 stat 错误，就跑 `Chain.Check`（第 6 节）。截止时间在 HEAD 途中到了，剩下的对象记 `SKIPPED`，链不跑，付费部分全部记 `SKIPPED`。取消则立即返回错误，没走到的行在 `Coverage` 里没有条目。

`uploaded_at` 为空不算最近，否则没有时间戳的历史行会被全部下载。

### 5.2 付费的部分：一个队列

每个段按下面第一条符合的规则放进一档；同一段的本地解析和对象哈希在同一档，本地在前（对象优先和这一轮的本地摘要比）。

| 档 | 条件 |
|---|---|
| 1 强制 | 开着 `OBJECT_CORRUPT` 或 `VERIFY_FAILED`，或者行大小未知时开着 `I6_OBJECT_SIZE_MISMATCH`，**并且本地文件还在**（第 6.3 节） |
| 2 最近 | 自上一次改动之后还没按内容验过（`last_verified_at` 为空，或早于 `max(uploaded_at, sealed_at)`），并且还没上传（本地是唯一一份）或 24 小时内上传；被开着的 finding 点名的段不进这一档。每轮最多花 `C` 的 `verify.recent_share_percent`（默认 25%），超出的那一段落到它在下面两档的位置（QA r3 F1） |
| 3 轮换槽 | 这一周期是它的槽（决定 5），且这一周期开始之后还没验过（每周期最多一次；排序时间见下） |
| 4 最久没验过 | 排序时间为空（从没验过，或最后一次改动之后没验过），或者距今不短于 `N × 周期`；被开着的 finding 点名的段不进这一档（它不会前进，放进来会一直占着队头） |
| 5 末档 | 只在轮换关掉时有：自最后一次改动之后还没按内容验过、又不在最近档的段（最近档超出份额的工作，上传已经超出最近窗口的段，不论从没验过还是上传之前验过；QA r5 P3）。轮换开着时这些段按从没验过进第 4 档。只花前面几档付完之后剩下的预算（QA r4 G1） |
| — | 都不是：本地只 stat（`STAT`），对象只 HEAD（`HEAD`） |

档内按排序时间从旧到新（空的最旧），再按段名。排序时间是 `last_verified_at`，但它早于这一段最后一次改动（`max(sealed_at, uploaded_at)`，未来的时间不算，第 5.1 节）时按空算：上传之前在本地验过、上传之后对象还没读过的积压，和从没验过的段排在一起（QA r5 P2-1）。`Options.LastVerified` 是调用方传进来的每段 `last_verified_at`；工人把每轮的 `VerifiedAt` 写回去，下一轮再传进来。

最近档看新鲜度，不只看状态：未上传的行（`PENDING`、`UPLOAD_FAILED`）解析过一次就离开这一档，之后和别的段一样按轮换槽和 `last_verified_at` 排；上传是一次改动，上传之后对象再按最近档读一次。第 3 轮的最近档只看「没上传或 24 小时内上传」，不看 `last_verified_at`：桶故障积压 `UPLOAD_FAILED` 时，这些行每轮都排在轮换槽和最久档前面，QA r3 量到 10 个旧对象 200 个周期一次都没哈希过。`VerifyRow.SealedAt` 是行在上传前的最后一次改动；为零时只看 `uploaded_at`。

最近档有份额上限：一份最近档的工作开始前，若这一轮最近档已付的字节加上它超过 `⌊C × 份额⌋`（`C` 在付费部分开始时按第 5.3 节算：`PassBytes` 与「令牌桶余额 + 速率 × 剩余截止时间」中较小的；两者都不限时不设上限），这一段剩下的最近档工作（本地和对象一起，这样对象哈希不会跑到它要用的本地解析前面）落到它的下一档：轮换槽、最久没验过，或者都不是就进末档。所以一次大积压（桶恢复后一小时上传了几千段）不会把一整天的预算都吃掉，而这一轮剩下的预算也不会空着。第 4 轮这里记 `SKIPPED`：QA r4 量到桶恢复后 30 段一起上传、`C = 630560` 时第一天只花了 55%，最晚的一段第 20 天才第一次哈希。现在第一天付满 `C`（20 段），第二天付完剩下的（`TestVerify_BucketRecoveryHashesTheBacklog`）。第二天那 10 段已经出了 24 小时窗口；它们上传之后还没验过，排序时间为空，排在第 4 档队头（轮换关掉时进末档）。

`R` 还要给后两档留出一段：`R = min(⌊C × 份额⌋, max(0, C − F − S))`（`F`、`S` 见第 5.3 节，在付费开始时从队列算出）。份额配置成 100% 时最近档也拿不走整轮（QA r4 G2）。`R` 小于最近档里最小的一份副本时，最近档这一轮等于关闭，那些工作全部落到下一档或末档。

按顺序付费。一份工作开始之前先问「放得下吗」：`PassBytes` 剩下的够不够，以及按令牌桶现在的余额和速率，截止时间之前付不付得完。放不下就记 `SKIPPED`（原因「放不下这一轮剩下的预算」），`Report.Deferred` 加一，**接着试下一份更小的**，不让一个大文件堵住后面的。截止时间到了，剩下的全部 `SKIPPED`（原因「截止时间到了」），这一轮正常返回。读的途中等不到预算（`ErrBudgetDeadline`，例如另一轮并发拿走了令牌）也只是这一份 `SKIPPED`。只有上下文被取消才返回错误。

本地解析：整文件读一次，解析、SHA-256、位点一起出（第 4 节）。对象哈希：摘要用这一轮干净解析出的本地摘要，没有就用行里的 `sha256`；两者都没有而本地解析被推迟了，对象也推迟（记 `SKIPPED`，不算无法验证）。GET、流式算 SHA-256、计费，记 `HASHED`，`Digest` 写明比的是 `local` 还是 `catalog`。不符 → `OBJECT_CORRUPT`，证据同样写明来源。GET 时或读的途中对象消失，按缺失报；其它读错误记 `ERROR`。正文以干净的 `io.EOF` 结束、却比 HEAD 的大小短，是读到一半断了，按 `io.ErrUnexpectedEOF` 记 `ERROR`，不报 `OBJECT_CORRUPT`（QA r4 G4）。

本地文件在、却读不了（stat、打开或读的途中出错，不是「不存在」、截止时间或取消）→ 本地记 `ERROR`，并报 `LOCAL_UNREADABLE`。第一次 WARNING；未上传的行（这个文件是唯一的一份）连续第二次失败升为 CRITICAL，之后一直 CRITICAL 到关闭。关闭要连续两次干净读：第一次干净读把它以 detail `DetailUnreadableRecovering` 再报一次、保持开着，第二次才关。所以隔一轮出一次错，既不升级，也不每轮开关（QA r5 P2-2）。状态只在 finding 的 severity 和 detail 里：工人用现有的 `task_findings` upsert 写回（这两列每次都覆盖），下一轮原样放进 `Options.OpenFindings`（第 10 节）。一个任务里每个 stat 都以同样的错误失败（目录不能搜索、盘掉了），只报一条 subject 为空的任务级 `LOCAL_UNREADABLE`（evidence 写文件数和错误），它覆盖并关掉逐文件的那些；升级和关闭规则相同，「干净」是有本地文件 stat 成功。第 4 轮这里只有 `ERROR`、没有结论，QA r4 G3 用 chmod 000 连跑 30 天，未上传段的唯一副本坏了也完全静默。读的途中出错不算解析失败，不报 `VERIFY_FAILED`；`budgetReader` 记下这个错误，后面的读不会接着把剩下的字节当成一份完整的文件。

### 5.3 进度上界

QA r2 要求：同一个短截止时间反复跑很多轮，必须有进展，不能每轮停在同一处。QA r3 指出第 3 轮的上界漏了最近档（未上传的行永远在最近档）。第 4 轮把最近档的消耗按份额上限计入。第 5 轮把份额再封顶在 `C − F − S`，`C − F ≥ S` 时 `q ≥ 1`（QA r4 G2）。记号：

- `C`：一轮最多能付的字节，`PassBytes` 与「突发 + 速率 × 截止时间」中较小的；
- `F`：强制段每轮的内容字节；
- `S`：非强制段里放得进 `C − F` 的，单段内容字节（本地 + 对象）的最大值；
- `R = min(⌊C × verify.recent_share_percent / 100⌋, max(0, C − F − S))`：最近档每轮最多花的字节（默认 25%）；
- `q = ⌊(C − F − R) / S⌋`：每轮至少完成的轮换槽、最久档段数。`C − F ≥ S` 时 `R ≤ C − F − S`，所以 `q ≥ 1`；
- `k`：会进队列的非强制段数（比 `C − F − R` 还大的段排除，它们每轮记 `SKIPPED`、`Deferred` 计数，要调大预算或截止时间）；`N`：轮换长度。

上界：若 `q ≥ 1` 且 `q × N > k − 1`，令 `P` 为满足 `q·t ≥ (k − 1)·(1 + ⌈t/N⌉) + 1` 的最小 `t`。则每段两次内容检查之间最多隔 `N + P` 个周期，从没查过的段在前 `P` 轮内第一次查到。预算不限时就是 `N` 个周期（`N × 周期`，决定 5）。

推导：

1. 每轮的付费顺序是强制档（最多 `F`）、最近档（份额上限保证最多 `R`）、然后轮换槽和最久档，最后才是末档。所以后两档每轮至少有 `C − F − R`。前 `q` 段每段不超过 `S`，一定付得完，所以每轮至少完成后两档队头的 `q` 段。末档排在它们之后，只花剩下的，不影响这个计数。
2. 最近档的工作是一次性的：一段只在「改动之后还没按内容验过」时在这一档，验过就离开。超过份额的那一段落进轮换槽或最久档，按它的排序时间排（最后一次改动之后没验过时为空），它本身就是 `k` 个段之一，下面的计数已经包含它。
3. 一段上次验过之后，最晚 `N` 个周期变成「最久没验过」。排在它前面的只有：比它更旧的段（最多 `k − 1` 个，验过之后时间更新，排到它后面，不会再插回来），和这一轮轮到槽的段（每个别的段在任意 `t` 轮里最多 `⌈t/N⌉` 次；槽每周期最多读一次）。`t` 轮一共完成 `q·t` 段，超过排在它前面的总数，就轮到它了。
4. 新段：上面的 `k` 是固定的一组段。每轮新来 `a` 段，份额内的由最近档付；份额外的落进最久档，排序时间为空、排在最前，要加进右边（`+ a·t`）。新段持续快于 `q` 时上界不成立。
5. 过载时（`q × N ≤ k − 1`，上面的上界不成立）还有一条：最后一次改动之后没验过的段（新段、上传之前验过的积压）排序时间为空，在第 3 档自己的槽里也排在最前。第 3 档每轮至少完成 `q` 段，所以同一个槽里这种段不超过 `q` 个时，每一段在自己的槽那个周期内（最晚 `N` 个周期）第一次查到，不论过期的段有多少。同一个槽里这种段超过 `q` 个时，多出来的要等大约 ⌈m/q⌉·N 个周期（m 是这个槽里排序时间为空的段数）：槽每 N 个周期才轮到一次，每次只付得完 q 段。q=1 的过载探针里，同一槽的第 3、第 4 段到第 25 天才第一次哈希，晚了一整个轮换。QA r5 P2-1：`C = 6S`、120 和 400 段过期、30 段积压（一段坏）时，r5 上坏的那段第 23 天才查到、400 段时 30 段里 29 段 40 天没哈希；现在两种情况都到第 19 天全部哈希完，坏的那段第 3 天查到（`TestVerify_StaleBacklogSortsAsNeverVerified` 断言 `N` 个周期之内）。

前提：

- 调用方把每轮的 `VerifiedAt` 写回，下一轮作为 `Options.LastVerified` 传进来（第 6.4 节）。不写回时每段一直是「从没查过」，第 4 档每轮按段名排、停在同样的前几段，名字靠后的段只在自己的槽那天查到：仍然有进展，但上界退回到 `N` 个周期，不是 `P` 轮。
- `q ≥ 1`，即 `C − F ≥ S`。**强制集合没有上限**：每条开着、本地还在的 `OBJECT_CORRUPT` / `VERIFY_FAILED` / `LOCAL_UNREADABLE` 每个周期都把整段读一遍（第 6.3 节），`F` 随开着的 finding 数增长。`F > C − S` 时 `q = 0`，后两档一段也付不到，上界不成立，`Deferred` 会一直不为零。这时要处理掉开着的 finding（重新上传、修复本地），或者调大预算。`F` 按「一轮」算：调用方把 `Report.Read` 写回时，一个周期里强制段只在第一轮计入 `F`。

用到的数：

| 场景 | C | F | R | S | q | k | N | P | 上界 | 实测 |
|---|---|---|---|---|---|---|---|---|---|---|
| QA r2 的 64 KiB/s、450ms、25 轮，一段开着 `OBJECT_CORRUPT`（`TestVerify_StarvationUnderAFixedDeadline`；`C` 按截止时间扣 150ms 余量） | 85196 | 31528 | 21299 | 31528 | 1 | 3 | 20 | 5 | 第一次检查在前 5 轮内 | 前 2 轮全部查到；强制段每轮都查 |
| QA r3 的 pending_local_hog_3：3 段未上传的本地段 + 10 段只剩对象，`PassBytes = 47292`，200 轮，写回（`TestVerify_PendingBacklogDoesNotStarveTheRotation`） | 47292 | 0 | 11823 | 15764 | 2 | 13 | 20 | 13 | 前 13 轮首查，间隔 ≤ 33 | 见第 14 节 |
| QA r4 G2：`C = S`（`TestVerify_RecentShareLeavesRoomForOneSegment`） | 1000 | 0 | 0 | 1000 | 1 | — | — | — | 最近档关闭，后两档每轮一段 | 最久的那段付到 |
| 4 段本地 + 20 段只剩对象 + 1 段 300 KiB 的超大对象，`PassBytes = 94584`，120 轮（`TestVerify_QueueBoundsTheGap`） | 94584 | 0 | 23646 | 31528 | 2 | 24 | 20 | 35 | 55 个周期 | 见第 14 节；超大对象每轮 `SKIPPED` |
| 同上，不限 | — | — | — | — | — | — | 20 | — | 20 个周期 | 20 |

`R` 是上限，不是每轮都花掉：最近档空着的那一轮，后两档实际拿到的是 `C − F`，上界是按最坏情况算的。前三行的 `R` 在第 4、5 轮相同（`⌊C × 25%⌋` 都不超过 `C − F − S`）；第 4 轮里 `C = S`（QA r3 的 base20obj_q1）和 forced_near_C 这类配置 `q = 0`、上界不成立，第 5 轮它们是 `q = 1`。

生产的量级：4 MiB/s、截止时间一天时 `C` ≈ 337 GiB，`R` ≈ 84 GiB；1 GiB 的段 `S` = 2 GiB，`q` ≈ 126，`q × N` ≈ 2520 段，即大约 2.5 TiB 的段能在 `N + P` 内轮完。数据更多时上界不成立（每天轮到槽的量本身就超过一轮的预算），`Deferred` 会一直不为零：要调大预算或轮换长度。

## 6. 链检查、覆盖记录，以及消失的结论

调用方把已经建好的 `*Chain` 交进来时，在免费部分之后、付费部分之前跑 `Chain.Check(now)`，每条 `Violation` 变成一条 finding：kind 用违反的名字（`I1_REGRESSION` 等），subject 是第一个段，`I4_EMPTY_LEFTOVER` 是 INFO，其余 CRITICAL。没传入链，`ChainChecked` 为 false。任务目录列不出来、任何一个本地文件 stat 失败、或者截止时间已过，也不跑链：链的结论描述的是本地字节，用调用方手里那份可能过期的链去关 finding 不对。放在付费部分之前，是为了短截止时间下链也每轮都跑。

### 6.1 覆盖记录

第 1 轮的报告只有三个粗粒度的「scope 完整」标志。QA 找到的三个问题（没抽中的那天把 `OBJECT_CORRUPT` 关掉、ctx 取消后的报告看起来完整、只 HEAD 就推进 `last_verified_at`）是同一个原因：报告声称「这一块查完了」，而不是记下它到底查了什么。从第 2 轮起按段记：

```text
Report.Coverage[段名] = {
  Local:  { Method, Reason, Found, SizeChecked }          // PARSED | STAT | ABSENT | MISSING | ERROR | SKIPPED
  Object: { Method, Reason, Digest, Found, SizeChecked }  // HASHED | HEAD | UNVERIFIABLE | MISSING | NOT_APPLICABLE | ERROR | SKIPPED
  ClockChecked                                            // sealed_at（已上传时还有 uploaded_at）和 now 比过
}
```

| Method | 意思 |
|---|---|
| `PARSED` | 本地文件从头读到尾（magic、事件长度、CRC32、GTID 事件、位点），算了 SHA-256，和行比过 |
| `STAT` | 本地文件在，大小比过，没解析（大小不符，或这一轮没排它） |
| `ABSENT` | 没有本地文件，也不需要有（已上传，或保留期删的） |
| `MISSING` | 本该在的副本不在（本地：封存未上传；对象：HEAD/GET 404） |
| `HASHED` | 对象正文读过，SHA-256 和一个摘要比过 |
| `HEAD` | 对象在，正文没读 |
| `UNVERIFIABLE` | 对象在，但没有摘要可比 |
| `NOT_APPLICABLE` | 行不是 UPLOADED，没有对象可查 |
| `ERROR` | 读失败，不是 404 |
| `SKIPPED` | 没去看，或者排上了但没付到（预算、截止时间）；`Reason` 写原因 |
| 零值 | 这一轮没走到（取消） |

`Found` 是这一轮亲眼看到这份副本存在（stat 或 HEAD 回答了）；`SizeChecked` 是这一轮比过它的大小（行大小已知时 stat/HEAD 比过，或行大小未知时正文哈希和本地文件相符）。这两个是事实，推迟或读错不会抹掉：HEAD 到了、后来 GET 超时，对象仍是 `Found`。

条目只在一份副本的检查结束之后才写。取消时 `Verify` 立刻返回错误，已经写进去的保持原样，没走到的就是零值。截止时间和预算不是提前返回：排上了没付到的副本明确记 `SKIPPED`。关闭和 `last_verified_at` 都只读 `Coverage`，零值和 `SKIPPED` 什么内容类结论也关不掉、什么也推不动。

### 6.2 关闭

`Closures(上一轮的 finding, 这一轮的报告)` 返回要关的 finding 和原因；`Disappeared` 是只要 finding 的那一份；`Verify` 用 `Options.OpenFindings` 算好放在 `Report.Closed`。上一轮有、这一轮没有再报，并且下面一条成立，才关：

| kind | 这一轮怎样才能关 | 原因 |
|---|---|---|
| 按行的 kind：`VERIFY_FAILED`、`OBJECT_CORRUPT`、`UPLOAD_CLOCK_SKEW`、`LOCAL_UNREADABLE` 和 `I6_*`（`I6_MISSING_ROW` 除外） | `Options.CatalogComplete` 为真（调用方读到的是这个任务完整的目录），而行里没有这个 subject（任何状态的行都算还在） | `row removed` |
| `VERIFY_FAILED`（sha 不符那一种） | 本地副本 `ABSENT`（保留期删了），对象 `HASHED`、比的是目录行摘要且相符、没有报 `OBJECT_CORRUPT`，并且对象流边哈希边解析、位点和 GTID 集合都和行相符（`Parsed`） | `local removed; object matches catalog` |
| `VERIFY_FAILED`（解析失败、位点不符、GTID 不符、大小不符） | 同上，`Parsed` 必须为真 | `local removed; object parses and matches the catalog row` |
| `VERIFY_FAILED` | 本地副本 `PARSED` 且和行相符 | `local parsed and matches the catalog row` |
| `I6_MISSING_LOCAL` | 本地 `Found`（文件回来了）；或链这一轮查过。`ABSENT` 不关（保留期标记不代表文件在） | `local file present` / `chain checked` |
| `I6_MISSING_OBJECT` | 对象 `Found`；subject 不在这一轮的行里时，链查过 | `object found by HEAD` / `chain checked` |
| `I6_OBJECT_SIZE_MISMATCH` | 对象 `SizeChecked`；subject 不在这一轮的行里时，链查过。行大小为 0 时 HEAD 不算 | `object size compared` / `chain checked` |
| `OBJECT_CORRUPT` | 对象 `HASHED` 且相符 | `object hashed and matches` |
| `UPLOAD_CLOCK_SKEW` | `ClockChecked`（每个封存行都比；`sealed_at`、`uploaded_at` 都不再超前） | `uploaded_at compared with now` |
| `LOCAL_UNREADABLE` | 连续第二次本地 `PARSED`（读到了尾，不论是否相符；第一次只再报成 recovering）；或本地 `ABSENT` / `MISSING`（文件不在了，缺失由 `I6_MISSING_LOCAL` 接手）；或文件又能 stat、大小与目录行不符（`VERIFY_FAILED` 接手，QA r5 P3）；或这一轮报了任务级的那一条。任务级（subject 为空）：拆成逐文件时用「split into per-file findings」关（逐文件继承任务级的 severity 和 detail，不把这一条再报成 recovering）；整目录恢复才是连续第二轮每个本地文件都能 stat 且没有 `ERROR`，原因仍是 `local files stat again`。不按「row removed」关。逐文件收成任务级时，只要还有一条未上传的逐文件是 CRITICAL、或 detail 还不是 recovering，任务级从 CRITICAL 起；正在 recovering 的 WARNING 合并后仍是 WARNING | `local file read to the end` / `local file no longer present` / `local file readable again; its size differs…` / `covered by the task-level LOCAL_UNREADABLE` / `local files stat again` / `split into per-file findings` |
| 其它（链的违反名） | `ChainChecked` | `chain checked` |

链的 kind（`I1_*` 到 `I5_*`、`I7_*`）不按「行已删除」关：删掉一行补不上 GTID 空洞，也不会让区间不再重叠，只有同一轮链检查过、不再报它才关（QA r3 F3）。`I6_MISSING_ROW` 说的正是「本地有文件、目录没有行」，行不在是它成立的条件，所以只有链能关它。

本地已删时，「对象和目录行摘要相符」只说明对象就是当初封存的那份字节，不说明那份字节是好的（QA r3 F2：封存摘要按写下来的文件算，解析失败的段，目录摘要就是坏字节的摘要）。所以开着 `VERIFY_FAILED`、本地 `ABSENT`、要按目录摘要哈希对象时，同一份字节同时喂给解析器和位点跟踪（`readSegment`，不多花预算）。哈希相符而解析失败、位点或 GTID 不符，就再报一条 `VERIFY_FAILED`（CRITICAL），detail 前缀 `object (sole copy): `，表示缺陷在剩下的唯一一份上，finding 保持开着、继续点名该段。finding 的种类由 detail 区分：`DetailSHA` 等常量（`VERIFY_FAILED` 的 detail 从这一版起是稳定的）。只有 sha 不符那一种用「local removed; object matches catalog」关；detail 不认识的旧 finding 按非 sha 处理，要求对象能解析。工人按「行已删除」关闭时记一条事件，操作员删掉一个缺失对象的行是值得留痕的事（QA r2 F3）。行从一份**不完整**的目录里消失，什么也不关。`OBJECT_CORRUPT` 在行变成非 UPLOADED 时也不关：重新上传成功、行回到 UPLOADED 之后，强制重查会把它哈希一遍再关。

### 6.3 强制重查

`Options.OpenFindings` 是上一轮还开着的 finding。subject 上开着 `OBJECT_CORRUPT`、`VERIFY_FAILED` 或 `LOCAL_UNREADABLE`（或者行大小未知时开着 `I6_OBJECT_SIZE_MISMATCH`）的段，**只要本地文件还在**，进强制档：本地重新解析，对象重新哈希，直到某一次重查关掉它。

强制段每个周期最多读一次（QA r4 G6：每小时一轮时一天读 24 遍，`F` 按轮数放大）。每轮把这一轮读完的段记进 `Report.Read`（`ReadMark`：时间、本地大小、本地 mtime、对象 HEAD 大小、行的 `uploaded_at`；排上的每份副本都读出了结论、至少一份按内容读过才记）。调用方把它写回、下一轮作为 `Options.LastRead` 传进来。强制段在这一周期开始之后读过、而本地大小、本地 mtime 和对象大小都没变，这一轮就不读（本地 `STAT`、对象 `HEAD`，原因「forced recheck already done this period and nothing changed since」），finding 保持开着，不关也不前进。任何一个变了（重新上传、修好本地文件）就照常读。同样大小的重新上传 HEAD 看不出来，但 `uploaded_at` 变了，也照常读（QA r5 P3：r5 上要等到下一个周期，最长 23 小时）。开着 `LOCAL_UNREADABLE` 的段不按这条跳过：读就是它的检查，关闭要第二次干净读。不写回 `Report.Read` 时和第 4 轮一样每轮都读。

本地文件已经不在（保留期删了）时不强制：这台机器上没有任何东西能修它，每天下载一遍只是浪费（QA r2 F2 量到 30 轮 30 次 GET）。它按轮换槽每 `N` 个周期重查一次（最近窗口内也会查）。`VERIFY_FAILED` 这时由第 6.2 节的「本地已删、对象和目录行相符」关闭；对象和目录行不符，就同时开着 `OBJECT_CORRUPT`，两条都每个轮换周期重查一次，不再每轮下载。这是对第 2 轮「强制每轮」的一处收窄：本地还在、能用本地副本重新上传的段仍是每轮。

没有摘要可比的强制段仍是 `UNVERIFIABLE`，不下载，finding 保持开着，等操作员处理。工人必须把开着的 finding 传进来；忘了传也不会错关（只 HEAD 关不掉 `OBJECT_CORRUPT`），只是要等到它的轮换槽才会重查。目录行已删除的 subject 没有行，自然不再被强制。

### 6.4 `last_verified_at`

只给这一轮按内容验过、而且没有任何 finding 点名的段（`UPLOAD_CLOCK_SKEW` 不算点名：它说的是时间戳，内容验过就前进，QA r4 G5）：本地副本 `PARSED`（或者不需要本地副本，`ABSENT`），对象 `HASHED`（或者没有对象，`NOT_APPLICABLE`），并且两者里至少一个真的读过内容。「点名」包括这一轮报出的 finding，和 `OpenFindings` 里这一轮没有关掉的 finding（QA r2 F2：开着的 `VERIFY_FAILED` 不能因为对象哈希相符就让它前进）。只 HEAD、`STAT`、`UNVERIFIABLE`、`SKIPPED`、`ERROR`、没走到，都不前进。

同一段同一 kind 只留一条。对象检查和 `Chain.Check` 如果都看到同一个缺失对象，合并成一条。工人落库时，对象类 I6 要写成健康注册表已经在用的 subject：段名加 `#object`（`health.ObjectSubject`）。纯函数返回的 subject 是段名，因为链的违反用的是段名；`#object` 是健康层的键，不是巡检层的键。这个映射留给工人，避免这一份去改健康注册表。

## 7. 字节预算

`NewByteBudget(bytesPerSec)`。`<= 0` 表示不限速（测试和「显式不限」）。限速用已经在用的 `golang.org/x/time/rate` 令牌桶，突发等于速率，但不超过 64 KiB，这样大段不会一次借走一整秒以上的额度。每次读最多读一个突发那么多字节，读到之后先付钱再用；付不成（截止时间之前等不到）这些字节就扔掉，不计入这一轮，那一份工作记 `SKIPPED`。所以 `Report.Bytes` 恰好是这一轮付给预算的字节（QA r2 F6：第 2 轮中止的那一轮多算了读了没付的一块）。

计入预算的是：本地整文件解析读到的字节（位点不再单独读；解析失败也把剩下的读完，摘要覆盖整份文件，花的仍是事先估算的文件大小）、对象 GET 的正文（需要时边哈希边解析，同一份字节，不另计）。stat 和 HEAD 不计入，它们不搬段数据。

一份工作开始之前先估算放不放得下（第 5.2 节）：令牌桶现在的余额加上到截止时间为止能补的量，以及 `Options.PassBytes`（这一轮最多读多少，0 不设上限）。估算放得下、读的途中被另一轮并发拿走令牌而付不成，那一份 `SKIPPED`，这一轮接着试别的。

预算被多轮并发共用：累计值是原子计数（`ByteBudget.Charged`），令牌桶本身是并发安全的。`Report.Bytes`（以及 `LocalBytes`、`ObjectBytes`）是**这一轮**自己付的字节，不是预算的累计值；两轮并发共用一只桶时各自报自己的数，加起来等于累计值。

默认 4 MiB/s 是进程级上限（决定 6）。配置键是 `verify.budget_bytes_per_sec`。接上工人之前，复核器仍用它自己的 `health.rehash_bytes_per_sec`，两只桶互不影响。工人落地时合成一只，并让复核器跳过本轮巡检已经哈希过的段。现在不合成：复核器挂在评估器上，评估器的所有权判断正是 ADR 0011 要换掉的那一层。

## 8. Finding 与指标

这一份只**返回** finding，不注册进 `health.Registry`，也不打指标。名字先定死，工人按这个名字注册（ADR 0009 §3 的表与这里一致）：

| kind | 严重度 | 何时开 | 何时关（第 6.2 节） | 处置 |
|---|---|---|---|---|
| `VERIFY_FAILED` | CRITICAL | 封存段解析失败，或和目录行的大小、SHA-256、位点、可选 GTID 文本不一致 | 重新整文件解析并和行相符；或本地已被保留期删掉、对象和目录行摘要相符；或行已删除 | 本地字节或目录行有一边错了：要人查，先别动对象。本地还在时每轮强制解析并哈希对象；本地不在时每个轮换周期一次 |
| `OBJECT_CORRUPT` | CRITICAL | 哈希过的对象 SHA-256 与本地或目录行不同 | 下一次哈希它并且相符；或行已删除 | 桶里的字节变了：用本地副本重新上传。本地还在时每轮强制重查；本地不在时没有副本可传，每个轮换周期重查一次，要人从别处恢复 |
| `I6_MISSING_OBJECT` | CRITICAL | UPLOADED 段 HEAD/GET 不在 | HEAD 到了；或行已删除（记事件） | 查桶的生命周期和删除记录；本地还在就重新上传 |
| `I6_OBJECT_SIZE_MISMATCH` | CRITICAL | HEAD 的大小和 `size_bytes` 不同 | 这一轮比过大小且相符（行大小为 0 时只有和本地文件相符的哈希算比过）；或行已删除 | 对象被截断或换掉（或目录行错了）：对照本地后重新上传 |
| `I6_MISSING_LOCAL` | CRITICAL | 封存、没上传、本地文件也不在 | 文件回来了，或链不再报，或行已删除 | 这段字节已经没有任何一份；从源库重拉这一段，或接受这个缺口 |
| `UPLOAD_CLOCK_SKEW` | WARNING | `uploaded_at` 或 `sealed_at` 比 now 晚超过 1 小时 | 两者都不再超前；或行已删除 | 修写入方或这台机器的时钟；这一行按旧对象轮换，不会每轮整个下载；不挡 `last_verified_at` |
| `LOCAL_UNREADABLE` | WARNING；行未上传时连续第二次失败升为 CRITICAL，到关闭为止 | 本地文件在，stat、打开或读的途中出错（不是不存在、截止时间或取消） | 连续两次读到了尾；或本地文件不在了；或大小不符由 `VERIFY_FAILED` 接手；或行已删除。每个 stat 都同样失败时只报一条任务级的（subject 为空） | 查磁盘、权限和内核日志。未上传时这是唯一一份，先想办法把它上传或从源库重拉。本地还在时每个周期强制重读一次 |
| 无法验证（不是 finding） | — | 没有摘要可比：`Report.UnverifiableNoDigest`；大小无从比：`Report.UnverifiableSize` | — | 本地还在时补摘要（`Report.NoDigestLocal` 是还能补的数）；补不了就只靠 HEAD |
| 推迟（不是 finding） | — | 排上了没付到：`Report.Deferred` | — | 长期不为零说明预算或截止时间不够轮完（第 5.3 节）：调大预算、截止时间或轮换长度 |
| `DRILL_FAILED` | CRITICAL | 演练回放后的 `gtid_executed` 与计划不符，或校验 SQL 失败 | 下一次演练通过。演练没开时不关、也不开 | 查还原计划和演练实例 |
| `DRILL_REFUSED` | WARNING | 演练 DSN 的 host:port 等于任务源地址 | 两者不再相同，或 DSN 被清空 | 改演练 DSN |

链上的 kind 不新造名字，用 `Chain.Check` 已有的封闭枚举。它们的开关规则仍是阶段 2 注册表里的那一份；巡检只是把 `Check` 的结果交出去。

指标（工人落地时加，名字和 ADR §2.2 对齐）：

| 名字 | 类型 | 含义 |
|---|---|---|
| `binlog_server_task_last_verified_timestamp{task_id}` | gauge | 最近一次没有被取消、没有任何副本是 `ERROR`、`Deferred` 为 0 的巡检的 Unix 时间。有 finding 也更新 |
| `binlog_server_verify_bytes_total{task_id,source}` | counter | `source` 为 `local` 或 `object` |
| `binlog_server_verify_pass_incomplete{task_id,scope}` | gauge | 0/1。scope 为 `local`、`object`、`chain`；从 `Coverage` 推出（有副本是 `ERROR`、`SKIPPED` 或零值），链看 `ChainChecked` |
| `binlog_server_verify_objects_unverifiable{task_id}` | gauge | `Report.UnverifiableNoDigest` |
| `binlog_server_verify_digest_backfill_pending{task_id}` | gauge | `Report.NoDigestLocal`：本地还在、能补摘要的段数 |
| `binlog_server_verify_objects_size_unverifiable{task_id}` | gauge | `Report.UnverifiableSize` |
| `binlog_server_verify_deferred{task_id}` | gauge | `Report.Deferred`：这一轮排上了没付到的副本数 |
| `binlog_server_restore_drill_last_success_timestamp{task_id}` | gauge | 最近一次演练通过的时间。没开时不输出，避免「没有序列」被看成失败或成功 |
| `binlog_server_finding_open` / `binlog_server_findings_reported_total` | 已有 | 工人把上面的 kind 交进现有 finding 出口之后，这两个序列自然带上 |

`verify_age` 用 `last_verified_timestamp`。ADR 的健康定义是 `verify_age <= verify_every × 2`。工人接上之前，评估器不读这个时间，健康定义不变。

## 9. 配置

键在 `internal/config` 的加载器里，读者是 `internal/app` 的 `startVerifier`。负数在加载时拒绝启动。`verify.recent_share_percent` 为 0 或 1–100 以外时拒绝启动：加载器调用 `assurance.CheckRecentSharePercent`，并额外拒绝 0。`Verify` 自己把 0 当默认，只拒绝 1–100 以外的非零值。`verify.budget_bytes_per_sec` 显式 0 被接受，工人把它当成下面的默认 4 MiB/s，不是不限速。`verify.pass_deadline_sec` 为 0 时工人用 `verify.every_sec`。配了对象存储时，启动对桶做一次 HEAD：桶在算通；鉴权、网络、桶不在只打 WARN，写在 `/api/health` 的 `detail`，不改变 `status`，也不拒绝启动（对象存储可以暂时不可用，本地捕获照常跑）。探针只做一次，detail 留到进程退出。对象 HEAD 分不清「桶不在」和「key 不在」（空的 404 会被报成 NoSuchKey），所以探针打在桶上。默认值如下。

| 键 | 默认 | 理由 |
|---|---|---|
| `verify.every_sec` | 86400 | ADR 的 24 小时 |
| `verify.budget_bytes_per_sec` | 4194304 | 第 7 节。0 在工人里表示用这个默认，不表示不限速；测试里的「不限」是直接传空预算，不是配置 |
| `verify.rotation_periods` | 20 | 轮换长度，按周期数（决定 5）。最坏间隔 = 这个数 × `verify.every_sec`，不取整。同尺寸的对象损坏（`OBJECT_CORRUPT`）要等该段的轮换槽才会被哈希，最坏检测延迟就是这个乘积 |
| `verify.recent_share_percent` | 25 | 最近档每轮最多花这一轮容量 `C` 的百分之几（第 5.2、5.3 节），再封顶在 `C − F − S`。加载时 1–100 以外的值（包括 0）拒绝启动，用 `assurance.CheckRecentSharePercent` 加上「不能为 0」；`Verify` 自己对 1–100 以外的非零值返回错误，0 当默认 |
| `verify.pass_deadline_sec` | 等于 `verify.every_sec` | 一轮的截止时间，到了剩下的记 `SKIPPED`、正常结束（第 5.2 节）。取消只在进程退出或失去主人时发生 |
| `verify.pass_max_bytes` | 0 | 一轮最多读多少字节（`Options.PassBytes`），0 不设上限 |
| `verify.object_recent_sec` | 86400 | 第 5.2 节的「最近」窗口 |
| `verify.drill_dsn` | 空 | 空就是关。不设 `verify.drill_enabled`，两个开关会漂 |
| `verify.drill_every_sec` | 604800 | 7 天 |
| `verify.drill_check_sql` | 空 | 操作员可选的校验 SQL。空则只比 `gtid_executed` |

演练 DSN 按源密码的同一套方式保护（不明文进日志）。这一份不读它。

## 10. 所有权接缝

工人（`internal/tasks/verifier.go`）每个 `verify.every` 对任务做：

```text
ok, err := owner.Allows(ctx, taskID)
if err != nil || !ok {
    跳过
    不开 finding，也不关
    不更新 last_verified_at
}
opt.LastVerified = 存下的 last_verified_at
opt.LastRead = prev.Read          // 上一轮的 Report.Read，按段合并后存下
opt.OpenFindings = 这个任务开着的 finding（带最新的 severity 和 detail）
report := assurance.Verify(ctx, dir, rows, objects, chain, opt)
把 report 交给现有的 finding 出口（只写 Allows 通过的任务）；再报的 finding 覆盖 severity 和 detail
消失的键用 report.Closed 去关，原因写 Closure.Reason；「row removed」另记一条事件
存下 report.VerifiedAt（last_verified_at）和 report.Read（下一轮的 prev.Read）
```

`Report.Read → Options.LastRead` 和 `Report.VerifiedAt → Options.LastVerified` 一样是契约：不写回时强制段每轮都读（`F` 按轮数放大，第 5.3 节的前提不成立），`LOCAL_UNREADABLE` 的升级和两次干净读靠的是 `OpenFindings` 里上一轮写下的 severity 和 detail。**工人那一份必须测这两条**：连跑多轮、把 `Report.Read` 写回之后，强制段每周期只读一次；`LOCAL_UNREADABLE` 连续两次失败升为 CRITICAL、连续两次干净读才关（`verify_qa_test.go` 的 `passState` 就是这条写回的样子）。

工人传 `CatalogComplete: true` 只在这一轮读到了这个任务的全部目录行时（不是分页的一页、不是读到一半出错的结果）。

`Allows` 是唯一要 ADR 0011 PR-2 实现的函数：

- 没有集群租约的进程（standalone，含带 meta 但不抢租约的）：恒为 true。一台进程就是主人。
- 集群：true 当且仅当本节点是该任务**当前**租约持有者。实现放在 PR-2 的过渡模型后面，读的是那份模型接受之后的主人，不是今天 `scheduler_cluster_lease.go` 里的续租循环。
- `per_node` 存储下，不是链所在节点的进程即使抢到租约，也不许用空目录做结构检查。PR-2 若把「租约持有者」和「目录所在节点」合成一个主人，`Allows` 跟着那个定义。在那之前不猜测。
- 出错与 false 相同：跳过，不关。

`internal/assurance` 不 import 租约、认领或 `internal/ownership`。纯函数的注释写明：调用方必须已经是主人。这一份不定义一个没有实现的 `Owner` 接口。接口要等第一个实现一起出现，否则只是一个没人调用的类型。

standalone 的 finding 文件和集群的 `task_findings` 都由现有出口写。巡检不新开一张表，也不自己 `Resolve`。

## 11. 迁移

没有新迁移。

`binlog_files.sha256` 已经由 `000008` 在封存时写入。ADR 说「上传时要把 SHA-256 写进目录行」，指的是本地被保留期删掉之后，对象还有东西可对。封存写入已经覆盖了新段。缺口是：封存时列还不在（schema 6/7）、`ReadSealFacts` 失败、或者更早的行，上传成功时摘要仍是空的。

上传落库前如果行上的 SHA-256 还是空的，就从本地封存文件流式算一遍，写入同一列，并在任务目录的 `.seal-sha256` 账本补一行（没有 meta 的 standalone 靠这本账）。已有摘要不动，避免一次重试把封存时的摘要盖掉。算失败不让上传失败：上传仍是尽力而为；摘要留空，巡检按决定 7 处理。事件时间只在行上还没有时补上，不把已有的 `first_event_at` / `last_event_at` 写成 NULL。

`last_verified_at` **不加列**。工人把 `Report.VerifiedAt` 和 `Report.Read` 写在 `{data_dir}/.verify.json`（原子替换）。文件丢了，下一轮多读一次，finding 仍在原来的出口里。不占迁移序号。v0.5.x 的 schema 下限保持 6。

演练不需要表。结果是 finding 加一条事件 `RESTORE_DRILL`，事件类型阶段 2 已经能存。

## 12. 还原演练

默认关（决定 8）。打开之后，每 `verify.drill_every_sec`：

1. 在可恢复窗口里取一个时间点。取法要稳定到「同一次演练」，不要每次进程重启换一个点又把上次的失败关了。用周期起点做种子。
2. 用阶段 5 的还原计划从起点集合 `B` 放到该点。阶段 5 还没有计划接口；在那之前演练不启动。不在巡检器里复制一份 `mysqlbinlog` 命令拼装。
3. 在演练实例上执行，比较 `gtid_executed` 和计划的终点集合。配置了 `verify.drill_check_sql` 再跑那一条。
4. 通过：更新 `binlog_server_restore_drill_last_success_timestamp`，关 `DRILL_FAILED`。失败：CRITICAL `DRILL_FAILED`。
5. 演练客户端的唯一地址是 `verify.drill_dsn`。构造函数不接收源 DSN。host:port 与任务源相同则 `DRILL_REFUSED`，不拨号。

演练失败不代表链断了。它是一条独立 finding，不改任务的 dump 状态。

## 13. 和 ADR、阶段 2 草图的差异

| 来源 | 原文 | 这里 | 理由 |
|---|---|---|---|
| ADR §3（原文） | 对象不符 → `VERIFY_FAILED` | 字节不符用 `OBJECT_CORRUPT`，大小不符用 `I6_OBJECT_SIZE_MISMATCH` | 和第 3 节决定 3 相同；草图已经用了这个名字。ADR §3 第 2 条已随本设计修订 |
| ADR §3 | 抽样 5% | 按段名的固定槽轮换，轮换长度按周期数配置（默认 20），外加「最久没验过」一档 | 决定 4、5。同样的成本，两次内容检查的间隔有上界，配置不取整 |
| 第 2 轮本设计 | 先解析全部本地段，再查对象；预算等不到就中止整轮 | 先 stat 和 HEAD，再按队列付费；预算或截止时间用完记 `SKIPPED`、正常结束 | 决定 4、第 5.2 节。QA r2 F1：短截止时间下对象永远查不到 |
| 第 2 轮本设计 | 开着 `OBJECT_CORRUPT` / `VERIFY_FAILED` 每轮强制 | 本地文件还在才每轮；不在时随轮换 | 第 6.3 节。QA r2 F2 |
| ADR §3 | 每任务 `verify_budget` | 进程级一把预算，默认等于单任务上限 | 决定 6。多任务相加会打满磁盘 |
| ADR 决定 4 | 抽样加上「上一轮之后新上传的全部」 | 最近档：未上传的行和 24 小时内上传的行，各在改动之后读一次（看 `last_verified_at`），每轮最多花 `min(25% × C, C − F − S)`；超出的和出了窗口还没验过的上传进末档，用剩下的预算 | 决定 4、第 5.2 节。「上一轮之后」用 `last_verified_at` 表达；份额上限让积压不挤掉轮换，末档让剩下的预算不空着 |
| ADR §3 | 复用 ADR 0001 的租约判断主人 | 只留 `Allows`，实现等 ADR 0011 PR-2 | 0001 的续租循环会被 PR-2 换掉。现在接上就是在即将删除的代码上加一个调用方 |
| 阶段 2 §11 | 巡检让 `ScopeChainObjects` 变为 Complete 并 `MarkDirty` | 纯函数返回报告；`Produced` 保持不变 | 决定 9。没有周期调用就标成已生产，健康会误报 UNKNOWN |
| 阶段 2 §11 | 新 kind 直接注册 | 名字定了，注册表不动 | 注册表一项没有 `Derive` 就会在评估器里开出空行为。等工人一起加 |
| ADR 第 1 节草图 | 巡检器自己列举对象再喂给链 | HEAD 的结果就是对象见证 | 再列一遍会和 HEAD 看到的不一致，同一段可能开两条 I6 |

## 14. 测试与 CI

`go test ./...` 里、用假的对象存储：

| 测试 | 证明 |
|---|---|
| `TestVerify_ReportsDeletedObjectFlippedObjectAndFlippedLocal` | 干净的一轮没有这三条；删对象、改对象一个字节、改本地一个字节，下一轮三条都在；修回去之后 `Disappeared` 把它们交出来 |
| `TestVerify_CleanChainHasNoFindings` | 阶段 1 的 live 夹具加上与字节一致的行和对象，`Chain.Check` 与结构、对象检查都为空；每段 `PARSED` + `HASHED`，预算计费等于本轮字节 |
| `TestVerify_ObjectSizeMismatch`、`TestVerify_LocalMismatchesAgainstTheCatalogRow` | 截断对象、目录行大小 +1、本地大小、SHA-256、首尾位点、GTID 文本各自报对 kind 和覆盖 |
| `TestVerify_WorkThatDoesNotFitIsSkippedNotBlocking` | 队头放不下的一份记 `SKIPPED`，后面放得下的照样付（杀 R09） |
| `TestVerify_PendingBacklogDoesNotStarveTheRotation` | QA r3 F1 的 pending_local_hog_3（C=47292、F=0、S=15764、k=13、N=20，写回）：推出 q=2、P=13；200 轮里每段前 13 轮内首查，最坏间隔 20 ≤ 33 |
| `TestVerify_NotUploadedRowIsRecentUntilParsed`、`TestVerify_NamedSegmentGetsNoRecentWork` | 未上传的行解析过就离开最近档，`sealed_at` 改动、上传都让它回来（杀 Y10，和 `TestTierOf_NotVerifiedSinceItsLastChange` 一起）；被开着的 finding 点名的段不进最近档 |
| `TestVerify_NewUploadIsReadAtMostTwiceInItsFirstDay` | 每小时一轮、预算充足、48 小时每小时上传一段：每段头 24 轮最多读 2 次（最近档一次，槽最多一次） |
| `TestVerify_RecentShareCapsTheRecentTier` | 最近档超过份额的落到下一档：轮换开着时进第 4 档、排在验过的过期段前面（QA r5 P2-1），轮换关掉时进末档、用剩下的预算（杀 Z08）；份额累计、封顶在 `C − F − S`；只有预算和截止时间时按它们算 `C`；份额 −1、101 被拒绝 |
| `TestVerify_BucketRecoveryHashesTheBacklog` | QA r4 G1：30 段 `UPLOAD_FAILED` 待上传时解析过、桶恢复后一起上传，`C = 630560`、每天一轮、写回：第一天付满 `C`，全部对象两天内第一次哈希 |
| `TestVerify_RecentShareLeavesRoomForOneSegment` | QA r4 G2：份额 100% 时后两档仍付到一段（上传之后没验过的那段排在第 4 档最前）；`C = S` 时 `R = 0`、最近档关闭、`q = 1` |
| `TestVerify_LocalReadErrorIsLocalUnreadable` | QA r4 G3：解析途中一次性读错 → 本地 `ERROR` + `LOCAL_UNREADABLE`（第一次 WARNING），不报 `VERIFY_FAILED`（杀 Z16）；下一轮强制重读，第一次干净读标 recovering，同一周期第二次干净读才关（开着 `LOCAL_UNREADABLE` 不按 `Report.Read` 跳过）；打不开（chmod 000）每轮都报，文件删掉后关；stat 失败也报；打开时文件已不在不报 |
| `TestVerify_ShortObjectBodyIsError` | QA r4 G4：对象正文以干净的 EOF 提前结束 → `ERROR`（`io.ErrUnexpectedEOF`），不报 `OBJECT_CORRUPT`，不关开着的 |
| `TestVerify_FutureSealedAtIsClockSkew` | QA r4 G5：`sealed_at` +3 天的未上传行每小时一轮，超前期间只读一次；报 `UPLOAD_CLOCK_SKEW`，不挡 `last_verified_at`；开着的 `UPLOAD_CLOCK_SKEW` 不让从没验过的行离开最近档；`sealed_at` 正常后关 |
| `TestVerify_ForcedSegmentReadOncePerPeriod` | QA r4 G6：写回 `Report.Read` 时开着 `OBJECT_CORRUPT` 的段两个周期 48 轮只哈希 2 次；新周期、本地 mtime 变了、对象大小变了都照常读 |
| `TestVerify_LocalRemovedVerifyFailedNeedsTheObjectToParse` | QA r3 F2：本地已删，对象和目录摘要相符但解析失败（目录摘要就是坏字节的）、`end_log_pos` 不符、GTID 不符，三种都不关、不前进，改报对象侧的 `VERIFY_FAILED`；对象好时 sha 那种按「local removed; object matches catalog」关、其它按「object parses」关；对象字节只付一次 |
| `TestVerify_RowRemovedLeavesChainKindsToTheChain` | QA r3 F3：行删了、这一轮没查链，I1/I2/I3/I4/I5/I7 都不关；同一轮查了链才关 |
| `TestVerify_HeadPhaseStopsAtTheDeadline` | QA r3 F4（Y14）：第一次 HEAD 撞上截止时间之后不再发 HEAD，剩下的 `SKIPPED`，正常返回 |
| `TestVerify_StarvationUnderAFixedDeadline` | QA r2 F1 原样的 64 KiB/s、450ms、25 轮：每轮都 HEAD 全部对象、没有一轮返回错误；按第 5.3 节推出的上界（5 轮）内每份副本都查到；强制段每轮都查 |
| `TestVerify_QueueBoundsTheGap`、`TestQueueBound` | 25 段 120 轮、`PassBytes` 封顶：每段第一次在 `P` 轮内、之后间隔不超过 `N + P`；超大对象每轮 `SKIPPED` 不堵队；不封顶时间隔不超过 `N` |
| `TestVerify_QueueOrder`、`TestVerify_OverdueAtRotationTimesPeriod` | 强制 → 最近 → 轮换槽 → 最久没验过；档内旧的先；被开着的 finding 点名的段不进最后一档；恰好 `N × 周期` 算过期 |
| `TestVerify_HeadsEveryObjectBeforeAnyContent`、`TestVerify_DeadlineEndsThePassNormally` | 一个字节都不让读时仍 HEAD 全部对象并报出删掉的对象；截止时间已过或预算付不及都正常返回，剩下的 `SKIPPED` 并计入 `Deferred` |
| `TestVerify_BytesAreWhatThePassPaid` | QA r2 F6：另一轮并发拿走令牌、这一轮读了付不成，`Report.Bytes` 只算付了的；一次大读切成一个突发 |
| `TestSpanTracker_MatchesEventSpan`、`TestVerify_ChargesEveryReadToTheBudget` | 流式位点和 `binlog.EventSpan` 在夹具和 450 份损坏副本上一致；设了位点的段也只付一次 |
| `TestVerify_CorruptObjectStaysOpenUntilRehashed`、`TestVerify_ForcedRecheckWithoutLocalFollowsTheRotation` | 本地在：开着的 `OBJECT_CORRUPT` 每轮强制、修好就关；本地不在：45 天里只在槽那天下载 |
| `TestVerify_VerifyFailedWithLocalRemoved`、`TestVerify_OpenFindingsBlockLastVerified` | QA r2 F2：对象和目录行相符时带「local removed; object matches catalog」关闭，关之前不前进、只在槽那天下载；不相符时一直开着；开着的 finding 挡住 `last_verified_at` |
| `TestVerify_RowRemovedClosesEveryKind` | QA r2 F3：完整目录里行没了，按行的 kind 按「row removed」关，`I6_MISSING_ROW` 和链的 kind 除外，也不再强制；目录不完整什么也不关；未封存的行不算删除 |
| `TestVerify_UnknownRowSize` | QA r2 F4：行大小为 0 时 HEAD 不关大小结论；和本地文件相符的哈希才关；开着的大小结论强制哈希；没有本地文件时用目录摘要哈希也不算比过大小 |
| `TestVerify_RetentionMarkerDoesNotCloseMissingLocal` | QA r2 X14：保留期标记的 `ABSENT` 不关 `I6_MISSING_LOCAL` |
| `TestVerify_CancelLeavesUnreachedSubjectsUnverified` | 取消：没走到的内容不关不推进；HEAD 看到的照常关 `I6_MISSING_OBJECT`；HEAD 途中取消，后面的行没有条目 |
| `TestVerify_LastVerifiedOnlyForContentChecks`、`TestVerify_DigestSourcesAndBackfillCount`、`TestVerify_DeferredLocalParseDefersTheDigestlessHash` | 只 HEAD、没有摘要、保留期删掉、未上传丢失都不推进；本地摘要优先；两个计数；本地解析被推迟时无摘要的对象也推迟、不算无法验证 |
| `TestDisappeared_ClosesOnlyWithTheDetectingMethod` | 第 6.2 节的表，逐格，带原因 |
| `TestVerify_FutureUploadedAtIsClockSkew`、`TestRecentUpload_Window` | 10 年后的 `uploaded_at` 开 `UPLOAD_CLOCK_SKEW`、验过的行不下载（杀 W28）；24 小时边界、1 小时容差、时区 |
| `TestVerify_StaleBacklogSortsAsNeverVerified` | QA r5 P2-1：120 和 400 段过期（验过 100 天）、30 段上传之前验过的积压一起上传、一段坏，`C = 6S`、每天一轮、写回：`N` 个周期内积压全部哈希、坏的那段查到；每轮不超过 `C`（杀 V01、V02） |
| `TestVerify_RotationOffHashesNeverVerifiedOnce` | QA r5 P3：轮换关掉，从没验过和上传之前验过的旧上传各从末档哈希一次，上传之后验过的不读（杀 T05） |
| `TestVerify_FutureSealedAtWithinToleranceReadsOnce` | QA r5 P3：`sealed_at` +30 分钟和 +3 小时，10 分钟一轮 6 小时：只读一次，而且在 `sealed_at` 之后（杀 V16） |
| `TestVerify_ReadMarkComparesEveryField` | `ReadMark` 每个字段：本地大小（杀 W09）、本地 mtime、对象大小、`uploaded_at`（同样大小的重新上传，杀 V03）任何一个变了就重读；都没变才跳过 |
| `TestVerify_ReadMarkNeedsEveryScheduledCopy` | 对象读错（杀 W11）或本地读错（杀 W12）时不记 `ReadMark` |
| `TestVerify_ObjectOneByteShortIsError` | 正文只短一个字节也是 `ERROR`（杀 W17） |
| `TestVerify_LocalUnreadableClosesWhenSoleCopyGone`、`TestVerify_LocalUnreadableClosesOnSizeMismatch` | 未上传段的文件没了：`LOCAL_MISSING`，`LOCAL_UNREADABLE` 关（杀 W21）；文件又能 stat、大小不符：`VERIFY_FAILED` 接手，`LOCAL_UNREADABLE` 关（QA r5 P3，杀 V12） |
| `TestVerify_LocalUnreadableHysteresis` | QA r5 P2-2，按工人写回的样子（`passState`：severity 和 detail 覆盖）：未上传段失败、失败 → WARNING、CRITICAL，干净读不降级，连续两次干净读才关；隔一轮一错一直 WARNING、只开一次、不中途关；已上传段永远 WARNING（杀 V04–V08、V11） |
| `TestVerify_LocalUnreadableTaskLevel` | QA r5 P2-2：目录不能搜索、每个 stat 都 `EACCES`：只报一条 subject 为空的，关掉逐文件的那条，完整目录下不按「row removed」关；第二轮 CRITICAL；恢复后两轮才关（杀 V10、V13–V15；V09 由 `TestVerify_LocalReadErrorIsLocalUnreadable` 的单文件 stat 失败打死） |
| `TestRotation_BoundsTheGap` | 2000 个名字一年：每个名字恰好每 20 天一次；`WorstGap` = 轮换 × 周期，不取整；任意轮换长度每轮恰好一次 |
| `TestVerify_ConcurrentPassesShareOneBudget` | 两轮并发共用一只预算：各自的 `Bytes` 等于单轮，累计等于两轮之和（`-race` 下跑） |
| `TestApplySealedUpload_FillsEmptySHA256`、`TestApplySealedUpload_FillKeepsStoredEventTimes` | 行上没有摘要时补上；已有摘要和已有事件时间不被盖掉 |

变异：`go run ./internal/assurance/mutants/kill.go` 跑 QA r1 的 25 个（M01–M25）、第 2 轮覆盖模型的 29 个（N 开头）、QA r2 的 20 个（X01–X20）、第 3 轮队列与关闭规则的 44 个（R 开头）、QA r3 的 Y10 和 Y14、第 4 轮的 20 个（S 开头：新鲜度、最近档份额、对象侧解析、链 kind）、QA r4 的 Z08 和 Z16，以及第 5 轮的 24 个（T 开头：末档、份额封顶、`LOCAL_UNREADABLE`、短正文、`sealed_at` 偏差、强制段每周期一次；T05 随 QA r5 P3 反过来，改成「轮换关掉时从没验过的不进末档」），QA r5 留下的 6 个（W09、W11、W12、W17、W21、W28；W28 按新的「未来时间不算改动」改写），以及 QA r5 后续的 16 个（V 开头：排序时间、`ReadMark.UploadedAt`、`LOCAL_UNREADABLE` 的升级、两次干净读、任务级合并、大小不符时关闭、容差内的 `sealed_at`），再加上工人之前的收尾 6 个（H06、H07、H08、H19、H23、H25：合并时少算一个成功的 stat、混合 errno 也合并、悬空符号链接当失败的 stat、被点名的过期段用 `last_verified_at` 进槽、容差内的未来 `uploaded_at` 当改动、`LOCAL_UNREADABLE` 在任意 `STAT` 成功时关闭），共 194 个。除标明等价的 N30、X05、R15（理由写在 kill.go 里）外全部被打死，没有一个靠编译失败算打死。H25 由 `TestVerify_CancelDoesNotCloseUnreadLocalUnreadable` 打死：取消的那一轮没读到的强制段停在 `STAT`，关闭只作用于这一轮真正读过的行。N08 的「未上传且本地缺失不报」现在打在 `markAbsent`（目录里没有，和 stat 得到不存在，同一处报 `LOCAL_MISSING`）。

真实 MinIO 不在任何现有 CI job 里。`e2e.yml` 的 `unit-and-e2e` 跑 `run-suite.sh --profile quick`，quick 的场景名单是写死的，里面没有 MinIO。每个会拉 MinIO 的场景（`smoke-retry-upload`、`smoke-split-retention`）各有一个 job，场景名写在 workflow 里。`workflow_dispatch` 的 profile 也是 workflow 里的闭枚举。不改 workflow 就没法把新场景加进这些 job，而把 MinIO 塞进 quick 会让 `go test` 那个 job 被镜像仓库拖垮。现有做法是把 MinIO 隔在自己的 job 里。

因此场景放在 `scripts/e2e/smoke-verify.sh`（自己起 MinIO，跑 `TestVerifyAgainstMinIO`，不连源库）。`run-suite.sh --scenarios smoke-verify` 也能调到它，但它不在 quick 或 full 里。要在每个 PR 跑，在 `e2e.yml` 里照 `e2e-retry-upload` 加一个 job 即可，步骤改成直接跑脚本，免得为了 MinIO 再把整套源库 compose 拉起来：

```yaml
e2e-verify:
  if: ${{ github.event_name != 'workflow_dispatch' || github.event.inputs.e2e_profile == 'verify' }}
  runs-on: ubuntu-latest
  timeout-minutes: 40
  steps:
    - uses: actions/checkout@v7
    - uses: actions/setup-go@v7
      with:
        go-version-file: go.mod
        cache: true
    - name: E2E Verify
      run: ./scripts/e2e/smoke-verify.sh
```

`workflow_dispatch` 的 `e2e_profile` 选项加上 `verify`。这一份改不了 workflow，所以上面的 MinIO job 先不加。A4 不靠它：`TestVerifier_APIReportsMissingLocalAndCorruptObject`（`internal/api/verifier_api_test.go`）让运行中的工人发现缺本地文件和损坏对象，经 `GET /api/tasks/{id}/findings` 和 `GET /api/health` 报出，并在 `/metrics` 上看到 `finding_open` 与 `binlog_server_verify_bytes_total`。`TestApp_VerifierTickerReportsTwoPasses` 从进程启动走到第一轮和 ticker 第二轮。它在 `go test ./...` 里，现有 `unit-and-e2e` 每个 PR 都跑。工人写回的多轮测试在 `internal/tasks/verifier_test.go`。集群仍不巡检（`TestStartVerifier_ClusterIsInert`），所以 A4 是 PARTIAL，等 ADR 0011 的主人（PR-2/PR-4）。可选演练仍关。

## 15. 留给后面的清单

工人这一份已经做了的，标在每条后面。ADR 0011 PR-2 仍要做的是第 1 条的集群实现。

1. 实现集群的 `Allows`（第 10 节），只从 ADR 0011 PR-2 的过渡结果判断当前主人。这一份在集群上恒为 false（不开也不关），standalone 不设，恒为允许。不读今天的租约循环。
2. 周期循环：已做（`internal/tasks/verifier.go`）。finding 交进现有出口，`Report.Closed` 的原因写进该 finding 的 detail。链传 `nil`。每轮把还开着的 finding（severity 和 detail）放进 `Options.OpenFindings`，`last_verified_at` 放进 `Options.LastVerified`，`ListFiles` 成功时 `CatalogComplete`，ctx 用 `verify.pass_deadline_sec`（0 则用 `verify.every_sec`）。`per_node` 下「不是目录所在节点」等第 1 条的主人定义，这一份不猜测。
3. `health.Produced` 的 `chain.objects` 和 `verify` **保持 false**（评估器不生产它们）。注册表已补 `VERIFY_FAILED`、`OBJECT_CORRUPT`、`UPLOAD_CLOCK_SKEW`、`LOCAL_UNREADABLE`、`DRILL_FAILED`、`DRILL_REFUSED`，未托管。对象类 I6 落库时 subject 用 `ObjectSubject`，读回 `OpenFindings` 时去掉。
4. 配置键和指标：已做。`verify.recent_share_percent` 在加载时拒绝 0 和 1–100 以外的值。`Report.Read` 与 `Report.VerifiedAt` 写在 `{data_dir}/.verify.json`。`internal/tasks/verifier_test.go` 覆盖强制段每周期一次，以及 `LOCAL_UNREADABLE` 的 severity / detail 经文件库再喂回。`VerifyRow.SealedAt` 已填。
5. 不加 `binlog_files.last_verified_at` 列。进度在 `.verify.json`。
6. 不和复核器合成一把字节预算。巡检自己一把。
7. 阶段 5 的还原计划到位之后再开演练。演练默认仍关，工人不拨 `verify.drill_dsn`。
8. 不改 `e2e.yml`。standalone 的周期巡检由 `go test ./...` 里的 `TestVerifier_APIReportsMissingLocalAndCorruptObject` 和 `TestApp_VerifierTickerReportsTwoPasses` 支撑。集群 `Allows` 仍关，A4 是 PARTIAL，等 ADR 0011 的主人（PR-2/PR-4）。第 14 节的 MinIO job 仍要改 workflow 才能进 CI。
9. 两个 gauge 已从本轮 `Report` 导出。趁本地文件还在把空 `sha256` 写回目录行的后台任务还没有。
10. 「row removed」关闭时记一条单独事件：还没有。关闭原因写在 finding 的 detail 里。

## 16. 已定的问题（原开放问题）

1. `OBJECT_CORRUPT` 单列，不并进 `VERIFY_FAILED`：处置不同。对象字节不符可以用本地副本重新上传；本地或目录行不符要人查。ADR 0009 §3 的表已经改成这样。
2. 进程级 4 MiB/s 作为默认。多任务之间公平分摊放到 PR-2 的周期循环里做，免得一个大任务把别的任务推到下一个周期。
3. 演练等阶段 5 的还原计划，不在巡检器里复制一份回放命令。
4. 本地文件不在时，开着的 `OBJECT_CORRUPT` / `VERIFY_FAILED` 不再每轮强制，随轮换重查（第 6.3 节）。这台机器上没有副本可传，每轮下载修不了它。
5. `I6_MISSING_ROW` 不按「row removed」关：行不在正是它报的事。链的 kind（I1–I5、I7）也不按它关，只靠同一轮的链检查（QA r3 F3）。
6. 最近档的消耗用份额上限计入上界（默认 25%），不用「按到达速率算」：份额上限对任何积压都成立，按到达速率算要假设积压的大小（QA r3 F1）。代价是上界按最坏情况扣掉 `R`，比实测松。
7. 本地已删、开着 `VERIFY_FAILED` 时，对象边哈希边解析；对象坏就改报对象侧的 `VERIFY_FAILED`，不新开一个 kind（处置相同：这一段唯一的副本不能用，要人查）。只在这种情况下解析对象，不对每个按目录摘要哈希的对象都解析：后者会给从来没有本地问题的段新开结论，超出这一版的范围，留作后续。
8. 轮换槽每周期最多读一次（这一周期开始之后验过就不再读）。被 finding 点名、`last_verified_at` 不前进的段，在槽那个周期里每一轮都会读；一天一轮时就是一次，一轮比一个周期更频繁时会多读，留给工人按周期调度。强制段已经用 `Report.Read` 收住（第 6.3 节）。
9. 最近档的溢出不记 `SKIPPED`（QA r4 G1）。轮换开着时，最后一次改动之后没验过的段（溢出、上传之前验过的积压、从没验过的）排序时间为空，进第 4 档队头（QA r5 P2-1）；第 4 轮把积压放进末档，过期的段一多它就排不上。轮换关掉时它们进末档，只用剩下的预算，各读一次：从没验过的上传和上传之前验过的一样处理（QA r5 P3；第 5 轮从没验过的不读）。
10. `LOCAL_UNREADABLE` 第一次读错就报 WARNING，未上传的行连续第二次升 CRITICAL（QA r5 P2-2）；连续两次干净读才关，CRITICAL 不因一次干净读降级。状态放在 finding 的 severity 和 detail 里，不另加存储。每个 stat 都同样失败时只报一条任务级的。打开时文件已不在不报（下一轮会看到它不在）。`MISSING`（未上传、文件没了）也关它，缺失由 `I6_MISSING_LOCAL` 报；文件又能 stat 但大小不符时关它，`VERIFY_FAILED` 接手。
11. `sealed_at` 超前用已有的 `UPLOAD_CLOCK_SKEW`，不新开 kind；同一段两个时间都超前时只有一条。`UPLOAD_CLOCK_SKEW` 不再挡 `last_verified_at`、不算点名：否则超前的行永远「从没验过」，每轮都进最近档。
12. 强制段每周期一次靠调用方写回的 `Report.Read`，不另加存储；`ReadMark` 比本地大小、本地 mtime、对象 HEAD 大小和行的 `uploaded_at`（QA r5 P3：同样大小的重新上传不用等下一个周期）。没有用 ETag：`ObjectStore.Head` 只给大小，加 ETag 要改接口和每个实现，`uploaded_at` 已经在行上。
