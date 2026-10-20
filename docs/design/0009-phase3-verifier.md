# ADR 0009 阶段 3 设计：巡检器 (Verifier)

状态：设计（2026-10-20）。上位决定：[ADR 0009](../adr/0009-backup-assurance.md) 第 3 节与「分阶段实施」第 3 行。前置是已落地的备份链（`internal/assurance` 的 `Chain` / `Check` / `Window`）和阶段 2 的健康评估器。本文只回答巡检器**查什么、怎么限速、结论叫什么、配置和迁移是什么、还原演练为什么默认关、以及它怎样挂到任务主人上**。不改 ADR 0009 的决定；和 ADR 原文或阶段 2 草图不一致的地方写在第 12 节，并给出理由。

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
- 用假对象存储的单测覆盖：删一个对象、改对象一个字节、改本地一个字节，下一轮都报出来；干净的链没有 finding；预算真的卡住读取。

这一份不做（等 ADR 0011 PR-2 给出「当前主人」之后再接）：

- 周期循环、按任务分摊、把 finding 写进 `task_findings`。
- 把 `health.Produced` 里的 `chain.objects` / `verify` 改成 true。那个测试锁着「阶段 3 之前健康不会因为这两个 scope 变成 UNKNOWN」。现在翻开会让还没跑巡检的进程把任务标成 UNKNOWN。
- 还原演练的进程、DSN 和 MySQL。
- 真实 MinIO 场景进 CI。场景脚本在 `scripts/e2e/smoke-verify.sh`，现有 workflow 不会选中它。要进 CI 必须改 workflow，本仓库的 token 改不了 workflow。改法写在第 14 节。

## 3. 核心决定

1. **巡检是一次纯计算，不是一个新的写者。** 输入是这一刻的目录行、本地字节和对象字节，输出是 finding 集合和每个通过的段的 `last_verified_at`。同一份输入给出同一份输出。谁来周期调用它、谁把结果落库，是主人接缝后面的事。理由：阶段 2 已经有唯一的 finding 生命周期（`Reconcile`）。再做一个会写 `task_findings` 的循环，就会在 ADR 0011 换掉单调性之前多出一个写者。
2. **只有当前主人为这个任务跑巡检。** 纯函数自己不问。调用方先问第 10 节的缝；缝说不是，或者缝出错，就跳过这个任务，**既不开也不关** finding。理由：本地字节在主人那台机器上（ADR 0011 §3.3）。一台没有目录的机器跑结构检查，会把「文件不在我这儿」写成损坏。缝出错同样不能关 finding：关需要一份完整观察，观察失败不是「问题消失了」。这和 ADR 0011 §3.5「不完整的观察既不开也不关」是同一条。
3. **三条检查各报各的种类，不合成一个 `VERIFY_FAILED`。** 结构失败是 `VERIFY_FAILED`，对象不在是 `I6_MISSING_OBJECT`，对象大小不符是 `I6_OBJECT_SIZE_MISMATCH`，对象字节不符是 `OBJECT_CORRUPT`。理由：处置不同。对象没了要查桶的生命周期；字节不符是静默损坏；本地解析失败是这段备份不能读。ADR 原文把对象「不符」笼统写成这三个名字里的 `VERIFY_FAILED`。阶段 2 草图已经单列了 `OBJECT_CORRUPT`。沿用草图的名字，避免一条 finding 盖住两种事故。`I6_SIZE_MISMATCH` 不拿来用：那个名字在代码里是「旧段的本地大小和目录行不一致」，和对象大小不是一件事。
4. **HEAD 每个已上传段；SHA-256 只抽一部分。** 已上传段每次都 HEAD（存在和大小）。下载并算 SHA-256 的是：最近 24 小时上传的全部，加上其余段里稳定的 5%。理由：HEAD 便宜，能抓住误删和截断。全量下载的流量随数据量线性涨，ADR 已经拒绝作为默认。24 小时而不是「上一轮之后」，是因为纯函数看不到上一轮的游标；`verify.every` 的默认就是 24 小时，两者对齐。操作员把周期改短时，24 小时比「上一轮」多查而不是少查。
5. **抽样按段名和周期取哈希，不掷骰子。** `sha256(段名 + "|" + 周期起点)` 落在百分比内才下载。同一周期内重复跑，抽中的是同一批，结论不会闪。下一个周期换一批。理由：真随机会让同一次损坏这次报、下次不报，告警来回开关。固定的 5% 又会让另外 95% 永远不被读到。按周期旋转是这两头的中间。
6. **字节预算是整个进程一份，不是每个任务一份。** 默认 4 MiB/s，和现有 `health.rehash_bytes_per_sec` 同一个数。理由：ADR 写的是「每任务每轮上限」。100 个任务各 4 MiB/s 就是 400 MiB/s，QA 要求的「限速下源延迟不升高」做不到。一个任务可以吃满整个预算，多个任务分享它。复核器今天已经是全局限速；接上主人之后，巡检和复核器用同一只桶，不加倍。在那之前复核器不动，避免在所有权改写的同时改评估器的读盘行为。
7. **没有记录过的 SHA-256，且本地文件已经不在，内容对比跳过，不开 finding。** HEAD 和大小对比照做。理由：把「旧行没有摘要」报成损坏，会在升级后的第一轮把保留期删掉的历史段全部打成 CRITICAL。新上传的段由第 11 节补上摘要。旧段只靠 HEAD，直到有人回填。
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

