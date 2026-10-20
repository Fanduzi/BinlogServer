# ADR 0009 阶段 3 设计：巡检器 (Verifier)

状态：设计（2026-10-20；第 2 轮按 QA 改为覆盖模型，见第 6 节）。上位决定：[ADR 0009](../adr/0009-backup-assurance.md) 第 3 节与「分阶段实施」第 3 行。前置是已落地的备份链（`internal/assurance` 的 `Chain` / `Check` / `Window`）和阶段 2 的健康评估器。本文只回答巡检器**查什么、怎么限速、结论叫什么、配置和迁移是什么、还原演练为什么默认关、以及它怎样挂到任务主人上**。不改 ADR 0009 的决定，只修订 ADR §3 第 2 条的 finding 表（对象的三种结论分开，见决定 3 与第 8 节）；和 ADR 原文或阶段 2 草图不一致的地方写在第 13 节，并给出理由。

集群所有权正在按 [ADR 0011](../adr/0011-cluster-ownership-and-failover.md) 重做。PR-1（纯过渡模型）还在 QA，PR-2 到 PR-5 会换掉今天的租约与 finding 单调性。本阶段**不读、不写、不封装**今天的 `task_leases`、认领循环或 failover。巡检器只通过第 10 节那条窄缝问「我是不是这个任务现在的主人」。那条缝的集群实现留给 ADR 0011 PR-2。

## 1. 要解决什么

阶段 1 的 `Chain.Check` 能在**有人来问**的时候判定 I1–I7。阶段 2 的评估器按 sweep 把链上已经算出来的违反写成 finding。两件事都不会做 ADR 0009 第 3 节要求的三件定期工作：

| 缺口 | 今天 | 后果 |
|---|---|---|
| 本地封存段没有被完整解析过 | 评估器复用账本的 size+mtime 缓存；复核器只重算 SHA-256，不看事件 CRC、位点和 GTID | 校验和没变、但事件结构坏了的文件可以一直绿 |
| 对象只在上传当时对过一次 ETag | 上传后桶里的对象被生命周期删掉或改掉一个字节，没有第二个人去 HEAD / 读 | #173 那类「看起来已上传」会在上传之后再次出现，而且本地保留删掉之后无本地可比 |
| 链上的违反没有一个固定的周期作者 | 评估器会派生它读到的违反，但对象侧 scope 还没有被标成「有人生产」（`health.Produced` 不含 `chain.objects` 和 `verify`） | 对象丢失不会变成一条会自己关掉的 finding |

还原演练（每 7 天在一台专用实例上放到一个时间点）同样没有。生产清单 A4 因此是 NOT MET。

## 2. 这一份实现的边界

先做**不依赖主人是谁**的部分，因为主人的定义马上要换。

这一份做：

- 纯函数：给定目录行、本地封存段、对象存储的 HEAD/GET，返回本轮应当开着的 finding。
- 字节预算。
- 上传成功时，如果目录行还没有 SHA-256，把它补进**已经存在**的 `binlog_files.sha256`（迁移 `000008`）。不新建列。
- 用假对象存储的单测覆盖：删一个对象、改对象一个字节、改本地一个字节，下一轮都报出来；干净的链没有 finding；预算真的卡住读取。关闭、提前返回、`last_verified_at`、强制重查和轮换上界各有单测，`internal/assurance/mutants/kill.go` 用它们打变异。

这一份不做（等 ADR 0011 PR-2 给出「当前主人」之后再接）：

- 周期循环、按任务分摊、把 finding 写进 `task_findings`。
- 把 `health.Produced` 里的 `chain.objects` / `verify` 改成 true。那个测试锁着「阶段 3 之前健康不会因为这两个 scope 变成 UNKNOWN」。现在翻开会让还没跑巡检的进程把任务标成 UNKNOWN。
- 还原演练的进程、DSN 和 MySQL。
- 真实 MinIO 场景进 CI。场景脚本在 `scripts/e2e/smoke-verify.sh`，现有 workflow 不会选中它。要进 CI 必须改 workflow，本仓库的 token 改不了 workflow。改法写在第 14 节。

## 3. 核心决定

