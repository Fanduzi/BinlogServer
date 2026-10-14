# ADR 0009 阶段 1 设计：备份链 (Backup Chain)

状态：设计（2026-10-14）。上位决定：[ADR 0009](../adr/0009-backup-assurance.md)「分阶段实施」第 1 行。本文只回答阶段 1 的**类型、不变量、模块接缝**和 PR 切分；不改变 ADR 的任何决定。

## 1. 要解决什么

今天「这个任务存了什么、连续吗、每段在哪」有三份各自推导的答案：

| 谁 | 读什么 | 推导什么 |
|---|---|---|
| `tasks/window.go`（611 行） | 目录行 + 段内容 | 断点（缺文件、GTID 洞、切换洞、段内洞、未上传、不可读）、时间窗、GTID 集合 |
| `tasks/storage_health.go` + `stored_hole.go` + `legacy_failback.go` | 段账本 + 切换事件 | 损坏 / 旧版回切 / 已存缺口，三选一 |
| `/replay`（`SelectReplayFiles` + `replayCut`） | 目录行 + 健康结论 | 回放哪些段、在哪截断 |

I4（重叠）、I6（三处一致）、I7（上传完整）今天**没有任何检查**。阶段 1 把这些推导收进一个深模块 `internal/assurance`，让三处调用方只读它的结果。

## 2. 模块与接缝

```
binlog  (Ledger, GTIDSet, ClassifySegment, ReadSegmentContents)
   ▲
assurance  ── 只依赖 binlog；不 import tasks、meta、upload，不做网络 I/O
   ▲
tasks  ── 适配器 chain_inputs.go：BinlogFile → assurance.Row，事件 → assurance.Switch
   ▲
api / replication（不直接用 assurance）
```

- **接缝在 `assurance.Inputs`**：调用方把「此刻的事实」作为数据交进来（目录行、对象清单快照、checkpoint、起点集合、切换记录），本地段内容通过 `*binlog.Ledger` 读取（沿用账本的 size+mtime 缓存）。
- 与 ADR 草图的差异：ADR 写的是 `ObjectLister` 接口。阶段 1 只有一个实现（巡检器在阶段 3 才出现），按「一个适配器 = 假想接缝」原则，先收**快照数据** `Objects map[key]size`（`nil` = 未列举，I6 对象部分不判定）。阶段 3 巡检器负责何时列举、如何缓存；`assurance` 永远不发网络请求，测试不需要假对象存储。
- 删除测试：删掉 `assurance`，I1–I7 的判定会重新散落到 window / health / replay / 巡检四处——它是深模块，不是转发层。

## 3. 类型

```go
package assurance

type Inputs struct {
    Flavor     string           // "mysql" | "mariadb" | ""（= mysql）
    StartGTID  string           // B：任务起点集合
    Checkpoint Checkpoint       // C
    Order      []string         // 链：按还原顺序的段磁盘名（阶段 1 由 tasks.SelectReplayFiles 给出）
    Rows       []Row            // 全部目录行（含不在链上的）
    Local      *binlog.Ledger   // 任务目录；nil = 本进程没有该目录
    Objects    map[string]int64 // 对象 key → 大小；nil = 未列举
    ObjectStorage bool
    UploadGrace   time.Duration // ≤0 时取 DefaultUploadGrace = 15min（ADR 决定 2）
    Switches   []Switch         // 每次继续的换源：新身份与记录的已存集合
}

type Row struct {            // binlog_files 的无依赖投影
    Name, Source string; Epoch int64; Sealed bool
    Size int64; UploadState, Checksum, ObjectKey string
    CreatedAt, SealedAt time.Time
}

type Segment struct {        // 链上一段
    Name  string; Named binlog.SegmentName
    Row   *Row               // nil = 没有目录行
    Local bool; LocalSize int64
    Object *int64            // nil = 未列举或不存在（见 ObjectListed）
    Contents *binlog.SegmentContents // nil = 不可读（ReadErr 说明原因）
    ReadErr error
}

type Kind string             // 封闭枚举，调用方按种类判断，不看 Detail 前缀
const (
    I1Regression Kind = "I1_REGRESSION"
    I2IndexGap = "I2_INDEX_GAP"; I2SwitchUnrecorded = "I2_SWITCH_UNRECORDED"; I2OrderInvalid = "I2_ORDER_INVALID"
    I3GTIDHole = "I3_GTID_HOLE"; I3StoredHole = "I3_STORED_HOLE"; I3LegacyFailback = "I3_LEGACY_FAILBACK"
    I4Overlap = "I4_OVERLAP"
    I5CheckpointAhead = "I5_CHECKPOINT_AHEAD"
    I6MissingLocal = "I6_MISSING_LOCAL"; I6MissingObject = "I6_MISSING_OBJECT"
    I6OrphanRow = "I6_ORPHAN_ROW"; I6MissingRow = "I6_MISSING_ROW"; I6SizeMismatch = "I6_SIZE_MISMATCH"
    I7UploadLate = "I7_UPLOAD_LATE"; I7Unverified = "I7_UNVERIFIED"
)

type Violation struct {
    Kind     Kind
    Segments []string // 涉及的段，第一个是首个违反点
    Evidence string   // 机器可比的证据（GTID 集合、大小、位点）
    Detail   string   // 给人看的一句话
}

func ChainOf(in Inputs) *Chain                 // 不做 I/O 以外的工作；读取延迟到问题被问时
func (c *Chain) Segments() ([]Segment, error)  // 只有任务目录不可读才是错误
func (c *Chain) Check(now time.Time) []Violation
func WindowOf(segs []WindowSegment, flavor string, objectStorage bool, startGTID string) (Window, error) // PR P1-2，见 §8
```