解析失败（CRC、截断、坏的事件长度）本身就是 `VERIFY_FAILED`，不再继续比摘要。一段只开一条结构 finding，按上面的顺序取第一条。严重度 CRITICAL。subject 是段名。

位点用现有的 `EventSpan` 规则（连续事件链上最后一个 `end_log_pos > 0`）。它会再读一遍文件；这一遍的字节数事先计入预算。不把 `EventSpan` 改成流式接口，避免为了巡检去动续传读法。

GTID 摘要不加列。链的 I3/I4 已经在段与段之间比 GTID。结构检查要回答的是「这个文件还是封存时的那个文件，并且读得动」。摘要列要在封存路径上写，而封存路径不在这一份的范围内。调用方如果已经有文本，可以传进来比；不传就不比。

## 5. 对象检查

对象存储没配（调用方没有传入对象读取器）时，这一轮**不判定**对象，也不关闭已有的对象 finding。关掉「存储被拆掉」是阶段 2 已经写过的 CONFIG 事实，要由工人显式给出，不能从「这次没传读取器」推断。

对每个 `upload_state = UPLOADED` 且 `object_key` 非空的封存行：

1. HEAD。对象不在 → `I6_MISSING_OBJECT`。其它错误（超时、503）→ 本轮对象 scope 不完整：已经 HEAD 成功并确认的问题可以留下，**不关闭**任何对象 finding。大小和行不一致 → `I6_OBJECT_SIZE_MISMATCH`。
2. 抽中的段再 GET，流式算 SHA-256，计入预算。本地封存文件还在且这一轮结构解析成功：和本地摘要比。否则和行里的 `sha256` 比。两边都没有：跳过内容，不开 finding（决定 7）。不符 → `OBJECT_CORRUPT`，证据写明比的是本地还是目录行。
3. GET 途中对象消失，按缺失报。其它读错误让对象 scope 不完整。

抽样：`SampleObject(段名, uploaded_at, now, recent, period, percent)`。`uploaded_at` 为空不算「最近」，否则没有时间戳的历史行会被全部下载。`uploaded_at` 在未来（时钟偏了）算最近，避免漏掉新上传。percent 为 0 时按 5。100 为全量。负数为「旧对象一个都不下」，最近窗口仍然全下。

## 6. 链检查，以及消失的结论