1. **巡检是一次纯计算，不是一个新的写者。** 输入是这一刻的目录行、本地字节和对象字节，输出是 finding 集合和每个通过的段的 `last_verified_at`。同一份输入给出同一份输出。谁来周期调用它、谁把结果落库，是主人接缝后面的事。理由：阶段 2 已经有唯一的 finding 生命周期（`Reconcile`）。再做一个会写 `task_findings` 的循环，就会在 ADR 0011 换掉单调性之前多出一个写者。
2. **只有当前主人为这个任务跑巡检。** 纯函数自己不问。调用方先问第 10 节的缝；缝说不是，或者缝出错，就跳过这个任务，**既不开也不关** finding。理由：本地字节在主人那台机器上（ADR 0011 §3.3）。一台没有目录的机器跑结构检查，会把「文件不在我这儿」写成损坏。缝出错同样不能关 finding：关需要一份完整观察，观察失败不是「问题消失了」。这和 ADR 0011 §3.5「不完整的观察既不开也不关」是同一条。
3. **三条检查各报各的种类，不合成一个 `VERIFY_FAILED`。** 结构失败是 `VERIFY_FAILED`，对象不在是 `I6_MISSING_OBJECT`，对象大小不符是 `I6_OBJECT_SIZE_MISMATCH`，对象字节不符是 `OBJECT_CORRUPT`。理由：处置不同。对象没了要查桶的生命周期；字节不符是静默损坏；本地解析失败是这段备份不能读。ADR 原文把对象「不符」笼统写成这三个名字里的 `VERIFY_FAILED`。阶段 2 草图已经单列了 `OBJECT_CORRUPT`。沿用草图的名字，避免一条 finding 盖住两种事故。`I6_SIZE_MISMATCH` 不拿来用：那个名字在代码里是「旧段的本地大小和目录行不一致」，和对象大小不是一件事。
4. **HEAD 每个已上传段；SHA-256 只算一部分。** 已上传段每次都 HEAD（存在和大小）。下载并算 SHA-256 的是三类：最近 24 小时上传的全部；轮到这一天的那一批（决定 5）；以及上一轮还开着 `OBJECT_CORRUPT` 或 `VERIFY_FAILED` 的段（第 6 节）。理由：HEAD 便宜，能抓住误删和截断。全量下载的流量随数据量线性涨，ADR 已经拒绝作为默认。24 小时而不是「上一轮之后」，是因为纯函数看不到上一轮的游标；`verify.every` 的默认就是 24 小时，两者对齐。操作员把周期改短时，24 小时比「上一轮」多查而不是少查。
5. **抽样是轮换，不是每天重新抽。** 每个段名有一个固定的槽：`sha256(段名)` 的前 8 字节对 `N` 取模，`N = ceil(100 / percent)`（默认 5% 即 `N = 20`）。第 `d` 个周期（`floor(unix 秒 / 周期)`，UTC 对齐）只下载槽号等于 `d mod N` 的段。同一周期里重复跑，选中的是同一批，结论不会闪。每个段恰好每 `N` 个周期被哈希一次，所以一个旧对象两次内容检查之间最多隔 20 天，成本和原来的 5% 一样。理由：第 1 轮按「段名 + 周期」重新掷 5%，QA 量到一个文件第 91 天才第一次被哈希，2000 个名字一年里最长间隔 198 天。轮换给出确定的上界。percent 为 0 按 5；100 及以上为每轮全量；负数为「旧对象不下」，最近窗口和强制重查仍然下载。
6. **字节预算是整个进程一份，不是每个任务一份。** 默认 4 MiB/s，和现有 `health.rehash_bytes_per_sec` 同一个数。理由：ADR 写的是「每任务每轮上限」。100 个任务各 4 MiB/s 就是 400 MiB/s，QA 要求的「限速下源延迟不升高」做不到。一个任务可以吃满整个预算，多个任务分享它。复核器今天已经是全局限速；接上主人之后，巡检和复核器用同一只桶，不加倍。在那之前复核器不动，避免在所有权改写的同时改评估器的读盘行为。
7. **没有可比的摘要时，对象只 HEAD，并且记成「无法验证」，不算验证过。** 目录行 `sha256` 为空、本地文件又不在（或这一轮没能干净地解析它）时，没有东西可以和对象正文比。这时不下载正文（下载了也白下），对象记 `UNVERIFIABLE`，`Report.UnverifiableNoDigest` 计一次，不开 finding，`last_verified_at` 不前进，已开的 `OBJECT_CORRUPT` 也不会因此关闭。另一个计数 `Report.NoDigestLocal` 是「行上没有摘要、但本地文件还在」的封存段数：这些段现在还能从本地文件补摘要，保留期删掉之后就不能了。理由：把「旧行没有摘要」报成损坏，会在升级后的第一轮把保留期删掉的历史段全部打成 CRITICAL；但也不能把它们算成验证过。补摘要的任务和对应的 gauge 留给 PR-2（第 15 节第 9 条）。新上传的段由第 11 节在上传时补上摘要。
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