ADR 枚举之外新增三个子类，原因：`I2_SWITCH_UNRECORDED`（跨源身份但没有切换记录，ADR I2 的第二句）、`I3_LEGACY_FAILBACK`/`I3_STORED_HOLE`（ADR：「旧版回切与已存缺口 = I3 的两个子类」）、`I6_MISSING_ROW`（ADR I6「`row(S)` 存在」的反面；与 `I6_ORPHAN_ROW` 方向相反）。

## 4. 不变量的判定（`Check` 的规格）

| # | 判定 | 证据 | 首个违反点 |
|---|---|---|---|
| I1 | 段内 `end_log_pos` 倒退或 Rotate 指向自己/更早文件（`binlog.Ledger.SegmentDamage`，即今天的 `segmentDamage`） | 倒退的两个位点或 Rotate 目标 | 该段 |
| I2 | 同一源前缀相邻两段序号不相邻（`missingIndexBreak`）；前缀变化处没有对应 `Switch`；`I2_ORDER_INVALID`：调用方给的顺序漏掉本地账本里的段（例如整段回切 stint），或同一前缀内序号后退 | 缺的文件名 / 漏掉或后退的段 | 后一段 / 漏掉的段 |
| I3 | 段内洞（`intraSegmentHole`）、相邻段洞（`gtidHole`）、切换洞（`switchHole`）、链内部洞（`StoredHoles(B ∪ Stored)`）；已存缺口与旧版回切由 tasks 的现有规划器给出结论后作为输入（阶段 1 不搬这两个规划器，见 §6） | 缺的 GTID 集合 | 洞之后第一段 |
| I4 | 两段 GTID 集合相交；同一 `(源, 序号)` 有两个封存段都在 `Order` 上 | 交集 | 后一段 |
| I5 | `C.gtid_set ⊄ B ∪ Stored ∪ 头部历史`（仅 MySQL）；`C.file` 是打开尾部且 `C.pos` > 尾部落盘截断点；`C.file` 是链上封存段且 `C.pos` > 该段末尾（本地大小，否则行大小）；`C.file` 是链上同前缀最后一个源文件之后的文件 | 超前的集合 / 位点 | 打开尾部 / 所指封存段 / 链上最后一段 |
| I6 | 封存段无行（MISSING_ROW）；行在、本地无、对象无（ORPHAN_ROW）；行说 LOCAL/UPLOAD_FAILED 但本地无（MISSING_LOCAL）；行说 UPLOADED 但对象清单里没有（MISSING_OBJECT）；对象大小或本地大小 ≠ 行大小（SIZE_MISMATCH）。`Objects == nil` 时对象相关判定跳过 | 行与实物 | 该段 |
| I7 | 配置了对象存储时，封存早于 `now − UploadGrace` 且不是 `UPLOADED+match`（UPLOAD_LATE）；`UPLOADED` 但 `checksum ≠ match`，或本地已删而不是 `UPLOADED+match`（UNVERIFIED） | 状态与年龄 | 该段 |