调用方把已经建好的 `*Chain` 交进来时，跑 `Chain.Check(now)`，每条 `Violation` 变成一条 finding：kind 用违反的名字（`I1_REGRESSION` 等），subject 是第一个段，`I4_EMPTY_LEFTOVER` 是 INFO，其余 CRITICAL。没传入链，链 scope 这一轮不完整，不关闭链上的 finding。

本地目录列不出来时，结构 scope 和链 scope 都不完整。链的结论描述的是本地字节；目录都列不出，不能用调用方手里那份可能过期的链去关 finding。

关闭是纯函数 `Disappeared(上一轮的 finding, 这一轮的报告)`：上一轮有、这一轮没有、且该 kind 所属 scope 这一轮是完整的，才算消失。kind 的 scope：

| kind | scope |
|---|---|
| `VERIFY_FAILED` | 本地结构 |
| `I6_MISSING_OBJECT`、`I6_OBJECT_SIZE_MISMATCH`、`OBJECT_CORRUPT` | 对象 |
| 其它（链的违反名） | 链 |

同一段同一 kind 只留一条。对象检查和 `Chain.Check` 如果都看到同一个缺失对象，合并成一条。工人落库时，对象类 I6 要写成健康注册表已经在用的 subject：段名加 `#object`（`health.ObjectSubject`）。纯函数返回的 subject 是段名，因为链的违反用的是段名；`#object` 是健康层的键，不是巡检层的键。这个映射留给工人，避免这一份去改健康注册表。

`last_verified_at` 只打在这一轮**没有** finding 点名、且它依赖的 scope 都完整的封存段上。失败的段不更新，否则 `verify_age` 会在损坏期间看起来是新鲜的。没被抽中做 SHA-256 的段，只要结构和 HEAD 过了，仍然更新：没抽中不是失败。

## 7. 字节预算

`NewByteBudget(bytesPerSec)`。`<= 0` 表示不限速（测试和「显式不限」）。限速用已经在用的 `golang.org/x/time/rate` 令牌桶，突发等于速率，但不超过 64 KiB，这样大段不会一次借走一整秒以上的额度。读取按实际读到的字节等待；单次等待不超过桶的突发，大文件拆开等。

计入预算的是：结构解析读到的字节、位点第二遍事先计入的文件大小、对象 GET 的正文。HEAD 不计入，它不搬段数据。上下文取消时等待返回，这一轮中止，不关 finding。

默认 4 MiB/s 是进程级上限（决定 6）。配置键是 `verify.budget_bytes_per_sec`。接上工人之前，复核器仍用它自己的 `health.rehash_bytes_per_sec`，两只桶互不影响。工人落地时合成一只，并让复核器跳过本轮巡检已经哈希过的段。现在不合成：复核器挂在评估器上，评估器的所有权判断正是 ADR 0011 要换掉的那一层。

## 8. Finding 与指标

这一份只**返回** finding，不注册进 `health.Registry`，也不打指标。名字先定死，工人按这个名字注册：

| kind | 严重度 | 何时开 | 何时关 |
|---|---|---|---|
| `VERIFY_FAILED` | CRITICAL | 封存段解析失败，或和目录行的大小、SHA-256、位点、可选 GTID 文本不一致 | 下一次完整的结构检查不再报这段 |
| `I6_MISSING_OBJECT` | CRITICAL | UPLOADED 段 HEAD 不在 | 下一次完整的对象检查 HEAD 到了，且大小相符 |
| `I6_OBJECT_SIZE_MISMATCH` | CRITICAL | HEAD 的大小和 `size_bytes` 不同 | 下一次完整的对象检查大小相符 |
| `OBJECT_CORRUPT` | CRITICAL | 抽中的对象 SHA-256 与本地或目录行不同 | 下一次抽中它并且相符，或它不再是 UPLOADED |
| `DRILL_FAILED` | CRITICAL | 演练回放后的 `gtid_executed` 与计划不符，或校验 SQL 失败 | 下一次演练通过。演练没开时不关、也不开 |
| `DRILL_REFUSED` | WARNING | 演练 DSN 的 host:port 等于任务源地址 | 两者不再相同，或 DSN 被清空 |