任务目录列不出来时（目录不在这台机器上、权限错），每段的本地副本都是 `SKIPPED`，不开 `I6_MISSING_LOCAL`，链也不查。列得出来但某个文件读失败（不是不存在）时，那一段记 `ERROR`，链不查。

## 5. 对象检查

对象存储没配（调用方没有传入对象读取器）时，这一轮**不判定**对象，每个已上传段的对象记 `SKIPPED`，也不关闭已有的对象 finding。关掉「存储被拆掉」是阶段 2 已经写过的 CONFIG 事实，要由工人显式给出，不能从「这次没传读取器」推断。

不是 `UPLOADED`（或没有 `object_key`）的行，对象记 `NOT_APPLICABLE`。对每个 `upload_state = UPLOADED` 且 `object_key` 非空的封存行：

1. 先看时钟。`uploaded_at` 比 now 晚超过 1 小时（`ClockSkewTolerance`）→ WARNING `UPLOAD_CLOCK_SKEW`，这一行**不算**最近，按轮换决定下不下载。1 小时以内的未来时间算最近（小的时钟差不能让新上传漏掉）。这一步不做 I/O，所以在碰对象之前就记下（`Coverage.ClockChecked`）。
2. HEAD。对象不在 → `I6_MISSING_OBJECT`，记 `MISSING`。其它错误（超时、503、凭据错）→ 这个对象记 `ERROR`，`Report.ObjectErr` 留下第一个错误，接着查下一个对象。大小和行不一致 → `I6_OBJECT_SIZE_MISMATCH`，记 `HEAD`，不读正文。
3. 选摘要：本地文件这一轮解析干净 → 本地摘要；否则行里的 `sha256`；都没有 → `UNVERIFIABLE`（决定 7），不读正文。
4. 不在最近窗口、不在今天的轮换槽、也没被强制 → 记 `HEAD`，原因是「不在这一周期的轮换槽」。
5. 否则 GET，流式算 SHA-256，计入预算，记 `HASHED`，`Digest` 写明比的是 `local` 还是 `catalog`。不符 → `OBJECT_CORRUPT`，证据同样写明来源。GET 时或读的途中对象消失，按缺失报；其它读错误记 `ERROR`。

`uploaded_at` 为空不算最近，否则没有时间戳的历史行会被全部下载。

## 6. 链检查、覆盖记录，以及消失的结论

调用方把已经建好的 `*Chain` 交进来时，跑 `Chain.Check(now)`，每条 `Violation` 变成一条 finding：kind 用违反的名字（`I1_REGRESSION` 等），subject 是第一个段，`I4_EMPTY_LEFTOVER` 是 INFO，其余 CRITICAL。没传入链，`ChainChecked` 为 false。任务目录列不出来，或任何一个本地文件读失败，也不跑链：链的结论描述的是本地字节，用调用方手里那份可能过期的链去关 finding 不对。

### 6.1 覆盖记录

第 1 轮的报告只有三个粗粒度的「scope 完整」标志。QA 找到的三个问题（没抽中的那天把 `OBJECT_CORRUPT` 关掉、ctx 取消后的报告看起来完整、只 HEAD 就推进 `last_verified_at`）是同一个原因：报告声称「这一块查完了」，而不是记下它到底查了什么。第 2 轮改成按段记：

```text
Report.Coverage[段名] = {
  Local:  { Method, Reason }            // PARSED | STAT | ABSENT | MISSING | ERROR | SKIPPED
  Object: { Method, Reason, Digest }    // HASHED | HEAD | UNVERIFIABLE | MISSING | NOT_APPLICABLE | ERROR | SKIPPED
  ClockChecked                          // uploaded_at 和 now 比过
}
```

