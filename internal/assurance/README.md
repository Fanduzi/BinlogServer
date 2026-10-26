# internal/assurance Module

备份链（ADR 0009 阶段 1，设计见 `docs/design/0009-phase1-backup-chain.md`）和阶段 3 巡检的纯函数（`docs/design/0009-phase3-verifier.md`）。链只依赖 `internal/binlog`。巡检另外用 `golang.org/x/time/rate` 按字节限速；对象的 HEAD/GET 由调用方注入，本包不拨号。

## Files

P2-5.4（设计 0009 §26）：`Row.RetentionRemoved` 表示本地保留删过本地文件、行还在。有标记时不报 `I6_ORPHAN_ROW`；前缀完整且对象不在时改报 `I6_MISSING_OBJECT`。前缀未列举完时两者都不报（链上与链下）。
- `chain.go`: `Inputs`（链顺序、全部目录行、任务目录账本、对象清单快照、checkpoint、起点集合、已记录的换源、tasks 规划器已得出的 I3 子类结论）→ `ChainOf` → `Chain`。`Segments()` 按还原顺序给出每段的目录行、本地是否在与大小、对象是否列举与大小、段内容。`ChainSteps` / `ResetChainSteps` 数 `Segments` 里的段访问，测试用来区分线性走查和按段嵌套的走查。`Check(now)` 给出 I1–I7 违反：封闭枚举 `Kind`、涉及的段（第一个是首个违反点）、证据和一句说明；按链顺序排列，同样的输入给出同样的结果。`Objects == nil` 时不判定对象相关的 I6；未配置对象存储时不判定 I7；MariaDB 不做 GTID 判定。P2-3 起 I4 按 replay 顺序判定时包含 open 副本：open 副本只有在内容相同（GTID 相交，或一个文件的字节是另一个的前缀）时才算重复，延续的新 epoch 不算；空残留 open 段（只有 magic、只有头事件、或不足一个事件头且无完整事件，且不是顺序上最后一段）给 INFO `I4_EMPTY_LEFTOVER`，不参与 I4，也不是 `Health()` 结论，replay 不截断。 `Inputs.NoCatalog`（standalone 无 meta）时没有目录行不算 I6_MISSING_ROW。
  - #348 QA 后：`UploadGrace` 未设置（≤0）时取 `DefaultUploadGrace`（15min）。I5 还覆盖 checkpoint 指向链上最后一个同前缀源文件之后的文件、以及 pos 超过所指封存段末尾（本地大小，否则行大小），位置判定不限 MySQL。新增 `I2_ORDER_INVALID`：调用方给的顺序漏掉本地账本里的段（例如整段回切 stint），或同一前缀内序号后退。链上段既无目录行也无本地文件报 `I6_MISSING_ROW`；只有头部事件的 open 文件不算 I4 副本（同一源文件的每份副本都以相同的头开始；#405：legacy failback 修复后续写的 open 文件在第一个事务前被误判为第二份副本）。本地大小与对象大小都不等于行大小时报 `I6_SIZE_MISMATCH` 与 `I6_OBJECT_SIZE_MISMATCH` 各一条（P2-5.1; P2-5.3 (§22): Switches suppress cross-stint I3; CheckCheckpoint separate from CheckBody 起对象大小不等是独立 kind；本地大小只在行已封存、`.seal-sha256` 无该段记录（旧段）时与行比较，账本有记录时以账本为准，计划中有 legacy failback 时不报，设计 0009 §20.2）；UPLOADED 行 `object_key` 为空报 `I6_MISSING_OBJECT`；内部 GTID 洞逐个区间报告，只跳过邻段判定已在同一后继段上报过的那个。
- `gtid.go`: I3 的 GTID 洞判定（段内、相邻源文件、换源处），从 `tasks/window.go` 搬来，现在是唯一一份，`Check` 和 `WindowOf` 共用。
- `window.go`: `WindowOf(segs, flavor, objectStorage, startGTID)`，`GET /window` 的唯一推导：最早/最晚 UTC 事件时间、MySQL 已存 GTID 集合和缺口（同一源文件名前缀内缺序号、相邻源文件或换源处的 GTID 洞、段内洞、已存序号中间的空缺、校验和 mismatch、配了对象存储时封存分段 `UPLOAD_FAILED`/`LOCAL_ONLY`、读不到）。起点集合算作已存。字节由调用方读好传入（tasks 经 `OpenTaskSegment`，可能来自另一个进程），损坏截断由调用方加。

