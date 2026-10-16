# internal/assurance Module

备份链（ADR 0009 阶段 1，设计见 `docs/design/0009-phase1-backup-chain.md`）。只依赖 `internal/binlog`，不做网络 I/O。

## Files
- `chain.go`: `Inputs`（链顺序、全部目录行、任务目录账本、对象清单快照、checkpoint、起点集合、已记录的换源、tasks 规划器已得出的 I3 子类结论）→ `ChainOf` → `Chain`。`Segments()` 按还原顺序给出每段的目录行、本地是否在与大小、对象是否列举与大小、段内容。`Check(now)` 给出 I1–I7 违反：封闭枚举 `Kind`、涉及的段（第一个是首个违反点）、证据和一句说明；按链顺序排列，同样的输入给出同样的结果。`Objects == nil` 时不判定对象相关的 I6；未配置对象存储时不判定 I7；MariaDB 不做 GTID 判定。P2-3 起 I4 按 replay 顺序判定时包含 open 副本：open 副本只有在内容相同（GTID 相交，或一个文件的字节是另一个的前缀）时才算重复，延续的新 epoch 不算；空残留 open 段（只有 magic、只有头事件、或不足一个事件头且无完整事件，且不是顺序上最后一段）给 INFO `I4_EMPTY_LEFTOVER`，不参与 I4，也不是 `Health()` 结论，replay 不截断。 `Inputs.NoCatalog`（standalone 无 meta）时没有目录行不算 I6_MISSING_ROW。
  - #348 QA 后：`UploadGrace` 未设置（≤0）时取 `DefaultUploadGrace`（15min）。I5 还覆盖 checkpoint 指向链上最后一个同前缀源文件之后的文件、以及 pos 超过所指封存段末尾（本地大小，否则行大小），位置判定不限 MySQL。新增 `I2_ORDER_INVALID`：调用方给的顺序漏掉本地账本里的段（例如整段回切 stint），或同一前缀内序号后退。链上段既无目录行也无本地文件报 `I6_MISSING_ROW`；本地大小与对象大小都不等于行大小时报 `I6_SIZE_MISMATCH` 与 `I6_OBJECT_SIZE_MISMATCH` 各一条（P2-5.1 起对象大小不等是独立 kind；本地大小只在行已封存、`.seal-sha256` 无该段记录（旧段）时与行比较，账本有记录时以账本为准，计划中有 legacy failback 时不报，设计 0009 §20.2）；UPLOADED 行 `object_key` 为空报 `I6_MISSING_OBJECT`；内部 GTID 洞逐个区间报告，只跳过邻段判定已在同一后继段上报过的那个。
- `gtid.go`: I3 的 GTID 洞判定（段内、相邻源文件、换源处），从 `tasks/window.go` 搬来，现在是唯一一份，`Check` 和 `WindowOf` 共用。
- `window.go`: `WindowOf(segs, flavor, objectStorage, startGTID)`，`GET /window` 的唯一推导：最早/最晚 UTC 事件时间、MySQL 已存 GTID 集合和缺口（同一源文件名前缀内缺序号、相邻源文件或换源处的 GTID 洞、段内洞、已存序号中间的空缺、校验和 mismatch、配了对象存储时封存分段 `UPLOAD_FAILED`/`LOCAL_ONLY`、读不到）。起点集合算作已存。字节由调用方读好传入（tasks 经 `OpenTaskSegment`，可能来自另一个进程），损坏截断由调用方加。

## Exports
- `Inputs`、`Row`、`Switch`、`Checkpoint`、`Segment`、`Violation`、`Kind`（`AllKinds`）、`ChainOf`、`Chain.Segments`、`Chain.Check`、`DefaultUploadGrace`。

## Dependencies
- Upstream: 尚无（P1-2 起 `internal/tasks`）。
- Downstream: `internal/binlog`。

## Update Rule
- 新增或改变一种 `Kind` 的判定时，更新本文件、设计文档 §4，并在 `chain_test.go` 加至少两个构造目录用例。
- `health.go`: `Chain.Health()` 是存储健康结论：先看 `Ledger.Damage`（I1，带检查点、起点集合和 flavor），再按 `Inputs.Plan` 给出的顺序取第一个 `I3_LEGACY_FAILBACK` / `I3_STORED_HOLE`；分段已损坏时不调用 `Plan`，`Plan` 最多调用一次（`Check` 也会把它的结论附上）。其它 Kind 不构成结论，保持 ADR 0009 之前的行为。`KeepBefore(links, segment)` 是 /replay 和 /window 的截断：保留排在问题分段之前、且不是同一源序号副本的分段；问题不指名分段时什么也不保留。链顺序和代际比较仍由调用方给出（阶段 1）。
  - #348 QA 第 2 轮：顺序自检跳过同一源文件里低于最高 open epoch 的 `.open` 段（`SelectReplayFiles` 有意丢弃）；同一源序号的两个封存段只有在一段读不到、GTID 相交（MySQL）或事件位点区间重叠时才报 `I4_OVERLAP`，首尾相接的 epoch 不算。
- `I1_UNREADABLE`：封存段事件 size 损坏，`Check` 报出，`Health` 在损坏（I1）之后、规划类之前给出该判定，回放和窗口截在它之前（#361）。
- `Health` 的 I4（#370）：在规划类之后，按 `Inputs.Order`（tasks 给的回放顺序）找同一源文件的第二个封存副本，判定与 `Check` 共用 `sealedRepeats`，只算两份都在本地目录的情形；问题段是后一个副本，`KeepBefore` 把两份及其后的分段都截掉。