| Method | 意思 |
|---|---|
| `PARSED` | 本地文件从头读到尾（magic、事件长度、CRC32、GTID 事件），算了 SHA-256，和行比过 |
| `STAT` | 本地文件大小和行不同，没解析 |
| `ABSENT` | 没有本地文件，也不需要有（已上传，或保留期删的） |
| `MISSING` | 本该在的副本不在（本地：封存未上传；对象：HEAD/GET 404） |
| `HASHED` | 对象正文读过，SHA-256 和一个摘要比过 |
| `HEAD` | 对象在，大小比过，正文没读 |
| `UNVERIFIABLE` | 对象在，大小比过，但没有摘要可比 |
| `NOT_APPLICABLE` | 行不是 UPLOADED，没有对象可查 |
| `ERROR` | 读失败，不是 404；这份副本这一轮什么都不知道 |
| `SKIPPED` | 没去看，`Reason` 写原因（没有对象存储、目录列不出来） |
| 零值 | 这一轮没走到（提前返回） |

条目只在一份副本的检查**结束之后**才写进去。ctx 取消、截止时间内等不到预算（`ErrBudgetDeadline`）时 `Verify` 立刻返回，已经写进去的保持原样，没走到的就是零值。这不靠在返回前补标志：关闭和 `last_verified_at` 都只读 `Coverage`，零值什么也关不掉、什么也推不动。调用方即使只记日志不看 `err`，拿这份报告去调 `Disappeared` 也是安全的。

### 6.2 关闭

`Disappeared(上一轮的 finding, 这一轮的报告)`：上一轮有、这一轮没有再报，并且这一轮**用能发现该 kind 的方法查过这个 subject**，才算消失。

| kind | 这一轮怎样才能关 |
|---|---|
| `VERIFY_FAILED` | 本地副本 `PARSED`（重新整文件解析并和行比过） |
| `I6_MISSING_LOCAL` | 本地副本 `PARSED` 或 `STAT`（文件回来了），或链这一轮查过 |
| `I6_MISSING_OBJECT` | 对象 `HEAD`、`UNVERIFIABLE` 或 `HASHED`（HEAD 拿到了大小）；subject 不在这一轮的行里时，链查过 |
| `I6_OBJECT_SIZE_MISMATCH` | 同上（这三种都比过大小） |
| `OBJECT_CORRUPT` | 对象 `HASHED`。只 HEAD、`UNVERIFIABLE`、`NOT_APPLICABLE`、`ERROR` 都不关 |
| `UPLOAD_CLOCK_SKEW` | `ClockChecked` |
| 其它（链的违反名） | `ChainChecked` |

行从目录里删掉之后，它的 finding 在纯函数这里永远不会关（没有观察就不关）。工人看到行没了，用明确的事实去关（第 15 节第 10 条）。`OBJECT_CORRUPT` 在行变成非 UPLOADED 时也不关：重新上传成功、行回到 UPLOADED 之后，强制重查会把它哈希一遍再关。

### 6.3 强制重查

`Options.OpenFindings` 是上一轮还开着的 finding。subject 上开着 `OBJECT_CORRUPT` 或 `VERIFY_FAILED` 的段，每一轮都下载对象正文，不管它在不在最近窗口或今天的槽里，直到某一轮重查关掉它为止。没有摘要可比的强制段仍是 `UNVERIFIABLE`，不下载，finding 保持开着，等操作员处理。工人必须把开着的 finding 传进来；忘了传也不会错关（只 HEAD 关不掉 `OBJECT_CORRUPT`），只是要等到它的轮换槽才会重查。

### 6.4 `last_verified_at`

只给这一轮按内容验过、而且没有任何 finding 点名的段：本地副本 `PARSED`（或者不需要本地副本，`ABSENT`），对象 `HASHED`（或者没有对象，`NOT_APPLICABLE`），并且两者里至少一个真的读过内容。只 HEAD、`UNVERIFIABLE`、`SKIPPED`、`ERROR`、没走到，都不前进。结果是：未上传的本地段每轮前进；已上传的段在上传后的 24 小时内每轮前进，之后每 20 天（轮换周期）前进一次。

同一段同一 kind 只留一条。对象检查和 `Chain.Check` 如果都看到同一个缺失对象，合并成一条。工人落库时，对象类 I6 要写成健康注册表已经在用的 subject：段名加 `#object`（`health.ObjectSubject`）。纯函数返回的 subject 是段名，因为链的违反用的是段名；`#object` 是健康层的键，不是巡检层的键。这个映射留给工人，避免这一份去改健康注册表。

## 7. 字节预算