## Exports
- `Inputs`、`Row`、`Switch`、`Checkpoint`、`Segment`、`Violation`、`Kind`（`AllKinds`）、`ChainOf`、`Chain.Segments`、`Chain.Check`、`DefaultUploadGrace`。
- `verify.go`: 阶段 3 巡检的纯函数（设计 `docs/design/0009-phase3-verifier.md`）。列出来的名字 stat 得到「不存在」（悬空符号链接）按缺文件处理，未上传则 `LOCAL_MISSING`，不计入失败的 stat。逐文件 `LOCAL_UNREADABLE` 收成任务级时，未上传且已是 CRITICAL 或还没 recovering 的从 CRITICAL 起；拆回逐文件时带上任务级的 severity 和 detail，任务级以 `split into per-file findings` 关闭，整目录恢复仍是 `local files stat again`。`Verify` 读目录行、本地封存段和对象 HEAD/GET，返回 `Report`：`Coverage` 按段记下这一轮对本地副本和对象各做了什么（`PARSED`、`STAT`、`ABSENT`、`MISSING`、`HASHED`、`HEAD`、`UNVERIFIABLE`、`NOT_APPLICABLE`、`ERROR`、`SKIPPED`）以及原因，没走到的副本是零值；本轮应当开着的 finding；只对按内容验过的段推进的 `VerifiedAt`；本轮自己的字节数；`UnverifiableNoDigest` 与 `NoDigestLocal` 两个计数。`Closures`（`Report.Closed`）只关「这一轮用能发现该 kind 的方法查过这个 subject」的 finding，并带原因（含「local removed; object matches catalog」和「row removed」，后者不关链的结论；本地已删的 `VERIFY_FAILED` 要对象边哈希边解析通过才关），`Disappeared` 是它的简写。HEAD 不计预算；本地解析和对象哈希排进同一个队列（强制重查（写回 `Report.Read` 作 `Options.LastRead` 时每周期一次）、最近档（改动之后还没验过；每轮最多 `min(RecentSharePercent × C, C − F − S)`，默认 25%，1–100 以外报错，`CheckRecentSharePercent`）、轮换槽（每周期一次）、最久没验过（改动之后没验过的按从没验过排在最前）、末档（轮换关掉时改动之后没验过的段，只花剩下的）），放不下的记 `SKIPPED`，本地读不了报 `LOCAL_UNREADABLE`（未上传的连续两次失败升 CRITICAL，连续两次干净读才关，每个 stat 都同样失败时合成一条任务级的），对象正文短于 HEAD 记 `ERROR`，截止时间到了正常结束，只有取消才返回错误；`Report.Bytes` 只记付过的字节。轮换长度按周期数（`Options.RotationPeriods`、`InRotation`、`WorstGap`），`RecentUpload` / `ClockSkewed` 管最近窗口和时钟偏差。`ByteBudget` 按字节限速，可以被并发的多轮共用。调用方必须已经是该任务的主人；本文件不问租约。
- `verify_test.go`、`verify_coverage_test.go`、`verify_queue_test.go`、`verify_fresh_test.go`、`verify_r5_test.go`、`verify_qa_test.go`: 检出、比对、预算计费、覆盖模型、关闭表、取消与截止时间、轮换上界、队列顺序、反复短截止时间下的进度上界（设计 §5.3）、付费字节、最近档的新鲜度和份额、本地已删时对象边哈希边解析、行删除不关链的结论、HEAD 阶段的截止时间、末档与份额封顶、`LOCAL_UNREADABLE`、短正文、`sealed_at` 偏差、强制段每周期一次；`verify_qa_test.go` 是 QA r5 的后续（积压按从没验过排序、轮换关掉时从没验过的读一次、容差内的 `sealed_at`、`ReadMark` 每个字段、`LOCAL_UNREADABLE` 的升级、两次干净读和任务级合并）。`mutants/kill.go` 用这些测试打 194 个变异（3 个等价：N30、X05、R15，理由在文件里；H25 由取消轮不关未读 `LOCAL_UNREADABLE` 的测试打死）。ADR 0011 PR-3 另有 SM01–SM07：集群再丢掉 `I6_MISSING_LOCAL`、`OWNER_CHANGED` 关掉 `SEGMENT_MISSING`、未验证就写 `chain_owner`、不完整的 `chain.local` 关掉 `SEGMENT_MISSING`、MySQL 对每个持有者都写 `chain_owner`、读到目录却不置 `ChainVerified`、没读到目录也置 `ChainVerified`。

## Dependencies
- Upstream: 尚无（P1-2 起 `internal/tasks`）。
- Downstream: `internal/binlog`。

## Update Rule
- 新增或改变一种 `Kind` 的判定时，更新本文件、设计文档 §4，并在 `chain_test.go` 加至少两个构造目录用例。
- `health.go`: `Chain.Health()` 是存储健康结论：先看 `Ledger.Damage`（I1，带检查点、起点集合和 flavor），再按 `Inputs.Plan` 给出的顺序取第一个 `I3_LEGACY_FAILBACK` / `I3_STORED_HOLE`；分段已损坏时不调用 `Plan`，`Plan` 最多调用一次（`Check` 也会把它的结论附上）。其它 Kind 不构成结论，保持 ADR 0009 之前的行为。`KeepBefore(links, segment)` 是 /replay 和 /window 的截断：保留排在问题分段之前、且不是同一源序号副本的分段；问题不指名分段时什么也不保留。链顺序和代际比较仍由调用方给出（阶段 1）。
  - #348 QA 第 2 轮：顺序自检跳过同一源文件里低于最高 open epoch 的 `.open` 段（`SelectReplayFiles` 有意丢弃）；同一源序号的两个封存段只有在一段读不到、GTID 相交（MySQL）或事件位点区间重叠时才报 `I4_OVERLAP`，首尾相接的 epoch 不算。
- `I1_UNREADABLE`：封存段事件 size 损坏，`Check` 报出，`Health` 在损坏（I1）之后、规划类之前给出该判定，回放和窗口截在它之前（#361）。
- `Health` 的 I4（#370）：在规划类之后，按 `Inputs.Order`（tasks 给的回放顺序）找同一源文件的第二个封存副本，判定与 `Check` 共用 `sealedRepeats`，只算两份都在本地目录的情形；问题段是后一个副本，`KeepBefore` 把两份及其后的分段都截掉。