链上的 kind 不新造名字，用 `Chain.Check` 已有的封闭枚举。它们的开关规则仍是阶段 2 注册表里的那一份；巡检只是把 `Check` 的结果交出去。

指标（工人落地时加，名字和 ADR §2.2 对齐）：

| 名字 | 类型 | 含义 |
|---|---|---|
| `binlog_server_task_last_verified_timestamp{task_id}` | gauge | 最近一次**完整**巡检的 Unix 时间。有 finding 也更新。scope 不完整不更新 |
| `binlog_server_verify_bytes_total{task_id,source}` | counter | `source` 为 `local` 或 `object` |
| `binlog_server_verify_pass_incomplete{task_id,scope}` | gauge | 0/1。scope 为 `local`、`object`、`chain` |
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
| ADR §3 | 对象不符 → `VERIFY_FAILED` | 字节不符用 `OBJECT_CORRUPT` | 和第 3 节决定 3 相同；草图已经用了这个名字 |
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
| `TestVerify_CleanChainHasNoFindings` | 阶段 1 的 live 夹具加上与字节一致的行和对象，`Chain.Check` 与结构、对象检查都为空，并且预算被计入 |
| `TestByteBudget_IsRespected` | 1000 字节/秒读 2500 字节，耗时被桶卡住 |
| `TestVerify_RecentUploadIsHashedWhenSampleIsOff` | 百分比为「旧对象不下」时，1 小时内的上传仍会因一个字节被报 `OBJECT_CORRUPT` |
| `TestVerify_OldObjectOutsideSampleIsNotRead` | 48 小时前的对象在同一设置下只 HEAD，不 GET |
| `TestApplySealedUpload_FillsEmptySHA256` | 行上没有摘要时，上传落库补上 `000008` 的 SHA-256；已经有的摘要不被重算盖掉 |

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
2. 一个周期循环调用 `Verify`，把 finding 交进现有出口，用 `Disappeared` 关闭。`per_node` 下不是目录所在节点的进程不跑。
3. 同一改动里把 `health.Produced` 的 `chain.objects` 和 `verify` 设为 true，并给 `VERIFY_FAILED`、`OBJECT_CORRUPT`、`DRILL_FAILED`、`DRILL_REFUSED` 补注册表项。对象类 I6 落库时 subject 用 `ObjectSubject`。
4. 加上第 9 节的配置键和第 8 节的指标。
5. 迁移：可空的 `binlog_files.last_verified_at`。序号让给先合并的那一份迁移。
6. 和复核器合成一把字节预算；复核器跳过本轮已经哈希的段。
7. 阶段 5 的还原计划到位之后再开演练。演练默认仍关。
8. 按第 14 节给 `e2e.yml` 加 `e2e-verify`。在那之前 A4 保持 PARTIAL。

## 16. 开放问题

1. `OBJECT_CORRUPT` 这个名字是否就按阶段 2 草图定下来？ADR 原文把对象字节不符写进 `VERIFY_FAILED`。这里分开，是为了处置时分得清。
2. 进程级 4 MiB/s 是否足够？它和复核器的默认相同。多任务分享时，一个大任务会把别人的巡检推迟到后续周期。这是故意的：推迟巡检比打满磁盘好。`verify_age` 的宽限是两个周期，推迟一个周期仍然健康。
3. 演练和阶段 5 绑在一起，是否接受「A4 的演练半边在还原计划之前一直不做」？ADR 的阶段表把演练放在阶段 3，计划放在阶段 5。先做演练就要在巡检器里复制一份回放命令，阶段 5 再删。这里选择等计划。