`NewByteBudget(bytesPerSec)`。`<= 0` 表示不限速（测试和「显式不限」）。限速用已经在用的 `golang.org/x/time/rate` 令牌桶，突发等于速率，但不超过 64 KiB，这样大段不会一次借走一整秒以上的额度。读取按实际读到的字节等待；单次等待不超过桶的突发，大文件拆开等。

计入预算的是：结构解析读到的字节、位点第二遍事先计入的文件大小、对象 GET 的正文。HEAD 不计入，它不搬段数据。上下文取消，或者令牌桶说在截止时间之前等不到（`ErrBudgetDeadline`），这一轮就在那里中止（第 6.1 节）。

预算被多轮并发共用：累计值是原子计数（`ByteBudget.Charged`），令牌桶本身是并发安全的。`Report.Bytes`（以及 `LocalBytes`、`ObjectBytes`）是**这一轮**自己读的字节，不是预算的累计值；两轮并发共用一只桶时各自报自己的数。

默认 4 MiB/s 是进程级上限（决定 6）。配置键是 `verify.budget_bytes_per_sec`。接上工人之前，复核器仍用它自己的 `health.rehash_bytes_per_sec`，两只桶互不影响。工人落地时合成一只，并让复核器跳过本轮巡检已经哈希过的段。现在不合成：复核器挂在评估器上，评估器的所有权判断正是 ADR 0011 要换掉的那一层。

## 8. Finding 与指标

这一份只**返回** finding，不注册进 `health.Registry`，也不打指标。名字先定死，工人按这个名字注册（ADR 0009 §3 的表与这里一致）：

| kind | 严重度 | 何时开 | 何时关（第 6.2 节） | 处置 |
|---|---|---|---|---|
| `VERIFY_FAILED` | CRITICAL | 封存段解析失败，或和目录行的大小、SHA-256、位点、可选 GTID 文本不一致 | 重新整文件解析并和行相符 | 本地字节或目录行有一边错了：要人查，先别动对象。开着期间每轮强制哈希对象 |
| `OBJECT_CORRUPT` | CRITICAL | 哈希过的对象 SHA-256 与本地或目录行不同 | 下一次哈希它并且相符 | 桶里的字节变了：用本地副本重新上传。开着期间每轮强制重查 |
| `I6_MISSING_OBJECT` | CRITICAL | UPLOADED 段 HEAD/GET 不在 | HEAD 到了 | 查桶的生命周期和删除记录；本地还在就重新上传 |
| `I6_OBJECT_SIZE_MISMATCH` | CRITICAL | HEAD 的大小和 `size_bytes` 不同 | HEAD 大小相符 | 对象被截断或换掉（或目录行错了）：对照本地后重新上传 |
| `I6_MISSING_LOCAL` | CRITICAL | 封存、没上传、本地文件也不在 | 文件回来了，或链不再报 | 这段字节已经没有任何一份；从源库重拉这一段，或接受这个缺口 |
| `UPLOAD_CLOCK_SKEW` | WARNING | `uploaded_at` 比 now 晚超过 1 小时 | `uploaded_at` 不再超前 | 修写入方或这台机器的时钟；这一行按旧对象轮换，不会每轮整个下载 |
| 无法验证（不是 finding） | — | 没有摘要可比：`Report.UnverifiableNoDigest` | — | 本地还在时补摘要（`Report.NoDigestLocal` 是还能补的数）；补不了就只靠 HEAD |
| `DRILL_FAILED` | CRITICAL | 演练回放后的 `gtid_executed` 与计划不符，或校验 SQL 失败 | 下一次演练通过。演练没开时不关、也不开 | 查还原计划和演练实例 |
| `DRILL_REFUSED` | WARNING | 演练 DSN 的 host:port 等于任务源地址 | 两者不再相同，或 DSN 被清空 | 改演练 DSN |

链上的 kind 不新造名字，用 `Chain.Check` 已有的封闭枚举。它们的开关规则仍是阶段 2 注册表里的那一份；巡检只是把 `Check` 的结果交出去。

指标（工人落地时加，名字和 ADR §2.2 对齐）：