规则：
- `Check` 是纯函数：同样的 `Inputs` 与 `now` 给出同样的、按链顺序排列的结果。
- 不可读的段不产生 I1/I3/I4 违反（没有证据），只影响窗口（今天的 `breakUnreadable` 语义不变）。
- MariaDB：I3/I4/I5 的 GTID 判定跳过（今天 window 只对 mysql 做 GTID 判定，保持一致）。

### 4.1 阶段 1 没有覆盖的部分（如实记录）

阶段 1 的 `Check` 比 ADR 0009 的不变量窄。下表列出缺口和补上的阶段：

| 不变量 | 阶段 1 不判定的内容 | 由哪个阶段补上 |
|---|---|---|
| I2 | 不检查前一段是否以 `Rotate(name(S_{i+1}), 4)` 结尾、后一段是否以 FDE 开头；跨身份只看「有没有该身份的 `Switch` 记录」，不比对切换记录的 GTID 集合与切换位置；普通前缀改变（如 `mysql-bin` → `other-bin`）不算切换 | 阶段 2（链顺序由 assurance 自己从目录行与 `.source-chain` 推导时，一并读 Rotate/FDE 并核对切换记录） |
| I4 | 被替换的旧 epoch 在目录行上没有「已替换」标记，所以只能按 GTID 相交和同一 `(源, 序号)` 两个封存段报告；段不可读时只能按名字查重 | 阶段 2（目录行加替换标记，与链顺序一起落地） |
| I5 | 阶段 1 已覆盖 ADR 的「C.file/pos 不超过截断点」：打开尾部、封存段末尾、链之后的文件（#348 QA 后补上）；仍不判定 checkpoint 落在链之前的文件（落后不是超前，属于恢复点目标），也不判定 MariaDB 的 GTID 位置 | 阶段 3（评估器的 RPO 与 MariaDB GTID） |
| I6 | 「已按保留期删除」没有标记，未上链的行只按「本地无且对象无」报孤行；`Objects == nil` 时跳过链外行 | 阶段 2（保留期删除写标记） |
| I7 | 默认 grace 15min（`DefaultUploadGrace`，未设置即取它） | — |

#350/#351 QA 补充的缺口：

| 范围 | 阶段 1 的行为 | 由哪个阶段补上 |
|---|---|---|
| 跨前缀 / 跨 stint 的顺序 | `I2_ORDER_INVALID` 只检查「本地段都在顺序上」和「同一前缀内序号不后退」。整个回切 stint 被排到前面时不报顺序错误，只会冒出 I3 | 阶段 2（assurance 自己从 `.source-chain` 推导 stint 顺序） |
| 只在桶里的段 | 没有本地字节，`Contents` 为空：不做 I1、段内/相邻 I3、GTID 版 I4；I4 只能按名字判定（同一源序号的两个封存段一律视为重叠）；内部洞照常计算，但该段的 GTID 不算已存 | 阶段 2（用 000008 的 sha256/首尾时间，或按需读对象） |
| 读不到的本地段 | 没有任何证据：`checkInteriorHole` 直接跳过，Health 不看它；`/window` 报 `segment is not readable` | 阶段 2（单独的 Kind，带读错误） |
| 单源任务的 GTID 内部空洞 | 已存缺口告警只对换过服务器的任务生效（与 v0.5.64 相同），单源任务的空洞（例如源库 `gtid_next` 跳号 363-381）只出现在 `/window` 的缺口里，没有告警或 finding | 阶段 3（评估器把内部洞记成 finding），见 #352 |
| 没有 GTID 的第二个封存副本 | MySQL 上只有两段都带 GTID 时才用 GTID 相交判 I4；任一段没有 GTID（垃圾字节、只有 magic、只有文件头、全零）时改比事件位点，读不出位点就按名字报 I4（#354 QA 第 2 轮的 P2 回归）。不判定这样的副本本身是否损坏，那是 I1 | — |

同一源文件的多个 epoch 首尾相接（failback fixture 里 `mysql-bin.000003` 存 1156-1255、`.sealed.e1` 存 1256-1555、`.open.e1` 存 1556-1605）不是 I4。两段都可读时，只有 GTID 相交（MySQL，且两段都有 GTID）或事件位点区间重叠时才报 I4；有一段读不到时仍按名字报。`SelectReplayFiles` 有意丢弃的低 epoch `.open` 段，不算「不在顺序上」。

调用方给出的顺序在阶段 1 被当作事实，但 `I2_ORDER_INVALID` 会自检两件事：本地账本里的每一段都在顺序上，同一前缀内序号不后退。

## 5. 不变式与行为冻结

- 阶段 1 是行为冻结的重构：`/window`、`/replay`、存储告警的输出与 main `e39b6a9` 逐字节一致。证明方式与 ADR 0006/0007 相同：**先**在当前实现上录 golden（不改代码，测试通过），**再**搬实现，golden 必须仍然通过。
- 已有 golden：`internal/binlog/testdata/ledger/golden.json`（33 例，账本）。P1-2 新增 window golden（同一组 11 种目录变化 × {无对象存储, 有对象存储} × {mysql} + 目录行状态变化），P1-3 新增 health/replay golden。
- 每个 PR 第一提交是失败测试（新行为）或 golden + 结构测试（重构：断言 `tasks` 里不再有自己的推导函数）。

## 6. PR 切分

| PR | 内容 | 第一提交（红） | 不做 |
|---|---|---|---|
| P1-0 | 本设计文档 | — | — |
| P1-1 | `internal/assurance`：`Inputs/Row/Segment/Violation`、`ChainOf`、`Segments`、`Check`（I1–I7）；`binlog.Ledger.SegmentDamage` 导出；纯 GTID 判定函数从 `window.go` 复制为 `assurance` 的实现（P1-2 删除 `window.go` 的副本） | 每种 `Kind` ≥ 2 个构造目录用例（含 #223 重叠、#209 孤行、#173 未验证），桩实现返回空 → 全红 | 不接线，不改任何现有行为 |
| P1-2 | `Chain.Window()`；`Scheduler.RecoveryWindow` 改为构造 `Inputs` 后调用它；删除 `window.go` 里的推导函数 | 先录 window golden（绿），再加结构测试「`tasks` 不定义 `assessRecoveryFrom/gtidHole/...`」（红） | 健康与回放选择 |
| P1-3 | 存储健康成为 `Check` 的视图（损坏 = I1，旧版回切/已存缺口 = I3 子类，规划器结论作为输入）；`/replay` 截断读链 | 先录 health/replay golden，再加结构测试 | 规划器本身的算法 |
| P1-4 | 目录行增加 `first_event_at`、`last_event_at`、`sha256`（迁移 `000008`，只加可空列）；封存时写入；旧行为 NULL | 「封存后目录行带首尾时间与 SHA-256」测试红 | 覆盖查询（阶段 4） |

P1-4 的 schema 下限（Adrian 已决定，2026-10-14）：不提到 8。`000008` 与 `000007` 一样是只加列、可选的迁移，`minRequiredSchemaVersion` 保持 6。代码必须容忍三列缺失：封存时写新列遇到 MySQL 1054（未知列）就回退到不带新列的写法并在进程内记住，读取时缺列就当作 NULL。schema 6/7 的进程照常读写；跑过 `000008` 的库被旧二进制读写也不受影响（列可空，无默认值依赖）。

## 7. 风险

- **顺序**：链顺序今天由 `SelectReplayFiles`（按源前缀的最早创建时间排序）决定，`Ledger.Segments()` 只按序号排序。阶段 1 让 `Order` 由调用方给出，`assurance` 不重新排序，避免在冻结重构里引入第二个顺序。把选择与排序收进链是 P1-3 之后的工作。
- **成本**：`Check` 读每段内容一次（账本缓存）；对象清单是快照，`assurance` 不列举。

## 8. 补记（P1-2 实现时的调整）

原计划是 `Chain.Window()` 从 `Inputs` 读字节。实现时发现 `/window` 经 `OpenTaskSegment` 读分段，字节可能来自持有该任务的另一个进程，而 `Chain` 的 `Local` 只是本机 `Ledger`。为了保证 golden 逐字节不变、又不给 `Inputs` 再加一个只有一个实现的读取接口，P1-2 把推导做成纯函数 `assurance.WindowOf([]WindowSegment, ...)`：tasks 负责读字节、投影行，并在之后附加损坏截断；assurance 负责全部规则。`Chain` 在 P1-3 拿到回放顺序后，可以用同一个 `WindowOf` 提供 `Chain.Window()`，不会出现第二份推导。