| 名字 | 类型 | 含义 |
|---|---|---|
| `binlog_server_task_last_verified_timestamp{task_id}` | gauge | 最近一次没有提前返回、并且没有任何副本是 `ERROR` / `SKIPPED` 的巡检的 Unix 时间。有 finding 也更新 |
| `binlog_server_verify_bytes_total{task_id,source}` | counter | `source` 为 `local` 或 `object` |
| `binlog_server_verify_pass_incomplete{task_id,scope}` | gauge | 0/1。scope 为 `local`、`object`、`chain`；从 `Coverage` 推出（有副本是 `ERROR`、`SKIPPED` 或零值），链看 `ChainChecked` |
| `binlog_server_verify_objects_unverifiable{task_id}` | gauge | `Report.UnverifiableNoDigest` |
| `binlog_server_verify_digest_backfill_pending{task_id}` | gauge | `Report.NoDigestLocal`：本地还在、能补摘要的段数 |
| `binlog_server_restore_drill_last_success_timestamp{task_id}` | gauge | 最近一次演练通过的时间。没开时不输出，避免「没有序列」被看成失败或成功 |
| `binlog_server_finding_open` / `binlog_server_findings_reported_total` | 已有 | 工人把上面的 kind 交进现有 finding 出口之后，这两个序列自然带上 |

`verify_age` 用 `last_verified_timestamp`。ADR 的健康定义是 `verify_age <= verify_every × 2`。工人接上之前，评估器不读这个时间，健康定义不变。

## 9. 配置

键现在**不进**加载器。加载器只接受已经会读的键；加进 `config.go` 却没有读者，文档测试会要求 `configuration.md` 写上一个还不生效的开关。等工人落地再加，默认值如下。负数在加载时拒绝启动。

| 键 | 默认 | 理由 |
|---|---|---|
| `verify.every_sec` | 86400 | ADR 的 24 小时 |
| `verify.budget_bytes_per_sec` | 4194304 | 第 7 节。0 在工人里表示用这个默认，不表示不限速；测试里的「不限」是直接传空预算，不是配置 |
| `verify.object_sample_percent` | 5 | 整数，避免为 0.05 加一个浮点解析。100 为全量 |
| `verify.object_recent_sec` | 86400 | 第 5 节的「最近」窗口 |
| `verify.drill_dsn` | 空 | 空就是关。不设 `verify.drill_enabled`，两个开关会漂 |
| `verify.drill_every_sec` | 604800 | 7 天 |
| `verify.drill_check_sql` | 空 | 操作员可选的校验 SQL。空则只比 `gtid_executed` |

演练 DSN 按源密码的同一套方式保护（不明文进日志）。这一份不读它。

## 10. 所有权接缝

工人（不在这一份）每个 `verify.every` 对任务做：

```text
ok, err := owner.Allows(ctx, taskID)
if err != nil || !ok {
    跳过
    不开 finding，也不关
    不更新 last_verified_at
}
report := assurance.Verify(ctx, dir, rows, objects, chain, opt)
把 report 交给现有的 finding 出口（只写 Allows 通过的任务）
消失的键用 Disappeared 的结果去关，原因 EVAL
```

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

`last_verified_at` **先不加列**。纯函数把它放在返回值里。没有工人去写的列是死的。工人落地时再加一条只加不改的迁移（建议 `000011`，可空 `DATETIME(6)`，挂在 `binlog_files` 上）。v0.5.x 的 schema 下限保持 6。ADR 0011 自己的迁移也用过 000011 这个序号的设想；谁先合并谁占用序号，另一边顺延。这一份不占序号。

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
| ADR §3 | 抽样 5% | 按段名的固定槽轮换，5% 即每 20 天一轮 | 决定 5。同样的成本，两次内容检查的间隔有上界 |
| ADR §3 | 每任务 `verify_budget` | 进程级一把预算，默认等于单任务上限 | 决定 6。多任务相加会打满磁盘 |
| ADR 决定 4 | 抽样加上「上一轮之后新上传的全部」 | 加上最近 24 小时 | 决定 4。纯函数没有游标；默认周期也是 24 小时 |
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
| `TestVerify_CorruptObjectStaysOpenUntilRehashed` | QA F1：旧对象在它的槽那天报 `OBJECT_CORRUPT`；第二天只 HEAD，不关、不推进；传入开着的 finding 时离槽也哈希；修好后重查关掉 |
| `TestVerify_CancelLeavesUnreachedSubjectsUnverified` | QA F3：第二个对象打开时取消，只有第一段的 finding 能关，其余没走到的既不关也不推进，链不算查过；本地解析途中超时，什么也没覆盖 |
| `TestVerify_LastVerifiedOnlyForContentChecks` | QA F4：只 HEAD、没有摘要、保留期删掉、未上传丢失，都不推进；未上传丢失开 `I6_MISSING_LOCAL` |
| `TestDisappeared_ClosesOnlyWithTheDetectingMethod` | 第 6.2 节的表，逐格 |
| `TestVerify_FutureUploadedAtIsClockSkew`、`TestRecentUpload_Window` | QA F5：10 年后的 `uploaded_at` 开 `UPLOAD_CLOCK_SKEW`、不下载；24 小时边界、1 小时容差、时区 |
| `TestRotation_BoundsTheGap` | 2000 个名字一年：每个名字恰好每 20 天一次，每天约 5%，同一天内稳定 |
| `TestVerify_ConcurrentPassesShareOneBudget` | 两轮并发共用一只预算：各自的 `Bytes` 等于单轮，累计等于两轮之和（`-race` 下跑） |
| `TestVerify_ChargesEveryReadToTheBudget`、`TestByteBudget_BurstIsCapped` | 解析、位点第二遍、对象正文都计费；HEAD 不计；突发上限 64 KiB |
| `TestVerify_DigestSourcesAndBackfillCount` | 本地摘要优先；本地坏了用目录行；两边都没有是 `UNVERIFIABLE`；两个计数 |
| `TestApplySealedUpload_FillsEmptySHA256`、`TestApplySealedUpload_FillKeepsStoredEventTimes` | 行上没有摘要时补上；已有摘要和已有事件时间不被盖掉 |

变异：`go run ./internal/assurance/mutants/kill.go` 跑 QA 的 25 个（M01–M25）加覆盖模型的 29 个（N01 起），除标明等价的 N30 外全部被打死。

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

`workflow_dispatch` 的 `e2e_profile` 选项加上 `verify`。这一份改不了 workflow，所以 A4 只能是 PARTIAL：单测每个 PR 都跑，周期任务和真实 MinIO 不在 CI 里。

## 15. 留给 ADR 0011 PR-2 的清单

1. 实现 `Allows`（第 10 节），只从 PR-2 的过渡结果判断当前主人。
2. 一个周期循环调用 `Verify`，把 finding 交进现有出口，用 `Disappeared` 关闭。`per_node` 下不是目录所在节点的进程不跑。每轮把上一轮还开着的 finding 放进 `Options.OpenFindings`。
3. 同一改动里把 `health.Produced` 的 `chain.objects` 和 `verify` 设为 true，并给 `VERIFY_FAILED`、`OBJECT_CORRUPT`、`UPLOAD_CLOCK_SKEW`、`DRILL_FAILED`、`DRILL_REFUSED` 补注册表项。对象类 I6 落库时 subject 用 `ObjectSubject`。
4. 加上第 9 节的配置键和第 8 节的指标。
5. 迁移：可空的 `binlog_files.last_verified_at`。序号让给先合并的那一份迁移。
6. 和复核器合成一把字节预算；复核器跳过本轮已经哈希的段。
7. 阶段 5 的还原计划到位之后再开演练。演练默认仍关。
8. 按第 14 节给 `e2e.yml` 加 `e2e-verify`。在那之前 A4 保持 PARTIAL。
9. 摘要回填：迁移 `000008` 之前封存的行 `sha256` 为空。一个限速的后台任务趁本地文件还在，从本地文件算出摘要写回目录行（和第 11 节上传时的补法相同，已有摘要不动），并导出 `binlog_server_verify_digest_backfill_pending`（`Report.NoDigestLocal`）和 `binlog_server_verify_objects_unverifiable`（`Report.UnverifiableNoDigest`）两个 gauge。保留期删掉本地文件之后就补不了了，这些对象只剩 HEAD。
10. 目录行被删掉的段，它的巡检 finding 由工人用明确的「行已删除」事实关闭；纯函数不会因为看不到它就关。

## 16. 已定的问题（原开放问题）

1. `OBJECT_CORRUPT` 单列，不并进 `VERIFY_FAILED`：处置不同。对象字节不符可以用本地副本重新上传；本地或目录行不符要人查。ADR 0009 §3 的表已经改成这样。
2. 进程级 4 MiB/s 作为默认。多任务之间公平分摊放到 PR-2 的周期循环里做，免得一个大任务把别的任务推到下一个周期。
3. 演练等阶段 5 的还原计划，不在巡检器里复制一份回放命令。
