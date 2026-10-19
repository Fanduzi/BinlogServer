# ADR 0009 阶段 2 设计：健康评估器 (Health Evaluator)

状态：设计（2026-10-15，按 BinlogServerQA 对 b8530d01、25ed843e 三轮评审修订，见 §16），未实现；开放问题按建议默认值处理，见 §15（Adrian 可推翻）。上位决定：[ADR 0009](../adr/0009-backup-assurance.md)「分阶段实施」第 2 行，epic #326。前置：[阶段 1 备份链](0009-phase1-backup-chain.md)（已发布于 v0.5.64–v0.5.66）。本文回答阶段 2 的**类型、生命周期、触发与清除规则、集群语义、迁移**和 PR 切分；不改变 ADR 的任何决定，只在 §12 记录与 ADR 草图的差异。

## 1. 要解决什么

阶段 0/1 让失败**被看见**：`Scheduler.Report` 写 `task_findings`，`assurance.Chain.Check` 给出 I1–I7。但「谁负责关闭一条 finding」今天没有答案：

| 现状 | 后果 |
|---|---|
| 16 种 `FindingKind`，`Report`/`Resolve` 散落在十几个调用点（scheduler、runner、metrics、storage_health、watchdog） | 每加一种 kind 就要再找一个「成功路径」去 `Resolve`；漏了就永远 open |
| `SEGMENT_UNREADABLE`、`SEGMENT_OVERLAP` 只在 `storageHealthOf` 里 `Report`，从不 `Resolve` | 条件清除后告警清了，finding 仍 OPEN（#370/#372 QA 已知缺口） |
| `Resolve` 只对「本进程 book 里 open 的 key」生效 | 重启或换 owner 后，存储里的 open 行没人能关（#366：STOPPED 任务的 `DUMP_STALLED` 挂 40s+） |
| `Check` 的 I2/I3/I5/I6/I7 违反没有 finding | 单源 GTID 洞只在 `/window`（#352）；对象丢失、上传迟到无告警 |
| ADR 决定 F「没有数据 = 不健康」没有实现 | 评估停了与「一切正常」在指标上无法区分 |
| 没有通知 | 用户必须盯着 Console |

阶段 2 的单一目标：**每一条 finding 的打开与关闭都由一个评估器根据「此刻的事实」决定**；API、指标、Console、通知读同一份结果。

## 2. 核心决定（摘要）

1. **一个评估器，期望状态调和（desired-state reconcile）**。评估器对一个任务算出「此刻应当 open 的 finding 集合」`Desired`，与已存的 open 集合比较，差集产生 `Open`/`Close` 转移。**只对标为 `Managed` 的 kind 生效**：各 PR 逐个接管 kind，未接管的 kind 保持 v0.5.66 的 `Report`/`Resolve` 生命周期，评估器不碰（§4、§5.6）。全部接管后没有代码路径直接写 finding 行。
2. **两类输入，同一个出口**。条件型（链、任务状态、lease）由评估器自己读；事件型（一次写失败、一次读超时）由热路径调用 `Signal`（非阻塞，只记事实，不写 finding），评估器把事实变成 finding。现有 `Report`/`Resolve` 降为 `Signal` 的包装，最后在 P2-4 改为包内私有，并由 lintguard 禁止新调用。
3. **作用域（scope）防误关**。一次评估只关闭它**本轮有完整输入**的 kind；输入不可得（对象清单未列举、目录读失败、meta 不可读、任务行与 lease 表不一致）时这些 kind 的状态是 `Unknown`，保持原样并让任务健康变为 `UNKNOWN`。信号型 kind 只有在**已有 open 行**时，于本进程首次成功前是内部的 `Unknown`（不关闭，也不让健康变 UNKNOWN，§5.5）。
4. **触发 = 标脏 + 合并**，周期 sweep 只是兜底：所有触发点只把任务标脏并附原因，单 worker 按任务合并执行；sweep 每 60s 把本节点负责的任务全部标脏。触发与 sweep 走同一个 `evaluate(task)`，不存在两套逻辑。
5. **幂等键 (task_id, kind, subject)** 不变；转移带单调 `generation`，存储写入是条件写（新 generation 或同 generation 才覆盖），重复、乱序、重放都收敛到同一行。
6. **每种 kind 绑定一条清除规则**（§6 表）：kind 注册表是封闭的，注册项里写明 invariant、severity、scope 和 `ClearRule`；没有清除规则的 kind 不能注册（编译期表 + 单测）。
7. **集群：评估权跟随任务 lease**。只有持有 `(owner, epoch)` 的节点评估该任务；栅栏只比较**任务 lease epoch**（`_health` 的 epoch 是另一个计数器，从不与之比较），写入前在同一事务里 `SELECT … FOR UPDATE` 显式比较并与 `task_leases` 交叉校验（§8）。#360 作为 P2-6 的前置一并修掉。无人持有的任务由 `_health` 持有者评估 `task.state`/`store`；非共享存储下链 scope 委派给最后 owner，委派绑定**任务最后一个 lease epoch + 节点 id**。
8. **健康是 finding 的视图**：`task_health` 表 + `binlog_server_task_healthy{task_id}` 每个任务总有序列；评估超过 `health.stale_after`（默认 5×sweep）没跑 = `UNKNOWN` = 不健康；meta 不可用时指标层用缓存输出 0（§7.2）。
9. **通知 = 转移的 outbox**：只在转移（打开、升级、关闭）时通知，带去抖与静音（mute），升级首轮合并为摘要；单节点（`_notify` lease）至少一次投递 webhook，按 `(key, generation, transition)` 去重。
10. **迁移只加不改**：`000009` 给 `task_findings` 加可空列、新建 `task_health` 和 `finding_transitions`；v0.5.x 中 `minRequiredSchemaVersion` 保持 6，缺表缺列时退回 v0.5.66 行为并报 `HEALTH_STORE_DEGRADED`；**v0.6.0 把下限提到 9 并删除无栅栏回退**（§15 D1）。

## 3. 模块与接缝

```
binlog ◀── assurance (Chain, Check)          纯，阶段 1
               ▲
            health   (新包，纯：KindSpec 注册表、Facts、Desired、Reconcile、Health)
               ▲
            tasks    evaluator.go：触发队列、sweep、收集 Facts、调用 health、写 Store、lease 栅栏
               ▲
     api (alerts、health、metrics)   notify (outbox → webhook)   ui (告警页)
```

- **`internal/health` 是深模块**：输入是「一个任务此刻的事实」，输出是期望 finding 集合、转移列表和健康结论。不做 I/O，不 import `tasks`/`meta`。删掉它，清除规则会重新散回十几个调用点——这正是要消灭的状态。
- `tasks.FindingKind`/`Finding` 类型**搬到 `health`**，`tasks` 保留类型别名一个版本（`type FindingKind = health.Kind`），避免一次改动 api/meta/replication。
- `assurance` 不变；评估器把 `Chain.Check(now)` 与 `Chain.Health()` 的结果作为 `Facts.Chain` 交给 `health`。

## 4. 类型与签名

```go
package health

// Kind 是封闭集合；每个 Kind 必须在 Registry 里有一项。
type Kind string

type Severity string // CRITICAL | WARNING | INFO

// Scope 是一组 kind 共享的输入来源。一轮评估只对 Complete 的 scope 关闭 finding。
type Scope string

const (
    ScopeChainLocal   Scope = "chain.local"   // 任务目录 + 目录行（账本可读）
    ScopeChainObjects Scope = "chain.objects" // 对象清单快照（Objects != nil）
    ScopeTaskState    Scope = "task.state"    // 任务行、run、lease
    ScopeSignals      Scope = "signals"       // 热路径事实；每个信号在本进程首次成功后才 complete（§5.5）
    ScopeStore        Scope = "store"         // 元数据库能力（schema、只读）
    ScopeVerify       Scope = "verify"        // 阶段 3 巡检器，阶段 2 预留
)

type ClearRule struct {
    Invariant string // "I4" / "S2" / "lease" ...，给人和告警页看
    // Holds 在本轮事实下判断「该条件已不成立」。Desired 不含该 key 且 Holds 为真才关闭；
    // 对条件型 kind，Holds 通常就是「Desired 里没有」，对信号型 kind 是「成功事实晚于失败事实」。
    Holds func(f *Facts, key Key) bool
    Text  string // 一句话：什么时候会自动关闭（API 的 clears_when）
    // HoldDown: 条件消失后至少持续这么久才关闭（去抖），默认 0。从 Facts.AbsentSince[key]
    // （评估器自己的注入时钟）起算，从不使用库里的 last_seen 墙钟。
    HoldDown time.Duration
}

type KindSpec struct {
    Kind     Kind
    Severity Severity
    // Scopes: 该 kind 依赖的全部 scope。关闭/更新要求全部 complete；Derive 在第一个 complete 时运行。
    // 例：SEGMENT_MISSING/ORPHAN_ROW = {chain.local, chain.objects}；DUMP_STALLED、SOURCE_UNREACHABLE =
    // {task.state, signals}；UPLOAD_UNVERIFIED = {chain.local, signals}（P2-1 的 TestRegistryScopesMatchDesign 对照 §6）。
    Scopes   []Scope
    Clear    ClearRule
    // Derive 从事实算出该 kind 此刻应当 open 的实例（subject → detail/evidence）。
    Derive func(f *Facts) []Instance
    // Segments 为真表示 subject 是段名，阶段 4/5 可按段与时间窗求交。
    SegmentScoped bool
    // Managed 为真时评估器拥有该 kind 的打开与关闭；为假时 Reconcile 对它不产生任何转移，
    // 生命周期仍是旧的 Report/Resolve。每个 PR 只翻转自己接管的 kind（§13）。
    Managed bool
    // Version 随 Derive/Clear 语义变化递增；写进 finding_transitions，审计时可还原当时的 clears_when。
    Version int
    // Waivable 为真时允许按 evidence 豁免（目前只有 GTID_HOLE，§7.1）。
    Waivable bool
}

var Registry = map[Kind]KindSpec{ /* §6 */ }

type Key struct {
    TaskID  string
    Kind    Kind
    Subject string
}

type Instance struct {
    Key
    Severity Severity // 可覆盖 KindSpec 默认（例如 I7 迟到 > 24h 升级为 CRITICAL）
    Detail   string
    Evidence string   // 机器可比；变化本身不产生转移，只更新行
    Segments []string // 涉及的段（来自 assurance.Violation.Segments）
    FactAt   time.Time // 事实时间（信号发生时刻）；零值 = Facts.Now
}

// Facts 是一个任务在一个时刻的全部输入。字段为 nil 表示该来源本轮不可得。
type Facts struct {
    TaskID     string
    Now        time.Time
    Task       *TaskFacts          // 状态、desired、owner、epoch、run 是否在跑、进入当前状态的时间、last_error 类别；
                                   // 任务行 (owner, epoch) 与 task_leases 不一致时为 nil（task.state 不完整）
    Waivers    []Waiver            // 按 evidence 的豁免（§7.1）
    Chain      *ChainFacts         // assurance.Check 与 Health 的结果；nil = 目录或账本不可读
    Objects    bool                // Chain 里的对象判定是否基于已列举清单
    Signals    map[SignalKey]SignalState
    Store      *StoreFacts         // schema 版本、可选列/表是否存在、是否只读
    Complete   map[Scope]bool
    AbsentSince map[Key]time.Time // 评估器首次看到条件消失的时间（注入时钟），HoldDown 从此起算；缺失 = 刚刚
}

type ChainFacts struct {
    Violations []assurance.Violation
    Verdict    *assurance.Violation // Chain.Health() 的结论（回放截断点）
}

// 信号：热路径只记录「最后一次失败」与「最后一次成功」，不做 I/O。
type SignalKey struct {
    Kind    Kind
    Subject string
}
type SignalState struct {
    FailureSeq  uint64    // 进程内单调序号，不用墙钟（§5.5）
    SuccessSeq  uint64    // 零值 = 本进程启动后还没成功过 → 已有 open 行时内部 Unknown，不关闭
    Failures    int64
    FactAt      time.Time // 最近一次失败的事实时间，只用于展示与通知
    Detail      string
}

type Mute struct {
    TaskID, Kind, Subject string // Kind "*" = 任务级
    Reason, Actor         string
    StartedAt, Until      time.Time
}

type Waiver struct {
    Key
    Evidence string    // 豁免的确切证据，例如 GTID 区间 "uuid:363-381"
    Reason   string
    Actor    string
    At       time.Time
}

type Transition struct {
    Key
    Op         Op       // OPEN | UPDATE | ESCALATE | DOWNGRADE | CLOSE（豁免关闭 = CLOSE + Reason WAIVED）
    Reason     string   // EVAL | UPGRADE | OWNER_CHANGED | WAIVED | NODE_GONE；UPGRADE 不投递通知（§9）
    FactAt     time.Time // 事实发生时间（信号失败/成功时刻），通知与时间线按它排序（§7.4）
    KindVersion int
    Generation int64
    At         time.Time
    Instance   Instance // CLOSE 时为最后一次 open 的内容
}

// Reconcile 是唯一的生命周期函数：纯、幂等。
//   - Desired 有、open 没有          → OPEN
//   - 两边都有，severity 升高        → ESCALATE；detail/evidence 变化 → UPDATE（不通知）
//   - open 有、Desired 没有、scope Complete、Clear.Holds 且已过 HoldDown → CLOSE
//   - 所需 scope 不全 Complete       → 不动（Produced 中的非信号 scope 计入 Health.Unknown）
//   - 严重度降低                     → DOWNGRADE（显式，不是静默 UPDATE）
//   - 同一 key 的重复已存行         → 只产生一条转移
//   - kind 不是 Managed              → 永不产生转移（open 行原样保留，由旧路径负责）
//   - open 必须是该任务的完整 open 集合（调用方分页读完，§5.6）
// 同样的 (open, facts) 给出同样的转移；对上一轮输出再调用一次给出空列表。
func Reconcile(open []Finding, f *Facts) ([]Transition, Desired)

func Desire(f *Facts) Desired // 对 Registry 逐项 Derive，只含 Complete scope 的 kind

type Status string // HEALTHY | DEGRADED (仅 WARNING) | UNHEALTHY (有 CRITICAL) | UNKNOWN
type TaskHealth struct {
    TaskID      string
    Status      Status
    Reasons     []Key          // 决定状态的 finding；UNKNOWN 时为不完整的 scope
    EvaluatedAt time.Time
    Generation  int64
    Owner       string
    OwnerEpoch  int64
}
// Produced：本阶段有评估器产生的 scope（阶段 2 = chain.local、task.state、signals、store）；
// chain.objects、verify 在阶段 3 Verifier 落地前不使 HealthOf 变 UNKNOWN。
var Produced map[Scope]bool

func HealthOf(open []Finding, f *Facts) TaskHealth
```

`Finding`（存储行）在 v0.5.66 字段之外增加：

```go
type Finding struct {
    // ... TaskID, Kind, Subject, Severity, Detail, FirstSeen, LastSeen, Count, ResolvedAt（不变）
    Evidence    string    `json:"evidence,omitempty"`
    Segments    []string  `json:"segments,omitempty"`
    Generation  int64     `json:"generation"`
    OwnerEpoch  int64     `json:"owner_epoch"`
    EvaluatedAt time.Time `json:"evaluated_at"`   // 最近一次评估确认它仍 open 的时间
    Invariant   string    `json:"invariant"`      // 由 Registry 填；finding_transitions 存 kind_version 以便审计
    ClearsWhen  string    `json:"clears_when"`    // 由 Registry 填
    Waived      *Waiver   `json:"waiver,omitempty"`
    Mute        *Mute     `json:"mute,omitempty"`  // 来自 task_mutes 的生效记录（key 级或任务级），只读视图
}
```

`tasks` 侧：

```go
type Evaluator struct { /* 队列、sweep、store、lease 视图、signal book */ }

type Reason string // SEAL | UPLOAD | RETENTION | TRANSITION | LEASE_ACQUIRED | LEASE_LOST |
                   // SIGNAL | CATALOG | STARTUP | SWEEP | API | VERIFY(阶段 3)

func (e *Evaluator) MarkDirty(taskID string, r Reason)      // 非阻塞，可在 s.mu 内调用
func (e *Evaluator) Signal(taskID string, kind health.Kind, subject string, ok bool, detail string) // 非阻塞
func (e *Evaluator) EvaluateNow(ctx context.Context, taskID string) (health.TaskHealth, error)    // API/测试
func (e *Evaluator) Run(ctx context.Context, sweep time.Duration)

type HealthStore interface {
    // ApplyTransitions 在一个事务里（§8.3）：
    //   1. SELECT owner, epoch FROM task_leases WHERE task_id=? FOR UPDATE，必须等于 fence；
    //   2. SELECT fence_epoch, fence_node, generation FROM task_health WHERE task_id=? FOR UPDATE，
    //      stored.fence_epoch > fence.Epoch → ErrFenced；相等但 fence_node 不同 → ErrFenced；
    //   3. generation 在本事务内分配：g = task_health.generation + 1（唯一分配点，§5.4），
    //      重放按内容判定（transition_digest 相同则空操作），upsert finding 行、追加 finding_transitions、写 task_health。
    //   ts 里的 Generation 由调用方留空，返回值给出分配结果。
    ApplyTransitions(ctx context.Context, taskID string, fence Fence, ts []health.Transition, h health.TaskHealth) error
    // ListOpenFindingsForTask 分页读完该任务的全部 open 行（keyset 分页，无上限），只给评估器用。
    ListOpenFindingsForTask(ctx context.Context, taskID string) ([]health.Finding, error)
    ListFindings(ctx context.Context, q FindingQuery) ([]health.Finding, error)
    ListHealth(ctx context.Context) ([]health.TaskHealth, error)
    PendingNotifications(ctx context.Context, after int64, limit int) ([]TransitionRow, error)
    AckNotification(ctx context.Context, id int64) error
}
var ErrFenced = errors.New("health write fenced by a newer owner epoch")

// Fence 永远是任务 lease 的 (epoch, node)。_health lease 只决定「谁来评估无主任务」，
// 它的 epoch 不写进 finding 行，也不参与比较。
type Fence struct {
    Mode      FenceMode
    TaskEpoch int64  // 集群：任务 lease epoch（无主任务取最后一次）；standalone/never-leased：0
    Node      string // 持有者、被委派的最后 owner，或 standalone 节点 id
    Instance  string // standalone：进程实例 id（启动时生成，写入 task_health.fence_instance）
    Delegated bool   // 非共享存储下最后 owner 评估 chain.*（§8.2）；只能写 chain.* kind
}

type FenceMode string
const (
    FenceCluster    FenceMode = "cluster"    // 需要 task_leases 行（§8.3 第 1 步）
    FenceStandalone FenceMode = "standalone" // 无集群 lease（含 standalone + meta_dsn）：不查 task_leases，按 (node, instance) 比较
    FenceNeverLeased FenceMode = "never"     // 集群模式下从未有 lease 行的任务（CREATED、从未启动）：epoch 0，由 _health 持有者写
)

// ErrLeaseLagging：owner 与 epoch 都相同，只是 lease 已过期（续约迟到）。可重试，不是 ErrFenced（§8.3、R2-6）。
var ErrLeaseLagging = errors.New("lease expired but still ours; retry after renew")
```

## 5. 生命周期与触发

### 5.1 一次评估

```
evaluate(task):
  1. 读 lease 视图得到 Fence（§8）；本节点既不是任务 lease 持有者、也不是无主任务的评估者或被委派者 → 跳过
  2. 收集 Facts（Task、Chain=ChainOf(Inputs).Check/Health、Signals 快照、Store 能力），标记 Complete
  3. open := ListOpenFindingsForTask（分页读完，无上限；进程内缓存只在写成功后更新，缓存缺失时读库，§5.6）
  4. ts, _ := health.Reconcile(open, facts)；h := health.HealthOf(...)
  5. store.ApplyTransitions(fence, ts, h)；ErrFenced（显式比较得出，owner 或 epoch 不同，§8.3）→ 丢弃本轮并停止评估该任务，直到本节点再次获得该任务 lease 或被重新委派；`ErrLeaseLagging`（同 owner 同 epoch 仅过期）→ 保留转移，续约后重试，不停止评估。每个 sweep 都重新读 lease 视图，即使没有 LEASE_ACQUIRED 事件也能恢复（R2-6）
  6. 写失败 → 转移留在 pending，按 (task, generation) 有序重放，健康标 UNKNOWN(store)；standalone 下 pending 持久化进 `.findings.json`，meta 模式下进程重启会丢 pending，由下一轮调和从事实重算补齐（§7.4）
```

步骤 2 的读取（账本有 size+mtime 缓存）每轮 O(段数)；`Chain.Check` 在 1500 段的任务上 < 50ms（P2-3 有基准测试作为 QA 条件）。

### 5.2 触发点

| 触发 | 来源 | Reason |
|---|---|---|
| 段封存 / 封存事实写入 | `sealed_upload.go`、runner seal | SEAL |
| 上传成功、失败、回读校验 | `sealed_upload.go`、`scheduler_retry_upload.go` | UPLOAD |
| 保留期删除本地或对象 | retention | RETENTION |
| 任务状态转移提交 | `commitTransitionLocked` | TRANSITION |
| lease 获取 / 续约 / 同 owner 重新认领（epoch 不变）/ 丢失 / 换 epoch | `scheduler_cluster_lease.go` | LEASE_ACQUIRED（含 renew 与 reclaim）/ LEASE_LOST |
| 任何 `Signal` | 热路径 | SIGNAL |
| 目录变化（ListFiles 发现新增/消失文件，`.source-chain` 变化） | `disk_files.go`、sweep 内的目录指纹比较 | CATALOG |
| 进程启动、加载任务 | `app.go` | STARTUP |
| 周期 sweep（默认 60s，`health.sweep_interval`） | evaluator | SWEEP |
| `POST /api/tasks/{id}/health/evaluate` | api | API |

### 5.3 与周期 sweep 的调和

- 所有触发只调用 `MarkDirty`：把 `taskID → reasons` 放进脏集合（map，天然合并），唤醒 worker。sweep 就是对本节点负责的所有任务调用 `MarkDirty(id, SWEEP)`。**同一个函数，同一个结果**；触发只决定「多早」，sweep 保证「一定」。
- 每个任务同一时刻最多一轮评估（per-task 串行）；评估中再次标脏只置位，结束后再跑一轮。worker 数默认 4，任务间并行。
- 去抖：同一任务两轮评估间隔下限 1s（突发的 SEAL/UPLOAD 合并）。LEASE_LOST 不排队，立即停止该任务的评估。
- 文件系统外部改动（人手动挪走副本）没有触发源，由 sweep 的目录指纹（文件名+size+mtime 哈希）在 ≤1 个周期内发现——这是 #372 QA「移除副本后 finding 不关闭」的修复路径。
- 指标 `binlog_server_health_eval_lag_seconds{reason}`（标脏到完成）让「触发失效、只靠 sweep」可见。

### 5.4 幂等打开与关闭

- 键 `(task_id, kind, subject)`，subject 规范化：段名用 basename；无 subject 的 kind 用 `""`；集群级 finding 用节点 id（今天 `_cluster` 下的 `"sync"`/`"count"` 保留原 subject）。
- `generation`：**唯一分配点是 `ApplyTransitions` 事务**：在 `SELECT … FOR UPDATE` 锁住 `task_health` 行后取 `generation + 1`（行不存在则插入 1）。任何写者（持有者、`_health` 代写、被委派的最后 owner）都不从进程缓存取号，所以两个写者不会撞号。
- **重放按内容识别**：每条转移计算 `transition_digest = sha256(task, kind, subject, op, severity, evidence, fact_at)`，`finding_transitions` 上有 `(task_id, transition_digest)` 唯一索引；同 digest 再写是成功空操作，不同内容一定得到新 generation。generation 只用于排序，不再用于判定重放。
- 重复 OPEN 只更新 `last_seen`、`evaluated_at`、`count`；重开已关闭的行：`resolved_at=NULL`，`first_seen` 保留最早值，`finding_transitions` 记一条 OPEN（历史不丢，与 000007 语义一致）。
- CLOSE 写 `resolved_at=now`；对已关闭的行再 CLOSE 是空操作（条件 `resolved_at IS NULL`）。
- 信号型 kind 的 `Count` 是失败次数（`SignalState.Failures`），条件型 kind 的 `Count` 是确认 open 的评估轮数。

### 5.5 信号的完整性（重启不误关，也不误报）

- **Unknown 是内部状态，只对「存在 open 行的信号 key」生效**：已有 open 行、且本进程启动后还没观察到该信号成功 → 不关闭该行。它**不**进入 `Health.Unknown`：健康由那条 open 行本身决定（仍是 UNHEALTHY/DEGRADED），不会因为「没观察到」额外变成 UNKNOWN。
- 没有 open 行的信号 key 视为 complete。所以无 finding 时重启，第一轮就是 HEALTHY，`task_healthy` 不会掉到 0。
- 关闭规则是「成功晚于失败」。比较不用墙钟：失败事实以已存行的 `generation` 和本进程的单调序号表示；进程启动后观察到的第一次成功，必然晚于启动前写下的任何失败。跨节点不比较 `last_seen`（避免时钟偏差，R2-P3），`last_seen`/`first_seen`/`resolved_at` 一律由库的 `NOW(6)` 写入。
- meta 仍只读时重启，第一次写仍失败 → 只更新原行，不 CLOSE 再 OPEN，不翻转、不通知。
- **很少成功的信号有替代的关闭条件**（`ClearRule.Probe`）：

| Kind | 替代成功证明 |
|---|---|
| `UPLOAD_RECORD_FAILED` | 该段目录行已是 `UPLOADED`（读目录即可）；或上传已在配置中关闭 |
| `RETENTION_RECORD_FAILED` | 下一次保留期轮完整跑完（即使本轮无删除） |
| `OPEN_TAIL_CUT_FAILED` | 该 open 段已封存或已不存在 |
| `DISK_LIST_FAILED` | sweep 的目录指纹读取成功 |
| `EVENT_READ_FAILED` | sweep 内一次读事件探测（`LIMIT 1`）成功 |
| `METRIC_SOURCE_FAILED` | 下一次 scrape 成功计算该源 |
| `DUMP_CLEANUP_PERSIST_FAILED` | `pending_dump_cleanup` 读回一致 |
| 其他信号 | `POST /api/tasks/{id}/health/evaluate?probe=1` 触发一次显式健康检查（写探测、读探测） |

  仍无成功证明的 open 行，在 `health.signal_stale_after`（默认 24h）后降为 INFO（不关闭，不影响健康，告警页可见）。

### 5.6 Managed 与旧路径共存

- `KindSpec.Managed=false` 的 kind：Reconcile 不读也不写它们，`Report`/`Resolve` 照旧。P2-3 只把 `SEGMENT_UNREADABLE`、`SEGMENT_OVERLAP` 设为 Managed；P2-4/P2-5 逐个翻转。一个 kind 一旦 Managed，对它的 `Report`/`Resolve` 调用在同一 PR 里改为 `Signal`，lintguard 按 Managed 表检查，保证同一行只有一个写者。
- 评估器读 open 集合时**按任务分页读完**（keyset：`(task_id, kind, subject) > ?`，每页 500），不共用 `/api/alerts` 的 `limit`，也不按 `last_seen` 截断（#339 教训）。

### 5.7 读缓存与静默损坏

- 账本的 size+mtime 缓存只缓存「不可读」和事件索引，**不把「可读」当成证明**：段的 I1 判定在缓存命中时也会读事件头。
- 原地 bitrot 不改 size/mtime。阶段 2 加一个**异步**低速 sha256 复核器 `Rehasher`，与评估路径完全分开：
  - **全局预算** `health.rehash_bytes_per_sec`（默认 4 MiB/s ≈ 240 MiB/min，与任务数无关），令牌桶限速。
  - **跨任务轮转游标**：游标 `(task_id, segment)` 存于进程内并每轮持久化到单行表 `rehash_cursor`（与 `notify_cursor` 相同做法，只有一份），按任务、段顺序前进，回到起点为一个完整周期；新封存段排到队尾。
  - **低 I/O 优先级**：单 goroutine，Linux 上 `ioprio_set(IOPRIO_CLASS_IDLE)`（失败则只靠限速），读用 `posix_fadvise(DONTNEED)` 不污染页缓存。
  - 复核结果作为事实（`RehashFacts{segment, ok, at}`）写进进程内表，并 `MarkDirty(task, REHASH)`；评估只读这张表，**不在评估路径上读段字节做哈希**。所以单任务评估 <50ms 的基准不含哈希。
  - 不一致 → `SEGMENT_UNREADABLE`（evidence=两个哈希）；`sha256` 为 NULL 的旧段跳过并计入 `rehash_unavailable` 指标，不影响健康。
  - 只在本节点持有（或被委派）的任务上运行；对象侧复核仍属阶段 3 Verifier。

## 6. Kind → 清除规则

所有已发布的 16 种 kind 加阶段 2 新增的链 kind。Scope 决定何时有资格关闭。

| Kind | Sev | Scope | 打开条件（Derive） | 清除规则（invariant） |
|---|---|---|---|---|
| `SEGMENT_UNREADABLE` | C | chain.local | `Check` 有 `I1_UNREADABLE`，subject=段 | I1：该段可读，或已不在链上（被挪走/替换） |
| `SEGMENT_OVERLAP` | C | chain.local | 在 **replay 顺序**（`Inputs.Order`，即 /replay 实际会返回的段）上判定 I4，不按 open/sealed 过滤，**但排除空的残留 open 段**（见下一行）：`.open.eN` 与 `.sealed.eM` 内容相同或区间相交也算（#372 openstale/openhand），subject=后一副本 | I4：replay 顺序上同一源文件只剩一份，且无 GTID/位点相交 |
| `SEGMENT_DAMAGED`（新，原 storage_alert 损坏） | C | chain.local | `I1_REGRESSION` | I1：位点不倒退 |
| `CHAIN_INDEX_GAP`（新） | C | chain.local | `I2_INDEX_GAP` / `I2_SWITCH_UNRECORDED` / `I2_ORDER_INVALID` | I2：序号连续且每次换源有记录 |
| `GTID_HOLE`（新，**#352**） | C | chain.local | `I3_GTID_HOLE`（含单源任务内部洞）/ `I3_STORED_HOLE`，subject=洞后首段 | I3：`B ∪ Stored` 在该点连续；或存在 evidence 完全等于该洞区间的 **waiver**（§7.1，Derive 不再产生，健康恢复）。静音（mute）不关闭也不改变健康 |
| `LEGACY_FAILBACK`（新） | C | chain.local | `I3_LEGACY_FAILBACK` | I3：规划器不再给出该结论 |
| `CHECKPOINT_AHEAD`（新） | C | chain.local | `I5_CHECKPOINT_AHEAD` | I5：C ≤ 链截断点 |
| `SEGMENT_MISSING`（新） | C | chain.local / chain.objects | `I6_MISSING_LOCAL` / `I6_MISSING_ROW` / `I6_MISSING_OBJECT`(objects) / `I6_SIZE_MISMATCH` | I6：行、本地、对象三处一致；保留期删除标记（**#341**）视为一致 |
| `ORPHAN_ROW`（新） | W | chain.objects | `I6_ORPHAN_ROW` | I6：行有实物，或行带保留期删除标记 |
| `UPLOAD_LATE`（新） | W→C(>阈值，默认 24h，§15 D5) | chain.local | `I7_UPLOAD_LATE`，封存后 `UploadGrace`（默认 15min，tasks 必须显式传入，P2-5a）之内不判定 | I7：`UPLOADED+match` |
| `UPLOAD_UNVERIFIED` | C | chain.local + signals | 信号失败，或 `I7_UNVERIFIED` | I7：`checksum=match` |
| `UPLOAD_RECORD_FAILED` | C | signals | 信号 | S8：同一段后续上传记录写成功 |
| `DUMP_STALLED` | W | signals + task.state | 信号失败且 desired=RUN | S12：之后收到事件/心跳；**或** desired=STOP 且任务已 STOPPED（有意停止）；**或** 任务 lease 已到另一个 epoch（#366，旧 run 不再代表该任务）。因 STARTING/RETRY_BACKOFF 循环离开 RUNNING **不**关闭；若同时 `SOURCE_UNREACHABLE` 打开，则在其 OPEN 之后交接关闭（reason=HANDED_OFF），任何时刻至少一条 open |
| `SEGMENT_EMPTY_LEFTOVER`（新，#357） | INFO | chain.local | `.open.eN` 只有 magic（4 字节）、只有 FDE 头、或小于最小事件大小（19 字节头）且无完整事件；不参与 I4，**永不截断 replay**（/replay 仍按阶段 1 行为列出或丢弃） | 文件被移除，或被封存成有事件的段 |
| `TASK_FAILED`（新） | C | task.state | desired=RUN 且 state=FAILED，subject=失败类别（`STREAM_REGRESSION` #362、`SOURCE_ERROR_1236` #354、`PERMISSION_DENIED` #338、`CONFIG`、`OTHER` 等，由 `last_error` 分类；类别表封闭，未知归 `OTHER`） | 任务离开 FAILED（重启成功进入 RUNNING 并收到事件，或进入 STARTING 后由 SOURCE_UNREACHABLE 等承接），或 desired=STOP；类别变化 = 旧 subject 关闭、新 subject 打开 |
| `SOURCE_UNREACHABLE`（新） | C | task.state + signals | desired=RUN 且 last_error 类别为不可达（连接拒绝/超时/读超时），或在 RETRY_BACKOFF/STARTING 间循环累计 > `health.unreachable_after`（默认 90s，可按任务覆盖）；连续两轮评估都满足才打开（去抖）；计划重启用任务级 mute（`kind=*`，§7.1）：finding 照常打开、健康照常下降，只是不通知 | 一次 dump 成功建立且收到事件或心跳（成功信号）；或 desired=STOP 且 STOPPED |
| `TASK_UNADOPTED`（新，#385，见 §19） | C | task.state | 无任务库的 standalone，任务只存在于 `{data_dir}/{id}`（有分段），本进程没有它的记录 | adopt；或目录不再有分段（移出 data_dir） |
| `METADATA_WRITE_BLOCKED` | C | signals | 信号 | S2：之后一次 checkpoint/任务行写成功 |
| `TASK_PERSIST_FAILED` | C | signals | 信号 | S2/S3：同任务后续任务行写成功 |
| `EVENT_PERSIST_FAILED` | W | signals | 信号，subject=事件类型 | S1：同类型事件后续写成功 |
| `EVENT_READ_FAILED` | W | signals | 信号 | S13：重试预算读回成功 |
| `DUMP_CLEANUP_PERSIST_FAILED` | C | signals | 信号 | S3：KILL 标记写成功或已清除 |
| `DISK_LIST_FAILED` | C | signals | 信号（_cluster，subject=节点:目录） | S15：下一次列目录成功 |
| `RETENTION_RECORD_FAILED` | W | signals | 信号 | S5/S6：下一次保留期记录成功 |
| `RETRY_LOOP_FAILED` | W | signals | 信号 | S7：下一次完整重试轮 |
| `OPEN_TAIL_CUT_FAILED` | C | signals | 信号 | S4：下一次截断成功或该段已封存 |
| `METRIC_SOURCE_FAILED` | W | signals | 信号，subject=指标源 | S10/S11：下一次计算成功 |
| `TASK_TRANSITION_STALLED` | W | task.state | STARTING/STOPPING 超过 60s 且无 run 驱动 | 任务离开该状态或有 run 驱动 |
| `SEAL_FACTS_ABSENT`（新，**#353**） | W | store | `000008` 列缺失而 schema 记录 ≥8，或进程内「列缺失」记忆与库不符 | 每轮 sweep 重新探测列；列存在即清除记忆并关闭 |
| `HEALTH_STALE`（新，集群级） | W | task.state | 某任务 `task_health.evaluated_at` 超过 `health.stale_after`（默认 5×sweep，可配）未更新且该任务应有评估者 | 该任务在 `health.stale_after` 内再次被评估（P2-6.1，§28：finding 在 `_cluster`，subject = 任务 id） |
| `HEALTH_STORE_DEGRADED`（新，集群级） | W | store | `000009` 缺失或 ApplyTransitions 连续失败 | 写成功 |

规则：
- 一个 `assurance.Kind` 只映射到一个 finding kind（多对一），映射表在 `health/registry.go`，单测断言 `assurance.AllKinds` 每一项都有映射——以后 assurance 加 kind 而不配 finding 会失败。
- `Chain.Health()` 的结论仍然决定 `/replay` 截断和 `storage_alert`（行为不变）；finding 只是它的持久化视图，二者从同一个 `Chain` 对象取，保证一致（ADR I6「三处一致」的告警面）。
- 段级 kind 的 subject 是段名。段被挪走后它不在链上 → Derive 不产生 → 关闭。这正是今天缺的评估器。

### 6.1 chain.objects 的前置条件（P2-5 之前必须满足，BinlogServerQA #376 第 2 轮）

P2-1 中 `SEGMENT_MISSING`/`ORPHAN_ROW` 按 kind 要求 `{chain.local, chain.objects}` 全部完整才关闭，同一 pass 缺少任一 scope 时也不 UPDATE/DOWNGRADE（只允许 ESCALATE）。P2-5 把这些 kind 设为 Managed 之前，必须先做到下面五条：

1. **按实例的来源关闭**：每个实例记录缺口在哪里（`origin = local | object`）。`origin=local` 的实例只要求 `chain.local` 完整即可关闭；`origin=object` 的实例要求 `chain.objects` 完整。scope 集合从 kind 级细化到实例级（`Instance.Scopes`，缺省取 kind 的 `Scopes`），这样本地文件恢复后，即使对象未列举也能关闭。
2. **移除对象存储导致的关闭**：任务的对象存储配置被移除时，`origin=object` 的 open 实例以 `Reason=CONFIG` 关闭，并在 `finding_transitions` 写审计说明（旧配置指纹、操作者或配置来源、时间），不能作为普通 EVAL 关闭。
3. **有上限的列举命中上限时保持不完整**：只有一次完整、一致的列举才能把 `chain.objects` 标为完整。分页或有上限的列举命中上限时，该次结果对未覆盖部分是不完整的；完整性按前缀（任务 + 源文件名前缀）记录，只有完整列举过的前缀上的实例可以打开或关闭。
4. **「未配置存储」必须是确认过的事实**：只有读到配置并确认该任务没有对象存储时，`chain.objects` 才算完整（视为空）。配置读不出、凭据缺失、或节点无法列举（混跑集群中的部分节点）时一律不完整。滚动发布期间，只有能列举的节点才把这些 kind 视为 Managed。
5. **按任务判断 chain.objects 的完整性**：`chain.objects` 是否完整按任务判断（是否配置、是否完整列举），`HealthOf` 与 `Reconcile` 读同一个按任务的判断（`Facts.Complete` 加上按任务的 `Facts.Produced`，取代全局 `Produced` 变量对该 scope 的作用），避免出现 `HealthOf` 说 HEALTHY 而 `Reconcile` 因对象未列举不关闭的不一致。

## 7. 对外表面

### 7.1 API

| 路由 | 内容 |
|---|---|
| `GET /api/alerts?state=open|all&severity=&kind=&task_id=&limit=` | 跨任务 finding（含 `_cluster`），每条带 `invariant`、`clears_when`、`segments`、`evaluated_at`、`acked_*`；响应头带 `X-Health-Evaluated-At` |
| `GET /api/tasks/{id}/health` | `TaskHealth` + 该任务 open findings + 不完整 scope |
| `GET /api/tasks/{id}/findings` | 不变（兼容），新增字段只增不删 |
| `GET /api/findings` | 不变，标记为 `/api/alerts` 的别名，一个版本后在文档里废弃 |
| `GET /api/tasks`、`/api/dashboard` | `open_findings` 保留；新增 `health`（status、evaluated_at） |
| `POST /api/alerts/mute` `{task_id,kind,subject,reason,until?}` | **静音**（D3a）：只抑制通知，**不**改变事实、finding、健康、`/api/alerts`、Console 与阶段 5 预检。两种粒度、一个 API、一张表 `task_mutes`：<br>• **key 级**：`kind`+`subject` 指定一条 finding，默认且最长 7 天；<br>• **任务级**（维护静音）：`kind=*`，覆盖该任务全部 kind，默认且最长 24h；任务的 dump 重新建立并收到事件（源恢复）后**自动提前结束**（op=END_EARLY，actor=`system`），恢复后的新故障不会被静音。<br>两者都总会过期、可续期（同键再提交，不超过各自上限），`reason` 必填。MUTE / RENEW / EXPIRE / END_EARLY（含 `DELETE` 手动结束）写入 `finding_transitions`，带 reason 与 actor。mute 存在 meta，由 `_notify` 持有者判断，与评估权、栅栏无关，换 owner 不影响 |
| `GET /api/alerts/mutes?task_id=` | 当前与最近 7 天的 mute 记录 |
| `POST /api/alerts/waive` `{task_id,kind,subject,evidence,reason}` | **豁免**：仅 `Waivable` kind（目前 `GTID_HOLE`）。绑定确切 evidence + 原因 + 操作者。evidence 规范格式：洞本身（不是已存集合）的 GTID 区间。MySQL：每个合并后的区间一项 `uuid:a-b`，uuid 小写、按 uuid 排序、同 uuid 区间升序合并，单点写成 `uuid:a-a`；输入也接受 MySQL 多区间写法 `uuid:1-5:7-9`；uuid 必须是 8-4-4-4-12 位十六进制。MySQL 8.4 带 tag 的 GTID（`uuid:tag:1-5`，tag 匹配 `[a-z_][a-z0-9_]{0,31}`，大小写不敏感，作用于其后的区间）规范为 `uuid:tag:a-b`（tag 小写），同一 uuid 内无 tag 的区间排在前面，tag 按字典序。MariaDB：每个 domain-server 一组，`domain-server:a-b`（十进制，按 domain 再按 server 数值排序，区间合并），输入接受单个 GTID `domain-server-seq` 与 `domain-server:a-b`；同一串里不能混用两种 flavor；服务端对提交值规范化后再比较，例如 `3e11fa47-71ca-11e1-9e33-c80aa9429562:363-381`，对该确切区间永久有效；Derive 跳过 evidence 完全相同的洞，finding 以 reason=WAIVED 关闭，健康恢复。洞的区间一旦变化（变大、移动、出现新洞）就是新的 evidence，重新打开。`DELETE` 撤销，所有操作记入 `finding_transitions`（op=WAIVE/UNWAIVE）（§15 D3b） |
| `POST /api/tasks/{id}/health/evaluate` | 立即评估一次，返回结果（受同一 lease 规则约束；非 owner 返回 409 + owner） |

### 7.2 指标

- 保留：`binlog_server_finding_open{task_id,kind,severity}`、`findings_reported_total`、`finding_sink_*`。
- 新增：
  - `binlog_server_task_healthy{task_id}` 0/1，**每个任务总有序列**；UNKNOWN 输出 0，`binlog_server_task_health_status{task_id,status}` 给出四态之一。
  - `binlog_server_task_health_evaluated_ts{task_id}`。
  - `binlog_server_finding_transitions_total{kind,op}`。
  - `binlog_server_health_eval_duration_seconds`（histogram）、`binlog_server_health_eval_lag_seconds{reason}`、`binlog_server_health_eval_errors_total{scope}`。
  - `binlog_server_notify_pending`、`binlog_server_notify_failures_total`。
  - `binlog_server_task_muted{task_id}` 0/1：任务级 mute 生效为 1，每个任务总有序列（与 `task_healthy` 同一份任务列表，meta 不可用时用缓存值）。
- 指标从 `task_health` 表读（每次 scrape 一条查询），叠加本进程未写成功的结果。meta 不可用时：指标层缓存最近一次成功读取的任务列表与健康，**对缓存里的每个任务输出 `task_healthy=0`、status=UNKNOWN**（序列不消失），并输出 `binlog_server_health_store_up=0`。2s 上限必须真实生效：scrape 查询走单独的连接池，DSN 带 `readTimeout=2s,timeout=2s`，且 driver 遵守 ctx（#339 re-QA 的 5.0s 问题）。
- `docs/` 提供推荐 Prometheus 规则：`(task_healthy == 0 unless on(task_id) task_muted == 1)` for 2m、`time() - task_health_evaluated_ts > 300`。任务级 mute 期间 Prometheus 侧也静音，但 `task_healthy` 本身照实为 0。

### 7.3 Console 告警页

- 新页面 `/ui/#/alerts`：按 severity → 任务 → kind 分组；每条显示 detail、「何时自动关闭」、涉及段、首次/最近、确认状态；一键跳到任务的窗口或回放视图。筛选 open / 最近 7 天已关闭。
- 处于任务级 mute 的任务在告警页与任务列表显示「维护中，到 HH:MM（操作者，原因）」，finding 与健康照常显示。
- 任务列表与任务详情加健康徽标（HEALTHY / DEGRADED / UNHEALTHY / UNKNOWN），UNKNOWN 显示「评估已 N 分钟未运行」。
- 告警页读 `/api/alerts` 与 `/api/tasks/{id}/health`，不自己推导。事件时间线里的 `FINDING_REPORTED/RESOLVED` 按事件时间排序（顺带满足 #368 在告警相关事件上的要求，见 §10）。

### 7.4 通知

- `finding_transitions` 是 outbox：每个 OPEN / ESCALATE / CLOSE 一行（UPDATE 不写），与 finding 行同一事务写入。
- 通知器：持有 `_notify` 集群 lease 的节点（单机模式即本进程）按 id 递增读 outbox，投递到配置的 sink；阶段 2 只做 **webhook**。格式说明：BinlogServer **以 Alertmanager 发给接收端的 webhook 格式**（`version: "4"`、`status`、`alerts[]` 带 `labels`/`annotations`/`startsAt`/`endsAt`/`fingerprint`）POST 到配置的 URL，所以任何 Alertmanager webhook 接收端（钉钉/飞书桥接等）可直接接。**另一种**部署是把 BinlogServer 当告警源推给 Alertmanager：设 `notify.mode=alertmanager`，按 Alertmanager `POST /api/v2/alerts` 格式发送，由 Alertmanager 路由（邮件用 `email_configs`）。两种格式不同，配置项二选一，文档分别给示例。其他通道不进二进制。
- 顺序：每条转移带 `FactAt`（事实时间）与 `generation`；outbox 写入顺序就是 generation 顺序（pending 按 generation 有序重放，§5.1 第 6 步）。接收端按 `(key, generation)` 去重、按 generation 排序（文档写明）；`startsAt/endsAt` 取 `FactAt`，而不是写库时间（#364 第 6 项）。
- 至少一次：投递成功才前移游标（`notify_cursor` 存在 `task_health` 同库的单行表里）；接收端用 `X-BinlogServer-Delivery: <task>/<kind>/<subject>/<generation>/<op>` 去重。
- 「计划停机不算 RPO 违约」不由评估器处理：阶段 5/6 判定器读 `task_mutes` 记录自行排除，评估器从不因 mute 少产生事实。
- 去抖：OPEN 默认立即（CRITICAL）或持续 1 个 sweep（WARNING）后通知；CLOSE 受 `ClearRule.HoldDown`；1 小时内同 key 翻转 >3 次合并为一条「flapping」通知。已静音的 key（或任务级 mute 下的任一 key）不再通知 OPEN/ESCALATE，仍通知 CLOSE，所以值班能看到「计划重启已结束」。`reason=UPGRADE` 的转移（升级后首轮，§9）不逐条投递，合并为一条摘要通知（「升级后关闭了 N 条过期告警」）。UNKNOWN 引起的健康变化只发 WARNING 级通知。
- **只有 webhook，没有内建发送器**（§15 D4）。路由到钉钉、飞书、邮件的方式写进 `docs/notifications.md`（P2-9 交付）：
  - Alertmanager：用 `notify.mode=alertmanager` 把告警推给 Alertmanager，由它的 `email_configs` 发邮件、`webhook_configs` 接钉钉/飞书桥接（如 `prometheus-webhook-dingtalk`、飞书机器人 relay）。源库计划内长时间重启时，先设任务级 mute（`kind=*`，≤24h，源恢复后自动结束）。
  - 不用 Alertmanager：任意 HTTP relay 把 JSON 转为钉钉/飞书机器人消息或 SMTP；文档给出字段映射与一个最小示例。
- 未配置 webhook 时 outbox 仍写（保留 30 天，sweep 清理），告警页可回看历史。

## 8. 集群模式与 lease 栅栏

基于 [0346](0346-dump-lease-fence.md) 的 lease（`(owner, epoch)`，CAS Renew）。

**Fence 的三种来源（R2-1）**：
- `cluster`：集群模式（`isClusterMode`，`WithClusterLease`）且任务有 `task_leases` 行，按下文第 1–3 条。
- `standalone`：没有集群 lease 的所有部署，包括 standalone 不带 meta（`.findings.json`）和 **standalone + meta_dsn**（task_leases 无行）。`Fence{Mode: standalone, TaskEpoch: 0, Node: node_id, Instance: 启动时生成的实例 id}`。`node_id` 是**持久化在数据目录**的随机 id（`{data_dir}/.node_id`，首次启动生成，原子写），不是主机名，容器重建、改主机名都不变；数据目录与 meta 一起迁到新主机时 id 随目录走。只迁 meta、换了新数据目录（新 id）时，用显式接管命令 `binlog-server health takeover --task <id>|--all`（或 `POST /api/health/takeover`，需确认）把 `task_health.fence_node` 改为本节点并记 transition（op=TAKEOVER，actor、reason）；§8.3 第 1 步跳过，第 2 步比较 `task_health.fence_node/fence_instance`：同节点新实例接管（重启）是允许的，并把 `fence_instance` 更新为自己；不同节点 → ErrFenced（防止误把两个 standalone 进程指向同一个 meta）。
- `never`：集群模式下从未有 lease 行的任务（CREATED、从未启动，例如 #364 的任务 2）。按 epoch 0 处理，由 `_health` 持有者以 `Fence{Mode: never, TaskEpoch: 0, Node: _health 持有者}` 写；第 1 步要求「`task_leases` 中无该任务行」，一旦出现 lease 行即转为 `cluster`，任何 epoch ≥1 的写都高于它。**只有一种栅栏计数器：任务 lease epoch。** `_health` lease 有自己的 epoch，只用来选出「谁评估无主任务」，从不写进 finding/health 行，也从不与任务 epoch 比较。

1. **评估权**：任务被某节点持有（lease 未过期）→ 只有该节点评估，`Fence{TaskEpoch: lease.epoch, Node: self}`。理由：链的本地字节只在 owner 节点上（阶段 1 §8），Signals 也只在 owner 进程里产生。
2. **无主任务**（STOPPED、无 lease、lease 过期且无人接手）：
   - `task.state`、`store` scope 由 `_health` 持有者评估，`Fence{TaskEpoch: 最后一次任务 lease epoch, Node: 最后 owner}` 由 `_health` 持有者**代为**使用：它在事务里校验 `task_leases` 仍是那个 (owner, epoch) 且已过期/释放，才可写这些 kind。
   - 链 scope（§15 D2）：共享存储时 `_health` 持有者直接读目录；非共享存储时委派给**最后 owner 本身**，`Fence{TaskEpoch: 最后 lease epoch, Node: 最后 owner, Delegated: true}`，只能写 `chain.*` kind。委派是按节点的 `_health` scope：`_health` 持有者在 `task_health.chain_owner` 记录被委派节点，最后 owner 看到自己被委派才评估。
   - **一旦任务产生新 lease（任何节点、更高 epoch），委派自动失效**：最后 owner 的下一次写在步骤 1/2 的比较中得 ErrFenced，因为库里 `fence_epoch` 已被新 owner 推高。
   - 新 owner 在另一个节点起新目录时，旧目录的链 finding 在新 owner 第一轮评估中统一以 `reason=OWNER_CHANGED` 关闭（它们描述的是旧节点上的目录，不再是该任务的链）；同一 subject 之后由新目录的事实决定是否重开。
   - 只有最后 owner 已消失（`worker_heartbeats` 过期 `health.node_gone_after`，默认 3 个 sweep）时，`chain.*` 为不完整，链 finding 保持原样，健康为 UNKNOWN(chain, node gone)。
3. **栅栏写（显式比较，不看 affected rows）**：`ApplyTransitions` 在一个事务里：
   1. （仅 `cluster`/`never`）`SELECT owner, epoch, expires_at FROM task_leases WHERE task_id=? FOR UPDATE`：持有者模式要求等于 `(Fence.Node, Fence.TaskEpoch)`；**owner 与 epoch 相同但已过期 → `ErrLeaseLagging`（可重试，不停止评估）**，owner 或 epoch 不同 → ErrFenced。无主/委派模式要求 epoch 仍等于 `Fence.TaskEpoch`（没人获得更新的 lease）；`never` 要求无行。
   2. `SELECT fence_epoch, fence_node, generation FROM task_health WHERE task_id=? FOR UPDATE`：`fence_epoch > Fence.TaskEpoch` → ErrFenced；相等而 `fence_node` 不同且非同一 lease 的代写 → ErrFenced；generation 由本事务分配为 `generation + 1`（§5.4），不再比较调用方的 generation；`transition_digest` 已存在 → 内容相同的重放，成功空操作。
   3. upsert 用 `INSERT … AS new ON DUPLICATE KEY UPDATE col = new.col`（MySQL 8.0.20+ 废弃 `VALUES()`；MariaDB 与 5.7 走 `VALUES()` 分支，按 flavor 选择），写 `finding_transitions` 与 `task_health`。
   这样重放返回 0 affected rows 也不会被误判（go-sql-driver 默认 `clientFoundRows=false`）；`_health` 代写与委派写并发时各自在锁内取号，不撞号、不丢转移（R2-2）。
4. **#360 作为 P2-6 的前置（P2-6a）**：任务行的 STOPPED/STOPPING 写（`failSafeStopLocked` 等）改为同样的条件写 `meta.fencedExec(taskID, owner, epoch, stmt)`：事务内先 `SELECT … FROM task_leases … FOR UPDATE` 校验 (owner, epoch)，不符则不写。并且评估器的 `TaskFacts` 与 `task_leases` 交叉校验：任务行的 owner/epoch 与 lease 表不一致时 `Facts.Task = nil`（task.state 不完整，健康 UNKNOWN），DUMP_STALLED/TASK_TRANSITION_STALLED 不会因读到错误的 STOPPED 而关闭（#355 run 3、#336）。
5. **接管**：`LEASE_ACQUIRED` → 分页读该任务全部 open 行作为 `open`，generation = max+1；第一轮按清除规则调和。旧 owner 的 `DUMP_STALLED` 因任务 lease 已到新 epoch 而关闭（#366）；`SOURCE_UNREACHABLE` 等条件型 kind 按新 owner 的事实重算。
6. **丢失**：`LEASE_LOST`（owner 或 epoch 已变）→ 立即停止评估该任务。仅过期而 owner/epoch 未变不是丢失：继续评估，写入遇 `ErrLeaseLagging` 时保留转移、等续约；sweep 每轮重读 lease 视图，续约或同 owner 重新认领（epoch 不变，#355 `SameOwnerExpiredReclaimKeepsEpoch`）都会 MarkDirty。，丢弃 pending 转移（不重试旧 epoch 的写），进程内缓存清空。
7. **冻结的 owner**（#364：SIGSTOP 约 60s；#355 run 3：宿主机停顿约 2 分钟）：醒来的旧 owner 持有的 Fence 是旧 epoch；步骤 1 在 `task_leases` 上就失败，步骤 2 兜底，它的 finding 写、健康写、任务行写（P2-6a）全部被拒，且它只在自己被拒后停止，不会把新 owner 锁死——新 owner 的写只与 `task_leases` 当前值比较，从不与 `_health` epoch 比较。
8. **时钟**：所有时间列由库 `NOW(6)` 写入，转移顺序以 generation 为准；`FactAt` 由事实产生节点给出，仅用于展示和通知 `startsAt`，从不参与关闭判定或栅栏（R2-P3）。
9. **集群级 finding**（`_cluster`）：subject 带节点 id，由产生它的节点评估；集群模式下节点消失后由 `_health` 持有者在心跳过期 `node_gone_after` 后关闭（reason=NODE_GONE）。**standalone** 没有心跳表：只有本进程，`_cluster` finding 由本进程评估，进程重启后按 §5.5 在首次成功后关闭。
10. **HEALTH_STALE**：`_health` 持有者每个 sweep 检查所有任务的 `evaluated_at`，超过 `health.stale_after`（默认 5×sweep，负载高时可调大）记 WARNING；TRANSITION 触发保留在热路径，TASK_TRANSITION_STALLED 不只依赖 sweep。

单机模式（无 meta DSN）：lease 视图恒为本进程，`.findings.json` 扩展为同时存 `task_health`、pending 转移与 outbox（原子写不变）。

## 9. 迁移与兼容

`000009_health.up.sql`（只加，不改已有列语义）：

```sql
ALTER TABLE task_findings
  ADD COLUMN evidence TEXT NULL,
  ADD COLUMN segments JSON NULL,
  ADD COLUMN generation BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN owner_epoch BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN evaluated_at DATETIME(6) NULL,
  ADD COLUMN waiver_evidence TEXT NULL,
  ADD COLUMN waiver_reason VARCHAR(512) NULL,
  ADD COLUMN waiver_actor VARCHAR(128) NULL,
  ADD COLUMN waived_at DATETIME(6) NULL,
  ADD COLUMN waiver_kind_version INT NULL;

CREATE TABLE task_health (
  task_id VARCHAR(64) PRIMARY KEY,
  status VARCHAR(16) NOT NULL,
  reasons JSON NULL,
  evaluated_at DATETIME(6) NOT NULL,
  generation BIGINT NOT NULL,
  owner VARCHAR(128) NOT NULL DEFAULT '',
  owner_epoch BIGINT NOT NULL DEFAULT 0,
  fence_mode VARCHAR(16) NOT NULL DEFAULT 'cluster',
  fence_instance VARCHAR(64) NOT NULL DEFAULT '',
  fence_epoch BIGINT NOT NULL DEFAULT 0,       -- 只存任务 lease epoch（§8.3）
  fence_node VARCHAR(128) NOT NULL DEFAULT '',
  chain_owner VARCHAR(128) NOT NULL DEFAULT ''  -- 非共享存储下评估 chain.* 的节点（§8.2）
);

CREATE TABLE finding_transitions (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  task_id VARCHAR(64) NOT NULL, kind VARCHAR(64) NOT NULL, subject VARCHAR(255) NOT NULL,
  op VARCHAR(16) NOT NULL, reason VARCHAR(32) NOT NULL DEFAULT 'EVAL', generation BIGINT NOT NULL,
  kind_version INT NOT NULL DEFAULT 1, fact_at DATETIME(6) NOT NULL, severity VARCHAR(16) NOT NULL,
  transition_digest CHAR(64) NOT NULL, UNIQUE KEY uk_ft_digest (task_id, transition_digest),
  detail TEXT NULL, at DATETIME(6) NOT NULL,
  INDEX idx_ft_key (task_id, kind, subject), INDEX idx_ft_at (at)
);
CREATE TABLE notify_cursor (id TINYINT PRIMARY KEY, last_id BIGINT NOT NULL);
CREATE TABLE rehash_cursor (id TINYINT PRIMARY KEY, task_id VARCHAR(64) NOT NULL, segment VARCHAR(255) NOT NULL, updated_at DATETIME(6) NOT NULL);
CREATE TABLE task_mutes (
  task_id VARCHAR(64) NOT NULL,
  kind VARCHAR(64) NOT NULL,          -- '*' = 任务级
  subject VARCHAR(255) NOT NULL DEFAULT '',
  reason VARCHAR(512) NOT NULL, actor VARCHAR(128) NOT NULL,
  started_at DATETIME(6) NOT NULL, until DATETIME(6) NOT NULL,
  ended_at DATETIME(6) NULL, end_reason VARCHAR(16) NULL,   -- EXPIRE | END_EARLY | DELETE
  PRIMARY KEY (task_id, kind, subject, started_at),
  INDEX idx_mutes_active (ended_at, until)
);
```

兼容规则（与 `000007`/`000008` 同一套路）：
- **v0.5.x**：`minRequiredSchemaVersion` 保持 6。缺 `task_findings` → 今天的内存模式；缺 `000009` 的列/表（1054/1146）→ 评估器仍运行、仍做调和，但写入回退到 v0.5.66 的 upsert（无栅栏、无 outbox），报 `HEALTH_STORE_DEGRADED`，每个 sweep 重新探测（**不**像 #353 那样记到重启）。集群模式下缺 `000009` 时在启动日志与 `/api/alerts` 显式说明「栅栏不可用」。
- 已存的 v0.5.66 open 行：`generation=0`，任何新转移都能覆盖；每个 kind 被翻成 Managed 后的第一轮把它们当作 `open` 调和，条件已清除的行关闭，转移标 `reason=UPGRADE`：写入 `finding_transitions` 便于审计，但**不逐条通知**，合并为一条摘要（§7.4）。v0.5.66 中「永不 resolve」的 `EVENT_PERSIST_FAILED` 积压按 §5.5 在首次成功后以 UPGRADE 关闭。
- **feature flag** `health.evaluator`（`off|shadow|on`，v0.5.x 默认 `off`）：`shadow` 只计算并写 `finding_transitions`（reason=SHADOW，不改 finding 行、不通知），用于升级前比对；`on` 才接管 Managed kind。滚动升级时先升级全部节点、保持 `off`，全部到新版本后再切 `on`；`worker_heartbeats` 中**没有版本字段的心跳一律视为 v0.5.66（旧版本）**。**v0.5.67 没有混版本自动退回**（tip QA b4c52f4 核实代码中不存在）：`on` 期间有旧版本节点在线时，评估器不会退回 `shadow`、也不报 `HEALTH_STORE_DEGRADED`、不记警告。混跑期间不切 `shadow`/`on` 由运维保证（发布说明已写明）。按心跳版本自动退回列为 P2-6 计划项，实现前本节不作承诺。
- 旧二进制读写跑过 `000009` 的库：新列有默认值或可空，旧 upsert 语句不涉及它们；旧二进制写的行 `owner_epoch` 不变，会被新二进制的下一轮覆盖。混跑集群（滚动升级）期间旧节点无栅栏，发布说明要求滚动升级窗口内不做手工迁移任务。
- `down`：删三张表和新增列；行为回到 v0.5.66。
- **v0.6.0**（§15 D1）：`minRequiredSchemaVersion = 9`，低于 9 拒绝启动并给出明确提示（当前版本、所需版本、`./migrate up` 命令）；`health.evaluator` 默认 `on`；6→9、7→9、8→9 直接升级各有迁移测试（P2-2）；删除无栅栏回退代码路径与 `HEALTH_STORE_DEGRADED` 的「缺 000009」分支（保留「写入连续失败」分支）。v0.5.x 的回退只存在到 v0.6.0 为止。
- Go API：`tasks.Finding`/`FindingKind` 保留为别名到 v0.6.0；`Scheduler.Report/Resolve` 在 P2-4 后变为 `Signal` 的包装并标 Deprecated，lintguard 禁止 `internal/tasks` 外新增调用。

## 10. 吸收哪些 P3

| Issue | 处理 | 落在 |
|---|---|---|
| #352 单源 GTID 洞无 finding | **吸收**：`GTID_HOLE` 由 `I3_GTID_HOLE`/内部洞导出，不分单源多源 | P2-5 |
| #353 000008 down/up 后封存事实写 NULL 无日志 | **吸收**：`SEAL_FACTS_ABSENT` + 每 sweep 重新探测列（Store scope） | P2-5 |
| #366 STOPPED 任务的 DUMP_STALLED 挂 40s+ | **吸收**：只对「desired=STOP 且 STOPPED」或「lease 已到另一 epoch」豁免；重试循环由 `SOURCE_UNREACHABLE` 承接 | P2-4 / P2-6 |
| #341 上传模式保留期删除不写 RETENTION_REMOVED | **吸收**：I6 需要保留期删除标记（阶段 1 §4.1 已排到阶段 2）；同一 PR 写事件、失败走 `RETENTION_RECORD_FAILED` 信号 | P2-5 |
| #342 lintguard 覆盖面 | **部分吸收**：新增「`Report/Resolve` 只能在 evaluator 内」规则时一并识别 logger 实例方法；其余条目不在本阶段 | P2-4 |
| #368 事件时间线按写入时间排序 | **部分吸收**：告警页与时间线中的 finding 事件按事件时间排；时间线整体排序随 Console 修 | P2-8 |
| #360 lease 丢失后 STOPPED 覆盖新 owner 行 | **吸收，作为 P2-6 的前置 P2-6a**：任务行写走 `meta.fencedExec`（事务内校验 `task_leases`）；评估器另外交叉校验任务行与 lease 表（§8.4） | P2-5.1 | P2-5 的第一步（§20）：`SEGMENT_DAMAGED`、`CHAIN_INDEX_GAP`（不含 `I2_SWITCH_UNRECORDED`）、`LEGACY_FAILBACK`、`SEGMENT_MISSING`（origin=local：非集群的 `I6_MISSING_LOCAL` 与旧段的目录行大小比较）翻 Managed；`KindSpec.Confirm` 两轮确认 | 47 个目录用例无误报；每种 kind 打开一次、修复后关闭一次；单轮的轮转竞态不打开 | — | P2-4 |
| P2-5.2 | §21：P2-5a（`upload.grace_sec` 全局 + `upload.grace_task_sec` 按任务）；§6.1 五条（实例来源 `#object`、CONFIG 关闭 + 审计、按前缀列举完整性、未配置 = 确认事实、按任务的 `chain.objects`）；对象列举（`ObjectLister`，有上限、有缓存）；`UPLOAD_LATE`（W→C，`health.upload_late_critical_after_sec` 默认 24h + 按任务）| 见 §21.7 | `UPLOAD_LATE`；`SEGMENT_MISSING` 的对象来源；`UPLOAD_UNVERIFIED` 的链部分开始派生 | P2-5.1 |
| P2-6a |
| #343 只读 meta 下 RUNNING 显示 1–3s | 不吸收（任务状态，不是健康）；健康会在首次写失败信号后 1 轮内变 UNHEALTHY，缩小但不消除该窗口 | — |
| #345 DUMP_STALLED 48–49s 才报 | 不吸收（runner 检测阈值）；评估器不改变检测时机 | — |
| #348 UploadGrace 默认值 | **吸收为 P2-5 的前置 P2-5a**：`assurance.DefaultUploadGrace`（15min）已有，但 tasks 侧未传入配置；P2-5a 加 `upload.grace` 配置并显式传入，Registry Derive 单测覆盖 | P2-5a |
| #344、#365、#367、#363 | 不吸收（操作接口、Console、回放列表，与 finding 生命周期无关） | — |

## 11. 阶段 3–6 草图（只证明类型够用）

- **阶段 3 巡检与演练**：Verifier 是一个新的事实来源。它列举对象（填 `assurance.Inputs.Objects`）、按段校验 sha256，结果写进 `Facts`（`ScopeChainObjects` 变为 Complete、`ScopeVerify` 的 `VerifyFacts{segment→result, at}`），并 `MarkDirty(task, VERIFY)`。新增 kind `OBJECT_CORRUPT`、`VERIFY_OVERDUE`、`DRILL_FAILED` 只是 Registry 新项；生命周期、栅栏、通知不变。I5 的 RPO（checkpoint 落后）作为 `RPO_EXCEEDED` 的 Derive。
- **阶段 4 覆盖查询**：`/coverage?time=|gtid=` 用 `assurance.WindowOf` 求窗口，再与该任务 open finding 中 `SegmentScoped` 的 `Segments` 求交，结果里每个断点带 `Key`（为什么断）。`Finding.Segments` + `first_event_at/last_event_at`（000008）就足够，不需新类型。
- **阶段 5 还原计划**：预检 = `HealthOf` 限定到计划涉及的段：计划段集合与任何 CRITICAL open finding 的 `Segments` 相交 → 红，并列出 `Key` 与 `ClearsWhen`；`UNKNOWN` scope 覆盖计划段 → 黄（要求先 `health/evaluate`）。
- **阶段 6 长稳**：判定器只读 `/api/alerts?state=all`、`finding_transitions` 与 `task_health_evaluated_ts`：每次注入故障必须在 1 个 sweep 内产生对应 OPEN，故障移除后在 `HoldDown+1 sweep` 内 CLOSE；任何时刻 `HEALTH_STALE` 必须为零。`finding_transitions` 是可审计的时间序列，判定器不需要读日志。

## 12. 与 ADR 草图的差异

- ADR 阶段 0 写的是 `000007` 同时建 `task_findings` 与 `task_health`；实际只建了 `task_findings`。`task_health` 推迟到本阶段的 `000009`，与它的唯一写者（评估器）一起出现。
- ADR 决定 F「评估器与巡检器推结论到持久表」：保留；本设计把「推」具体化为条件写的转移 + outbox。
- 新增 `health` 包而不是把评估放进 `assurance`：`assurance` 只回答「链对不对」，不知道任务状态、信号、lease；把它们放进去会破坏阶段 1「只依赖 binlog」的边界。

## 13. PR 切分

每个 PR 第一提交是失败测试；每个 PR 独立过 QA 才合并；P2-1 起每个 PR 都不改变 `/window`、`/replay`、`storage_alert` 的字节输出（沿用阶段 1 golden），除 P2-3 对「open 与 sealed 相同」新增的一条 golden 外。`health.evaluator` 在 P2-3 之前默认 `off`，每个 PR 的 QA 在 `on` 下跑。

| PR | 内容 | 第一提交（红） | Managed 变化 | 依赖 |
|---|---|---|---|---|
| P2-0 | 本设计文档 | — | — | — |
| P2-1 | `internal/health`：`Kind`/`KindSpec`（含 `Managed`、`Version`、`Waivable`）/`Registry`、`Facts`、`Waiver`、`Reconcile`、`Desire`、`HealthOf`；`tasks` 类型别名 | 表驱动：OPEN/UPDATE/ESCALATE/CLOSE/WAIVE、不完整 scope 不关、HoldDown、幂等；**非 Managed kind 永不产生转移**；**信号在首次成功前 Unknown 不关闭**；`assurance.AllKinds` 每项有映射 | 全部 false | — |
| P2-2 | 迁移 `000009` + `meta.ApplyTransitions`（显式 `SELECT … FOR UPDATE` 比较 `task_leases` 与 `task_health.fence_*`、`AS new` upsert、outbox）、`ListOpenFindingsForTask`（keyset 分页）、`ListHealth`；缺表缺列回退与每轮重探；standalone 文件存储扩展（含 pending） | 「旧 epoch 不覆盖新 epoch」「同 generation 重放不是 ErrFenced」「`_health` 代写与任务 epoch 交替写」「代写与委派写并发不丢转移」「standalone+meta 与从未启动任务可写」「同 owner 同 epoch 过期返回 ErrLeaseLagging」「缺 000009 回退且不记到重启」红 | — | P2-1 |
| P2-3 | `tasks.Evaluator`：脏集合、per-task 串行、sweep、目录指纹、触发点接线、feature flag `off/shadow/on`；异步 `Rehasher`（全局限速、跨任务游标、idle I/O，§5.7）；I4 按 replay 顺序判定（含 open 副本，排除空残留）；`SEGMENT_EMPTY_LEFTOVER` | 「挪走重复副本后 ≤1 sweep 关闭」「`.open.e4`≡`.sealed.e5` 打开 SEGMENT_OVERLAP」「原地改一个字节 ≤N 轮打开 SEGMENT_UNREADABLE」「meta 只读时 legacy finding 不被关」红 | `SEGMENT_UNREADABLE`、`SEGMENT_OVERLAP`、`SEGMENT_EMPTY_LEFTOVER` | P2-2 |
| P2-4 | 信号型 kind 逐个改走 `Signal` 并翻 Managed（含替代关闭条件 `Probe`）；新 kind `SOURCE_UNREACHABLE`（去抖、任务级阈值）、`TASK_FAILED`；`DUMP_STALLED` 新清除规则；`TASK_TRANSITION_STALLED` 改 Derive（保留 TRANSITION 触发）；`Report/Resolve` 对 Managed kind 禁用 + lintguard（含 #342 logger 方法） | 每种信号 kind：失败→OPEN、首次成功→CLOSE；「meta 仍只读时重启不关闭」；「黑洞 3 分钟内任何时刻至少一条 open」「无 finding 时重启 1 轮内 HEALTHY」「FAILED 任务健康不为 HEALTHY」红 | 16 种中除 SEGMENT_* 外的信号/状态 kind + `SOURCE_UNREACHABLE`、`TASK_FAILED` | P2-3 |
| P2-5a | #348：`upload.grace` 配置（全局默认 15min，任务级覆盖）由 tasks 显式传入 `assurance.Inputs.UploadGrace`（`assurance.DefaultUploadGrace` 已有，缺的是配置与传入） | 「全局 `upload.grace=2m` 时 2m 后报、15min 默认不生效」「任务级 `upload_grace=30m` 覆盖全局」红 | — | P2-3 |
| P2-5 | **前置：§6.1 五条**（实例级 origin/scope、CONFIG 关闭 + 审计、按前缀的列举完整性、未配置 = 确认事实、按任务的 chain.objects 判断）；`UPLOAD_LATE` 升级阈值（全局 `health.upload_late_critical_after` 默认 24h，任务级覆盖）；新链 kind：`SEGMENT_DAMAGED`、`CHAIN_INDEX_GAP`、`GTID_HOLE`(#352，Waivable)、`LEGACY_FAILBACK`、`CHECKPOINT_AHEAD`、`SEGMENT_MISSING`、`ORPHAN_ROW`、`UPLOAD_LATE`、`UPLOAD_UNVERIFIED` 条件化；waiver 存取；保留期删除标记 + 事件(#341)；`SEAL_FACTS_ABSENT`(#353) | 每种 kind ≥2 个构造目录用例：打开一次、修复后关闭一次；waiver 精确匹配 | 上述新 kind | P2-4、P2-5a |
| P2-6a | #360：任务行写走 `meta.fencedExec`；TaskFacts 与 `task_leases` 交叉校验 | 「#355 run 3 复现后旧 owner 的 STOPPED 写被拒」「行与 lease 不一致时 task.state 不完整」红 | — | P2-2 |
| P2-6b | #417：数据面端到端写栅栏（§23）：本地 lease 时钟 + meta 事务内校验 `task_leases`，覆盖追加、新 open 文件、封存、checkpoint、目录行、上传 | 「冻结 owner 醒来后的追加/checkpoint/目录行被拒」「过期 run 不建 open 文件」红 | — | P2-6a 平行、#416 |
| P2-6c | #421：`_health` lease 选出的节点评估无主任务的 `task.state`（代写栅栏 = 最后一次任务 lease 的 epoch）；FAILED 不被旧 owner 改成 STOPPED；任务行栅栏覆盖已释放 / 无行（§24） | 「集群 FAILED 打开 TASK_FAILED」「旧 owner 醒来后行仍是 FAILED」「已释放于更大 epoch 的 lease 拒绝写」红 | — | P2-6a、P2-6b |
| P2-6 | 拆成 P2-6.1–P2-6.3（§28）。评估权、`_health` 选举、ErrFenced、接管触发已在 P2-6a/b/c 交付，不重做 | — | — | P2-6c |
| P2-6.1 | `_health` 持有者把 `HEALTH_STALE` 写在 `_cluster`（`never` 栅栏，只比任务 epoch，从不比 `_health` epoch）；`health.stale_after_sec`（0 = 5×sweep） | 「evaluated_at 超过阈值打开」「5×sweep 内 120 个任务不误报」「非持有者不写」红 | `HEALTH_STALE` | P2-6c |
| P2-6.2 | 非共享存储：链 scope 委派给最后 owner（绑定最后 lease epoch + 节点 id）、`OWNER_CHANGED`、`node_gone_after` → UNKNOWN(node gone) | 「新 lease 后委派写被拒」「旧目录 finding 以 OWNER_CHANGED 关闭」红 | — | P2-6.1 |
| P2-6.3 | 混版本：`health.evaluator=on` 时，一条没有 version 的心跳加入，本节点评估退回 `shadow`（§9） | 「无 version 心跳加入后退回 shadow，不改 finding 行」红 | — | P2-6.1 |
| P2-7 | API（`/api/alerts` 分页、`/tasks/{id}/health`、mute（key 级 + 任务级，`task_mutes`）、waive、evaluate、standalone takeover）+ `task_muted` 指标+ 指标（`task_healthy` 总有序列、meta 宕机缓存、真实 2s）+ 推荐规则文件；swagger | 「新建任务即有序列」「meta 暂停时 2s 内返回且序列为 0」「评估停 `stale_after` → UNKNOWN」红 | — | P2-6 |
| P2-8 | Console 告警页（静音、豁免操作）、健康徽标、finding 事件按事实时间排序 | UI 测试：告警页数据与 `/api/alerts` 一致 | — | P2-7 |
| P2-9 | 通知器：`_notify` lease、outbox 游标、两种格式（AM webhook 格式 / AM `/api/v2/alerts`）、去抖、flapping、静音、UPGRADE 摘要；`docs/notifications.md`（钉钉、飞书、邮件路由） | 「投递失败重试且不丢」「`_notify` 切换不重复也不丢」「升级首轮只发一条摘要」红 | — | P2-6、P2-7 |

依赖图：P2-1 → P2-2 → P2-3 → P2-4 → P2-5（另需 P2-5a ← P2-3）；P2-2 → P2-6a；{P2-4, P2-5, P2-6a} → P2-6c → P2-6.1 → {P2-6.2, P2-6.3} → P2-7 → {P2-8, P2-9}。P2-7 仍等 P2-6.2 与 P2-6.3，不把 P2-6.1 当成整段 P2-6 完成。阶段 2 完成后发 v0.6.0，schema 下限提到 9，`health.evaluator` 默认 `on`（§15 D1）。

## 14. QA 计划（每个 PR）

通用：`go vet`、`go test ./...`、阶段 1 golden（window/replay/health）逐字节一致、e2e quick 全绿；MySQL 8.0 + MariaDB 11.4（链相关 PR 加 10.11）；集群用例在 box 上用 3 节点 + SIGSTOP 复现（#355/#364 的做法）。

| PR | QA 通过条件 |
|---|---|
| P2-1 | 单测覆盖 Reconcile 全部分支；性质测试：任意 (open, facts) 序列重放两次结果相同、对输出再调和为空；不完整 scope 下从不 CLOSE；非 Managed kind 在任意输入下零转移；`LastSuccess` 为零的信号零 CLOSE；waiver evidence 不完全相等时不豁免 |
| P2-2 | 在 schema 6/7/8/9 上各跑：读写正常、回退报 `HEALTH_STORE_DEGRADED`；**6→9、7→9、8→9 直接升级**与 `down` 后 v0.5.66 可读写；在线 `migrate down 1`/`up` 后 1 个 sweep 内恢复栅栏写（#353/#351 同型）；两写者不同任务 epoch 只留高 epoch；同 generation 重放（affected rows=0）不被判 ErrFenced；`_health` 代写与任务 epoch 写交替 100 次，结果只由任务 epoch 决定；MySQL 8.0 用 `AS new`、MariaDB/5.7 用 `VALUES()`；5000 条 open 分页读完无遗漏；**standalone + meta_dsn（task_leases 无行）与集群下从未启动的任务**都能打开/关闭 finding，同节点重启（新 instance）可接管、另一节点同 meta 被拒；**`_health` 代写与委派写并发 1000 次，`finding_transitions` 行数等于实际转移数**，无同号、无丢弃；内容相同重放为空操作、内容不同必得新 generation；同 owner 同 epoch 过期写返回 `ErrLeaseLagging`，续约后成功 |
| P2-3 | 复现 #372：零填充/完整副本 → OPEN；挪走 → ≤1 sweep CLOSE 并有 transition；`.open.e4`≡`.sealed.e5`（openstale）与 openhand → SEGMENT_OVERLAP OPEN，新增 golden；corrupt.py 原地改一字节、保持 mtime → 在一个复核周期内 SEGMENT_UNREADABLE OPEN；**100 个任务时记录复核读带宽 ≤ `rehash_bytes_per_sec`（±10%）**、源延迟不升高，评估基准在复核运行时仍 <50ms；#357 的 4 字节 `.open.e3` + sealed：新增 golden，不报 OVERLAP、不截断 replay，只有 INFO `SEGMENT_EMPTY_LEFTOVER`；meta 只读 60s 期间 tip-2c04 那 8 条 legacy finding 一条不被评估器关；`shadow` 模式不改 finding 行；1500 段单轮 <50ms；多任务 + 5000 条 open 无漏关 |
| P2-4 | S1–S15 注入测试改为「失败→OPEN，恢复后首次成功→CLOSE」；**meta 仍只读时重启进程不关闭、不翻转、不通知**；meta 只读 60s：`METADATA_WRITE_BLOCKED` OPEN→恢复 CLOSE；**黑洞源 3 分钟（STARTING/RETRY_BACKOFF 循环）期间每秒采样至少一条 open finding**，`SOURCE_UNREACHABLE` 在 90s 内打开，SIGCONT 后两者关闭；源计划重启 60s 不打开 `SOURCE_UNREACHABLE`（去抖）；任务级 mute 期间源停 5min：`SOURCE_UNREACHABLE` 照常打开、健康 UNHEALTHY、不通知，源恢复后 mute 自动 END_EARLY、CLOSE 发通知；**无 finding 时重启进程，1 轮内 HEALTHY、`task_healthy` 不掉 0**；未配置上传时 `UPLOAD_RECORD_FAILED` 旧行按 Probe 关闭；#362（MariaDB STREAM_REGRESSION）、#354（1236）、#338（permission denied）复现各打开对应 subject 的 `TASK_FAILED`，健康 UNHEALTHY，修复重启后关闭、Stop 后关闭；有意 Stop 后 `DUMP_STALLED` ≤1 sweep 关闭；lintguard 对 Managed kind 的 `Report` 调用 CI 失败 |
| P2-5a | 全局 `upload.grace=2m`：2m 后出现 `I7_UPLOAD_LATE`；任务级 `upload_grace=30m` 覆盖全局；不配置时 15min；MinIO 暂停 5min 再恢复（#351 retry）期间无 UPLOAD_LATE、无通知 |
| P2-5 | §6.1 前置：本地缺失的段恢复后，对象未列举时也关闭（origin=local）；对象缺失的实例在对象未列举时不开不关；移除任务的对象存储配置 → 对象类实例以 CONFIG 关闭并有审计说明；列举上限设为 10 而对象有 25 个 → 未覆盖前缀不完整、无误报 MISSING、无误关；配置文件不可读或凭据缺失 → chain.objects 不完整、健康不为 HEALTHY；混跑集群中不能列举的节点不接管这些 kind；同一任务 HealthOf 与 Reconcile 对 chain.objects 的判断一致（对象未列举时不报 HEALTHY 而 Reconcile 卡住）。#352 复现（`gtid_next` 跳号 363–381，单源）→ `GTID_HOLE` OPEN；对 `uuid:363-381` waive（提交大写 uuid 或拆分区间也规范化为同一 evidence）→ CLOSE(WAIVED)、健康恢复；源继续写入（已存集合增长）后 waiver 仍有效；再跳号 400–410 → 新洞重新 OPEN；MinIO 删对象（已列举）→ `SEGMENT_MISSING`，未列举时不开不关；UPLOAD_LATE WARNING→超阈值 CRITICAL，任务级覆盖生效；保留期删除后无 `ORPHAN_ROW` 且有 `RETENTION_REMOVED`；#353 复现 1 sweep 内 `SEAL_FACTS_ABSENT` OPEN，`up` 后关闭且封存事实恢复；ADR 0006/0007 的 47 个目录用例无误报 |
| P2-6a | #355 run 3 复现（宿主机停顿 2 分钟）：旧 owner 的 STOPPED 写被拒，任务行保持新 owner 的状态；人为制造行/lease 不一致 → 健康 UNKNOWN 而非 HEALTHY；36 次重复无覆盖 |
| P2-6.1 | 三节点进程内：只有 `_health` 持有者打开 WARNING `HEALTH_STALE`（task `_cluster`，subject = 任务 id），`task_health` 为 DEGRADED；另外两个节点不写。`evaluated_at` 在 5×sweep 以内（含 120 个任务）不开。没有健康行时第一次见到不开，过了 `stale_after` 才开。任务从名单消失后 CLOSE。`ListHealth` 失败不开也不关。写入栅栏是 `never`、epoch 0，不是 `_health` 的 epoch。`shadow` 不写。`/api/health` 仍只因 CRITICAL 降级（本 kind 是 WARNING，探针保持 ok），`/api/findings` 能列出。standalone 的 `_cluster` 路径不变。META_UNREACHABLE / HEALTH_STORE_UNAVAILABLE 的集群内存路径不被这一轮关掉。真机 SIGSTOP / kill -9 仍归已交付的 P2-6a/b/c，不在本步重跑 |
| P2-6.2 | 非共享存储下 STOPPED 任务的链 finding 由最后 owner 打开/关闭、健康不为 UNKNOWN；任务在另一节点重启后旧目录 finding 以 OWNER_CHANGED 关闭、旧 owner 的委派写被拒；kill 最后 owner，`node_gone_after` 后 UNKNOWN(node gone)；新 lease 后委派失效 |
| P2-6.3 | `health.evaluator=on` 期间加入一条没有 version 的心跳：本节点评估退回 `shadow`（不改 finding 行、不通知），该心跳消失后恢复 `on`。v0.5.67 不自动退回的现状保持到本步之前 |
| P2-7 | 每个健康条件单独破坏一次，`/api/alerts`、`task_healthy`、`task_health_status` 在 1 个评估周期内变化；`task_healthy` 对每个任务总有序列（含 STOPPED、从未启动）；**meta 暂停（SIGSTOP mysqld）时 scrape ≤2s 返回、每个缓存任务 `task_healthy=0`**；mute 不改变健康、waive 改变健康；key 级 mute >7d、任务级 >24h 被拒；MUTE/RENEW/EXPIRE/END_EARLY 均有 transition 且带 reason/actor；换 owner 后 mute 仍生效；`task_muted` 每个任务总有序列；standalone 换数据目录后写入被拒，`health takeover` 后恢复，`.node_id` 不随主机名变化；`/api/alerts` 分页可读完 5000 条 |
| P2-8 | 告警页与 `/api/alerts` 一致（20 个构造 finding）；徽标四态；静音/豁免操作需填原因并显示操作者；关闭的 finding 7 天内可见；#364 第 6 项复现：被拒事件按事实时间排在 FINDING 事件之前；浏览器控制台无错误；中英文 |
| P2-9 | 全部用**本地 mock 接收端**（离线可跑）：一个记录请求的 mock webhook（AM webhook 格式）、一个 mock Alertmanager（`/api/v2/alerts`）、mailpit 作为 mock SMTP（经真实 Alertmanager 容器的 `email_configs`）、一个 mock 钉钉/飞书 relay（校验转换后的机器人消息体）。接收端停 10 分钟后恢复：全部转移按 generation 到达、按 delivery id 无重复；meta 只读期间产生的转移在恢复后以 `FactAt` 排序；`_notify` 切换不丢不重；flapping 合并；静音抑制 OPEN 不抑制 CLOSE、7 天到期恢复、续期生效、无 reason 被拒；升级首轮只发一条摘要；任务级 mute 期间 Prometheus 规则 `task_healthy == 0 unless on(task_id) task_muted == 1` 不触发（用 promtool 规则单测 + 本地 Prometheus），mute 结束后触发；真实钉钉/飞书/邮件账号验证列为可选 |

阶段 2 收尾：BinlogServerQA 在 tip 上做全量回归（DBA 还原、failback、只读 meta、kill -9 + adopt、升级自 v0.5.66：已存过期行以 UPGRADE 关闭且只有一条摘要通知）后发版。

## 15. Decisions (proposed defaults, 2026-10-15; Adrian may override)

以下是建议默认值，**BinlogServerQA 第 3 轮评审已同意 D1–D5**；Adrian 尚未批准，可推翻。正文已按此修改。

| # | 问题 | 建议默认值 |
|---|---|---|
| D1 | schema 下限 | v0.6.0 把 `minRequiredSchemaVersion` 提到 **9**，删除无栅栏的集群回退；v0.5.x 的回退只保留到 v0.6.0。附加：feature flag `health.evaluator`（`off|shadow|on`，v0.5.x 默认 off，v0.6.0 默认 on），6→9、7→9、8→9 直接升级有测试，拒绝启动时给出明确的 `./migrate up` 提示（§9） |
| D2 | 非共享存储下 STOPPED 任务的链评估 | 最后 owner 继续评估自己本地目录，作为 `_health` lease 的按节点 scope；**委派绑定任务最后一个 lease epoch + 节点 id**，任务一获得新 lease 委派即失效；旧目录 finding 由新 owner 以 OWNER_CHANGED 关闭；只有最后 owner 消失时才 UNKNOWN（§8.2–8.3） |
| D3a | 通知静音（mute） | 只抑制通知，不改变事实与健康。key 级默认且最长 7 天；任务级（`kind=*`）最长 24h，源恢复（dump 收到事件）后自动结束；都可续期，记录原因与操作者；一个 API、一张表 `task_mutes`；导出 `task_muted`，推荐规则用 `unless`；不提供抑制事实的维护窗口（§7.1、§7.2） |
| D3b | 事实豁免（waiver） | 绑定 evidence（确切 GTID 区间 + 原因 + 操作者），对该确切区间永久有效（evidence 为规范化洞区间 `uuid:a-b`，§7.1）；Derive 不再产生该洞，健康恢复；区间变化即失效、重新打开。只有 `Waivable` kind（目前 `GTID_HOLE`）可豁免（§7.1、§6） |
| D4 | 通知通道 | 只做 webhook（Alertmanager 兼容格式，两种模式见 §7.4）；文档说明经 Alertmanager 或 relay 路由到钉钉、飞书、邮件；阶段 2 无内建发送器 |
| D5 | `UPLOAD_LATE` 升级 | 默认 24h 后升级为 CRITICAL，全局默认可配，任务级可覆盖；**依赖 P2-5a 让 #348 的 grace 可配置并显式传入**（§6、§13） |

开放问题：无（以上默认值待 Adrian 确认）。

## 16. 评审修订对照

### 16.1 第 1 轮（BinlogServerQA @ b8530d01，FAIL）

评审原文：`/workspace/binlog-server-verify-evidence-375/review.md`（box 上）。

| 评审项 | 处理 | 位置 |
|---|---|---|
| P1-1 DUMP_STALLED 在重试循环中误关 | 只对有意 STOPPED / 另一 epoch 豁免；新增 `SOURCE_UNREACHABLE` 并交接；QA 黑洞 3 分钟不间断 | §6、§10、P2-4、§14 |
| P1-2 两个 epoch 计数器 | 栅栏只用任务 lease epoch；委派绑定最后 lease epoch + 节点；冻结 owner 场景（#364/#355）复核 | §4 `Fence`、§8.2/8.3/8.7、D2、P2-2/P2-6 QA |
| P1-3 Reconcile 误关未接管 kind | `KindSpec.Managed`，逐 PR 接管 | §2.1、§4、§5.6、§13 Managed 列、P2-1/P2-3 QA |
| P1-4 重启后信号误关 | 首次成功前 Unknown | §2.3、§4、§5.5、P2-4 QA |
| P1-5 #360 未吸收 | P2-6a 前置：`fencedExec` + 与 `task_leases` 交叉校验 | §8.4、§10、§13、P2-6a QA |
| P2-1 open≡sealed 不可见 | I4 按 replay 顺序判定，含 open；新增 golden | §6、P2-3 |
| P2-2 分页导致漏关 | 评估器按任务 keyset 分页读完 | §4 `ListOpenFindingsForTask`、§5.1、§5.6、P2-2/P2-3 QA |
| P2-3 meta 宕机时指标 | 缓存任务列表输出 0/UNKNOWN；真实 2s | §7.2、P2-7 QA |
| P2-4 UPLOAD_LATE 无 grace | P2-5a 先落实 #348 15min grace | §6、§10、§13、D5 |
| P2-5 升级通知风暴 / 混跑翻转 | reason=UPGRADE 摘要；feature flag，混版本拒绝 on | §7.4、§9、D1 |
| P2-6 affected rows 判 ErrFenced | 显式 `SELECT … FOR UPDATE` 比较；`AS new` | §4、§8.3、P2-2 QA |
| P2-7 通知顺序 / pending | `FactAt` + generation 有序；standalone 持久化 pending，meta 模式由调和补齐 | §4、§5.1、§7.4、P2-9 QA |
| P2-8 ack 与 §6 矛盾 | 拆成 mute（限时）与 waiver（evidence 绑定） | §6、§7.1、D3a/D3b |
| P2-9 缓存不能证明可读 / bitrot | 只缓存「不可读」；sha256 轮转复核 | §5.7、P2-3 QA |
| P3 负载下 HEALTH_STALE | 阈值可配，默认 5×sweep，WARNING；UNKNOWN 只发 WARNING 通知 | §6、§7.4、§8.9 |
| P3 watchdog 时延 | 保留 TRANSITION 触发 | §8.9、P2-4 |
| P3 Alertmanager 格式 | 区分 AM webhook 格式与 `/api/v2/alerts`，二选一 | §7.4 |
| P3 P2-9 依赖外部账号 | mock webhook / mock AM / mailpit / mock relay，真实通道可选 | P2-9 QA |
| P3 `_cluster` standalone 规则 | 写明 standalone 规则 | §8.8 |
| 小处：clears_when 审计 | `kind_version` 存进 `finding_transitions` | §4、§9 |

### 16.2 第 2 轮（BinlogServerQA @ 25ed843e，FAIL，仅文档）

第 1 轮全部关闭。第 1 轮 P2-4 经更正降为 P3（`assurance.DefaultUploadGrace` 已存在，缺的是配置与显式传入），P2-5a 红测已改。

| 评审项 | 处理 | 位置 |
|---|---|---|
| R2-1 无 task_leases 行时栅栏失效 | Fence 三种模式：`cluster` / `standalone`（node + instance，跳过 task_leases）/ `never`（epoch 0）；QA 覆盖 standalone+meta 与从未启动任务 | §4 `Fence`、§8 开头、§8.3 第 1 步、§9、P2-2 QA |
| R2-2 两个写者撞 generation | generation 只在 `ApplyTransitions` 事务内按 `task_health.generation+1` 分配；重放按 `transition_digest` 内容识别；QA 并发 1000 次 | §4、§5.4、§8.3、§9、P2-2 QA |
| R2-3 sha256 预算线性放大、与 50ms 冲突 | 异步 `Rehasher`：全局字节/秒预算、跨任务轮转游标、idle I/O；评估路径不哈希；QA 记录 100 任务带宽 | §5.7、§9、P2-3 |
| R2-4 Unknown 让重启后全体 UNKNOWN / 少成功信号卡住 | Unknown 内部化，只对有 open 行的 key 生效、不进健康；替代关闭条件 `Probe` 表 + 显式健康检查；24h 后降 INFO | §2.3、§5.5、P2-4 |
| R2-5 #357 空 `.open` 被报 CRITICAL 并截断 | 排除空/只有头/小于最小事件的 open 段；INFO `SEGMENT_EMPTY_LEFTOVER`，永不截断；新增 golden | §6、P2-3 |
| R2-6 同 owner 过期 lease 卡死评估 | `ErrLeaseLagging` 可重试；renew/reclaim 触发 MarkDirty；sweep 每轮重读 lease | §4、§5.1、§5.2、§8.3、§8.6、P2-2/P2-6 QA |
| R2-7 FAILED 无 finding | `TASK_FAILED`（CRITICAL，按失败类别分 subject，含 #362、#354 1236、#338 权限）及清除条件，放进 P2-4 | §6、§13、P2-4 QA |
| P3 时钟偏差 | 时间列用库 `NOW(6)`，关闭判定按 generation/单调序号，不比较跨节点墙钟 | §5.5、§8.8 |
| P3 心跳无版本字段 | 计划（P2-6，未实现）：视为 v0.5.66，`on` 期间旧节点加入退回 `shadow`；v0.5.67 靠运维在混跑期间保持 `off` | §9、P2-6 QA |
| P3 §7.4 残留 Alertmanager 旧说法 | 删除，改为 `notify.mode=alertmanager` 一句 | §7.4 |
| P3 waiver evidence 规范 | 规范化洞区间格式（小写 uuid、排序、合并） | §7.1、D3b、P2-5 QA |
| P3 SOURCE_UNREACHABLE 计划重启 | 两轮去抖、任务级阈值；文档提示 mute（第 3 轮改为任务级 mute，见 §16.3） | §6、§7.4、P2-4 QA |
| P3 P2-5a 红测已绿 | 改为「配置的 grace」与「任务级覆盖」 | §13、§14 |

### 16.3 第 3 轮（BinlogServerQA @ f6edb2df，FAIL 仅维护窗口；改后 PASS）

R2-1 至 R2-7 与第 2 轮 P3 全部关闭；D1–D5 同意。

| 评审项 | 处理 | 位置 |
|---|---|---|
| P2 维护窗口抑制事实、无存储无审计、与 mute 重叠 | 删除维护窗口；改为任务级 mute（`kind=*`，≤24h，源恢复自动 END_EARLY）；与 key 级 mute（7d）共用 API 与 `task_mutes` 表；MUTE/RENEW/EXPIRE/END_EARLY 带 reason/actor 入 `finding_transitions`；`_notify` 持有者判断，换 owner 不影响；`task_muted` 指标与 `unless` 规则；健康/finding/预检照实 | §6、§7.1–7.4、§9、D3a、P2-4/P2-7/P2-9 QA |
| P3 `rehash_cursor` 按任务存 | 改为单行表 `rehash_cursor` | §5.7、§9 |
| P3 standalone `node_id` 来源与迁移 | `{data_dir}/.node_id` 持久化 id；显式 `health takeover` 命令/API | §8、P2-7 QA |

## 17. P2-3 实现说明（2026-10-15）

P2-3 按 §5、§5.7、§6 落地（`internal/tasks/evaluator.go`、`rehash.go`，`health.evaluator` 默认 `off`）。与正文的差异和留给后续 PR 的部分：

- **open 副本的 I4 判定只认「内容相同」**：GTID 相交（MySQL，两边都有 GTID），或一个文件的字节是另一个的前缀（#372 openstale/openhand；两边都要有 binlog magic 且不短于 23 字节）。正文写的「区间相交也算」对 sealed↔sealed 保持不变；对含 open 的一对不采用位点区间相交，因为阶段 1 的回放用例里，新 epoch 的 open 段与 sealed 段在合成数据上位点从 4 起算，按区间会被误判并截断 `/replay`，违反 §13「除 open≡sealed 那条 golden 外不改变 /replay 字节」。
- **空残留**不包括 replay 顺序上的最后一段：轮转后刚开的 open 文件只有头事件，它是正在写的段，不是残留。
- **新增 golden** 放在 `internal/tasks/testdata/replay/p2-3.json`（open≡sealed、#357 空残留），阶段 1 的 golden 不变。
- **性能**：评估路径不哈希；目录指纹与 replay 顺序不变时复用上一轮 `Check` 结论，`SegmentPositions` 与单段损坏检查按大小+mtime 缓存。1500 段热路径一轮约 3–4ms，冷启动一轮约 40ms。
- **standalone 无 meta**：评估器写 `.health.json`；`.findings.json` 里旧版本写下的 Managed 行并入评估器的 open 集合，关闭时同时在旧文件里关闭；`/api/findings` 合并评估器的 open 行。
- **meta 缺 000009**：`ApplyTransitions` 返回 `ErrSchemaAbsent` 时旧 `Report` 路径继续写 Managed kind（不出现无人写的窗口），下一次写成功后评估器重新接管。
- **留给后续 PR**：`_health` 代写与委派（P2-6，集群模式下 P2-3 只评估本节点持有 lease 的任务）；`health_eval_*` 指标与 `POST /api/tasks/{id}/health/evaluate`（P2-7）；`health takeover`（P2-7）；QA 中「100 任务时源延迟不升高」需要 e2e 环境，P2-3 只以单元测试证明复核带宽在预算 ±10% 内。BinlogServerQA 对 #378 的四条 P3 记在 #379。

### 17.1 QA 第一轮（#380 @ba1bd375）后的修正

- **复核器真正读字节**：预期校验和来自目录行的 000008 `sha256`（`ListSealedChecksums`，缺列时为空）；没有时来自任务目录的 `.seal-sha256` 账本，由封存时的 `withSealFacts`（以及 legacy failback 重写）追加。当前大小不等于封存时大小的段计 unavailable，交给链自己的检查。一个周期是一次目标快照，每步不再列目录行。
- **「不可读」只有一个定义**：封存段的读法（`ReadSealedSegmentContents` / 账本的 sealed 检查）在格式描述标明 CRC32 时校验每个事件的 CRC32，事件 size 小于头或事件体读不全也算不可读；`/window`、`/replay` 和链的 I1 都走这个读法。段中间写 0x3f 或 0xff 时，`/window` 的「不可读」与 SEGMENT_UNREADABLE 同时出现或同时不出现。
- **缓存与指纹带 ctime**：账本内容、封存损坏、损坏检查、`SegmentPositions` 的缓存和评估器的目录指纹按大小+mtime+ctime 判定，`touch -r` 式的原地改写会重新读；缓存有上限。真正的位翻转不改 ctime，由复核器发现。
- **有 meta 时的链顺序**：目录行与磁盘列表对账，磁盘上没有行的段（手工放进来的 `.sealed.e3`）进入顺序，是 I4 证据；目录行传给链，登记过的段不再报 I6_MISSING_ROW。无 meta 时不判 I6_MISSING_ROW（`NoCatalog`）。I6 在 P2-5 前仍不是 Managed kind。
- **半截 `.open.e3` 的「误关闭」**：不是评估器的问题。运行中的任务每次打开新文件时 `cleanupStaleOpenFiles` 会删掉其它 epoch 的 open 文件，所以在高频轮转（256KiB binlog）的 RUNNING 任务上，手工放的 `.open.e3` 几秒内就被删掉，CLOSE 是真实的；STOPPED 任务上它一直 open。`TestEvaluator_HalfOpenCopyStaysOpen` 固定了「文件在就一直 open」。
- 仍未做（P3，可选）：只有 magic+FDE 的残留不标 SEGMENT_EMPTY_LEFTOVER；无 meta 时任务的 `open_findings` 不含评估器的行。

### 17.2 #381：截短的封存段（QA 第二轮 #380）

- **归哪个 kind**：本地大小与封存时不同的封存段算 `SEGMENT_UNREADABLE`（I1），不算 `SEGMENT_MISSING`。理由：磁盘上的字节已经不是封存时的字节，与复核器的哈希不符同类；I6 在 P2-5 前不是 Managed kind，放进 I6 会在 P2-5 前没有写者。P2-5 翻转 I6 时不改这条：I6 判「段/对象不在」，I1 判「段在但不是封存时的字节」。
- **「不可读」的定义补两条**，仍是同一个读法（`/window`、`/replay`、I1 一致）：封存段不足 4 字节（含 0 字节）或断在事件头中间，返回包着 `ErrSegmentTruncated` 的 `ErrCorruptEventSize`；本地大小不等于 `.seal-sha256` 里该名字最后一行的大小（截在事件边界上，读法看不出来）。没有 magic 的封存段仍不由读法判定：§17 的零填充副本是 `SEGMENT_OVERLAP` 的证据，同大小的原地改写由复核器判定。
- **证据文本**：原有「事件 size 损坏」的段 evidence 不变（`corrupt binlog event size`），新原因（截短、封存时大小）各有自己的文本；`/window` 断点和 `/replay` 警告带同一个原因。
- **仍未做（P2-5）**：只有目录行 `size_bytes`、没有账本行的旧段不做大小比较（目录行在 legacy failback 重写后可能是旧大小，P2-5 的 I6 一起处理）。

## 18. P2-4 实现说明（2026-10-15）

P2-4 按 §5.5、§5.6、§6 落地（`internal/health/signals.go`、`internal/tasks/signals.go`、`internal/lintguard/managed_writer.go`）。16 种旧 kind 中除 SEGMENT_* 外的信号与任务状态 kind 都是 Managed，新增 SOURCE_UNREACHABLE、TASK_FAILED。

- **单一写入者**：热路径调用 `SignalFailure` / `SignalSuccess`。评估器拥有 Managed kind 时（`on` 且 store 有 000009）只进评估器的信号账本（进程内全局单调 seq，失败后第一轮评估时盖上任务的 lease epoch）并标脏 `SIGNAL`；否则（off、shadow、缺 000009）走旧 `Report/Resolve`，off 模式行为不变。旧 `Report/Resolve` 对 Managed 信号 kind 自动转成信号，对派生 kind 直接丢弃。lintguard `TestNoReportOfManagedKinds` 禁止任何名为 `Report/Resolve/report/resolve` 的调用（含 logger 方法和函数值，#342）在参数里提到 Managed kind。
- **信号的关闭**：只在本进程看到失败之后的成功（`SuccessSeq > FailureSeq`，或进程启动后只有成功）或探针证明时关闭；重启后 meta 仍只读则一直 open、不重复 OPEN。探针：UPLOAD_RECORD_FAILED（目录行已 UPLOADED，或未配置上传）、OPEN_TAIL_CUT_FAILED（该 open 段已不在）、DISK_LIST_FAILED（`ReadDir(subject)` 成功）、EVENT_READ_FAILED（`ListEvents(limit 1)` 成功）。本进程从未看到失败且 LastSeen 超过 `health.signal_stale_after_sec`（默认 24h）的 open 信号行降为 INFO。
- **源存活时钟（一个模型）**：每个任务一只时钟，记录「最近一次从源**在 socket 上**读到字节（事件或心跳）」的时间，以及最近一次失败的连接尝试（时间与原因）。dump 连接经自定义 Dialer 包装，每次 `Read` 返回字节就推进时钟——不是按事件被消费的时间（go-mysql 会把事件预读进很深的通道，负载下最后被消费的事件可能远比最后一个字节新）。任务 desired RUN、本节点驱动、且一个探测周期内没读到字节时，调度器每 10s 探测一次源（TCP 连接并读问候包首字节，5s 超时）：读到即推进时钟；失败（被黑洞接受连接但不回字节、拒绝连接）记为失败尝试；runner 返回 SOURCE_UNREACHABLE 错误也记为失败尝试。时钟在 STARTING、RETRY_BACKOFF 中照常走；Start 或 desired 变为 STOP 时重置。DUMP_STALLED 与 SOURCE_UNREACHABLE 只由这只时钟派生；评估器接管时 runner 的 DUMP_STALLED 信号被丢弃（off/shadow 下旧路径不变，off 下也不探测）。
- **DUMP_STALLED**：静默（现在 − 最后读到字节）≥ 45s（= dump 读超时，3 个心跳）且 SOURCE_UNREACHABLE 不成立，任何状态都成立；读到字节、停止、被别的节点持有时关闭。评估器每 5s 检查时钟是否越过阈值，越过即标脏（`LIVENESS`），首个 finding 的界是 45s + 5s + 一轮评估。runner 结束本次运行（"dump silent"）也按 socket 时钟判定，负载下读超时不再变成普通错误。
- **SOURCE_UNREACHABLE**：有一次失败尝试发生在最后读到字节之后至少 `health.unreachable_after_sec`（默认 90，可按任务覆盖），且之后没再读到字节。它度量两次观测到的事实之间的间隔，不看任务状态，所以退避让任务在源恢复后仍停在 RUNNING 之外并不计入：源 63s 后恢复，探测在约 73s 内读到问候，失败都在 63s 内，永远不会打开。界：90s + 探测周期 10s + 探测超时 5s + 5s ≈ 110s。detail 给出真实静默时长（自最后一个字节，不是自第一次重试）与最近一次失败的原因。关闭：失败后又读到字节，或停止、FAILED（TASK_FAILED 接管）、被别的节点持有。
- **交接与抖动**：两者在同一轮里互斥派生：SOURCE_UNREACHABLE 成立的那一轮在同一个 `ApplyTransitions` 里关闭 DUMP_STALLED（`HANDED_OFF`）并打开 SOURCE_UNREACHABLE，死源任何时刻至少有一条 finding open。评估只读一份时钟快照：两轮之间已恢复的中断（失败之后已读到字节）不产生任何转移，不存在一轮内打开又关闭；打开后的关闭只在后续一轮，且只有中断确实超过阈值时才会打开。
- **TASK_FAILED**：FAILED 会把 desired 置 STOP，所以用 `FailedSpecRevision == SpecRevision` 判断「当前请求失败」；subject 是失败类别（STREAM_REGRESSION、SOURCE_ERROR_1236、PERMISSION_DENIED、STORAGE_INCONSISTENT、SOURCE_SWITCHOVER、SOURCE_UNREACHABLE、CONFIG、OTHER）；重启、停止或新的请求时关闭。
- **TASK_TRANSITION_STALLED** 改为从任务行派生（STARTING/STOPPING 超过阈值、本进程没有 run、不是别的节点持有）；watchdog 仍调用 `convergeTask`，但评估器拥有时不再写 finding。
- **`_cluster`**：standalone 下评估器在有失败信号或有 open 行时评估 `_cluster`（所有 scope 视为完整）；集群模式下仍走旧路径，直到 P2-6 的 `_health` 持有者。
- **留给后续**：任务级 mute（P2-7/P2-9）；集群模式下 TASK_FAILED 要等 P2-6（FAILED 任务会释放 lease，持有者评估不到它）。

### 18.1 QA（#383 @1c267146）后的修正

- **F1 黑洞源无 DUMP_STALLED**：根因是用「事件被消费」度量静默。(a) runner 的 stall 判定用消费标记，负载下 go-mysql 预读通道里还有事件，读超时到点时标记仍新，读超时成了普通错误，进入 run 内重连，新会话又把标记重置；(b) P2-4 让 DUMP_STALLED 只能来自 runner 信号，runner 判不出 stall 就永远没有。与 main e4e2620 相比 runner 没变，tip-2c04/b5c9 的约 50s 来自旧 `Report` 路径在 runner 判出 stall 时的行为；回归在于 P2-4 让 finding 依赖那条信号。现在 runner 与评估器都用 socket 时钟。
- **F2 kill 后约 153s 才报、时长少算**：旧 SOURCE_UNREACHABLE 从评估器第一次看到 STARTING/RETRY_BACKOFF 起算且要两轮；现在从最后一个字节起算，探测保证失败证据最晚在阈值后约 15s 出现。
- **F3 63s 计划重启打开 CRITICAL**：旧模型用「不在 RUNNING 的时长」，退避对齐把它拉到 92s；现在用最后读到字节到最近一次失败的间隔，退避不计入；不存在一轮内打开又关闭。
- **测试**：`internal/replication/liveness_e2e_test.go`（真实 MySQL + 进程内 TCP 代理，可黑洞/kill，负载与空闲；阈值与退避按 1/15 缩放）断言首个 finding 与 SOURCE_UNREACHABLE 的时间界、死源期间无空档、runner 以 stall 结束运行、50/63/80s 计划重启不打开 SOURCE_UNREACHABLE、恢复后全部关闭；CI 无源时跳过。`liveness_test.go`、`internal/tasks/p24_signals_test.go`、`internal/health/signals_test.go` 在 CI 覆盖同一模型。


## 19. #385：standalone 残留目录（TASK_UNADOPTED，2026-10-15）

**问题**：无 `meta_dsn` 时任务只在内存里（ADR 0005 §Standalone）。进程重启（部署、崩溃、主机重启）后，每个任务都变成 data_dir 里的残留目录：`STOPPED`、source 为空，`start` 返回 400，复制静默停止，没有 finding、没有告警，备份一直落后于源，直到有人 adopt。

**本节的决定（不改 ADR 0005）**：先做 #385「至少」的那一半：让它不再静默。

| Kind | Sev | Scope | 打开条件（Derive） | 清除规则 |
|---|---|---|---|---|
| `TASK_UNADOPTED`（新，#385） | C | task.state | 无任务库的 standalone，任务只存在于 `{data_dir}/{id}`（目录里有分段），本进程没有它的记录（`TaskFacts.Unadopted`）；subject 为空；detail 写明复制已停、`POST /api/tasks/{id}/adopt` 恢复，或把目录移出 data_dir | adopt（有任务记录驱动它）；或目录不再有分段（移出 data_dir）。目录移走后任务不再出现在任务列表里，评估器按存储健康（`ListHealth`）里仍挂着 TASK_UNADOPTED 的任务再评估一次，只有 task.state 完整，所以只关闭任务状态类 kind；重启前移走的目录同样关闭 |

- 只在评估器 `on` 时写（与 P2-4 的 SOURCE_UNREACHABLE、TASK_FAILED 一样，旧 Report 路径不写）。有 meta（任务库）时永不派生。
- **删除也会打开**：standalone 的 `DELETE` 不删目录，删掉后目录仍有分段，就和重启后的残留目录一样，打开 TASK_UNADOPTED。处理方式一样：adopt，或把目录移出 data_dir。之所以不区分，是因为重启后两者无法区分（standalone 不落盘任务记录）。
- **没有做的另一半**：重启后自动恢复复制。Adrian 已决定做（#385），见 [ADR 0010](../adr/0010-standalone-task-records.md)：任务规格与源密码加密存于 `{data_dir}/.tasks.enc`，重启后按磁盘位点自动续传。TASK_UNADOPTED 仍用于没有记录的目录；records 开启时，删除的任务留墓碑，不再打开 TASK_UNADOPTED。
- 已知（§17.1 遗留的 P3）：无 meta 时任务列表的 `open_findings` 不含评估器的行；`/api/findings` 与任务健康已包含 TASK_UNADOPTED。

## 20. P2-5.1：chain.local 结构类 kind（2026-10-16）

§13 的 P2-5 太大，一个 PR 做不完，也没法一次 QA。按「事实是否已经可信」拆成几步。**P2-5.1** 只翻转那些事实只来自本节点磁盘（加可选的目录行）、而且在 47 个阶段 1 目录用例上没有误报的链 kind。其余的写明缺什么，留给后面的步骤。

### 20.1 本步翻转为 Managed 的 kind

| Kind | 来自哪些 assurance kind | 本步不派生的 assurance kind（及原因） |
|---|---|---|
| `SEGMENT_DAMAGED` | `I1_REGRESSION` | — |
| `CHAIN_INDEX_GAP` | `I2_INDEX_GAP`、`I2_ORDER_INVALID` | `I2_SWITCH_UNRECORDED`：评估器还没有可信的换源记录。`SOURCE_SWITCHOVER` 事件可能被事件保留期清掉，standalone 也可能没有事件库。不传 `Switches` 时，47 个用例里健康的 failback 目录会全部误报 |
| `LEGACY_FAILBACK` | `I3_LEGACY_FAILBACK`（规划器） | — |
| `SEGMENT_MISSING` | `I6_MISSING_LOCAL`（非集群）、`I6_SIZE_MISMATCH`（本地，见 20.2） | `I6_MISSING_ROW`：adopt 不回填目录行，有 meta 时被 adopt 的目录里每个旧段都会报。`I6_MISSING_OBJECT`、`I6_OBJECT_SIZE_MISMATCH`：需要 §6.1 的五条（P2-5.2）。集群模式下的 `I6_MISSING_LOCAL`：非共享存储时上一个 owner 的段不在本机，要等 P2-6 的委派 |

仍然不是 Managed：`GTID_HOLE`（需要 waiver 存取；不传 `Switches` 时跨换源的洞也会误报，P2-5.3）、`CHECKPOINT_AHEAD`（见 20.4）、`ORPHAN_ROW`、`UPLOAD_LATE`、`UPLOAD_UNVERIFIED` 的链部分，以及 P2-5a（这三项都只在配置了对象存储时有意义，P2-5.2 与 P2-5a 一起做）、`SEAL_FACTS_ABSENT`、保留期删除标记（#341/#353，P2-5.4）。

Scope：`SEGMENT_MISSING` 在本步只从 `chain.local` 判定，所以 kind 的 `Scopes` 暂时只有 `chain.local`。否则 Reconcile 要求 `chain.objects` 也完整才关闭，而现在没有任何评估器产出 `chain.objects`，打开的实例会永远关不上。P2-5.2 按 §6.1 第 1 条加实例级 origin/scope，再把 `chain.objects` 加回来。

### 20.2 旧段的大小比较（§17.2 留下的缺口）

有两个证人能说出封存段「封存时多大」：

1. `.seal-sha256` 账本（封存时由 `withSealFacts` 写，legacy failback 重写时也追加）。
2. 目录行 `size_bytes`。

规则：

- **账本有该名字的行**：账本说了算。本地大小不等于账本大小，就是 `SEGMENT_UNREADABLE`（I1，§17.2，会截断 `/replay`），不再拿目录行比较。本地大小等于账本大小而目录行不同，说明目录行旧了，不报。这是 §17.2 担心的 legacy failback 重写后的旧大小。
- **账本没有该名字的行**（ledger 出现之前封存的旧段）：如果封存的目录行有 `size_bytes > 0` 而且不等于本地大小，就派生 `I6_SIZE_MISMATCH`，归 `SEGMENT_MISSING`（origin=local），evidence `local=<n> row=<m>`，subject 是段名。**它不进入存储健康判定，不截断 `/replay`**。目录行是比账本弱的证人（写入路径不同，未修复的 legacy failback 期间会指向另一个文件），只够打开一个 finding 让人去比对，不够改变回放。
- **规划器报告 legacy failback 时不报本地大小不一致**：未修复的 A→B→A 目录里，共用名字的目录行指向后一个 stint 的文件，大小必然不同。`LEGACY_FAILBACK` 已经说明了这件事；修复按磁盘重写目录行（`diskCatalogRow`），之后自然一致。
- 对象大小与目录行不同改用独立的 `I6_OBJECT_SIZE_MISMATCH`，origin 由 kind 本身表达。本步不派生（P2-5.2）。
- 只比较 `Sealed` 的目录行，OPEN 行的大小本来就在变。

### 20.3 两轮确认（`KindSpec.Confirm`）

链的事实分几次读出来：目录行、磁盘列表、账本、规划器。这几次读不是原子的。一次轮转发生在「读顺序」和「列本地段」之间时，刚出现或刚改名的段会被当成「不在链顺序上」（`I2_ORDER_INVALID`），一次 CRITICAL 打开下一轮就关闭，还会发出通知。P2-3 的三个 kind 没有这个问题，它们只比较同一次读到的文件内容。

- 新增 `KindSpec.Confirm`：这类 kind 的实例要在**同一任务连续两轮评估**里都被派生，才进入 desired 并打开。已经 open 的实例不受影响：它要关闭照样只看清除规则。
- 评估器为每个任务记住上一轮派生出的 Confirm 键（`Facts.Confirmed`）。本轮第一次出现的键记为 pending，评估器在 `Debounce` 后再标脏一次（`EvalReason` `CONFIRM`），不用等 60s 的 sweep。真实故障的打开延迟是一轮评估加一个 debounce（默认 1s）。
- P2-5.1 的四个 kind 都设 `Confirm`。P2-3/P2-4 的 kind 不变。
- shadow 模式同样两轮确认，日志里的 OPEN 与 on 模式一致。

### 20.4 为什么 `CHECKPOINT_AHEAD` 不在本步

I5 需要 checkpoint，评估器现在不读 checkpoint。要读就要先解决两件事，否则会在运行中的任务上持续误报：

1. **读的顺序**：先读 checkpoint，再读顺序和磁盘。checkpoint 写在数据落盘之后，反过来读，checkpoint 可能指向快照里还没有的新文件。
2. **缓存**：现在的链结论按「目录指纹 + 顺序 + 目录行」缓存。checkpoint 每个事务都在变，放进缓存键会让运行中的任务每轮都重算整条链。需要把 I5 从 `Check` 里分出来单独算（它只要已存集合与尾部位置）。

这两件事和 `GTID_HOLE` 的 `Switches` 输入一起做（P2-5.3）。

### 20.5 测试（先红）

- `assurance`：账本优先（有账本行时不比目录行；账本不一致仍是 I1）；没有账本行的旧段报 `I6_SIZE_MISMATCH`；规划器报告 legacy failback 时不报；对象大小不一致是 `I6_OBJECT_SIZE_MISMATCH`。
- `health`：P2-5.1 的 Managed 表；`SEGMENT_MISSING`/`CHAIN_INDEX_GAP` 不从本步排除的 assurance kind 派生；`SEGMENT_MISSING` 只在 `chain.local` 完整时就能关闭；`Confirm` 第一轮不打开、第二轮打开、已 open 的不受影响。
- `tasks`：
  - 47 个阶段 1 目录用例（含 `/objects`、`/start`、MariaDB）各评估两轮。`SEGMENT_DAMAGED` 只出现在 4 组 damaged 用例上，`CHAIN_INDEX_GAP` 只出现在 `live-hole`（真的缺了 000004），没有 `SEGMENT_MISSING` 和 `LEGACY_FAILBACK`。
  - legacy failback 目录打开 `LEGACY_FAILBACK`，修复后关闭。
  - 有 meta 时，旧段（无账本行）目录行大小不符打开 `SEGMENT_MISSING`，改回后关闭；有账本行时不报。
  - SEALED 行的本地文件不在时打开 `SEGMENT_MISSING`，集群模式不派生。
  - 单轮看到的「不在链顺序上」不打开。

## 21. P2-5.2：对象类 kind、P2-5a、UPLOAD_LATE、UPLOAD_UNVERIFIED（2026-10-16）

P2-5.1（§20）只翻转了本地事实。本步把「配置了对象存储时才有意义」的部分做完：先满足 §6.1 的五条前置，再让对象缺失、上传迟到、上传未校验进入评估器。`ORPHAN_ROW` 仍不翻转：它在对象保留期删除后必然出现，要等保留期删除标记（#341，P2-5.4）。

### 21.1 P2-5a：上传宽限（#348）

- `upload.grace_sec`（全局，默认 900 = `assurance.DefaultUploadGrace`）与 `upload.grace_task_sec: {"<task id>": 秒}`（按任务覆盖，写法与 `health.unreachable_after_task_sec` 相同，必须为正）。
- 评估器把它显式传入 `assurance.Inputs.UploadGrace`。封存后宽限之内不判定 `I7_UPLOAD_LATE`。
- 只影响评估器。`/replay`、`/window`、`storage_alert` 不读 I7，字节不变。

### 21.2 对象存储是否配置：确认过的事实（§6.1 第 4 条）

- 进程启动时读到的配置里没有上传端点（`Scheduler` 没有 uploader）：该任务**确认没有对象存储**。`Inputs.ObjectStorage=false`，`chain.objects` 对该任务不被产出（`Facts.Produced[chain.objects]=false`），I7 不判定。
- 配置了上传：`ObjectStorage=true`，`chain.objects` 被产出，是否完整看 21.3。uploader 不支持列举（旧实现、测试替身）或列举出错（凭据、网络）：`chain.objects` 不完整。
- 配置读不出来进程就起不来，所以「读不出配置」不会被当成「没有对象存储」。

### 21.3 对象列举（§6.1 第 3 条）

- uploader 实现可选接口 `tasks.ObjectLister.ListObjects(ctx, prefix, limit) (map[key]size, truncated, err)`。S3 实现用递归 ListObjects，读到 `limit` 个即停并报 `truncated`。
- 列举的前缀是该任务 UPLOADED 行 `object_key` 的目录部分（`path.Dir(key)+"/"`，即 `prefix/cluster_key/server_uuid/`），每个前缀一次。
- **按前缀记完整性**：命中上限或出错的前缀不完整。assurance 新增 `Inputs.ObjectPrefixes map[prefix]bool`（nil 时保持阶段 1 行为：`Objects` 非 nil 即视为全部已列举）。一个段只有在它的前缀完整时才 `ObjectListed`。不完整前缀上的段既不派生对象类实例，已 open 的也不关闭。
- **快照与新上传**：列举结果缓存 `health.object_list_interval_sec`（默认 300）。快照记下列举**开始前**已经是 UPLOADED 的 key；列举之后才变成 UPLOADED、而快照里没有的 key 不算「已列举」，下一次列举（不早于 30s 后）再判。否则刚上传完的段会被旧快照判成对象缺失。
- `health.object_list_limit`（默认 10000）是每个前缀的上限。

### 21.4 实例来源与按实例的 scope（§6.1 第 1 条）

- 对象来源的实例 subject 是 `<段名>#object`（`I6_MISSING_OBJECT`、`I6_OBJECT_SIZE_MISMATCH`），本地来源保持段名。同一个段本地和对象同时缺失时是两条 finding，各自打开、各自关闭。
- `SEGMENT_MISSING` 的 kind 级 `Scopes` 恢复成 §6 的 `{chain.local, chain.objects}`，但 Reconcile 按实例取 scope：本地来源只要 `chain.local` 完整；对象来源要 `chain.objects` 完整**且**该段的前缀完整（`Facts.ObjectSegments[段名]`）。关闭、UPDATE、DOWNGRADE 都按实例 scope 判断。
- 本地文件恢复后，即使对象还没列举，本地来源的实例也能关闭（QA 第 2 轮的要求）。

### 21.5 移除对象存储导致的关闭（§6.1 第 2 条）

- 任务确认没有对象存储（21.2）时，`#object` 实例与 `UPLOAD_LATE` 以 `Reason=CONFIG` 关闭，不作为普通 EVAL。
- transition 的 detail 追加审计说明：`object storage no longer configured (was <旧配置指纹>; source: config file; at <时间>)`。旧指纹是评估器记住的上一次「有对象存储」时的配置指纹（端点 + bucket + prefix 的短哈希），进程重启后未知则写 `unknown`。

### 21.6 按任务判断 chain.objects（§6.1 第 5 条）

- 新增 `Facts.Produced map[Scope]bool`：非 nil 时 `HealthOf` 用它代替全局 `Produced` 判断该任务哪些 scope 算数。评估器总是设置它（全局 `Produced` 加上该任务是否产出 `chain.objects`）。
- 这样配置了对象存储但列举失败的任务是 UNKNOWN 而不是 HEALTHY，而 Reconcile 也因为同一个 `Complete[chain.objects]=false` 不关闭对象实例。两者读的是同一份判断。

### 21.7 翻转与派生

| Kind | 本步 | 说明 |
|---|---|---|
| `UPLOAD_LATE` | Managed，`Confirm` | `I7_UPLOAD_LATE`，WARNING；封存超过 `health.upload_late_critical_after_sec`（默认 86400，`health.upload_late_critical_after_task_sec` 按任务）为 CRITICAL（ESCALATE）。Scope `chain.local`（只读目录行）。对象存储移除时 CONFIG 关闭 |
| `SEGMENT_MISSING` | 对象来源开始派生 | `I6_MISSING_OBJECT`、`I6_OBJECT_SIZE_MISMATCH`，subject `段#object`。`I6_MISSING_ROW` 仍不派生（§20.1） |
| `UPLOAD_UNVERIFIED` | 链部分开始派生（P2-4 起已是 Managed） | `I7_UNVERIFIED`（UPLOADED 而 checksum 不是 match）。上传先写 UPLOADED 再写校验结果时会有一瞬间不一致，所以**链来源的实例**两轮确认（实例级 `Instance.Confirm`）；信号来源照旧一轮 |
| `ORPHAN_ROW` | 不翻转 | 对象保留期删除后行仍在，必然误报；等 #341 删除标记（P2-5.4） |

对象保留期删除先删对象再删本地（删除失败时保留本地并报 `OBJECT_PURGE_FAILED`）。两步之间如果有一次列举，会看到「本地有、对象无」。两轮确认加上列举间隔让这种窗口不打开 finding。删除失败时那个段确实只剩本地一份，打开 `SEGMENT_MISSING#object` 是对的。

### 21.8 测试（先红）

- `config`：`upload.grace_sec`、`upload.grace_task_sec`、`health.upload_late_critical_after_sec`（含按任务）、`health.object_list_interval_sec`、`health.object_list_limit` 的解析、默认值与校验（负数、按任务非正数被拒）。
- `assurance`：`ObjectPrefixes` 不完整的前缀上不派生 `I6_MISSING_OBJECT`，完整的前缀上派生；nil 时行为不变。
- `health`：P2-5.2 的 Managed 表；`#object` 实例在 `chain.objects` 或前缀不完整时不关、本地来源实例只要 `chain.local`；对象存储移除时以 CONFIG 关闭且 detail 带审计；`Facts.Produced` 让列举失败的任务 UNKNOWN；`UPLOAD_LATE` 超阈值 ESCALATE 到 CRITICAL；链来源 `UPLOAD_UNVERIFIED` 两轮确认、信号来源一轮。
- `tasks`：宽限按全局与按任务传入；`UPLOAD_LATE` 在宽限后两轮打开、24h 后升级、上传完成后关闭；假列举器：对象被删 → `SEGMENT_MISSING/段#object` 打开，放回后关闭；列举命中上限 → 不开不关、健康 UNKNOWN；列举出错 → 同上；快照之后才上传的段不判缺失；移除 uploader（确认无对象存储）→ CONFIG 关闭。
- `upload`：S3 `ListObjects` 的上限与 `truncated`（用 MinIO 的 e2e 覆盖，单测覆盖参数与分页截断逻辑）。

## 22. P2-5.3：GTID_HOLE、waiver、CHECKPOINT_AHEAD（2026-10-17）

§20 把 `GTID_HOLE` / `CHECKPOINT_AHEAD` 留到本步：前者要 waiver 存取，且不传 `Switches` 时跨换源的洞会误报；后者要先读 checkpoint 再读顺序/磁盘，并把 I5 从链缓存里拆出去（§20.4）。本步不碰对象类、`ORPHAN_ROW`、`SEAL_FACTS_ABSENT`、#341（P2-5.4）和完整 alerts API（P2-7）。

### 22.1 翻转

| Kind | 本步 | Confirm | 说明 |
|---|---|---|---|
| `GTID_HOLE` | Managed，`Waivable` | 否 | `I3_GTID_HOLE` / `I3_STORED_HOLE`。洞的证据来自同一次 `Check` 读到的文件内容，没有多读竞态，不设 Confirm |
| `CHECKPOINT_AHEAD` | Managed | 否 | `I5_CHECKPOINT_AHEAD`。误报靠读顺序 + I5 不进链缓存消除，不靠 Confirm |

`I2_SWITCH_UNRECORDED` 仍不派生（§20.1）：本步只把 `Switches` 传进 assurance，用来压掉跨换源的 **I3** 误报，不翻转 I2。

### 22.2 Switches：跨换源洞不再误报

- 评估器从 `Scheduler.SourceChain` 构造 `assurance.Inputs.Switches`：每个 `SOURCE_SWITCHOVER` 的 `New`（`Stored` = 事件里的 `gtid_set`），以及 `.source-chain` 里**除第一个以外**的每个 server identity（没有事件时也能认出已记录的 stint）。
- `Check` 在相邻段跨前缀走 `switchHole` 时：若上一段或下一段的 server identity（`identityOf`，UUID 前缀名才非空）在 `Switches` 里，**不**报 `I3_GTID_HOLE`。有意的 VIP / failback 边界上的间隙不是洞；真缺事务仍由规划器的 `I3_STORED_HOLE` 负责。
- `I2_SWITCH_UNRECORDED` 的行为不变（仍用同一张 `Switches` 表），只是本步继续 `notDerived`。

### 22.3 Waiver 存取（评估器最小路径）

- 存：`task_findings` 的 `waiver_evidence` / `waiver_reason` / `waiver_actor` / `waived_at` / `waiver_kind_version`（migration `000009` 已有）。standalone 的 `.health.json` 在 finding 上加同样字段。
- 写：`healthstore.WaiverStore.PutWaiver`（规范化 evidence，`CanonicalGTIDRanges`；只允许 `Waivable` kind，目前仅 `GTID_HOLE`）在 finding 行写入上述列，并往 `finding_transitions` 记一条 `op=WAIVE`（`reason`/`actor` 进 detail）。`DeleteWaiver` 清空列并记 `op=UNWAIVE`。不在 Put 时 CLOSE：下一轮 Evaluate 的 Derive 跳过该 evidence，Reconcile 以 `reason=WAIVED` 关闭（已有逻辑）。
- 读：`ListWaivers` 返回该任务所有 `waiver_evidence` 非空的行（含已 resolved），评估器填入 `Facts.Waivers`。
- HTTP：`POST /api/alerts/waive` `{task_id,kind,subject,evidence,reason}`（actor 来自 auth 或 `operator`）走 PutWaiver 并 `MarkDirty`；`DELETE` 同路径 Unwaive。完整 alerts 列表 / mute 仍归 P2-7。

### 22.4 CHECKPOINT_AHEAD：读顺序 + I5 出缓存（§20.4）

1. **读顺序**：`chainFacts` 先 `GetCheckpoint`，再算 fingerprint / `chainOrder` / 列本地段。checkpoint 写在数据落盘之后；先读顺序再读 checkpoint 会在运行中的任务上把 C 指到快照里还没有的新文件。
2. **缓存**：链结论仍按「目录指纹 + 顺序 + 目录行（+ Switches 键）」缓存，**缓存的 Inputs.Checkpoint 恒为空**，`Check` 结果里不含 I5。每轮用本轮读到的 checkpoint 单独跑 `CheckCheckpoint`（只需已存集合与尾部位置），把 I5 违规追加进 `ChainFacts`。checkpoint 每个事务都在变，不得进缓存键。
3. assurance 导出 `CheckCheckpoint`（或等价：`Check` 在 Checkpoint 为空时自然不产 I5，另有只跑 I5 的入口），单测覆盖「空 checkpoint 的 Check 无 I5」「同一链改 checkpoint 只影响 CheckCheckpoint」。

### 22.5 测试（先红）

- `assurance`：跨换源相邻段在 destination/prev identity 属于 `Switches` 时不报 `I3_GTID_HOLE`，不属于时仍报；`CheckCheckpoint` 独立于 body；缓存场景下改 checkpoint 只改 I5。
- `health`：P2-5.3 Managed 表（`GTID_HOLE` Waivable 且 Managed，`CHECKPOINT_AHEAD` Managed，二者无 Confirm）；既有 waiver exact-match 用例仍绿。
- `healthstore` / `meta`：PutWaiver → ListWaivers 读回规范化 evidence；Unwaive 清空；关闭后 waiver 仍能 List 到。
- `tasks`：评估器传入 Switches 后，failback 黄金目录不因跨 stint 打开 `GTID_HOLE`；构造单源内部洞（#352）→ `GTID_HOLE` OPEN → PutWaiver 精确区间 → CLOSE(WAIVED)、健康恢复；checkpoint 超前 → `CHECKPOINT_AHEAD` OPEN，拉回后关闭；连续两轮评估中 checkpoint 前进不导致整链 Check 每轮重算（I5 除外）。
- `api`：`POST /api/alerts/waive` 400（非 Waivable / evidence 非法）、200 后 Evaluate 关闭该洞。

## 23. P2-6b：数据面端到端写栅栏（#417，2026-10-17）

### 23.1 问题

P2-6a 只栅住了任务行。集群里 owner 被冻住（SIGSTOP、宿主机停顿、长 GC）超过 lease TTL，新 owner 以 epoch+1 接管后，旧 owner 醒来时手里已经有读好的事件，它产生的副作用都没有栅栏：

| 副作用 | 原来的保护 | 冻结醒来后的后果 |
|---|---|---|
| open 分段的追加 / flush | 无 | 与新 owner 写同一个分段（#417），`STORAGE_INCONSISTENT re-dump damage` 永久挂住 |
| 接管时继续旧 epoch 的 open 文件 | rename，沿用 inode | 旧句柄写进新 owner 的分段或封存文件（#415，由 #416 改成复制） |
| 新 open 文件 | 无 | 旧 epoch 名字的残留文件 |
| 封存 rename | 封存前 `Verify`，之后 stat 再 rename | `Verify` 与 rename 之间冻住时覆盖或抢先占用封存名 |
| checkpoint、OPEN/SEALED 目录行、目录行删除 | 无 | 旧 epoch 的位点覆盖新 owner 的位点，续传从错的位置开始 |
| 上传后的目录行更新 | 无 | 旧 owner 改写新 owner 的目录行 |

### 23.2 栅栏令牌

令牌就是任务 lease 的 `(owner, epoch)`（§4，与 P2-6a 同一个计数器，从不与 `_health` epoch 比较）。每个写边界有两道检查：

1. **本地 lease 时钟（`tasks.LeaseClock`）**。调度器每次 Acquire / Renew 成功，记下**发出请求之前**的 `time.Now()`；Renew 返回 false、grace 用完、`yieldLostLeaseLocked` 时把该 epoch 记为 lost（同一 epoch 不会再复活，只有更高 epoch 能覆盖）。`Fresh(task, epoch)` 要求未 lost 且距上次确认不到 `ttl × 4/5`。比较用 Go 的单调时钟读数，进程被 SIGSTOP 或虚机暂停时单调时钟照走，所以醒来后的第一次写就会被拒，不需要先和 meta 通信。剩下的 1/5 吸收本机与数据库的时钟速率差。不启动 Acquire 的启动路径（同一 worker 已持有 lease）先同步 Renew 一次再确认。
2. **meta 事务内校验**。run 的 context 带上 `tasks.FenceIdentity`。`UpsertCheckpoint`、`UpsertBinlogFile`、`DeleteBinlogFile` 看到它时在同一个事务里先 `SELECT owner_worker_id, epoch FROM task_leases WHERE task_id = ? FOR UPDATE`，owner 和 epoch 都相同才写；lease 已释放（owner 为空）或没有行也拒绝（与 P2-6a 的 STOPPED 收敛写不同：数据写必须是当前持有者）。行锁让这次写和 Acquire 串行：接管先提交，旧写被拒；旧写先提交，新 owner 续传时能读到。

两道都返回 `tasks.ErrWriteFenced`，runner 包成 `ErrLeaseEpochMismatch` → `tasks.NewLeaseHandoff`：调度器只停掉本进程，不写 FAILED，不写 `CHECKPOINT_WRITE_FAILED`。

### 23.3 写边界

| 边界 | 本地时钟 | meta 事务 |
|---|---|---|
| run 开始（`resolveRunStart` 之前，含接管复制） | ✓ | — |
| 打开分段（`openBinlogWriterIn`，新建 open 文件、写 magic） | ✓ | OPEN 行 ✓ |
| 追加（`appendAndPersist`，在 Append/Flush 之前） | ✓ | — |
| checkpoint 与 OPEN 行（`flushCheckpoint`、rotate、LATEST 锚点） | ✓（flush） | ✓ |
| 封存（`Verify` 之后、rename 之前再查一次；rename 用 `renameat2(RENAME_NOREPLACE)`） | ✓ | SEALED 行 ✓ |
| 释放已被 UPLOADED 覆盖的 open 段 | ✓ | 行删除 ✓ |
| 上传（`uploadSealed` 之前；上传后的目录行更新在同一 context 下） | ✓ | ✓ |

后台补传不属于某个 run，没有令牌，按原来的方式写（它只处理已封存、已有目录行的文件）。standalone 不接本地时钟、不带令牌，行为不变。

### 23.4 与 #411、#416 的关系

- **#416（#415）叠在下面**：接管把旧 epoch 的 open 文件复制成新 inode。它保证就算旧 owner 在本地检查和 `write(2)` 之间冻住，写进去的也是一个没有名字的文件。本节的栅栏让旧 owner 一醒来就停，不再产生字节、位点、目录行。两者缺一不可：只有复制，旧 owner 还会写 checkpoint 和目录行；只有栅栏，检查与写之间仍有微秒级窗口。
- **#411（P2-6a）合入本 PR**：任务行的 `meta.FencedExec` 与本节 `execUnderFence` 共用一个 `leaseRowForUpdate`（同一条 `SELECT … FOR UPDATE`）。差别只在策略：任务行允许在 lease 已释放或没有行时收敛到 STOPPED，数据写只允许当前持有者。

### 23.4.1 为什么两个栅栏必须一起上（#420 QA 测试 3）

只有本节的数据面栅栏、没有 P2-6a 时，meta 暂停超过 lease TTL（QA 用 SIGSTOP 暂停 meta 的 mariadbd 30s）会把任务永久停住：

1. meta 恢复后，owner B 的本地 lease 时钟已过 `ttl × 4/5`，下一次追加被拒，run 按租约移交退出。这一步是对的。
2. 同一时刻 A 在 `task_leases` 上认领过期 lease（epoch+1），但还没写任务行。
3. B 的续约返回 false（owner 已变），`rowTakenOverLocked` 读任务行，行上还是 B，于是走老的 `failSafeStopLocked` 路径，把 STOPPING / STOPPED（`last_error = lease lost`，desired STOP）写进任务行。
4. A 读回任务行，看到 STOPPED，按远端 Stop 取消自己的 dump。任务停在 STOPPED、没有 owner，需要人工 Start。

main 上没有数据面栅栏，B 在恢复后往往先续约成功，所以这个竞态很少出现；它本身就是 #360。数据面栅栏让 B 先退出，把竞态变成了必现。P2-6a 让第 3 步的写在同一事务里对 `task_leases` 校验：lease 已是 A 的新 epoch，写被拒，B 只在内存里让出，任务行留给 A。续约返回 false 只可能是 owner/epoch 已变或 lease 已释放（`RenewTaskLease` 不看过期时间），所以第 3 步的写总能看到新的持有者。

回归测试 `tasks/p26_handoff_row_test.go` 覆盖两种先后顺序（栅栏先拒、续约先发现）；在没有 P2-6a 的提交上它复现 QA 的现象（`STOPPED`、owner 为空、`last_error = lease lost`）。

### 23.5 剩余窗口

本地检查与系统调用之间（微秒级）冻住超过 TTL：追加由 #416 的 inode 分离兜住；封存 rename 用 NOREPLACE，最多留下一个没有目录行的封存文件（SEALED 行的 meta 写会被拒），由链检查报出，不会静默覆盖。

行为变化：`LEASE_DEGRADED`（meta 续约报错）期间，超过 `0.8 × lease_ttl_sec` 没有续约成功就停止落盘并按租约移交退出，不再写到 `lease_grace_sec`。默认 TTL 15s、grace 30s，原来 lease 在库里过期后还会继续写约 15s，这正是可能与新 owner 双写的窗口。meta 恢复后任务按过期 lease 被重新认领。

### 23.6 测试（先红）

- `replication/write_fence_test.go`：运行中 lease 移走后放出冻结前读到的事件，分段字节不变、checkpoint 不再写、run 以带 `ErrWriteFenced` 的租约移交结束；一开始就过期的 run 不建 open 文件；封存目标已存在时 NOREPLACE 拒绝且不覆盖。
- `meta/write_fence_test.go`（sqlmock）：新 epoch、已释放、无行都拒绝且不执行 INSERT；匹配时 INSERT 与锁在同一事务；无令牌时仍是单条语句。
- `tasks/write_fence_test.go`：`LeaseClock` 的 4/5 窗口、lost 不复活、旧 epoch 不覆盖新 epoch。
- `tasks/p26_handoff_row_test.go`：§23.4.1 的两种顺序下旧 owner 都不改任务行。
- QA：两节点 + `rotload.sh` 高频轮转 + 对 owner `SIGSTOP` 20s/60s/120s 后 `SIGCONT`，旧 owner 日志出现 `fence: refused`，任务不进入 `STORAGE_INCONSISTENT`，最终恢复校验和一致。

## 24. P2-6c：无主任务的评估与 FAILED 不被改写（#421，2026-10-17）

### 24.1 问题（#421 复现）

两节点集群、各自独立的 `data_dir`。n1 被冻住超过 lease TTL，n2 以 epoch 2 认领，读不到 n1 目录里的 open 分段，任务进 `FAILED`（`SEGMENT_NOT_ON_WORKER`），`markFailedLocked` 把任务行写成 owner 空 / epoch 0 并释放 lease（`task_leases` = `'' / 2`）。之后：

1. **没人评估**：`Evaluator.fenceFor` 在集群里只让「任务行上的 owner 本人」评估。FAILED 的任务行没有 owner，于是 `TASK_FAILED` 永远不打开，`task_health` 停在 n1 epoch 1 写的 `HEALTHY`，`/api/health` = ok，0 finding。这不只是非共享盘的问题：**集群里任何原因的 FAILED 都是静默的**。§8 第 2 条写了由 `_health` 持有者代评，但没实现。
2. **FAILED 被改成 STOPPED**：n1 醒来，`applyRemoteStops` 读到行 `FAILED / desired STOP`，`noteRemoteStopLocked` 把它当成控制面 Stop：内存改 STOPPING、取消 dump。run 退出时 `markStoppedLocked` 读回的新行是 FAILED，用它替换了内存抄本，`writeStoppedLocked` 取的栅栏身份是新行上的 owner 空 / epoch 0，`persistTaskFencedLocked` 见身份为空就退回**不带栅栏**的 `UpsertTask`，于是写成 `STOPPED / desired STOP / owner 空 / epoch 0`。就算带上 n1 的身份 (n1, 1)，P2-6a 的规则「lease 已释放（owner 空）就放行」也不比较 epoch，`'' / 2` 一样放行。
3. 部署文档没写「集群节点要共享 `data_dir`，否则接管会 FAILED」。

### 24.2 谁评估无主任务

- **`_health` lease**：`task_leases` 中 `task_id = '_health'` 的一行（`tasks.HealthLeaseID`），用现有 `LeaseManager` 的 Acquire / Renew，TTL = `3 × sweep`（默认 sweep 60s → 3 分钟；持有者消失后最多一个 TTL 内由别的节点接手）。每次 sweep 先续约，续约失败再 Acquire。它的 epoch 只用来选人，从不写进 finding / health 行，也从不与任务 epoch 比较（§8 原则不变）。`task_leases` 没有外键，任务列表从 `tasks` 表来，这一行不会被当成任务。
- **无主** = 任务行上没有 owner（`owner_worker_id` 为空或 epoch ≤ 0）：`FAILED`、`STOPPED`、`CREATED`、尚未被认领的 `STARTING`。有 owner 的任务仍只由 owner 评估（§8 第 1 条不变）。
- 持有者在 `responsible()` 里加入全部无主任务；评估时：
  - **只评 `task.state`**。`Facts.Produced = {task.state}`：链（`chain.*`）与信号（`signals`）scope 不完整也不进 `Unknown`，这些 kind 的已有行保持原样（Reconcile 只动本轮完整的 scope）。持有者进程里可能还留着它以前持有该任务时的信号，代评时不读，避免用旧信号开关信号 kind。
  - 任务行就是事实：不做 `leaseRowAgrees` 交叉校验（那是防「旧 owner 写错的 STOPPED」，代评写入时栅栏已保证 lease 无人持有；行上有 owner 的任务根本不代评）。
  - 由此 `FAILED`（含 `SEGMENT_NOT_ON_WORKER`）打开 CRITICAL `TASK_FAILED`，健康 `UNHEALTHY`，`/api/health` 不再是 ok；运维 Start 后任务有了 owner，下一轮由新 owner 评估、按清除规则关闭。
- 链 scope 的代评 / 委派（§8 第 2 条第 2 点、§15 D2）**不在本步**：无主任务的链 finding 保持最后一个 owner 写下的样子。

### 24.3 代写栅栏

- lease 行存在：`Fence{Mode: cluster, TaskEpoch: lease.epoch, Node: lease.owner（已释放则为持有者自己）, Proxy: true}`。
- lease 行不存在（从未启动）：`Fence{Mode: never, Node: 持有者}`（§8 原有规则）。
- `healthstore.CheckFence` 对 Proxy 不变的部分：同一事务 `FOR UPDATE` 锁住 lease 行，要求 epoch 仍等于 `TaskEpoch` 且已过期或已释放；`task_health.fence_epoch > TaskEpoch` 拒绝。
- **新增一条放宽**：lease 已**释放**（owner 空）时，同 epoch 下 `fence_node` 不同也放行。理由：Acquire 对已释放的行总是 epoch+1（`owner_worker_id <> ?` 恒真），所以释放之后再也没有任何持有者能以这个 epoch 写，代写不会挡住谁；而释放时任务行与 lease 行都已清掉 owner，「最后 owner」已无处可查，按节点比较只会让代写永远被拒（#421 中 n2 以 epoch 2 写过一次健康后释放）。lease 只是过期、owner 仍在时，仍按节点比较，原 owner 同 epoch 重新认领（§8 第 6 条）不会被代写锁死。

### 24.4 FAILED 不被旧 owner 改写

规则：**`FAILED` 只能由运维 Start（或新的 owner 运行）离开；任何 lease 丢失 / 远端 stop 的收尾路径都不能把它写成 `STOPPED`。** 三处一起改，缺一处都会再漏：

1. **远端 stop 分支**：`noteRemoteStopLocked` 看到共享行是 `FAILED`、而本进程的运行没有写过 FAILED（内存不是 FAILED）时，这不是控制面 Stop，而是**别的 epoch 的失败**：取消本地 dump、在 lease 时钟上把本 epoch 记为 lost（数据面栅栏立即拒绝后续写），内存抄本换成共享行（`FAILED`、`last_error` 原样），记 `TASK_LEASE_LOST`（`row FAILED by another epoch`），**不写任务行**。run 退出时内存是 FAILED，`runTask` 的退出路径本来就跳过 FAILED，不写 STOPPED、不释放别人的 lease。
2. **`markStoppedLocked`**：读回的行是 `FAILED` 且 spec 不比内存旧（没有更新的运维 Stop / Start）时，只把内存换成该行，不写 STOPPED。栅栏身份始终用**本进程这次运行**的 (owner, epoch)（读回行之前的内存抄本），不再取读回行上的 owner 空 / epoch 0；没有身份才走无栅栏写（standalone）。
3. **任务行栅栏（补 P2-6a）**：规则收进一个纯函数 `tasks.CheckTaskRowFence`，MySQL 与测试用的内存实现共用：
   | `task_leases` 行 | 放行条件 |
   |---|---|
   | 有 owner | owner 与 epoch 都等于写者 |
   | 已释放（owner 空） | lease epoch 等于写者 epoch（释放的是自己这一代）；epoch 更大 = 之后有别的 epoch 运行并结束 → `ErrFenced` |
   | 没有行 | 同一事务 `SELECT owner_worker_id, epoch FROM tasks … FOR UPDATE`：行 epoch 为 0，或 owner 与 epoch 等于写者；行上是别的 owner / 更大 epoch → `ErrFenced` |
   被拒的写和原来一样只在内存里让出（`yieldLostLeaseLocked` / `markHealthDirty(LEASE_LOST)`）。
4. **真机复现补出的两条路径**（单测只覆盖了前三条，真机上旧 owner 仍把行写成 `STOPPED / last_error = context canceled`）：
   - 第 1 条取消 run 后，续约循环的 Renew 因 context 取消返回错误，`markLeaseDegradedLocked` 把内存的 FAILED 改成 `LEASE_DEGRADED` 并**无栅栏**写回任务行，run 退出时内存不再是 FAILED，又走了 STOPPED 收尾。改为：续约循环在 run 的 context 已结束时直接返回，不算降级；`markLeaseDegradedLocked` 对 FAILED 不动（与 `failSafeStopLocked` 一致）。
   - `finishStoredStopLocked`（STOPPED 之后「行上仍是 Stop 就再写一次」）用无栅栏的 `persistTaskLocked`，读回 `FAILED / desired STOP` 会当成未完成的 Stop 写 STOPPED；`writeStoppedLocked` 被栅栏拒绝时返回 nil，这一步照样执行。改为：读回 FAILED 直接返回。

### 24.5 集群存储前提（文档，不改行为）

接管按 `binlog_files.file_path` 走（deployment §6.3「Worker 异常退出与自动接管」）。集群节点**必须共享 `data_dir`**（同一路径挂同一块共享存储，或任务固定在一台机器上不接管）；否则 owner 换机后读不到 open 分段与未上传的封存分段，任务进 `FAILED`（`SEGMENT_NOT_ON_WORKER`）。本步之后这个 FAILED 会以 CRITICAL `TASK_FAILED` 告警，不再静默，但备份仍然停了，需要运维把目录挂过来再 Start。deployment 文档补上这条前提。**补充（§24.8）**：即使旧路径在新节点上「碰巧可读」（同机绝对路径、错误挂载），只要不在本节点配置的 `data_dir` 下，同样 `SEGMENT_NOT_ON_WORKER`，禁止续写外来目录。

以下作为**备选方案**列出，待 Adrian 决定，本步不实现：

| 方案 | 做法 | 代价 |
|---|---|---|
| A. 启动检查 | 每个节点在 `data_dir` 写 `.cluster_marker`（随机 id），首个节点把 id 记到 meta（新表或 `cluster_settings`）；其他节点启动时读不到同一 id 就拒绝以 worker/all-in-one 角色启动（或降级为不接管别人任务），日志与 `/api/health` 给出原因 | 需要一次迁移；只能发现「不共享」，不能让不共享的部署工作 |
| B. 相对路径 + 从对象续写 | `binlog_files.file_path` 改存相对 `data_dir` 的路径；接管时本地没有分段就从对象存储下载已上传的封存段，open 段未上传部分从源端按 checkpoint 重拉（源端 binlog 仍在时） | 依赖对象存储与源端保留；open 段上传或更短的封存周期才能缩小重拉窗口；改动面大（续传、目录、链检查） |
| C. 维持现状 + 告警 | 本步的 `TASK_FAILED` 告警 + 文档前提 | 不共享时仍需人工 |

### 24.6 测试（先红）

- `tasks/p26c_unowned_eval_test.go`：集群评估器、内存 lease；任务行 `FAILED / owner 空`、lease `'' / 2`、`task_health` 上一次由 n1 epoch 1 写成 HEALTHY：持有 `_health` 的节点一轮 sweep 后打开 CRITICAL `TASK_FAILED`、健康 `UNHEALTHY`；不持有 `_health` 的节点不评估；`_health` 被别的节点持有时本节点不代评；代评不读本节点的旧信号、不改链 kind。
- `healthstore/storetest`：已释放 lease 的同 epoch 代写在 `fence_node` 不同时放行；仅过期（owner 仍在）时仍按节点拒绝。
- `tasks/p26c_failed_preserved_test.go`：共享行被别的 epoch 写成 FAILED、lease 已释放在 epoch 2，本节点 epoch 1 的 run 仍在：`applyRemoteStops` 之后 run 退出，任务行仍是 `FAILED`、`last_error` 不变；内存为 FAILED。`markLeaseDegradedLocked` 与 `finishStoredStopLocked` 遇到 FAILED 都不写（§24.4 第 4 条）。
- `meta/fenced_exec_test.go`、`tasks.CheckTaskRowFence` 表驱动：已释放于更大 epoch 拒绝、释放于同 epoch 放行、无 lease 行时按任务行判断。
- `tasks/resume_test.go` / `replication/takeover_segment_test.go`（§24.8）：catalog `file_path` 可读但在本 worker `data_dir` 外 → `ResolveTakeover.Missing` / Start 永久 `SEGMENT_NOT_ON_WORKER`，不往外来目录写、不开 dump；路径在本 `data_dir` 下则照常续写；UPLOADED 物化仍进本 `data_dir`。
- 真机（#421 复现脚本改写）：非共享 `data_dir` 下 (a) CRITICAL `TASK_FAILED` 打开、`/api/health` 非 ok，(b) 旧 owner 醒来后任务行仍是 FAILED，(c) 共享目录下冻结接管照常续写；(d) 同机可读外来路径（sc1）同样 FAILED + finding。证据在 `/workspace/binlog-server-verify-evidence-421/` / `…-424/`。

### 24.8 可读但不在本节点 data_dir ≡ 不在本 worker（#421 sc1，2026-10-17）

**问题**：两节点各自独立的 `data_dir`、却落在**同一主机**（或把旧 owner 的绝对路径挂成可读）时，`binlog_files.file_path` 指向的 open 分段对 new owner **仍然可读**。旧规则把「可读的 catalog 目录」当成接管续写目录（`ResolveTakeover` → `TakeoverResume.Dir`），于是 n2 认领后 `RUNNING`、往**别人的**目录续写或（更糟）在错误假设下几乎不写、0 finding、`/api/health` = ok，只有 `task_health=UNKNOWN`——比 ENOENT / chmod 000 的大声失败更危险。

**规则**：接管 / run start 时，catalog 的 `file_path`（或其目录）必须落在**本节点配置的 `data_dir`** 之下（两侧先 `filepath.Abs`——相对的 `data_dir` 相对进程工作目录——再在存在时 `EvalSymlinks`，解析后再比；比较用 `filepath.Rel`，禁止 `..` 逃逸。相对 `data_dir` 与 catalog 里的绝对路径（或反过来）指同一目录时算本节点自己的分段，#424 QA 前补）。可读但落在本 `data_dir` **之外** → 与不可读 / ENOENT 相同：永久 `SEGMENT_NOT_ON_WORKER`（点名该分段），**不得**因可读就继续写入外来绝对路径，也不得在本机另起一套静默目录假装续写。

**共享 `data_dir` 不受影响**：部署前提（§24.5）下 catalog 路径本就在共享根下，继续今日行为（含 #415 换 inode 续写）。

**UPLOADED**：本地文件缺席或路径在外来目录时，仍走对象物化到本节点 `data_dir`（`Covering`）；不得把 `Dir` 设成外来路径。仅当已上传对象对应的本地文件就在本 `data_dir` 下时，才直接 `Apply`+`Dir`。

**风险**：同一块盘经不同 bind-mount 路径挂到两台（`/data` vs `/mnt/n1`）且未共享同一 `data_dir` 配置字符串时，仍会判「外来」并 FAILED——这符合「集群必须共享 data_dir 路径」的前提；真要跨路径等同，需另做 marker / 相对路径方案（§24.5 A/B）。

### 24.9 兜底：拿着任务却没有进展（`TASK_NO_PROGRESS`，#421 sc1 QA，2026-10-17）

**为什么还要兜底**：§24.8 堵住了 sc1 这一条路，但 sc1 暴露的是一类问题：节点拿着 lease、任务行 `RUNNING`，实际没有往本节点 `data_dir` 写任何东西，而现有 kind 全都不响——`DUMP_STALLED` / `SOURCE_UNREACHABLE` 只看源端有没有字节（sc1 里源端一直有字节），`TASK_TRANSITION_STALLED` 只看 STARTING/STOPPING 且是 WARNING，链 scope 读不到目录只会让健康变成 `UNKNOWN`，而 `UNKNOWN` 不让 `/api/health` 降级。以后任何一种「拿着任务但没干活」的 bug 都会同样静默。兜底按结果判断，不按原因判断。

**事实**（`health.TaskFacts.Progress`，只在本进程开着这个任务的 run 时有）：
- `RunSince`：本进程这次 run 登记的时间（`startTask` 登记 `s.runs[id]`）。同一次 run 内的重试不重置它，所以 RUNNING↔RETRY_BACKOFF 来回跳不会一直推迟告警。
- `LastAdvance`：最近一次「有进展」的时间。进展 = runner 上报的 (file, pos) 前进了，或者确认已在源端 tip（idle 探测确认 at-tip，或收到心跳事件：MySQL / MariaDB 只在 dump 线程已发完 binlog、在末尾空等时才发心跳）。空闲的源库因此不会误报。
- `File` / `InDataDir`：runner 当前分段的源文件名，以及本节点 `{data_dir}/{task_id}` 下有没有这个文件的分段。不在本节点 `data_dir` 下的进展**不算进展**（sc1 式往别处写）。`data_dir` 为空（只在测试里）时不做这项检查。

**规则**（`TASK_NO_PROGRESS`，CRITICAL，scope `task.state`，subject 空，每个任务最多一条）：
- 前提：desired `RUN`；状态不是 `FAILED`（`TASK_FAILED` 负责）；不是别的节点拥有（`OwnedElsewhere`，由 owner 评估）；`SOURCE_UNREACHABLE` 没有在说同一件事。
- 本进程开着 run：`since = max(RunSince, LastAdvance（仅当 InDataDir）)`。
- 本进程没开 run（无主任务由 `_health` 持有者代评、或 owner 是本节点但 run 没起来）：`since = StateSince`（任务行 `updated_at`）。这覆盖「desired RUN 但没有任何节点在跑」：dispatched 的 STARTING 一直没人认领、lease 只是过期后 fail-safe 写成的 `STOPPED / desired RUN` 一直没人重启。
- 本进程是「没有 runner」的节点（`RunHere` 恒真、没有进度账本）：不判。
- `now - since ≥ health.no_progress_after_sec`（默认 **120s**，0 = 默认，负数拒绝启动）即打开；detail 写明是哪种情况、多久、阈值，以及当前分段是否在本节点 `data_dir` 外。
- 清除：同一规则不再成立（有了进展、任务停了 / FAILED / 被别人拥有）。

**为什么 120s**：心跳周期 15s，idle 探测 2s，正常 run 从登记到第一次上报只要几秒；120s 比 `DUMP_STALLED`（45s）和 `SOURCE_UNREACHABLE`（90s）都晚，源端问题先由那两个 kind 报，兜底只抓它们看不见的。QA 的 sc1 在 70s 时还静默，加上默认 sweep 60s 和 5s liveness tick，sc1 类问题最迟约 125s 打开。

**评估时机**：除 sweep 外，`markLivenessCrossings`（默认每 5s）把「越过阈值」也当作一次相位变化，标脏立即评估，不等下一次 sweep。

### 24.10 失败路径矩阵（每个 PR 的 `go test` 都跑）

Adrian 指出 #421 这类 bug 测试本该拦住。缺口是：每条失败路径只在各自的单测里验证了「状态对」，没有统一断言「有告警、`/api/health` 降级、状态不被改写」。`internal/tasks/p26d_failure_matrix_test.go` 把会让任务停在 FAILED / STOPPED / 卡住的路径列成一张表，集群和单机各跑一遍，每一行都断言三件事：
1. 打开了预期的 CRITICAL finding（kind + 严重级别）；
2. `Scheduler.HealthSummary` 是 `degraded`，reasons 里有这条 finding；
3. 再跑一轮（旧 owner 醒来的 `applyRemoteStops`、control loop、再 sweep 一次）后任务行的状态和 `last_error` 没被改写。

运维主动 Stop 是唯一预期「没有 finding、health ok」的行，作为对照放在表里。表里每一行写明对应的代码路径；新增一条会让任务停下的路径时必须在表里加一行（表头注释写明这条规则）。

| 行 | 模式 | 怎么造 | 预期 |
|---|---|---|---|
| permanent-error | 单机 / 集群 | runner 返回永久错误 → `markFailedLocked` | `TASK_FAILED`；集群由 `_health` 持有者代评 |
| segment-not-on-worker | 集群 | runner 返回 §24.8 的 `SEGMENT_NOT_ON_WORKER`（真机路径见 replication 的 sc1 回归） | `TASK_FAILED`，旧 owner 醒来后仍 FAILED |
| unclassified-error | 单机 / 集群 | runner 返回不在重试白名单里的错误（如 `sealed file already exists`）→ 一次即 FAILED | `TASK_FAILED` |
| running-no-progress | 单机 / 集群 | run 开着、RUNNING，但从不上报进展 | `TASK_NO_PROGRESS` |
| progress-outside-data-dir | 单机 / 集群 | run 上报进展，但分段不在本节点 `data_dir` 下（sc1 形状） | `TASK_NO_PROGRESS` |
| unclaimed-starting | 集群 | desired RUN、STARTING、无 owner，没人认领 | `TASK_NO_PROGRESS`（代评） |
| lease-expired-stopped | 集群 | lease 释放后 fail-safe 写成 `STOPPED / desired RUN`，没人重启 | `TASK_NO_PROGRESS`（代评） |
| operator-stop（对照） | 单机 / 集群 | 运维 Stop | 无 finding，health ok，行是 `STOPPED / desired STOP` |
| relative-data-dir-own-segments（对照） | 单机 / 集群 | `data_dir` 配成相对路径 `data`，接管时 catalog 记录的是本节点分段的绝对路径，run 先走 `ResolveTakeover` | 不报 `SEGMENT_NOT_ON_WORKER`，无 CRITICAL，health ok，行保持 `RUNNING` |
| dead-worker-control-plane-only | 集群 | 唯一 worker 被 kill -9，只剩 control-plane 节点（无 worker_id）；任务行停在 `RUNNING / 死 owner / epoch`，lease 过期（§24.13） | `TASK_NO_PROGRESS`（control-plane 代评，详情“no worker is running … (lease expired)”） |
| meta-read-only | 集群 | meta 只读期间：所有节点 health degraded（`FINDINGS_PERSIST_FAILED`），任务不被静默写成 `STOPPED / desired STOP`；恢复后任务重新 `RUNNING`、health 回到 ok（§24.14，#425） | 恢复后无 open CRITICAL，行 `RUNNING` |
| retrying-owner-no-run | 单机 / 集群 | owner 持有 lease 但每次启动都在 leftover dump KILL 处失败、进 RETRY_BACKOFF，从不开 run（§24.15） | `TASK_NO_PROGRESS`（在 claim + no_progress_after_sec + 一个 sweep 内） |

源端连不上时任务停在 `RETRY_BACKOFF`，由 `SOURCE_UNREACHABLE`（CRITICAL）报；它和 `DUMP_STALLED`、链类 kind 已有各自的回归，不重复进表。

### 24.11 反向栅栏顺序（集成测试）

QA 只在真机上跑了「先 FAILED、旧 owner 后醒」。反向顺序——旧 owner 先醒、它的 renew / 远端 stop 收尾先于新 owner 写 FAILED——此前只有单测。`internal/tasks/p26d_fence_order_test.go` 用两个真实的 `Scheduler`（共享任务表、`MemoryLease`、按 lease 检查栅栏的健康存储），真实的 renew 循环和 claim 路径：
1. worker-b 以 epoch 1 运行；冻结它的 renew（lease 管理器上的闸门），等 lease 过期；
2. worker-a 走 `ClaimExpiredTasks` 以 epoch 2 认领，它的 runner 停在「还没失败」；
3. 解冻 worker-b，分两种顺序：(i) renew 先发现丢失；(ii) 先跑 `applyRemoteStops`。断言 worker-b 的 run 退出、任务行仍是 worker-a / epoch 2、没有 `TASK_STOPPED`、worker-b 没写任何行；
4. worker-a 的 runner 返回 `SEGMENT_NOT_ON_WORKER` → FAILED、释放 lease；
5. worker-b 再跑一次 `applyRemoteStops` 和 control loop，两个节点各 sweep 一次：任务行仍 FAILED 且 `last_error` 不变，CRITICAL `TASK_FAILED` 打开，`HealthSummary` degraded。

### 24.12 集群 e2e 进每个 PR（交接）

`cluster-roles`、`worker-crash-recovery`、`meta-failover` 三个集群 e2e 只在手动触发（meta-failover 还有 push main）时跑。改成每个 PR 都跑需要改 `.github/workflows/e2e.yml`，本仓库自动化用的 token 没有 workflow 权限，改动写在交接文档里由有权限的人执行。上面的矩阵和栅栏顺序测试是纯 `go test`，不依赖这一步，本 PR 起每个 PR 都跑。

### 24.13 只剩控制面节点时 owner 死掉（QA r3 (b)，2026-10-17）

**现象**：唯一的 worker n1 被 `kill -9`，只剩 control-plane 节点 n2。任务行停在 `RUNNING / n1 / 1` 超过 100s，0 finding，n2 `/api/health` = ok；n2 的评估器启动日志是 `node )`（空 node id）。

**根因**：
1. control-plane 角色不解析 worker id（`resolvedWorkerID` 只在 worker 角色里赋值），评估器 `NodeID` 为空，`refreshHealthLease` 在 `NodeID == ""` 时直接放弃，控制面节点永远不持有 `_health`，也就不代评任何任务。
2. 即使持有 `_health`，「无主」只认任务行（`owner == "" || epoch <= 0`）。owner 死掉后行仍写着 `n1 / 1`，lease 已过期但没有活着的 worker 去认领，于是它既不是「无主」，也没有节点按持有者评估它。

**规则**：
- 评估器在每种角色上都运行，并有稳定的 node id：worker 用 worker id；control-plane 用 `cluster.worker_id`（若配置），否则用 data_dir 里持久化的 node id（与单机同一个 `LoadOrCreateNodeID`）。control-plane 节点从不按持有者评估任务（`EvaluatorConfig.ControlPlaneOnly`：即使 id 恰好等于某个 worker 的 owner 也不行），只通过 `_health` 代评。
- `_health` 持有者每轮用 store 的 `ListTaskIDsWithExpiredOwnerLease`（没有时退回 `ListTasksWithExpiredLease`；按 meta 的 `NOW(6)` 判过期，不用本地时钟）找出 lease 已过期的任务，把它们与「行无主」的任务同样代评 `task.state`。代写栅栏不变：`(last owner, last epoch, Proxy)`，store 端已经允许「epoch 相同且 lease 过期」的代写；新 owner 一认领，epoch 前进，代写即被栅栏拒绝。
- 这种任务的 `TASK_NO_PROGRESS` 计时起点是 max(任务行 `updated_at`, 本节点第一次看到它 lease 过期的时间)，不是行的 `updated_at`：一个跑了几天的任务 lease 刚过期，不应立刻 CRITICAL。界：lease TTL + 1 个 sweep（发现）+ `no_progress_after_sec` + 1 个 sweep。QA 配置（TTL 15s、sweep 5s、NP 30s）下约 55s，小于 2×NP。detail 写明 `lease of owner n1 expired; no worker is running the task`。
- 关闭：有 worker 认领（epoch 前进、行有新 owner），或 desired STOP / FAILED。
- 候选集合来自 store，不只来自内存：control-plane 节点没有认领循环，在 worker 上创建、运行的任务从不进它的内存（QA r3 (b) 真机复现：修了身份之后 n2 已持有 `_health`，`responsible()` 只看内存名单，仍是 0 个任务）。`_health` 持有者每轮再读一次 store 全量任务表，把内存里没有、且「行无主」或「lease 已过期」的任务加入候选。只有持有者付这一次全表读；store 读失败时什么都不加。查过期 lease 用专门的 `ListTaskIDsWithExpiredOwnerLease`（只返回 id；owner 非空、epoch > 0、desired RUN、非 FAILED/STOPPED/CREATED）。

### 24.14 meta 只读之后静默 STOPPED（QA r3 (d)，#425，2026-10-17）

**现象**：meta `read_only=ON` 100s（非 SUPER 用户）。期间只有 n1 `/api/health` degraded（`FINDINGS_PERSIST_FAILED`），n2 一直 ok；`read_only=OFF` 后任务变成 `STOPPED / 无 owner / desired STOP`，0 finding，不再恢复。

**根因**（本地复现 + meta general log 逐条核对）：
1. 只读期间 n1 的行写失败（`STARTING n1/1` 等），任务进 `unpublished`，sink tick 每 5s 重写一次内存行（`republishUnpublished`）。
2. n1 的本地 lease 时钟在 0.8×TTL 后拒绝写，runner 以 lease handoff 退出，内存行改成 `STOPPED / "" / 0`（「让出」，按设计不落库）。但 `unpublished` 没清，republish 继续重写的是这份让出后的内存行。
3. 无 owner、epoch 0 的行走 `persistTaskLocked`，不过栅栏（`persistTaskFencedLocked` 对 `owner == "" || epoch <= 0` 直接退化为不带栅栏的 `UpsertTask`），spec_revision 相同，upsert 接受。
4. `read_only=OFF` 后 n2 在 11:04:33 认领 epoch 2 写 `RUNNING n2/2`；n1 的 republish 在 11:04:35 把 `STOPPED / "" / 0 / desired RUN`（updated_at 还是只读期间的 11:04:03）写回。
5. n2 的远端 Stop 轮询把「共享行 STOPPED」当成控制面 Stop：取消自己的 dump，写 `STOPPED / desired STOP` 并释放 lease。desired 被改成 STOP，所以 `TASK_NO_PROGRESS` 也不会开。

和 #424 是同一套栅栏/状态代码（旧 owner 在失去 lease 后改写共享行），在 #424 里修。

**规则**：
- 让出（lease handoff）后本进程对这行没有任何要发布的东西：清掉 `unpublished`（与 `ErrStaleTaskWrite` 的处理一致），关闭 `TASK_PERSIST_FAILED`。
- republish 带栅栏：内存行有 owner/epoch 时走 `persistTaskFencedLocked(owner, epoch)`；新 owner 已认领时 store 拒绝（`ErrStaleTaskWrite`），本进程丢弃这份待发布。
- 新 owner 的防线：共享行 `STOPPED` 但 desired 仍是 RUN、spec_revision 没有比本次 run 打开的更新、且行上的 owner 不是本 run，这不是 Stop（控制面 Stop 总是写 desired STOP，或先 STOPPING），而是旧 owner 的陈旧写。本 run 继续，并把自己的行重新发布（带栅栏）。
- 所有节点的 health 都反映 meta 写失败：每个评估器每轮都写 `_health` lease（持有者续约、其他节点尝试获取）。这个写返回 store 错误时，记入 `FINDINGS_PERSIST_FAILED`（detail：metadata store refused the `_health` lease write），所以只读期间每个节点 `/api/health` 都 degraded；写恢复后关闭。
- 本节点不再评估的任务（不是 owner、也不代评），其「健康写失败」记录在下一次 health 汇总时丢弃，否则 meta 恢复后 n1 永远挂着过期的 `FINDINGS_PERSIST_FAILED`。

### 24.15 重试中的 owner 与 SOURCE_UNREACHABLE 的余量（QA r3 (c)，2026-10-17）

QA (c)：新 owner 认领后一直 `RETRY_BACKOFF`（旧 dump 连接 KILL 不到源，run 根本没打开），`SOURCE_UNREACHABLE` 在 fault+60s 才开，正好压在 2×NP 上；期间 `TASK_NO_PROGRESS` 没开。

**根因**：进度账只在 run 打开时存在（run 退出即删）；没有 run 时退回到行的 `updated_at`，而重试循环每隔几秒把 `RETRY_BACKOFF ↔ STARTING` 写回行，`updated_at` 一直被刷新，计时永远到不了阈值。

**规则**：进度账按「本节点以 epoch E 持有任务」记，不按单个 run 记：本节点拿到（或认领）任务、确定 owner/epoch 时开账（同一 epoch 已有账则沿用起点），run 退出、重试、KILL 失败都不清；换 epoch、操作员 Start、删任务时重开或删除。本节点是 owner 且有账时，`TASK_NO_PROGRESS` 一律按账计（max(持有起点, 最后一次在 data_dir 下推进)），不看行的 `updated_at`。界：认领 + `no_progress_after_sec` + 1 个 sweep，QA 配置下约 fault+15+30+5 = 50s，SOURCE_UNREACHABLE 之前先开。

### 24.16 测试（先红）

- 矩阵行 `dead-worker-control-plane-only`（集群）：worker-a 以 epoch 1 跑任务后死掉（run 取消、lease 不再续、行仍 `RUNNING a/1`），剩下的节点是 control-plane（无 runner、无 worker id，node id 来自配置/持久化）。lease 过期后在阈值内打开 CRITICAL `TASK_NO_PROGRESS`，每个节点 `/api/health` degraded，行不被改写。
- 矩阵行 `meta-read-only`（集群）：a 跑任务；meta 只读（任务行写、lease 写都报错）期间 a、b 两个节点 `/api/health` 都 degraded；a 的行写失败后 lease handoff 让出；lease 过期、只读解除，b 认领 epoch 2；a 的 sink tick republish、b 的远端 Stop 轮询之后，行仍是 `RUNNING b/2 desired RUN`，不出现静默 STOPPED，恢复后两个节点 health ok。
- 矩阵行 `retrying-owner-no-run`（集群/单机）：owner 的启动在 KILL 旧连接失败后反复 `RETRY_BACKOFF`（run 从未打开），`TASK_NO_PROGRESS` 在阈值内打开。
- `app` 单测：control-plane 角色的评估器 node id 非空（`cluster.worker_id` 或持久化 id），并且 `ControlPlaneOnly`。

### 24.7 本步不做

- `HEALTH_STALE`（§8 第 10 条）：需要持有者给**有主**任务写 finding，而有主任务的写权在 owner，代写栅栏要求 lease 无人持有，两者冲突；改为 `_cluster` 级 finding（subject = 任务 id，`never` 栅栏，持有者写）。落地是 P2-6.1（§28）。
- 链 scope 的代评与非共享存储的委派、`OWNER_CHANGED`、节点消失清理留到 P2-6.2（§28.7）。混版本自动退回 `shadow` 留到 P2-6.3。

## 25. #427：meta 被黑洞时 `/api/health` 有界返回，并留下 `META_UNREACHABLE`（2026-10-19）

本节把 P2-7 中「meta 宕机缓存、真实 2s」里 `/api/health` 的部分提前做，`/metrics` 的独立连接池与缓存序列仍留在 P2-7。

### 25.1 问题（#424 r4 (h) 复现）

两节点集群，`meta_dsn` 指向一个 TCP 代理；对代理 `kill -STOP` 100s（连接挂住，而不是被拒绝）。

1. 整整 100s，两个节点的 `/api/health` 在 3s 内一次都没有返回。
2. 写入停止（128161 → 128824 字节），但恢复后 `finding_transitions` 为空：这次中断在 meta 里没有任何记录，事后无法审计。
3. n1 日志只有 info 级别的 `health evaluate task=1: context deadline exceeded`。

对照（r4 (g)）：代理被 kill、连接被拒绝时，30s 内两个节点都 degraded（`FINDINGS_PERSIST_FAILED; findings store unreadable: ... connection refused`），恢复后补记了 METADATA_WRITE_BLOCKED / EVENT_PERSIST_FAILED / TASK_NO_PROGRESS 的开关。

### 25.2 根因（假设，先用红测证实）

- `Scheduler.HealthSummary` 用请求自己的 ctx（没有 deadline）调 `OpenFindings`：一次 `ListOpenFindings`，加上每个任务一次 `ListOpenFindingsForTask`，都走共享的 32 连接池。DSN 没有 `readTimeout`，连接挂住时查询一直等到 TCP 恢复，handler 也就一直不返回。连接被拒绝时 driver 立刻报错，所以 (g) 正常。
- `openManagedFindings` 读失败时返回 nil：meta 读不到时，评估器管理的 CRITICAL finding 从汇总里**静默消失**，只剩 store 错误这一条把状态拉成 degraded。
- 评估器每轮都有 deadline，超时后只打日志，不产生信号，所以没有任何 kind 会因「meta 读不到」打开。恢复后事实看起来一切正常，于是没有转移。

### 25.3 规则

**D1 `/api/health` 的读有上限。**
- `HealthSummary` 的全部 meta 读共用一个预算 `healthReadBudget = 1s`（从进入 handler 算起，不按每次查询算）。第一次超时之后，剩下的读立即失败，不会叠加成 N × 1s。
- 读在单独的 goroutine 里跑，handler 在预算到点时直接返回，不依赖 driver 是否遵守 ctx（#339 re-QA 测到过 5.0s）。同一时刻最多一个读在路上（single-flight）：并发的探针共用这一次读的结果，不在挂住的连接上堆积 goroutine 和连接。
- 预算内没读完：返回 503 `degraded`，reason 含 `META_UNREACHABLE: findings store read did not answer within 1s`。内存里的 finding（未持久化的、评估器 `unsavedOpen` 的、`persistFailureFinding`）照常列出。
- `openManagedFindings` 读失败时不再返回空：它把错误交回 `OpenFindings`，汇总里写明「managed findings of task X unreadable」，而不是假装这个任务没有 open finding。

**D2 `META_UNREACHABLE`：本节点观察到的 meta 不可达。**
- 新的信号 kind（S17），CRITICAL，task = `_cluster`，subject = 本节点 node id（集群里每个节点各报各的，互不覆盖）。Managed，唯一写者是评估器（lintguard 的 Managed 表一并更新）。
- 观测来源（都走同一个 `metaReach` 记录器，本地时钟）：
  1. `/api/health` 的有界读：超时或连接错误记一次失败，读成功记一次成功；
  2. 每个节点独立的 ping（§25.8 R1：每 1s 一次，不经过评估循环；r1 之前挂在 `LivenessTick` 上，默认 5s），上限 1s；
  3. 评估器一轮里 meta 读返回超时或连接类错误（`context.DeadlineExceeded`、`driver.ErrBadConn`、`net.Error`、MySQL 2002/2003/2006/2013）。SQL 语义错误（权限、只读 1290 等）不算不可达，那些有各自的 kind。
- 第一次失败记下 `since`。连续失败满 `meta_unreachable_after`（默认 10s，避免一次慢查询就打开）后 `SignalFailure`，detail 带 `since` 与最近一次错误（`timed out after 1s` 或 `connection refused`）。下一次成功 `SignalSuccess`。
- 打开期间 meta 写不进去：finding 留在评估器内存里（`unsavedOpen`），`/api/health` 与 `/api/findings` 从内存列出。恢复后走现有的补记路径（与 r4 (g) 的 METADATA_WRITE_BLOCKED 相同），写入 OPEN 与 CLOSE 两条转移，`FactAt` 分别是本地观察到的开始与恢复时间。
- 连接被拒绝与连接挂住走同一套逻辑；被拒绝时 `FINDINGS_PERSIST_FAILED` 照旧，两条同时存在。

### 25.4 本步不做

- `/metrics` 的独立连接池、`readTimeout=2s` 的 DSN、缓存任务列表输出 `task_healthy=0`（P2-7）。
- 写路径（checkpoint、任务行、lease 续约）的超时不变；黑洞期间 dump 写不进去，仍由 `TASK_NO_PROGRESS` 在恢复后补记。

### 25.5 测试（先红）

- `tasks` 单测：store 的 `ListOpenFindings` 一直阻塞到 ctx 结束 → `HealthSummary` 在 1s + 200ms 内返回 degraded，reason 含 `META_UNREACHABLE`；store 忽略 ctx、永远阻塞 → 仍在上限内返回；10 个并发调用只触发一次 store 读。
- `tasks` 单测：`ListOpenFindingsForTask` 返回错误 → 汇总写明不可读，不报 ok。
- 评估器单测（假时钟）：Ping 连续失败 9s 不开、满 10s 打开 `META_UNREACHABLE/<node>`；恢复后一轮内关闭；store 在故障期间拒绝写、恢复后接受 → 有 OPEN 与 CLOSE 两条转移，`FactAt` 等于故障开始与恢复时间。
- 错误分类单测：1290（只读）不算不可达，2003、`context.DeadlineExceeded` 算。
- `api` 单测：handler 在 store 挂住时 2s 内返回 503。

### 25.6 QA

- 重跑 r4 (h)：每 10s `curl -m 3 /api/health`，从 fault+10s 起每次都在 2s 内返回 degraded，reason 含 `META_UNREACHABLE`；恢复后每个节点各有一对 `META_UNREACHABLE` OPEN/CLOSE，时间与故障窗口相差不超过一个 LivenessTick + `meta_unreachable_after`。
- 重跑 r4 (g)（连接被拒绝）：结果与 r4 一致，另多一对 `META_UNREACHABLE`。
- 重跑 sc5（meta 暂停 12s、30s）：12s 的暂停打开并关闭一次 `META_UNREACHABLE`（这是真实的不可达，不是误报）；恢复数据一致。
- 健康时 0 条 `META_UNREACHABLE`；1 小时空闲源无误报。

### 25.7 实现说明（2026-10-19）

- 观测汇总在 `internal/tasks/meta_reach.go`（`metaReach`）。只有任务存储实现 `tasks.MetaProbe`（`internal/meta/reach.go`：`PingMeta`、`MetaUnreachable`）时才记录，没有 meta 的 standalone 永远不会出现这个 kind。错误分类放在 meta 包里（它认识 driver 的错误类型），1290/1045/1146 等服务端错误不算不可达。
- 评估一轮失败只记失败，不记成功；成功来自 ping 与 `/api/health` 读。
- **恢复时的 OPEN/CLOSE**：故障期间评估器读不到 store，OPEN 存不下；如果恢复时立刻 `SignalSuccess`，下一轮看到「失败之后又成功」就什么也不写，中断就丢了。所以评估器拥有该信号时（standalone），恢复只记 `recovering`，信号保持失败；`_cluster` 那一轮把 OPEN 存下后，`settleMetaReach` 才发成功，再下一轮 CLOSE。OPEN 的 `FactAt` 是第一次失败（`signalAt`），CLOSE 的 `FactAt` 是恢复时第一次成功（§25.8 R3；r1 之前是关闭的那一轮，比恢复晚一个 debounce）。
- **集群**：`_cluster` 在 P2-6 之前不归评估器，`META_UNREACHABLE` 走 legacy 账本：故障期间在内存里（`/api/health`、`/api/findings` 照常列出），恢复后 report 与 resolve 合并成一次写入，`task_findings` 里留下一行 `first_seen` = 第一次失败、`resolved_at` = 恢复的已关闭行，没有 `finding_transitions`。P2-6 让 `_health` 持有者评估 `_cluster` 后改为 OPEN/CLOSE 两条转移。§25.6 第一条 QA 在集群里按这一行验收。
- `clusterHasWork` 原来用无超时的 ctx 读 `_cluster` 的 open finding；它在 sweep 循环里跑，挂住会让 ping 也停下。现在限时 2s。
- 故障期间（standalone，评估器拥有该信号）OPEN 存不下，`metaReachFinding` 从 `metaReach` 状态合成这条 finding，`/api/health` 的内存汇总与 `OpenFindings` 都列出它。`/api/findings` 本身的 store 读仍无上限（黑洞时会挂住），归 P2-7 与 `/metrics` 一起处理。
- 本步不加配置项：`meta_unreachable_after` 固定为 10s（`EvaluatorConfig.MetaUnreachableAfter` 供测试使用）。

### 25.8 QA r1 修订（#430 sc5，2026-10-19）

QA 在 01d6c6fa 上跑 sc5（两节点集群，meta 进程 `kill -STOP` 12s，间隔 25s 再停 30s）：数据一致，但 `META_UNREACHABLE` 的记录不符合 §25.6。

**问题与根因（日志证实）**

1. **P2：30s 暂停只有 n2 留下一行，n1 什么都没记。**
   - 评估器 `Run` 循环的 sweep 分支同步调用 `markResponsible`，它先跑 `refreshHealthLease`（`_health` lease 的 `Renew`/`Acquire`）和 `refreshExpiredLeases`（`ListTaskIDsWithExpiredOwnerLease`），用的都是 `context.Background()`，没有 deadline。meta 被 SIGSTOP 时这些调用一直挂到恢复，循环停在 sweep 分支里，`LivenessTick` 分支的 ping 一次都没发出去（`time.Ticker` 丢 tick）。
   - 于是剩下的唯一观测来源是「评估一轮超时」，而只有对某个任务负责的节点（任务 owner 或 `_health` 持有者）才有这样的评估。sc5 第一次暂停后 n2 接管了任务（epoch 2）和 `_health`（epoch 2），第二次暂停期间 n1 两者都不是，没有任何观测，所以没有行。
   - 佐证：n2 那一行 `first_seen=09:07:35.979`，正好是它第一条 `health evaluate task=1: context deadline exceeded` 的时间，而不是暂停开始的 09:07:25，说明 ping 没在跑。r4 (h) 两个节点都有行，是因为 QA 每 10s curl 一次 `/api/health`，那是唯一的观测；集群 `first_seen` 比故障晚约 10s 也是同一个原因。
2. **P3：12s 暂停两边都没记。** 原来的规则要求「失败观测之间」跨度达到 10s 才打开。ping 每 5s 一次、超时 1s，12s 的暂停里两次失败观测的跨度经常不到 10s（这次 ping 干脆没跑）。恢复时又直接清零，这段中断就丢了。
3. **P3：时间。** standalone 的行由健康存储写，`first_seen`/`last_seen`/`resolved_at`/`evaluated_at` 都是 SQL 里的 `NOW(6)`：取会话时区（QA 机器 +08:00），而集群 legacy 行是 Go 传进来的 UTC；`first_seen` 还是写入时刻（恢复以后），不是第一次失败。CLOSE 的 `fact_at` 是关闭那一轮的时间，比恢复晚一个 debounce。

**规则修订**

- **R1 ping 不经过评估循环。** `META_UNREACHABLE` 的 ping 由独立的 goroutine 按 `metaPingInterval`（1s）发出，单次上限 `metaPingTimeout`（1s），同一时刻最多一个 ping 在路上；上一个还没回来时，这个 tick 直接记一次失败。评估循环里任何 meta 调用卡住都不影响它。`markResponsible` 里的 lease 续约/抢占和过期 lease 列表也改为限时 2s，超时或连接错误同样计入（只记失败）。这样**每个节点**，不论是不是任务 owner、是不是 `_health` 持有者、是不是纯控制面，都有自己的观测。
- **R2 窗口从第一次失败算起，恢复时补记。**
  - `since` 取第一次失败的观测**发出**的时间（ping 或 `/api/health` 读开始的时刻），不是超时返回的时刻。
  - 连续失败期间，`now − since ≥ meta_unreachable_after` 就打开（与原来相同）。
  - 第一次成功时，如果还没打开，但 `成功时间 − since ≥ meta_unreachable_after`，就补记一次：先以 `since` 打开，再以成功时间关闭。
  - 结论：故障开始后 1 个 ping 间隔内有第一次失败，恢复后 1 个间隔内有第一次成功，所以任何持续 ≥ `meta_unreachable_after` + 1 个 ping 间隔（默认 11s）的中断一定留下一对记录；短于 `meta_unreachable_after`（10s）的一定不记。§25.6 第三条（12s 暂停打开并关闭一次）不变。
- **R3 时间一律 UTC，首见 = 第一次失败，解决 = 第一次成功，两种模式一致。**
  - 健康存储写 `task_findings`、`finding_transitions`、`task_health` 的时间戳改用 `UTC_TIMESTAMP(6)`（与 Go 驱动默认 `loc=UTC` 写入的值同一时区）。lease 表的 `NOW(6)` 不变：它只和 `NOW(6)` 比较，从不和 Go 时间比较。
  - OPEN 转移写 `first_seen` = 转移的 `FactAt`（signal 的第一次失败），而不是写入时刻；行已打开时保留原来的 `first_seen`。CLOSE 转移写 `resolved_at` = CLOSE 的 `FactAt`。
  - signal 的成功可以带事实时间（`SignalState.SuccessFactAt`）；CLOSE 的 `FactAt` 取它（不晚于本轮）。`META_UNREACHABLE` 在恢复时记下第一次成功的时间，评估器存下 OPEN 后发出的成功带上这个时间，所以 CLOSE 的 `fact_at` 与 `resolved_at` 都是恢复时刻。其他 kind 不带事实时间，行为不变。
  - 集群 legacy 行：`first_seen` = `since`，`resolved_at` = 第一次成功的观测时刻，`UpsertFinding` 转 UTC 写入（原来就是）。
  - 已经按会话时区写进去的旧行不迁移。

- **R4 集群里每次中断都留下记录（本地 sc5 复现时发现）。** R1–R3 之后在真实两节点集群上重跑 sc5，12s 和 30s 两次暂停都被观测到，但 `task_findings` 每个节点只剩一行：`first_seen` 是第一次暂停的开始，`resolved_at` 是第二次的结束，`count` 累加，看起来像一次 67s 的中断。原因：
  - legacy 的 `upsertFindingSQL` 对已存在的行一律 `first_seen = LEAST(...)`、`count = GREATEST(...)`，一行已经 resolved 之后再次打开，仍保留上一次的 `first_seen` 与次数。
  - 集群里（P2-6 之前）`_cluster` 没有 `finding_transitions`，legacy sink 又对 `_cluster` 不写 `task_events`，所以同一节点的上一次中断被下一次覆盖后，在 meta 里没有任何痕迹。

  修订：
  - legacy upsert：已 resolved 的行再次打开时，`first_seen` 与 `count` 取新值（与健康存储的 `findingUpsertSQL` 一致）；仍打开的行照旧保留更早的 `first_seen`。
  - legacy sink 对 `_cluster` 的 `META_UNREACHABLE` 也写 `task_events`（`task_id = _cluster`）：只写 `FINDING_RESOLVED`，message 为 `META_UNREACHABLE <node>`，`event_time` 是第一次应答，detail 写明这次中断的起止（UTC）。REPORTED 不写：故障期间它写不进去，恢复后才落地，还常常是两份（重试的一份加较新的一份），RESOLVED 本身已带着完整窗口。其他 `_cluster` kind 不变。
  - 每个节点在打开和关闭时各打一行日志（`META_UNREACHABLE opened ... since` / `resolved: outage <起> – <止> (<时长>)`）。
  - P2-6 让 `_health` 持有者评估 `_cluster` 之后，集群也改为 OPEN/CLOSE 转移，届时这一条事件可以去掉。

**测试（先红）**

- `internal/tasks/p26f_meta_outage_matrix_test.go`：失败路径矩阵新增 meta 中断一组，真实的 `Evaluator.Run` 循环与 finding sink，meta 调用（lease、健康存储、finding 存储、ping）在暂停期间挂住直到 ctx 结束，时间按比例缩小（`meta_unreachable_after` = 300ms，ping 间隔 20ms）。
  - 集群三个节点：任务 owner（worker-a）、`_health` 持有者（worker-b）、两者都不是（worker-c）。standalone 一个节点（带运行中的任务）。
  - `meta-pause-30s`（3×）：每个节点各一行 `META_UNREACHABLE/<node>`，`first_seen` 在暂停开始后 1 个 ping 间隔内，`resolved_at` 在恢复后很短时间内；standalone 有 OPEN/CLOSE，`fact_at` 同样分别对应暂停开始与恢复。
  - `meta-pause-12s`（1.2×）：同上，每个节点都有记录。
  - `meta-pause-5s`（0.5×，对照）：任何节点都没有记录。
  - `meta-pause-12s-then-30s`（sc5 原样：12s，间隔 25s，再 30s）：集群每个节点两条 `FINDING_RESOLVED META_UNREACHABLE <node>` 事件，时间分别是两次恢复；行的 `first_seen` 是第二次暂停的开始。standalone 依次是 OPEN、CLOSE、OPEN、CLOSE。
- `internal/meta`：真实 MySQL（`BINLOG_TEST_META_DSN`）上，已 resolved 的 legacy 行再次打开时 `first_seen` 与 `count` 取新值。
- `internal/meta`：sqlmock 断言健康存储的写入用 `UTC_TIMESTAMP(6)` 而不是 `NOW(6)`，OPEN 的 `first_seen` 和 CLOSE 的 `resolved_at` 用转移的 `FactAt`。
- `internal/health`：带 `SuccessFactAt` 的 signal 关闭时，CLOSE 的 `FactAt` 是这个时间；不带时仍是本轮时间。

**QA**：重跑 sc5（12s、30s）与 r4 (h)、(g)：每个节点（包括任务 owner 和非 `_health` 持有者）每次暂停各一对记录，`first_seen` 与故障开始相差不超过 1 个 ping 间隔，`resolved_at` 与恢复相差不超过 1 个 ping 间隔加一次写入；standalone 行的时间是 UTC，与集群行可直接比较。

## 26. P2-5.4：保留期删除标记、RETENTION_REMOVED、SEAL_FACTS_ABSENT（#341 / #353，2026-10-19）

§21.7 没有翻转 `ORPHAN_ROW`：配了上传时，本地保留删掉本地文件、桶里的对象和 `UPLOADED` 行都留着，行上没有「这次删除是保留期做的」标记，链检查会把它当成孤儿。§20 把 `SEAL_FACTS_ABSENT` 和这个标记留到本步。本步是 P2-5 的最后一块，不碰 `HEALTH_STALE`、完整 alerts API（P2-7）、整段桶过期仍删行（#209），也不把已经写成 NULL 的封存事实回填。

### 26.1 现状

- **#341**：`local_retention_days` 短于 `bucket_retention_days` 时，仍在桶窗口内且 checksum 为 match 的 `UPLOADED` 封存文件只删本地（`release` 的 `localOnly`）。对象和目录行留下，**不写** `RETENTION_REMOVED`。没配上传的路径（`dropGoneLocalOnlyRows`）会写这条事件，写失败才 `signalFail` `RETENTION_RECORD_FAILED`（S6）。所以配了上传时 S6 永远不出现，本地窗口为什么变短没有审计。数据没丢。
- **#353**：`writeSealFacts` / `ListSealedChecksums` 遇到 MySQL 1054 就把 `sealFactsAbsent` 记到进程结束，之后的封存 UPDATE 直接跳过，不打日志。在线 `migrate down` 再 `up` 回 000008 之后，同一进程仍把新封存行的 `first_event_at` / `last_event_at` / `sha256` 留成 NULL，直到重启。评估器关掉时这条路径也存在。
- **`ORPHAN_ROW` 仍不是 Managed**（§21.7，代码里 `p25` / `p253` / `p26d` 都不含它）。`I6_ORPHAN_ROW` 在「行在、本地不在、对象列举完整且 key 不在」时打开，不看保留期。桶窗口到期的整段删除会连行一起删，不会变成孤儿；误报来自留下的行。

### 26.2 删除标记（migration 000010）

只在「本地保留删掉本地文件、行和对象留下」时打标。整段过期仍删对象再删行，不留标记，也不写 `RETENTION_REMOVED`（行已经不在）。

- 新迁移 `000010_binlog_files_retention_marker`，加法、可空，下限仍是 6（与 000008 相同）：
  - up：`ALTER TABLE binlog_files ADD COLUMN retention_removed_at DATETIME(6) NULL;`
  - down：`DROP COLUMN retention_removed_at;`
- `listBinlogFiles` 的 SELECT 不改（sqlmock 钉着那条语句）。标记用旁路查询：`ListRetentionMarkers`（本任务 `retention_removed_at IS NOT NULL` 的 `source_file, epoch`）和单行是否已标记。
- 1054：进程内记住缺列，打**一条** WARN，之后每次 sweep / 下一次写入再探。缺列是 schema 6–9 的支持路径，**不**对每个文件开 `RETENTION_RECORD_FAILED`。幂等退化为进程内集合；重启后同一文件可能再写一条 `RETENTION_REMOVED`。事件是审计，finding 不因此常开。
  - 评审定案（§26.9 (b)）：没有 000010 时，每次进程重启后的第一轮回补会给本目录里每一条「本地已删、仍在桶窗口内、checksum match 的 `UPLOADED` 行」各再写一条 `RETENTION_REMOVED`，不只是刚删的那个文件。重复的是审计事件，不是 finding，也不改任何备份状态，所以接受；运维要去重时按 `(task_id, message)` 取最早一条，或者 `./migrate up` 到 10。
- 顺序：先写事件，再写标记。事件失败则标记保持空，下一轮重试，审计不会丢。事件成功、标记失败：下一轮会再写一条事件然后写上标记（重复一条，好过丢掉）。标记已有则跳过事件。
- 回补：之后的一轮里，本任务目录下 `file_path` 已不在、仍在桶窗口内、checksum 为 match 的 `UPLOADED` 行，标记为空，补事件和标记。覆盖「删完文件、记下之前崩溃」以及旧二进制已经删掉的文件。升级后第一轮可能集中写一批事件，和 #209「旧行下一轮清掉」同一类。`file_path` 不在本目录的行（别的 worker、只有对象）不标。

### 26.3 事件与 RETENTION_RECORD_FAILED

事件与今天没配上传的那条相同：type `RETENTION_REMOVED`，message `<name> removed by local retention`，detail 是 upload state，日志 `retention: removed ...`。成功后 `signalOK` 同一 subject（含原来的 `announceRetentionRemoved` 成功路径，它今天只在失败时 `signalFail`）。

失败（不是 `tasks.ErrWriteFenced`）→ `signalFail` `RETENTION_RECORD_FAILED`，subject 是文件名，并且这一轮**不算**干净的保留轮。`ErrWriteFenced` 是租约移交，不走这个信号；文件可能已经删掉，下一个 owner 按 26.2 回补。

§5.5 的关闭不依赖「这一轮刚好又删了同一个文件」：runner 只在 `cleanupTaskBinlogs` 返回 nil **且** 本轮没有记过保留记录失败时，写下 `RetentionCleanAt(task)`。评估器对已打开的 `RETENTION_RECORD_FAILED`：这个时间非零，且（本进程没见过这次失败，或该时间晚于信号的 `FactAt`）时把 `Probes[key]` 置真。不和 `LastSeen` 比（每轮评估都会把它往后推，永远关不上）。本轮 `signalFail` 之后不更新这个时间，所以旧的一轮关不掉新的失败。

### 26.4 I6 与 ORPHAN_ROW

`assurance.Row` 增加 `RetentionRemoved`。链缓存键带上这一位。

| 行 | 对象列举 | 结果 |
|---|---|---|
| 有标记，对象还在 | 前缀完整 | 不报 `I6_ORPHAN_ROW` |
| 有标记，对象不在 | 前缀完整 | `I6_MISSING_OBJECT`（subject `段#object`），**不**报孤儿。留下的那份副本丢了，这不是本地保留做的事 |
| 有标记 | 前缀不完整 | 不判断 |
| 无标记，本地和对象都不在 | 前缀完整 | `I6_ORPHAN_ROW`（#209） |
| 无标记 | 前缀不完整 | 不判断（`checkOffChainRows` 同样认 `ObjectPrefixes`；nil 时仍是阶段 1：`Objects` 非 nil 即全量） |

§6 把标记写成「`SEGMENT_MISSING` 也算一致」。本步不这么做：标记只免掉 `ORPHAN_ROW`。对象被列举确认丢失时仍开 `SEGMENT_MISSING#object`，否则对象丢了会静默。QA「保留期删除后无 `ORPHAN_ROW` 且有 `RETENTION_REMOVED`」仍然成立。

`ORPHAN_ROW` 改为 Managed，`Confirm`（目录、磁盘、列举不是同一次读，与 P2-5.1 的链 kind 一样两轮才打开；已打开的照旧只看清除规则）。`SEAL_FACTS_ABSENT` 不设 Confirm（QA 要求 1 个 sweep 内打开）。

确认没有对象存储时，已打开的 `ORPHAN_ROW` 以 `Reason=CONFIG` 关闭（与 `#object` / `UPLOAD_LATE` 相同），避免卸掉上传端点后 `chain.objects` 不再完整、finding 关不上。

未列举完的段上的 `I6_ORPHAN_ROW` 与对象类 kind 一样不进入本轮事实。

### 26.5 SEAL_FACTS_ABSENT（#353）

每轮评估探一次，写路径在 `sealFactsAbsent` 为真时也探一次（评估器关掉时同样恢复）。探针是 `SELECT first_event_at FROM binlog_files LIMIT 0`，加上 `schema_migrations` 的版本。`LIMIT 0` 没有行仍表示列在（`ErrNoRows`）。

- `seen`：本进程曾经探到列在。进程内一直为真，不随 down 清掉。
- 打开：列不在，且（schema 版本 ≥ 8，**或** `seen`）。一直停在 schema 6/7、从未见过这些列：不开（支持的部署）。曾经写过事实、然后 down 到 7：`seen` 为真、列不在 → 一轮内 OPEN。
- 关闭：探针看到列 → 清掉 `sealFactsAbsent`、`seen` 保持、finding CLOSE，之后的封存再写事实。记忆和库不一致在同一次探针里先改记忆再 Derive，不留着一条「不一致」的 finding。
- 第一次变成缺列打一条 WARN；列回来再打一条「封存事实恢复写入」。缺列期间不再每条封存都打。
- 探针失败：打日志，`store` scope 不完整（kind 变为 Managed 之后健康是 UNKNOWN），不改记忆，不关闭已打开的 finding。
- 没有 meta（standalone 文件）：`Complete[store]=true`，`SealFactsAbsent=false`。
- Derive 读 `Facts.Store.SealFactsAbsent`。subject 空，WARNING，健康 DEGRADED。不设 Confirm。detail 写 schema 版本，以及列回来之前不写封存事实。
- `store` 在全局 `Produced` 里。kind 一旦 Managed，每一轮成功的评估都必须把 `Complete[store]` 设上，否则所有任务变成 UNKNOWN。代评无主任务的 `Produced` 仍只有 `task.state`（§24.2），不因这次探针变成 UNKNOWN。

卡住窗口里已经写成 NULL 的旧行不回填。

不回填之后下游是否会把这些行看错（评审定案 §26.9 (c)）：
- 复核器（P2-3）的预期校验和是「目录行 sha256，否则任务目录的 `.seal-sha256` 账本」。账本在封存时由 `withSealFacts` 写，不依赖 000008，所以卡住窗口里封存的段照样有预期值、照样复核；两边都没有时复核器把这一段算作不可复核（`unavailable`），与 000008 之前一致，不算通过。
- `/window`、`/replay` 和恢复计划从段文件本身读事件头时间和 GTID（`assurance.WindowOf`），不读 `first_event_at` / `last_event_at`，NULL 不改变可恢复窗口。
- legacy 判断（P2-5.1 的本地大小比对）看的是 `.seal-sha256` 有没有这一行，不看目录行的 sha256。
- 对象一致性看 `checksum`（`match` / `mismatch`）与对象列举，不看 sha256。
所以这些 NULL 只是目录里少了冗余的封存事实，没有哪条路径因此把段判成正常或损坏。`SEAL_FACTS_ABSENT` 关闭的那一条 CLOSE 不声称补齐了旧行；回填留到以后单独做（§26.8）。

### 26.6 栅栏（§23 / §24）

保留跑在 `openBinlogWriter` 里，run 的 context 上有 `FenceIdentity`（standalone 与后台补传没有）。

| 写 | 栅栏 |
|---|---|
| `RETENTION_REMOVED` 的 `AppendEvent` | `execUnderFence`：有本任务令牌时与 lease 行同一事务；没有令牌时仍是单条 INSERT |
| `retention_removed_at` 的 UPDATE | 同上 |
| 封存事实 UPDATE | 同上，仍是 upsert 提交之后的第二条语句（不并进 upsert 事务） |
| 删本地文件之前 | `fenceWrite`（本地 lease 时钟）。拒绝则返回 `ErrWriteFenced`，不删、不记 S6 |

任务行和 finding 的栅栏不变。`ErrWriteFenced` 不打开 `RETENTION_RECORD_FAILED`。有令牌的其它 `AppendEvent` 调用方一并进 `execUnderFence`（run 里的事件不应在丢掉 lease 之后落地）；没有令牌的调用方语句不变：仍是一条 INSERT、不重试、出错原样返回（调度器的有序事件通道和 finding 汇都没有令牌，重试会让一次 meta 中断把通道堵住更久）。评审定案（§26.9 (d)）：今天只有 `run` 的 context 带令牌（`mysql_runner.go` 的 `WithFenceIdentity` 是唯一来源），里面写事件的只有 `RETENTION_REMOVED` 与 `RETENTION_SKIPPED_NOT_UPLOADED`；调度器的事件（`TASK_LEASE_LOST`、`SOURCE_SWITCHOVER`、状态事件）和 finding 汇的 `FINDING_*` 事件都用 `context.Background()` 派生的 context，不会因为 lease 已经释放而被挡掉。令牌只对同一个任务生效（`id.TaskID != taskID` 时走单条语句）。

### 26.7 测试（先红）

- `assurance`：本地和对象都没了、无标记 → `I6_ORPHAN_ROW`；同一行 `RetentionRemoved` → 没有 `I6_ORPHAN_ROW`；有标记且前缀完整、对象不在 → `I6_MISSING_OBJECT` 而不是孤儿；前缀不完整 → 两者都不报（链上与链下）。
- `health`：`ORPHAN_ROW` Managed 且 `Confirm`，`SEAL_FACTS_ABSENT` Managed 且无 Confirm；`Store.SealFactsAbsent` 且 `store` 完整时一轮打开、标志清掉后 CLOSE；孤儿第一轮不打开、第二轮打开；卸掉对象存储时 `ORPHAN_ROW` 以 CONFIG 关闭。
- `replication`：本地 1 天 / 桶 7 天，仍在桶窗口内的 `UPLOADED` 文件删掉本地后恰好一条 `RETENTION_REMOVED`，行上 `RetentionRemovedAt` 非空；桶窗口内、本目录没有 `file_path` 的行没有这条事件。事件写入失败 → `RETENTION_RECORD_FAILED`，这一轮不算干净。
- `meta`：封存 UPDATE 返回 1054 之后，下一次封存在探针成功时再写事实；探针仍是 1054 则继续跳过。有令牌时标记 UPDATE 与 lease 锁在同一事务；无令牌时仍是单条语句。
- `tasks`：评估器一轮内打开 `SEAL_FACTS_ABSENT`，列恢复的下一轮关闭且健康不是 UNKNOWN；保留轮的完成时间晚于失败 `FactAt` 时探针关闭 `RETENTION_RECORD_FAILED`。

### 26.8 本步不做

- `HEALTH_STALE`、完整 alerts API（P2-7）。
- 把整段桶过期改成留行。
- 回填卡住窗口里已经写成 NULL 的封存事实。
- `I6_MISSING_ROW`、集群模式下链 kind 的委派。

### 26.9 评审定案（2026-10-19）

- (a) 带标记的行，对象被完整列举确认不在时仍开 `SEGMENT_MISSING#object`：保留。标记只说明本地副本是保留期删的，不说明桶里那份还在；不开就是静默丢备份。
- (b) 没有 000010 时重启后会重复写 `RETENTION_REMOVED`：接受，见 §26.2。
- (c) 卡住窗口里的 NULL 不回填：下游不会因此误判，见 §26.5 末尾。
- (d) `AppendEvent` 有令牌时走栅栏：只有 run 里的保留事件带令牌，见 §26.6；没有令牌时保持单条语句、不重试。

## 27. #436 / #441：健康表缺失时不再静默，轮转不再误报 CHECKPOINT_AHEAD（2026-10-19）

§25 把「meta 不应答」做成内存里的 `META_UNREACHABLE`，`/api/health` 立刻 degraded。§22.4 规定 checkpoint 先于段列表读，免得运行中的任务把位点指到快照里还没有的新文件。这两处都还有一个洞：#436 是评估器开着、000009 不在时整套健康系统停了却仍然 `ok`；#441 是源端轮转时 I5 的「位点落在最后一段之后」在 1–2 秒里开一条 CRITICAL 再自己关上。本步只补这两个洞。不改 `minRequiredSchemaVersion`（仍是 6），不加配置项，不加迁移，不把 `CHECKPOINT_AHEAD` 改成两轮确认。

### 27.1 问题

**#436。** `health.evaluator=on`，在线 `migrate goto 7` 或 `8`（000009 的 `task_health`、`finding_transitions` 和新列都没了）。之后每一轮评估只打一行 INFO：`health evaluate task=1: health tables are absent; run ./migrate up to 000009`。finding 写不进去，`/api/health` 一直 `ok`。健康系统已经停了，操作员看不到。进程一开始就停在 schema 9 以下、评估器开着，也是同一件事。

QA 还看到：9→7 把 000008 的封存事实列和 000009 一起拿掉，`SEAL_FACTS_ABSENT`（§26.5）也开不出来。只拿掉 000008、留下 000009 时，它仍在一轮内打开、`up` 后一轮内关闭。

**#441。** 评估器开着，任务持续有写入，源端 `FLUSH BINARY LOGS`。standalone 和集群都能复现（v0.5.73 和 9bd6c9a）。`finding_transitions` 里一条 OPEN 和一条 CLOSE(EVAL)，大约 1–2 秒：

```text
CHECKPOINT_AHEAD  CRITICAL
checkpoint mbin.000007:4 is at a source file after the last chain segment mbin.000006
```

接了告警的人每次轮转都可能收到一条 CRITICAL。时间久了，真的 `CHECKPOINT_AHEAD` 也会被当成噪音。

### 27.2 根因（代码里的证据，不是猜测）

**#436。** `healthstore.ErrSchemaAbsent` 只在 `Evaluator.write` 里把 `schemaAbsent` 置上（`evaluator.go`）。评估一轮先 `ListOpenFindingsForTask`，这条 SELECT 用了 000009 的列（`evidence`、`segments`、`generation`），1054/1146 直接返回，`write` 走不到，`schemaAbsent` 一直是 false。于是 `ownsManaged` 仍为真：热路径不走 v0.5.66 的 upsert，评估器自己又写不进健康存储。`/api/health` 在有 meta 时读的是旧 finding 列（`openManagedFindings` 只在独立文件存储时读健康存储），读得到、里面没有 CRITICAL，状态就是 `ok`。日志是 `log.Printf`，经 zap 的标准库重定向落在 INFO，而且每个任务每一轮一次。

**#441。** `openSegment.rotate`（`internal/replication/run_segment.go`）的顺序是：把 checkpoint 写成**下一个**文件（新文件起点，pos 一般为 4），然后 `uploadSealed`（封存段上传，超时才继续，成功也要等它返回），**最后**才 `openAt` 把 `.open` 建出来。注释写明这是为了崩溃后续传从下一个文件开始，而不是在已封存的名字旁边再开一个 `.open`。

§22.4 的「先读 checkpoint 再列段」防的是另一件事：普通事务的 checkpoint 写在数据落盘之后，先列段再读位点会把 C 指到快照里还没有的文件。轮转相反：位点先于新文件出现，中间还隔着一次上传。上传要 1–2 秒时，5 秒一轮的 sweep 很容易落在这个窗口里。I5 的「`seq > lastSeq`」分支（`chain.go` `checkCheckpointPos`）看到的就是「checkpoint 在 `lastSeq+1`、pos ≤ 4，段列表里还没有这个 `.open`」。下一轮文件已经在，finding 关掉。两轮确认压得住症状，盖不住这个窗口；§22.1 也写了这个 kind 不靠 Confirm。

`assurance` 的用例「file after the tail」（checkpoint 在 `mysql-bin.000008:4`，链只到 `000007`，没有正在进行的轮转）是真的超前，必须继续打开。不能按「下一段、pos ≤ 4」一刀切掉。

### 27.3 决定

**D1 不拒绝启动。** schema 地板仍是 6，评估器在 v0.5.x 仍可单独打开。开着评估器、schema 低于 9，进程照常起来，用下面这条 finding 说明，而不是退出。v0.6.0 把地板提到 9 仍按 §9，本步不提前做。

**D2 `HEALTH_STORE_UNAVAILABLE`（新，S18）。** CRITICAL，Managed，task = `_cluster`，subject = 本节点 node id（与 `META_UNREACHABLE` 同一个 id；没有时用 `local`）。评估器模式是 `on`，并且健康存储的读或写返回 `ErrSchemaAbsent` 时打开。不等 10 秒：缺表不是慢查询。`shadow` 和 `off` 不开这条，也不把 `/api/health` 打成 degraded；`shadow` 对这个错误只打一次 WARN。

打开期间写不进 000009，所以和 §25 一样从内存列出：`/api/health`、`/api/findings`、`/api/findings?task_id=_cluster` 都能看到。`/api/health` 为 503 `degraded`。原因里除了 finding 那一行（`HEALTH_STORE_UNAVAILABLE/<node> (task _cluster)`），还有一句写明缺的是健康存储和怎么修：

- 探到 schema 版本且小于 9：`HEALTH_STORE_UNAVAILABLE: schema N < 9, run ./migrate up`
- 探不到版本，或版本已经 ≥ 9 但表/列仍缺：`HEALTH_STORE_UNAVAILABLE: health tables are absent; run ./migrate up to 000009`

版本来自现有的封存事实探针（`schema_migrations`）。探针失败不挡住这条 finding。

第一次打开打一行 WARN（`HEALTH_STORE_UNAVAILABLE opened ...`），关闭打一行 WARN。不再每个任务每一轮打 INFO。`work` 里对 `ErrSchemaAbsent` 不再 `log.Printf`。

指标不加新名字。`OpenFindings` 把这条算进去之后，已有的 `binlog_server_finding_open{task_id="_cluster",kind="HEALTH_STORE_UNAVAILABLE",severity="critical"}` 就是它。`binlog_server_finding_store_active` 只表示旧 finding 存储配了没有，不表示 000009 在不在，这里不用它。

**D3 关上，并且能写的时候把关写下来。** 一次健康存储写入成功（`ApplyTransitions` 返回 nil）就说明 000009 回来了。`schemaAbsent` 清掉。

- standalone（评估器拥有 `_cluster` 的信号，与 §25.7 相同）：恢复后先保持失败，把 OPEN 写入健康存储（`fact_at` = 第一次看到 `ErrSchemaAbsent`），写上之后再发成功，下一轮 CLOSE（`fact_at` = 第一次写成功的时刻）。一次缺失留下一对转移。
- 集群（P2-6 之前 `_cluster` 不归评估器，与 §25.7 相同）：内存里的这一条在恢复时交给旧 finding 路径，写成一行已关闭的 `task_findings`（`first_seen` = 第一次看到，`resolved_at` = 写成功的时刻），并写一条 `FINDING_RESOLVED`（`task_id=_cluster`，message 为 `HEALTH_STORE_UNAVAILABLE <node>`）。故障期间不写 REPORTED：000009 不在，健康存储的转移写不进去；旧列虽然还在，但这条 finding 的写者是评估器，不在缺口里改走旧 upsert，免得和恢复后的转移抢同一行。

`Run` 一开始先做一次健康存储读（`ListHealth` 即可）。进程启动时 schema 已经低于 9 的，不必等第一轮评估，`/api/health` 就 degraded。

`schemaAbsent` 一旦置上，`ownsManaged` 为假，其它 Managed kind 回到 §9 写的 v0.5.66 upsert。今天这条回退没有生效，就是因为读失败时标志置不上。本步把读失败也算进去。

**D4 和 `SEAL_FACTS_ABSENT` 的关系。** 两条各看各的，不互相代替。

| schema | 000008 列 | 000009 | 本步 |
|---|---|---|---|
| ≤ 7，本进程没见过列 | 无 | 无 | 只开 `HEALTH_STORE_UNAVAILABLE`。§26.5 本来就不开 `SEAL_FACTS_ABSENT`（支持的部署） |
| ≤ 7，本进程见过列（从 9 `goto 7`） | 无 | 无 | 只开 `HEALTH_STORE_UNAVAILABLE`。封存事实探针照旧跑，`sealFactsAbsent` 记忆照旧更新，所以列回来之后封存会再写事实。`SEAL_FACTS_ABSENT` 不另做一条内存 finding：000009 不在，它存不下；正常的 `./migrate up` 会把 000008 和 000009 一起带回来，下一轮列在，它不需要开。若只把 000009 补回、列仍缺（不是 `goto`/`up` 的路径），下一轮按 §26.5 打开并写进健康存储 |
| 8 | 在 | 无 | 只开 `HEALTH_STORE_UNAVAILABLE`。列在，`SEAL_FACTS_ABSENT` 不开 |
| ≥ 9，列缺、000009 在 | 无 | 在 | 与今天相同：一轮内 `SEAL_FACTS_ABSENT`，没有 `HEALTH_STORE_UNAVAILABLE` |
| ≥ 9，两者都在 | 在 | 在 | 两条都关着 |

缺口期间评估一轮在健康存储读失败处停下，不再做链的 Reconcile。链上的 finding 等 000009 回来的下一轮再判。操作员看到的是「健康存储不能用」，不是一条条存不住的链 finding。

**D5 轮转的快照（#441）。** 不改 I5 的判定，不加 Confirm。

1. 同一个任务上加一把锁 `HoldChain(taskID)`。轮转在「把 checkpoint 写成下一个文件」到「`.open` 已经在目录里」之间持有它。评估器在读这一轮的 checkpoint、目录指纹和段顺序时持有同一把锁，读完就放，不罩住后面的 `CheckBody`（那会读每个文件，把轮转堵住）。
2. 锁里面的顺序仍是先写 checkpoint、再创建文件（崩溃后续传必须从下一个文件开始，这条不变）。上传挪到放锁之后：挂住的上传不再把「位点已是新文件、文件还不存在」暴露给评估器，也不会占着这把锁。
3. 评估器看到的要么是「位点还在旧文件、新文件还没有」，要么是「位点在新文件、`.open` 已经在段列表里」。I5 的「after the last chain segment」在轮转窗口里看不到。
4. 没装上这把锁的读者（单测里的 runner、`/replay`）不产生 finding。生产里 app 把 scheduler 装到 runner 上。

残留，不靠两轮确认去盖：

- `openAt` 在 checkpoint 已经写上之后失败（磁盘满等）：锁已放，下一轮可能看到真的「位点在还没有的文件上」。这是轮转失败，不是每次 `FLUSH` 的常态；run 会返回错误。
- 进程在 checkpoint 提交之后、文件创建之前崩溃：进程已经不在评估。重启后续传会把文件打开。启动后第一轮若赶在打开之前，仍可能开一条短 finding。这不是 #441 的轮转窗口，本步不另加 Confirm，避免把现有的一轮打开用例拖成两轮。

真的超前（位点在链上不存在的更后文件、或同一文件上越过封存末尾、或 GTID 集合超前）仍在一轮内打开。现有 `TestCheckpointAheadOfTheChain` 和 `TestP253CheckpointAheadManaged` 保持。

### 27.4 类型与签名

- `health.HealthStoreUnavailable`（`HEALTH_STORE_UNAVAILABLE`），`tasks.FindingHealthStoreUnavailable`。`DefaultRegistry` 里是信号 spec，CRITICAL，scope `signals`，Managed，无 Confirm。`ManagedKinds`、lintguard 的 Managed 表一并加上。`isSignalKind` 加上它，但缺口期间不走 `SignalFailure`（见 D3：不改走旧 upsert）。
- `tasks.Scheduler.HoldChain(taskID string) (release func())`。未注册的任务也有自己的锁；`release` 可以多次调用约定为只放一次（调用方 `defer`）。
- `replication.WithChainGate(gate)`，`gate` 是 `HoldChain(taskID string) (release func())`。nil 时轮转不加锁，仍把 `openAt` 放在上传之前。`App` 在创建 runner 时把 scheduler 传进去。
- `openSegment.rotate`：checkpoint 与 `openAt` 在锁内，`uploadSealed` 在锁外、在 `openAt` 成功之后。
- `Evaluator.chainFactsInto`：checkpoint、指纹、`chainOrder` 在 `HoldChain` 内取，然后放锁再跑 `CheckBody` / `CheckCheckpoint`。
- 评估器上记缺口：第一次 `ErrSchemaAbsent`、schema 版本、是否已经打过打开日志、恢复时刻。`HealthSummary` / `OpenFindings` / `ListFindings` / `memoryOpenFindings` 在 `metaReachFinding` 旁边列出它。

Swagger 的 `/api/health` 是开放 object，finding 的 `kind` 也不是枚举。响应多了一句 reason、多了一种 kind 字符串，文档不改契约形状，本步不重生成 swagger。

### 27.5 失败路径矩阵

| 情况 | `/api/health` | finding | 日志 | 恢复后 |
|---|---|---|---|---|
| 评估器 `on`，schema 8（列在，无 000009） | degraded，reason 含 `schema 8 < 9` | 内存中一条 `_cluster` / 本节点 CRITICAL | 打开一次 WARN | `up` 后写入 OPEN+CLOSE（集群为已关闭行 + `FINDING_RESOLVED`），reason 消失 |
| 评估器 `on`，schema 7，没见过 000008 | 同上，`schema 7 < 9` | 同上 | 同上 | 同上；不出现 `SEAL_FACTS_ABSENT` |
| 评估器 `on`，从 9 `goto 7`（见过列） | 同上 | 只有 `HEALTH_STORE_UNAVAILABLE` | 同上；封存事实探针仍打自己的 WARN | `up` 后两条都不开（列和健康表一起回来） |
| 评估器 `on`，只缺 000008、000009 在 | 按 `SEAL_FACTS_ABSENT`（WARNING）degraded | 没有 `HEALTH_STORE_UNAVAILABLE` | 与 §26.5 相同 | 列回来即关 |
| 评估器 `off` 或 `shadow`，schema 7 | `ok`（没有别的 CRITICAL 时） | 无此 finding | `shadow` 一次 WARN | — |
| 评估器 `on`，schema ≥ 9，表被手工删掉 | degraded，reason 含 `health tables are absent` | 内存中一条 | 打开一次 WARN | 表回来后同上关闭 |
| 轮转，上传要 1–2 秒，评估器同时在跑 | 不因 `CHECKPOINT_AHEAD` 变化 | 零条 `CHECKPOINT_AHEAD` 开关 | 无 | — |
| checkpoint 停在链上没有的文件，没有轮转在进行 | degraded | `CHECKPOINT_AHEAD` 一轮打开 | 与今天相同 | 位点拉回后关闭 |
| 轮转中 `openAt` 失败 | 可能短暂 degraded | 可能一条 `CHECKPOINT_AHEAD`，直到重试打开文件或位点改回 | run 的错误照旧 | 见 D5 残留 |

### 27.6 测试（先红）

- `health`：`HEALTH_STORE_UNAVAILABLE` 在 Managed 表里，CRITICAL，无 Confirm；不从 assurance 派生。
- `tasks`：健康存储的读和写都返回 `ErrSchemaAbsent`、评估器 `on` 时，一轮之内（以及 `Run` 前的那一次探测）`HealthSummary` 为 degraded，reason 含 `HEALTH_STORE_UNAVAILABLE: schema 8 < 9, run ./migrate up`；`OpenFindings` 与 `ListFindings(_cluster)` 有一条 subject 为本节点的 CRITICAL。同一错误再评估几轮，打开日志只有一次。存储改为可写之后，standalone 留下 OPEN 与 CLOSE，`fact_at` 分别是第一次失败和第一次写成功；`/api/health` 回到 ok。schema 7 且封存事实探针报「列缺、见过列」时，不开 `SEAL_FACTS_ABSENT`，只开这一条。评估器 `off` 时 health 仍是 ok。
- `replication`：轮转里 checkpoint 已是下一个文件、上传还在进行时，并发的链快照读（持同一把锁）看不到「位点在最后一段之后」。不持锁、在上传之前就列目录的对照读得到这个窗口（证明测的是这把锁，不是把 I5 关了）。上传放在 `openAt` 之后：慢上传期间目录里已经有 `.open`。
- `assurance` / 现有 `tasks` P2-5.3：`TestCheckpointAheadOfTheChain` 的「file after the tail」仍是 I5；评估器上 GTID 超前仍一轮打开 `CHECKPOINT_AHEAD`，拉回后关闭。
- `lintguard`：新 kind 在 Managed 表里，没有新的 `Report` 调用。

### 27.7 本步不做

- 启动时因「评估器 on 且 schema < 9」退出。
- 把 `CHECKPOINT_AHEAD` 改成两轮确认，或按 pos ≤ 4 忽略下一段。
- 缺口期间把链 finding 也留在内存里。
- 新的 Prometheus 指标名、新配置、新迁移、swagger 重生成。
- P2-6 让 `_health` 持有者评估 `_cluster` 之后，集群这条改为 OPEN/CLOSE 转移（与 `META_UNREACHABLE` 同一笔账，到那时再改）。


### 27.8 评审定案（2026-10-19）

评审 PR #444 时按崩溃点和锁的两头又过了一遍。结论和要改的三处：

**崩溃点（轮转）。** 封存先把 `.open` 改名并写一行 `UPLOAD_FAILED`（`sealedUploadPending`），然后在锁内写下一个文件的 checkpoint、创建 `.open`，放锁，再上传。

| 崩溃在 | 磁盘 / 元数据 | 重启后 | 丢 / 重 |
|---|---|---|---|
| 封存之后、checkpoint 提交之前 | 封存段 + `UPLOAD_FAILED` 行，checkpoint 仍在旧文件 | 与 main 相同（本步没动这一段）；#391 让封存段的事务进续传集合 | 不变 |
| checkpoint 提交之后、`.open` 创建之前 | checkpoint = `next:4`，GTID 集合含已封存段 | 从 `next:4` 续传并创建文件；启动后第一轮若早于打开，可开一条短 `CHECKPOINT_AHEAD`，打开后关 | 无 |
| `.open` 创建之后、上传之前 / 上传中 | 只有 magic 的 `.open`，行仍是 `UPLOAD_FAILED` | 只有 magic 的 open 段从保存的 checkpoint 续传；后台重试上传并校验该行 | 无；不会有「已封存、没上传、也没记录」的段 |

上传在同一个 goroutine 里、`openAt` 之后同步执行，下一次轮转要等它返回，所以放锁不会让封存乱序。上传报错时 run 照旧返回错误，行仍是 `UPLOAD_FAILED`。

**R1 评估器不等轮转。** 本步之前评估器从不等复制热路径。加了 `HoldChain` 之后，轮转若卡在锁内（checkpoint 写在元数据上挂住、`openAt` 卡在磁盘上），评估器的 worker 会一直等这把锁。这一轮还没走到 `signalFacts`，所以这个任务的 `TASK_NO_PROGRESS` 永远判不出来：正好是轮转卡住时该报的那条。改为：评估器最多等 `ChainLockWait`（默认 2 秒）。等不到就把这一轮的 `chain.local` 记为不完整（UNKNOWN，不关已开的链 finding），其余 scope 照常评估。轮转一侧仍是阻塞取锁。

**R2 锁内的 checkpoint 读有上限。** `assuranceCheckpoint` 原来用 `context.Background()` 读元数据。它在锁内，元数据挂住时会把轮转堵在锁上，复制热路径因此等评估器。改为 scheduler 的内部读超时（`meta.timeout.read`）。读失败（超时、连接错误）时同样记 `chain.local` 不完整，不再当作「没有 checkpoint」去跑 I5：那样会把一条真的 `CHECKPOINT_AHEAD` 关掉。`ErrTaskNotFound` 仍按没有 checkpoint 处理。

**R3 每一轮扫描都探健康存储。** `probeHealthStore` 原来只在 `Run` 开始时跑一次，其后靠任务评估的读写发现缺口。集群里 `_cluster` 不归评估器，一个不负责任何任务的节点（没有 owner 的任务、没拿 `_health`）在线 `goto 8` 之后永远发现不了，`/api/health` 一直是 ok；反过来，它若在 schema 8 上启动，`up` 之后也永远关不掉，直到重启。改为 `Run` 的每个 Sweep tick 先探一次（有上限的 `ListHealth`），再 `markResponsible`。签名：`Scheduler.TryHoldChain(taskID, wait) (release func(), ok bool)`，`EvaluatorConfig.ChainLockWait`。

**shadow。** 只打一次 WARN，不 degraded。shadow 下评估器不拥有 finding，旧路径照常告警，这里不藏真实故障，按 D2 保持。

测试（先红）：
- 测试持着 `HoldChain` 不放时，`chainFacts` 在 `ChainLockWait` 量级内返回 nil。
- checkpoint 读直接报错时，`chainFacts` 返回 nil（不当作没有 checkpoint）。
- checkpoint 读一直挂住到 ctx 结束时，`chainFacts` 在读超时量级内返回 nil，且锁已放（随后 `HoldChain` 立刻拿到）。
- 运行中的评估器在 `Run` 之后健康存储改为 `ErrSchemaAbsent`、没有任何任务时，一个 Sweep 之内 `/api/health` degraded；改回可写后一个 Sweep 之内恢复。

## 28. P2-6.1：`_health` 持有者在 `_cluster` 上写 `HEALTH_STALE`（2026-10-19）

§13 的 P2-6 在 P2-6a/b/c 之后还剩三块，一块 PR 装不下。本步只做 P2-6.1。P2-6.2（链委派）和 P2-6.3（混版本退回 `shadow`）记在 §28.7，不在本步写代码。

### 28.1 已经交付（对照代码，不重做）

1. **有主任务的评估权跟随任务 lease。** `fenceFor`（`internal/tasks/evaluator.go`）在集群里只在 `OwnerWorkerID == NodeID && Epoch > 0` 时给出 `Fence{cluster, TaskEpoch: 任务 epoch}`。`MarkDirty(LEASE_LOST)` 丢掉 dirty 和 pending。`write` 遇到 `ErrFenced` 丢掉 pending，并把 `e.fenced[id]` 记成该任务 epoch，直到 `LEASE_ACQUIRED`。数据面栅栏是 P2-6b（`write_fence.go`，§23），任务行栅栏是 P2-6a/c（`CheckTaskRowFence`、`meta/fenced_exec.go`）。
2. **`_health` 选出谁评估无主任务。** `refreshHealthLease`（`evaluator_unowned.go`）用 `task_leases` 行 `_health`，TTL 3×sweep，CAS 只有一个持有者。`proxyFence` / `unownedFacts` 只评估任务行无 owner，或 owner 的任务 lease 已过期的任务的 `task.state`。`_health` 的 epoch 只用来选节点，不写进 finding 或 health 行。
3. **`ErrFenced` 与 `ErrLeaseLagging`。** `write` 对 `ErrFenced` 丢 pending；`ErrLeaseLagging` 不在这个 switch 里，pending 留下、下轮重试。`noteUnsaved` 不保留被栅栏拒绝的写。`scheduler_cluster_lease.go` 在 `LEASE_ACQUIRED` 时 `MarkDirty`，清掉 latch。`never` 和 `Proxy` 不 latch（`write` 里 `!fence.Proxy && fence.Mode != FenceNeverLeased`）。
4. **接管触发。** `ReasonLeaseAcquired` 把任务标脏，新持有者跑一轮期望状态调和。这不是链 finding 的 `OWNER_CHANGED`。`DUMP_STALLED` 在任务 epoch 变化时按已有清除规则关闭（P2-4）。
5. **`Proxy` 和 `Delegated` 栅栏类型。** `healthstore.CheckFence` 已实现，文件存储和 `storetest` 有用例。评估器从不把 `Delegated` 设为 true。`chain_owner` 只在 `fence.Delegated` 时写入（`internal/meta/health_store.go`）。
6. **冻结的旧 owner 锁不死新 owner。** 栅栏只比较任务 lease epoch。`_health` epoch 不进 finding / health 行。
7. **FAILED 不被改写成 STOPPED、`TASK_NO_PROGRESS`、有界 `/api/health`、`META_UNREACHABLE`、`HEALTH_STORE_UNAVAILABLE`。** 分别在 P2-6c、#427/#430、#445。

### 28.2 还缺什么

- **P2-6.1（本步）。** `HEALTH_STALE`。`stateSpec(HealthStale, …)`（`registry.go`）没有 `Derive`，也不在任何 `p2*Managed` 里，所以永远不开。`evaluate` 在 `cfg.Cluster` 时对 `_cluster` 直接 `errNotResponsible`。`responsible` 只在非集群且 `clusterHasWork` 时加入 `_cluster`。`OpenFindings` 只在非集群时把 `_cluster` 列入托管读。
- **P2-6.2（不做）。** 非共享存储下链 scope 委派给最后 owner（最后 lease epoch + 节点 id）、`OWNER_CHANGED` 关闭、`node_gone_after` 之后 `chain.*` 不完整、健康 UNKNOWN(node gone)。§24.3：FAILED 释放后任务行和 lease 行的 owner 都被清掉；代写还会改掉 `task_health.fence_node`。最后 owner 没有一行可靠的记录，不能在本步顺手做。共享存储下由持有者直接读目录也不在本步：没有「存储是共享的」这一事实，对着空的本地目录读会把链 finding 误关。
- **P2-6.3（不做）。** §9：`health.evaluator=on` 时，一条没有 version 的心跳加入，本节点评估退回 `shadow`。`worker_heartbeats.version` 从迁移 000001 起就是 `NOT NULL`，`effectiveBinaryVersion` 总会写入 build info 或 `"devel"`。没有任何代码读心跳来改评估模式。v0.5.67 不自动退回。什么叫「没有 version」（空串还是旧版本号）以及全局模式来回切，单独一步。

### 28.3 决定

**D1 切分。** 本 PR 只实现 P2-6.1。P2-7 仍等 P2-6.2 和 P2-6.3（§13 依赖图）。

**D2 finding 放哪。** task = `_cluster`，subject = 任务 id，WARNING，Managed，scope 只有 `task.state`，无 Confirm。写者只有 `_health` 持有者。栅栏是 `Fence{Mode: never, Node: 持有者, Instance: 持有者实例, TaskEpoch: 0}`，不是 Proxy，不是 Delegated。`_cluster` 没有 `task_leases` 行。`CheckFence` 的 `never` 不比较 `stored.Node`，后一任持有者可以覆盖。`_health` 的 epoch 不写入。

**D3 谁算过期。** 持有者每轮列出全部任务：本进程 `ListTasks` 与 store `ListTasks` 的并集（含有主任务；control-plane 没有认领循环，任务只在 store 里）。跳过 `_cluster` 和 `_health`。`evaluated_at` 有值时，年龄 = 库时钟 − `evaluated_at`。没有健康行时，年龄从本持有者第一次见到该任务（`knownSince`，进程内）起算，第一次见到不开。年龄 ≥ `stale_after` 才开。年龄 < `stale_after`，或任务已不在名单里，则 CLOSE。`ListHealth`、任务名单或库时钟失败：本轮 `task.state` 不完整，在 `write` 之前返回，不开也不关。`evaluated_at` 在库时钟之后（负年龄）不算过期。

**D4 配置。** `health.stale_after_sec`，0 = 5×sweep（默认 sweep 60s → 300s）。负数在启动时拒绝。正数按原值使用，不再乘 sweep。不把默认写成 300，这样它跟着 sweep 走。

**D5 负载。** 120 行 `evaluated_at = now−4×sweep` 时零条 `HEALTH_STALE`；其中一行 `now−5×sweep` 时恰好一条。没有健康行的任务第一次见到不开；过了 `stale_after` 仍没有行才开一条。detail 稳定，不写会变的「已过期多久」，否则每一轮都是 UPDATE。没有行：`task <id> has no task_health row; expected an evaluation since <RFC3339> (health.stale_after <duration>)`。有行：`task <id> has not been evaluated within health.stale_after (<duration>); last health write <RFC3339>`。

**D6 不碰其它 `_cluster` 结论。** 集群的这一轮只设 `Produced = {task.state}` 和 `Complete[task.state]`。不调用 `signalFacts`、`fillStoreFacts`、`settleMetaReach`、`settleHealthStore`。`FenceNeverLeased` 本来会走进 `proxy`（`unownedFacts`），本轮必须排除，否则会改掉 `META_UNREACHABLE` / `HEALTH_STORE_UNAVAILABLE` 的集群内存路径。standalone 的 `_cluster` 路径（`clusterHasWork`、信号、这两条 CRITICAL）不变。信号 scope 不完整，已开的 `META_UNREACHABLE` 不会被本轮关掉。`deriveRaw` 只在该 kind 的第一个 scope 完整时派生。

**D7 谁跑这一轮。** 本步 standalone 不开 `HEALTH_STALE`（进程没了没人写这条 finding）。只有集群，且只有持有者。`responsible` 在 `cfg.Cluster && healthHeld` 时加入 `_cluster`。`markResponsible` 先 `refreshHealthLease` 再 `responsible`，拿到 lease 的同一轮就包含 `_cluster`。

**D8 对外。** `/api/health` 只因 CRITICAL 降级，本 kind 是 WARNING，探针保持 ok。`_cluster` 的 `task_health` 在这条开着时是 DEGRADED，没有其它 finding 时是 HEALTHY。`OpenFindings` 在集群也列入 `_cluster`，这样 `/api/findings` 和已有的 `binlog_server_finding_open` 能看见它（meta 的 `ListOpenFindings` 本来就跨任务；单独的健康存储要靠这个 id）。集群里该 id 的 `ListOpenFindingsForTask` 返回 `ErrSchemaAbsent` 时忽略，不另加一条 “unreadable”（`HEALTH_STORE_UNAVAILABLE` 已经覆盖缺表）。standalone 对这个错误的行为不变。不加新的指标名。

**D9 `shadow`。** 照旧计算并打日志，不写。`_cluster` 的写入次数为 0。

**D10 不 latch。** `never` 的 `ErrFenced` 已有代码不写入 `e.fenced`。持有者换人后下一轮还能写。

**D11 单写者。** `HEALTH_STALE` 只由评估器派生，没有 `tasks.Finding` 常量，没有 `Report`。`managedWithoutConstant` 加上它。`isSignalKind` 不含它。

**D12 不改 `HealthSummary`。** 不把 WARNING 算进 `/api/health`，否则每一种 WARNING 都会变。

**D13 自己的行。** `_cluster` 这一轮会更新自己的 `evaluated_at`。它不是 `HEALTH_STALE` 的 subject。

**D14 换持有者。** `knownSince` 只在本进程、且只在没有健康行时使用。已有旧 `evaluated_at` 的任务，新持有者第一轮就开。新的一行第一轮就关。丢掉 `_health` 就停止评估；finding 留到新持有者调和。

**D15 时钟。** MySQL 把 `evaluated_at` 写成 `UTC_TIMESTAMP(6)`，忽略 Go 的时间（`health_store.go` 的 UPDATE）。年龄对照 `StoreNow`：`SELECT UTC_TIMESTAMP(6)`。`StoreNow` 失败则本轮不完整，不开也不关。没有这个接口的存储（本步的单测、文件存储）用 `cfg.Now()`。文件存储不实现它：standalone 不跑这一轮。节点墙钟与库时钟差 ±5 分钟不参与比较。

**D16 失败矩阵见 §28.5。**

### 28.4 类型与签名

- `health.Facts`：`StaleAfter time.Duration`；`HealthEvaluatedAt map[string]time.Time`（零值 = 本轮在名单里但没有可用的 `evaluated_at`；缺 key = 本轮不判断）；`HealthKnownSince map[string]time.Time`。
- `healthStaleDerive`。`DefaultRegistry` 里 `HEALTH_STALE` 改为带 `Derive` 的 spec：WARNING，scope `task.state`，Version 1，无 Confirm，清除文本 `the task is evaluated again within health.stale_after`。`p261Managed` 把它标成 Managed，`ManagedKinds` 一并带上。
- `EvaluatorConfig.StaleAfter`。`staleAfter()`：`> 0` 用配置，否则 `5 * Sweep`。
- `Evaluator.healthKnown`。`staleFacts`：在 `sweepMetaTimeout`（2s）内取任务并集、`ListHealth`、可选 `StoreNow`，然后填上面三个字段、`Complete[task.state]`、`Produced = {task.state}`。任一步失败就返回，评估在 `write` 之前停下。
- `authority`：集群且持有 `_health` 时，`_cluster` 用 `Fence{never, TaskEpoch: 0, Node, Instance}`。`proxy` 判定排除这一轮。
- `healthstore.StoreClock`：`StoreNow(ctx) (time.Time, error)`。`meta.MySQLTaskStore` 执行 `SELECT UTC_TIMESTAMP(6)`。
- `HealthConfig.StaleAfterSec`，viper 默认 0，`health.stale_after_sec` / `health_stale_after_sec`，校验 `>= 0`。`startHealthEvaluator` 把秒数赋给 `EvaluatorConfig.StaleAfter`。
- `OpenFindings`：集群也列入 `_cluster`（已在名单里则不重复）。集群且该 id 返回 `ErrSchemaAbsent` 时跳过，不记 unreadable。

Swagger 的 `kind` 不是枚举。多一种 kind 字符串，不重生成。

### 28.5 失败路径矩阵

| 情况 | 谁写 | finding | `_cluster` 的 task_health | `/api/health` |
|---|---|---|---|---|
| 持有者，某任务 `evaluated_at` 早于 5×sweep | 只有持有者，栅栏 `never`、epoch 0 | WARNING `HEALTH_STALE`，subject = 该任务 | DEGRADED | ok |
| 另外两个节点 | 不写 | 不变 | 不变 | ok |
| 120 个任务都在 4×sweep 以内 | 持有者写 `_cluster` 健康行，零条 OPEN | 无 | HEALTHY | ok |
| 没有健康行，持有者第一次见到 | 不 OPEN | 无 | HEALTHY | ok |
| 仍没有行，且 `knownSince` 已过 `stale_after` | 持有者 | 一条，detail 含第一次见到的时刻 | DEGRADED | ok |
| 任务从名单消失 | 持有者 | CLOSE | 按剩余 finding | ok |
| `ListHealth` / 任务名单 / `StoreNow` 失败 | 不写 | 保持原样 | 保持原样 | 不因本 kind 变化 |
| `shadow` | 不写 | 不改行 | 不改行 | ok |
| `_cluster` 上已有 `META_UNREACHABLE` | 持有者只动 `task.state` | 该 CRITICAL 仍开，并可另开 `HEALTH_STALE` | 因 CRITICAL 为 UNHEALTHY | degraded（因为那条 CRITICAL，不是因为 `HEALTH_STALE`） |
| standalone | 不跑本轮 | 无 `HEALTH_STALE` | 原 `_cluster` 路径 | 原路径 |
| 持有者换成另一节点 | 新持有者用 `never` 覆盖（不比节点，不比 `_health` epoch） | 按新一轮事实 | 随事实 | ok |

### 28.6 测试（先红）

先写、在本步实现之前失败：

- `health`：`HEALTH_STALE` 在 Managed 表里，WARNING，无 Confirm。
- `tasks`，三节点进程内（同一 `MemoryLease`，不进 workflow 的三节点 e2e；§24.12 的 workflow 改动留给人）：只有 `_health` 持有者打开上述 finding，另外两节点不写；栅栏是 `never`、`TaskEpoch` 0、非 Proxy、非 Delegated，且不等于 `_health` epoch。120 行里只有越过阈值的那一行打开。没有健康行时第一轮不开、过了 `stale_after` 才开。刷新 `evaluated_at` 后 CLOSE。任务从内存和 store 都消失后 CLOSE。`ListHealth` 失败时返回错误且不关已开的、也不新开。control-plane 节点（任务只在 store）能开。`_cluster` 上已开的 `META_UNREACHABLE` 本轮之后仍开。`shadow` 对 `_cluster` 的写入为 0。`/api/health` 在只有这条 WARNING 时为 ok，`/api/findings` 能列出。

实现提交再补上（依赖新字段，红提交里编译不过）：

- `healthStaleDerive`：4×sweep 不开，恰好 5×sweep 开，未来的 `evaluated_at` 不开，缺 key 的已开 finding 关闭，`_cluster` / `_health` 不作 subject，同一 `evaluated_at` 下两轮 detail 相同。
- 配置：`health.stale_after_sec: -1` 拒绝启动；缺省为 0。
- `StoreNow` 的 SQL 是 `UTC_TIMESTAMP(6)`。

### 28.7 本步不做

- P2-6.2：链 scope 委派、`OWNER_CHANGED`、`node_gone_after` → UNKNOWN(node gone)。落地见 §30。共享存储下持有者直接读链目录仍不做。
- P2-6.3：无 version 心跳加入时自动退回 `shadow`。
- 把集群的 `META_UNREACHABLE` / `HEALTH_STORE_UNAVAILABLE` 改成 OPEN/CLOSE 转移（§27.7）。
- 新的 Prometheus 指标名、schema 迁移、swagger 重生成。
- `HealthSummary` 把 WARNING 算进去。
- standalone 的 `HEALTH_STALE`。
- 评估器 `on` 且 schema < 9 时拒绝启动。
- 真机 SIGSTOP / kill -9。已交付的栅栏（P2-6a/b/c）和 P2-6.2 的盒子验收不在本步重跑。CI 的三节点 e2e 是 `workflow_dispatch`，本步用进程内三调度器代替。

## 29. #450：续租宽限把期望 RUNNING 写成 STOPPED，meta 恢复后不再续跑（2026-10-19）

v0.5.75 和 #449（`8301564`）都能复现，不是 #449 引入的。#425 堵住的是 meta 只读之后旧 owner 把行改写成 `STOPPED / desired STOP` 的那一条；续租宽限超时这条还在。本节是任务状态模型上的决定：期望状态和观察到的执行状态是两回事，只有运维 Stop 和 FAILED 可以写下期望 STOP。

### 29.1 现象（#450）

两节点或三节点，`lease_ttl_sec: 60`，`lease_renew_interval_sec: 5`，`lease_grace_sec` 仍是默认 30，评估器 on，任务期望 RUNNING。把节点到 meta 的通路黑洞或拒绝连接 35 秒以上（QA 测的是 36 秒、40 秒、45 秒）：

```
11:59:02  TASK_LEASE_DEGRADED
11:59:38  TASK_LEASE_GRACE_EXCEEDED, TASK_STOPPED
```

meta 恢复 90 秒后 `backup_tasks` 仍是 `STOPPED / owner 空 / desired STOP`。`task_findings` 没有未关闭的行，两个节点的 `/api/health` 都是 ok。v0.5.75 上同样停在 STOPPED，只是碰巧留下一条 `TASK_PERSIST_FAILED`。备份静默停掉，和运维手动停的样子一样。源端 purge 之后就丢数据。

### 29.2 根因

三问里，前两个都是第三个的后果。

**期望状态和观察到的执行状态被写成了同一件事。** `renewLeaseLoop` 在续租报错满 `lease_grace_sec` 时调用 `failSafeStopLocked`（`TASK_LEASE_GRACE_EXCEEDED`）。它把内存改成 STOPPING、取消 dump。run 退出时 `markStoppedLocked` → `writeStoppedLocked` 同时写三样东西：观察到的 `state = STOPPED`、`desired_run = STOP`、owner / epoch 清空，并释放 lease。`desired_run = STOP` 是运维 Stop 的信号（`StopTask` 先把 desired 写成 STOP，再写 STOPPING）。宽限超时不是运维 Stop，却走了同一条收尾。

续租返回 `!ok` 且行上没有新 owner（lease 只是被释放或过期、还没人认领）也走 `failSafeStopLocked`。续租循环 panic 同样。这三条都把「这一个 epoch 不能再写」写成「不要再跑」。

**调度器不会再认领，是因为行已经不像该跑的任务。** `ClaimExpiredTasks` 只认 lease 过期的 `RUNNING` / `LEASE_DEGRADED` / `RETRY_BACKOFF`。`decideConverge` 看到 desired STOP 就收尾，不再 Acquire。`STOPPED / desired STOP / owner 空` 和运维 Stop 无法区分。

**没有 finding，是因为告警看的是期望，而期望已经被改掉。**

1. `TASK_NO_PROGRESS`（§24.9）的前提是 desired RUN。写成 desired STOP 之后这条永不成立。矩阵里的 `lease-expired-stopped` 能报，是因为它手工留着 desired RUN；宽限路径不留。
2. `HEALTH_STALE`（§28）看 `task_health.evaluated_at`。无主的 STOPPED 仍由 `_health` 持有者代评 `task.state`，代评会刷新 `evaluated_at`。健康行一直是新的，HEALTH_STALE 不开。它本身也只是 WARNING，不让 `/api/health` 降级（§28 D8）。
3. `META_UNREACHABLE` 在 meta 一应答就关闭。恢复之后健康是 ok。

meta 还断着的时候，STOPPED 写往往失败，进 `unpublished`。`writeStoppedLocked` 已经把内存里的 owner / epoch 清掉，`republishUnpublished` 对空身份退回不带栅栏的 upsert（和 #425 同一形状）。meta 一恢复，这份 `STOPPED / desired STOP` 就落库，可能盖住已经认领的新 epoch。v0.5.75 上留下的 `TASK_PERSIST_FAILED` 就是这次失败的写。

默认数字对得上 QA 的 35 秒：宽限 30 秒，续租周期 5 秒，第一次失败后再过一个周期才判定宽限，大约 35 秒。`lease_ttl` 60 秒还没到，`0.8 × ttl` 的数据面栅栏（48 秒）也还没到。宽限先把 dump 停掉，并写下期望 STOP，过期认领根本看不到一行 RUNNING。

### 29.3 状态机

两条轴，不合并。

| 轴 | 存在哪 | 谁可以改 | 含义 |
|---|---|---|---|
| 期望 `desired_run` | 任务行 | 运维 Start / Stop；`markFailedLocked`（终态，离开只能再 Start） | 该不该有人跑 |
| 观察 `state` | 任务行 + 本进程内存 | 执行路径 | 现在有没有 dump 在跑 |

`STOPPED` 只表示「本进程这条 dump 已经结束」。它不是「不要再跑」。期望 STOP 只来自两处：运维 `StopTask`（先 desired STOP，再 STOPPING，run 退出写 STOPPED），以及 FAILED（desired STOP，`failed_spec_revision` = 当前 spec，离开只能运维 Start）。

续租失败、宽限用尽、lease 被别人拿走、续租循环 panic、进程被暂停，都只结束本 epoch 的 dump。期望保持 RUN。

```
期望 RUN
  ├─ 恰好一个节点持有未过期 lease，epoch 为栅栏，dump 在跑或在该 epoch 上重试
  └─ 否则：lease 已释放或已过期，任何节点可以 Acquire（释放后 owner 为空，epoch + 1）
        一个 sweep 内仍没有这样的持有者 → CRITICAL（§29.6）

期望 STOP（运维 Stop 或 FAILED）
  └─ 不认领、不改回 RUN、不开「没人跑」这条 finding
```

### 29.4 时序

记号：`G` = `lease_grace_sec`（默认 30），`T` = `lease_ttl_sec`（QA 为 60），`R` = 续租周期（QA 为 5）。宽限判定落在下一次续租 tick 上，所以观察到的停 dump 约在 `G` 到 `G+R`。数据面本地栅栏在 `0.8 × T`（QA 为 48 秒）拒绝本 epoch 的写。

**中断短于宽限（也短于 T）。** 续租报错，内存进入 `LEASE_DEGRADED`（写得进 meta 则行也是；写不进则 `unpublished`）。宽限之内续租成功：`markLeaseRenewedLocked` 回到 RUNNING，同一 epoch，dump 没有被取消。没有缺口，没有第二份 dump。v0.5.74 的 15 秒黑洞是这一档。

**中断长于宽限、短于 T（#450，约 35–60 秒）。** 宽限到了，本进程 `Lose` 这个 epoch（本地栅栏拒绝后续写）、取消 dump、尝试释放 lease。期望保持 RUN，行保持最后一次成功写下的 `RUNNING` 或 `LEASE_DEGRADED`（不写 STOPPED）。释放成功则 owner 被清空、`lease_expire_at = NOW`，下一个 Acquire 把 epoch 加一（释放后的 owner 是空，和任何 worker 都不同）。释放时 meta 仍不可达则记下重试，lease 自己也会在 T 到期。meta 恢复后认领循环 Acquire 新 epoch，从已存 checkpoint 续跑。旧 epoch 的写被栅栏拒绝。同一时刻最多一条 dump。

宽限已经 `Lose` 之后，还堵在 meta 上的 append 会在恢复时被本地栅栏拒绝（`WRITE_FENCED`）。这条不是新主人来了。它必须留在可认领状态上，不能写成 `STOPPED / desired RUN / owner 空`（#454，§29.10）。

**中断长于 T。** 宽限仍先于 T 取消 dump。lease 在 T 到期后即可被认领，不必等释放调用成功。期望仍是 RUN，所以认领发生。不会留下 desired STOP。

**脑裂：一个节点到得了 meta，另一个到不了。** 到不了的是 owner。它的续租报错，宽限后取消自己的 dump 并 `Lose`。释放调用到不了 meta，lease 行仍是它的、未过期，到得了 meta 的节点不认领，不会开第二条 dump。T 一到，对端 Acquire epoch+1。owner 醒来后的续租看到新 owner，走已有的 `yieldLostLeaseLocked`（只改内存，不写行，#346）。它晚到的行写带着旧 epoch，栅栏拒绝。

**节点 SIGSTOP（进程冻住，meta 正常）。** 进程不跑，宽限计时不走。冻的时间短于 T：解冻后的第一次续租仍是本 owner（续租 SQL 不看是否过期），`Confirm` 刷新本地栅栏，同一条 dump 继续。冻的时间长于 T 且对端已认领：解冻后续租 `!ok` 且行上是新 owner，`yieldLostLeaseLocked`，不写 STOPPED。这是 P2-6a/b 已有的路径，本节不改。SIGSTOP 不会走进宽限，因为续租调用被冻在内核里，返回的不是错误。

**运维 Stop 或 FAILED 与宽限重叠。** `StopTask` 先写 desired STOP。宽限路径看到 desired STOP 或已经是 STOPPING / STOPPED，就走原来的收尾，写成 STOPPED，不改回 RUN。FAILED 直接返回，不改行。run 的 context 已经结束时，续租循环返回，不计降级（#421）。

### 29.5 不变量

期望为 RUNNING 的任务，要么正在恰好一个节点上跑，并且由该 lease epoch 栅栏住（本地 `LeaseClock` 与 meta 里的 `task_leases` 是同一个 epoch），要么在一个 sweep 之内有一条 CRITICAL finding 开着，`/api/health` 降级。

「一个 sweep」：`_health` 持有者（standalone 则是本进程）在看到「期望 RUN、没有未过期 lease、本进程没有 run」时，从第一次看到算起满一个 sweep 就打开。认领循环默认 2 秒，短于默认 sweep 60 秒，正常的重新认领在下一次 sweep 之前完成，不开 finding。认领失败则下一次 sweep 打开，然后一直开到有人跑起来、运维 Stop 或 FAILED。

### 29.6 决定

**D1 切分。** 恢复和检测一起做。不新增配置，不迁移，不改 `lease_grace_sec` 的含义（到点仍必须停掉本 epoch 的 dump，避免和下一任双写）。

**D2 宽限、无人认领的 lease 丢失、续租 panic 都让出而不是 Stop。** 新的 `yieldForResumeLocked`：`LeaseClock.Lose`，取消本进程 dump，期望保持 RUN，观察状态留在 `LEASE_DEGRADED`（已经是则保持），不写 STOPPED，不改 desired。记 `TASK_LEASE_GRACE_EXCEEDED` 或原来的事件。run 退出时释放这个 epoch 的 lease，不调用 `markStoppedLocked`。释放报错（meta 仍不可达）则记在本进程，下一次 `ClaimRunnableTasks` 先重试释放再认领。已是 desired STOP、STOPPING、STOPPED 的，仍走 `failSafeStopLocked`。FAILED 不动。没有新主人的 `WRITE_FENCED` 走同一条让出（§29.10 D9），不把内存收成 `STOPPED`。

**D3 认领。** 释放之后行仍是 `RUNNING` 或 `LEASE_DEGRADED`、desired RUN。`ClaimExpiredTasks` 和本节点的空闲认领都会 Acquire。释放把 owner 清空，所以即使是同一个 worker 再次 Acquire，epoch 也加一（`task_leases` 的规则：owner 不同才加 epoch；空 owner 和谁都不同）。新 run 走现有的 checkpoint 续跑，不另起一份空文件。源端仍留着 checkpoint 之后的事件，续跑不产生 GTID 缺口，也不和旧 dump 重叠：旧 dump 在释放之前已经取消，旧 epoch 已被 `Lose`，`Confirm` 不能把它复活。已经落成 `STOPPED / desired RUN / owner 空` 的行同样认领（§29.10 D10）。运维 Stop 的 desired 是 STOP，不认领。

**D4 丢失的 epoch 不再被本进程拉起。** `LeaseClock.Lost`。`startTask` 在打开 dump 之前如果这个 epoch 已丢失，就再释放一次并返回，不 `resumeHeld`。下一次认领拿到的是新 epoch，`Confirm` 只刷新新 epoch。这避免「Lose 之后同一 epoch 的 Confirm 被拒绝，dump 开了却永远写不进」（P2-6b：丢失的 epoch 不能被 Confirm 复活）。

**D5 不把运维 Stop 或 FAILED 改回可运行。** desired STOP 的行，认领循环只收尾。FAILED 且 `failed_spec_revision` 等于当前 spec 的，`decideConverge` 仍是 `convergeNone`。本节不调用 `writeStartIntent`，不增加 spec。

**D6 检测并进 `TASK_NO_PROGRESS`，不新加 kind。** 没有本进程 run、也没有别的节点持有未过期 lease 时（无主、lease 已过期、`STOPPED / desired RUN`），时钟用 `min(sweep, no_progress_after)`，不再一律用 120 秒。评估器把这个值放进 `Facts.UnownedAfter`。0 表示沿用 `no_progress_after`（单测不填时保持 §24.9）。默认 sweep 60 秒、`no_progress_after` 120 秒时，无人跑的任务在一个 sweep 打开。本进程有 run 但没有进展的，仍用 `no_progress_after`（§24.9、§24.15 不变）。打开则 CRITICAL，`/api/health` 降级。关闭条件不变：有了进展、期望变为 STOP、FAILED、或别的节点持有。

**D7 进度。** 本进程已经让出的任务不再接受进度上报，直到新的 run 打开。否则宽限之后、认领之前的迟到上报会把「没人跑」的账记成有进展。

**D8 不改 HEALTH_STALE。** #450 的静默不是健康行过期，而是期望被改成 STOP 之后代评还在刷新健康行。D6 按期望 RUN 且没有持有者来报，不靠 HEALTH_STALE。#451（中断长于 `stale_after` 之后的第一次 sweep 对健康任务短暂 OPEN/CLOSE）和 #452（shadow 不写 `task_health`，每个 sweep 都对全部任务打 OPEN HEALTH_STALE）不是这条路径的后果，本节不改它们。

### 29.7 失败路径

| 情况 | 任务行 | 谁在跑 | finding | `/api/health` |
|---|---|---|---|---|
| 中断 < 宽限，然后恢复 | 仍是原 owner / 原 epoch，RUNNING，desired RUN | 原来那一条 dump | 无 | 中断期间 `META_UNREACHABLE`，恢复后 ok |
| 中断 ≥ 宽限（< T 或 > T），meta 恢复 | desired RUN，新 epoch，RUNNING | 恰好一个节点 | 认领成功则不开；一个 sweep 仍无人持有则 CRITICAL `TASK_NO_PROGRESS` | 无人持有时 degraded |
| 脑裂：owner 到不了 meta，对端可以 | 宽限后 owner 停 dump 但不写 STOPPED；T 后对端 epoch+1 | 最多一条 | 同上一行 | 同上一行 |
| 节点 SIGSTOP < T | 原 epoch，解冻后续租成功 | 原来那一条 | 无 | ok |
| 节点 SIGSTOP > T，对端已认领 | 新 epoch；旧 owner 只让出内存 | 对端那一条 | 无（认领已完成） | ok |
| 运维 Stop | `STOPPED / desired STOP` | 无 | 无 | ok |
| FAILED | `FAILED / desired STOP`，`last_error` 保留 | 无 | `TASK_FAILED` | degraded |
| 让出之后没有 runner 会认领（只剩控制面） | desired RUN，lease 已释放或过期 | 无 | 一个 sweep 内 CRITICAL `TASK_NO_PROGRESS` | degraded |

### 29.8 测试（先红）

`internal/tasks/p29_meta_outage_resume_test.go`，进程内三个调度器、一把 `MemoryLease`、一张带栅栏的任务表（与 §28 的三调度器同一形状，不进 workflow e2e）：

- 复现 #450：TTL 长于宽限，meta 调用在宽限之内挂起然后恢复。今天失败：行变成 `STOPPED / desired STOP`，认领循环拉不起来。修好后：desired RUN，恰好一个节点 RUNNING，epoch 比让出前大，两条 dump 的存活期不重叠。
- 中断短于宽限：同一条 dump、同一 epoch，不写 STOPPED。
- 只挡住 owner 的续租（对端到得了 meta）：宽限后对端在 lease 仍被持有时不开第二条 dump；释放或到期之后恰好一条。
- 续租调用被冻住（SIGSTOP）短于 TTL：不解出 STOPPED，解冻后同一条 dump 还在。
- 运维 Stop 之后认领循环不把它拉起。FAILED 之后也不拉起。
- 让出之后没有人认领：一个 sweep 内 CRITICAL `TASK_NO_PROGRESS`，`HealthSummary` degraded。

实现再改已有的续租单测：它们断言宽限或 `!ok` 会把行收到 STOPPED 并清空 owner。那是 #450 的旧行为。改为期望保持 RUN、观察状态留在 `LEASE_DEGRADED`、dump 已停、lease 已释放。直接调用 `failSafeStopLocked` 的用例仍覆盖「已经在收尾的 Stop」和栅栏拒绝。

#454 再加两条，先红：宽限取消时在途 append 返回 `WRITE_FENCED`，并在 run 退出后把内存行再发布一次。今天失败：行是 `STOPPED / desired RUN / owner 空`，认领拉不起来。修好后内存留在 `LEASE_DEGRADED` 或 `RETRY_BACKOFF`，行不是这份 STOPPED，认领后 epoch 比让出前大，恰好一条 dump，两条不重叠。另一条把已经落成 `STOPPED / desired RUN / owner 空`（spec 已不是 0）的行交给认领循环，epoch 加一续跑；运维 Stop 仍不拉起。

### 29.9 本步不做

- #451、#452（§29.6 D8），以及那两条之外的健康行抖动。
- 新的 finding kind、新配置、新迁移、swagger。
- 真机两节点黑洞。进程内三调度器是本 PR 的回归；QA 探针写在 PR 说明里。45 秒黑洞（宽限 < 中断 < TTL）要在 2 节点和 3 节点上多跑几次，覆盖在途 append 被栅栏拒绝（#454）。

### 29.10 #454：宽限让出时在途 append 被栅栏拒绝，行停在 STOPPED / desired RUN / 无 owner（2026-10-19）

PR #453 把「期望被写成 STOP」堵住了。2 节点、`lease_ttl` 60、宽限 30、续租 5，把到 meta 的通路黑洞 45 秒（宽限 < 中断 < TTL），4 次里有 1 次备份仍停住。3 节点和另外 3 次 2 节点都以 epoch 2 续跑。检测是好的：恢复后约 7 秒 `TASK_NO_PROGRESS` 为 CRITICAL，`/api/health` 为 503。要等人工 Stop/Start 才再跑。

**现象。** 宽限到点记 `TASK_LEASE_GRACE_EXCEEDED`。meta 一恢复，堵在事务里的 append 被本地栅栏拒绝（`fence: refused append`，`WRITE_FENCED`），runner 记 `TASK_LEASE_YIELDED`。200 秒后 `backup_tasks` 仍是 `STOPPED / desired RUN / owner 空`，`task_leases` 是空 owner、原 epoch。两个节点都不认领。

**根因。** 这条 `WRITE_FENCED` 没有新主人。宽限已经 `Lose` 了本 epoch，在途 append 的下一次写边界被本地栅栏拒绝，走的是「别人持有 lease」那条 handoff：内存写成 `STOPPED`、owner 和 epoch 清空，desired 不动。run 退出看到内存已经是 STOPPED，就不再 `markStoppedLocked`（那条会把 desired 写成 STOP），只释放 lease。宽限期间失败的落库还在飞，`dropUnpublished` 先清掉再发布标记，写超时之后 `notePersistFailure` 又把它设上。下一次 sink 把**当前内存**再发布出去。owner 已经是空，`persistTaskFencedLocked` 退回不带栅栏的 upsert，行就成了 `STOPPED / desired RUN / owner 空`。认领都匹配不到：`ListTasksWithExpiredLease` 的状态只有 RUNNING / LEASE_DEGRADED / RETRY_BACKOFF / STOPPING；`ListTaskIDsWithExpiredOwnerLease` 要求 owner 非空且排除 STOPPED；`isClaimableActiveState` 不含 STOPPED。运维 Stop 也是 STOPPED，但是 desired STOP，和这行不是同一种。

**D9 没有新主人就不写 STOPPED。** `IsLeaseHandoff` 先看任务行和 `task_leases`。别的 owner 或更新的 epoch 已经在上面（#346、#420、#425）仍只改内存、丢掉 unpublished，不盖住新主人。读失败不算被抢走。没有新主人（lease 还是本 epoch，或已经释放成空 owner）走 `yieldFencedEpochLocked`：期望保持 RUN，观察状态留在 `LEASE_DEGRADED`（已经是 `RETRY_BACKOFF` 则保持），不改 owner，不调用 `markStoppedLocked`。run 退出释放该 epoch。本进程已经让出、内存却是 `STOPPED / desired RUN / owner 空` 时，`republishUnpublished` 丢掉这次再发布，避免无栅栏覆盖。store 里留下的是上一次成功的 `RUNNING` 或 `LEASE_DEGRADED`。

**D10 已经写成该形状的行可以认领。** `desired_run = RUN` 且 `owner_worker_id` 为空且 `state = STOPPED`、lease 已过期：`ListTasksWithExpiredLease`、`ClaimExpiredTasks` 和本进程空闲认领都 Acquire。运维 Stop 的 desired 是 STOP，不在这条里。`spec_revision` 和 `applied_spec_revision` 都是 0 的旧行仍按状态推导期望，STOPPED 推导为 STOP，认领循环看到的是收尾而不是拉起。新 run 仍是 D3 的 checkpoint 续跑：epoch 加一，不另起一份空文件，不和旧 dump 重叠。

不变量不变（§29.5）：期望 RUN 要么恰好一个节点在跑并由该 lease epoch 栅栏住，要么一个 sweep 内有 CRITICAL finding。恢复后 epoch 比让出前大 1。

### 29.11 #457：旧 epoch 的 `.open.eN` 被接管删掉之后，`stat` ENOENT 把任务写成 FAILED（2026-10-19）

共享 `data_dir` 的三节点。owner 到不了 meta 的时间 ≥ lease TTL，对端以 epoch+1 接管，并按 #415 删掉旧的 `.open.eN`（复制完整事件到 `.takeover-*`，改名到新 epoch，再 `Remove` 旧文件，避免冻住的写者用旧 fd 改到续写或已封存的分段）。旧 owner 的 meta 恢复后，`flushCheckpoint` 在 checkpoint 写已经返回之后 `stat` 自己的 `.open.eN`，得到 `ENOENT`。这条不是 `WRITE_FENCED`，调度器把它当成未分类的 runner 错误，`markFailedLocked` 用不带栅栏的 upsert 写成 `FAILED / desired STOP`，并记 `TASK_FAILED` CRITICAL。新 owner 看到行被别的 epoch 写成 FAILED，取消自己还健康的 dump（`cancelled local dump: row FAILED by another epoch`）。备份停住，直到运维介入。

**这条 ENOENT 是移交，不是失败。** 本 run 的 epoch 对应的 open 文件不在了（`stat` / `open` / `rename` 的 `ENOENT`，basename 经 `ClassifySegment` 是 `.open.e{本 epoch}`），并且下面任一成立：本进程 `LeaseClock.Lost` 了这个 epoch，或 `task_leases` 的 `Verify` 明确返回不再持有，或任务行 / lease 上已经是别的 owner 或更大的 epoch。处理与已有的 lease handoff 相同：不调用 `writeFailed`，不把行写成 FAILED，期望保持 RUN。已经有新 epoch 在跑时，只改本进程内存，共享行仍是那个 RUNNING owner。没有新主人时留在可认领状态（§29.10），不写 desired STOP。`Verify` 或读行失败不算被抢走。

**下面仍要失败。** 本 epoch 还持有租约（未 `Lost`，`Verify` 为真，行上没有更新的 owner / epoch）时，缺的 open 文件是真的缺，包括未上传的尾巴。`SEGMENT_NOT_ON_WORKER`（`ResolveTakeover.Missing`、目录行路径不可读、路径在 `data_dir` 之外）不是这条 ENOENT，仍是永久失败。#415 的复制再删除不改。

## 30. P2-6.2：非共享存储的链委派（2026-10-19）

§8.2 是草图。本节是对照当前代码之后的决定。§29 是 #450 / #454 的续跑，不是这一步。P2-6.3（混版本退回 `shadow`）不在这里。

### 30.1 代码里已经成立的事实

1. **最后 owner 今天没有可靠的行。** FAILED 释放 lease（§24.3）把任务行的 `owner_worker_id` 和 `task_leases.owner_worker_id` 都清掉，epoch 留在 lease 行上（`Release` 不清 epoch）。`proxyFence` 在 owner 为空时用 `_health` 持有者自己的节点填 `Fence.Node`。`applyOnce` 每次都写 `fence_node = fence.Node`，并且只在 `fence.Delegated` 时写 `chain_owner`，否则写成空。代写因此既改掉 `fence_node`，又清掉 `chain_owner`。
2. **新 lease 已经拒绝委派写。** `CheckFence` 要求 `lease.Epoch == Fence.TaskEpoch`。`storetest` 的 ClusterFence 已断言「delegation must lapse on a new lease」。这一条不用新规则。
3. **已释放的同 epoch 不比节点。** `releasedProxy` 对 `Proxy` 和 `Delegated` 都成立：owner 为空时，同 epoch 不同 `fence_node` 不返回 `ErrFenced`。所以任何节点都能以 `Delegated` 在这个 epoch 上写。缺的是「只能是被记录的最后 owner」。
4. **栅栏只比任务 lease epoch。** 从不与 `_health` 的 epoch 比较（§4、§8）。委派写的 `TaskEpoch` 是任务 lease 行上的 epoch。
5. **没有「存储是共享的」这一事实。** `_health` 持有者读自己的本地目录，空目录会把链 finding 关掉。本步不读。
6. **standalone 在 `fenceFor` 里就返回**，不走集群栅栏。文件存储拒绝非 standalone 的 fence。
7. **评估器从不把 `Delegated` 设为 true。** 最后 owner 在行上的 owner 被清掉之后是 `errNotResponsible`，链 finding 停在打开，健康由代写的 `task.state` 决定（链 scope 不在本轮 `Produced` 里，所以不完整也不变成 UNKNOWN）。
8. **`chain_owner` 列已经在 `000009`。** 不新增列、不新增迁移。再加一列会让 schema 10 上的健康写入遇到 1054，`healthErr` 把它变成 `ErrSchemaAbsent`，评估器退回旧路径。

### 30.2 决定

**D1 最后 owner 记在 `task_health.chain_owner`，绑定的 epoch 是 lease 行的 epoch。** 不新增 `chain_epoch`。lease 行在释放后仍留着 epoch；委派写把这个 epoch 放进 `Fence.TaskEpoch`，`CheckFence` 已有的比较就是绑定。`fence_node` 继续被代写改掉，不拿它当最后 owner。

**D2 只有集群持有者的写入更新 `chain_owner`。** `Mode == cluster` 且不是 `Proxy`、不是 `Delegated` 时，`chain_owner = fence.Node`。代写、委派写、standalone、`never`、waiver 的 UPDATE 都写成 `chain_owner = chain_owner`。已经被旧代写清成空的行保持空，直到下一次持有者写入；不回填，不猜。

**D3 委派写多一道节点检查。** `Delegated` 在现有的 lease epoch 检查之后还要求：已有 `task_health` 行，且 `chain_owner` 非空，且等于 `Fence.Node`。否则 `ErrFenced`。新 lease（epoch 不同）仍由原检查拒绝。不把这次 `ErrFenced` 闩住：和 `Proxy` 一样，下一轮重新派生。`storetest` 的并发代写在开风暴之前先做一次持有者写入（转移为空，generation 仍是 0，只记下 `chain_owner`），否则空的 `chain_owner` 会把委派写全部拒绝。

**D4 谁评估。** `authorityFor`：行上 owner 是自己且 epoch > 0 时仍走 `fenceFor`（持有者，不是委派）。否则在代写之前试委派：集群、不是 control-plane、不是 `_cluster` / `_health`、`chain_owner` 是自己、lease 行在、epoch > 0、lease owner 为空或是自己。同一节点既持有 `_health` 又是最后 owner 时，一轮同时带 `Proxy` 和 `Delegated`：`task.state` 用 `unownedFacts`，链用 `chainFacts`（`facts == nil`，避免对象存储把 `Produced` 换成全局集合）。只委派的一轮只产生 `chain.local`，不读信号、不探 store。control-plane 只代写，不委派。`mayEvaluate` 不读库：持有者写成功或委派栅栏成立时记下这个任务，委派写被拒时清掉，避免未落库的健康被剪掉。

**D5 `OWNER_CHANGED` 只在节点变了的时候。** 新的集群持有者（不是代写、不是委派）读到的 `chain_owner` 非空且不等于自己，则 `Facts.ChainOwnerChanged`。`Reconcile` 把 scope 只有 `chain.local` 和/或 `chain.objects` 的打开 finding 以 `OWNER_CHANGED` 关闭，即使新目录不完整；不把这个 key 记进 seen，同一轮里新目录仍想要的实例再 OPEN。同一 key 的 CLOSE 排在 OPEN 前面（`sort.Slice` 不稳定，按 key 再按 op）。同一节点、新 epoch（目录还是那一份）不是 `OWNER_CHANGED`，仍是 `EVAL`。`UPLOAD_UNVERIFIED` 带 `signals`，不在这条里。读 `chain_owner` 失败则不置位，不误关。

**D6 `node_gone_after`。** 配置 `health.node_gone_after_sec`，0 = 3×sweep，负数拒绝启动。`_health` 持有者的代写轮：`chain_owner` 非空、不是自己、并且该 worker 的 `last_seen_at` 早于阈值（没有心跳行算消失）时，不读目录，把 `chain.local` 标成已产生但不完整，`Facts.NodeGone`。链 finding 保持打开，不以 `NODE_GONE` 关闭。心跳列表读失败不算消失；任务库没有心跳接口也不算消失。比较用 `StoreClock.StoreNow`，没有则用评估器时钟。

**D7 健康。** `NodeGone` 时，只含链目录 scope 的 finding 不参与 CRITICAL / WARNING；`chain.local` 强制进 `Unknown`；没有别的 WARNING / CRITICAL 时状态是 UNKNOWN，`TaskHealth.UnknownDetail = "node gone"`。这个字符串不入库（MySQL 只写 `status` 和 `reasons`）。非链的 CRITICAL（`TASK_FAILED`）仍是 UNHEALTHY。`HealthSummary` 仍只因 CRITICAL 降级，不改。

**D8 standalone 不变。** 不在文件存储上实现 `ChainOwner`。standalone 的空残留路径保持现在的开、关和 HEALTHY。

**D9 不读共享目录，shadow 不记 `chain_owner`。** shadow 只计算不写，所以不会记下最后 owner；本步不补。`_cluster` 上以 `NODE_GONE` 关闭集群 finding（§8.9）仍不做：那些 subject 是观察者节点或任务 id，不是链的最后 owner。

### 30.3 保证

| 情况 | 链 finding | 委派写 | 健康 |
|---|---|---|---|
| STOPPED，最后 owner 还在，目录仍是它的 | 它继续打开和关闭 | 同 epoch、`chain_owner` 是它，允许 | 目录完整时不为 UNKNOWN |
| 代写改了 `fence_node` | `chain_owner` 不动 | 最后 owner 仍可写 | 同上一行 |
| 别的节点、同一 epoch、`Delegated` | — | `ErrFenced` | — |
| 新 lease（任何节点，更高 epoch） | 新持有者按新目录重算 | 旧 epoch 的委派写 `ErrFenced` | 按新目录 |
| 新持有者在另一个节点 | 旧目录的链 finding 以 `OWNER_CHANGED` 关闭；新目录仍成立的同一 key 同一轮再打开 | 旧 owner 被拒 | 按新目录 |
| 同一节点，新 epoch | 按新一轮事实，原因 `EVAL` | 它现在是持有者，不是委派 | 按这个目录 |
| 最后 owner 的心跳早于 `node_gone_after`（或没有心跳行） | 保持打开 | 它已经不写 | UNKNOWN，`chain.local`，detail `node gone` |
| 同上，但另有非链 CRITICAL（`TASK_FAILED`） | 链 finding 仍开着 | — | UNHEALTHY |
| 心跳列表读失败，或任务库没有心跳接口 | 保持打开 | — | 不因节点消失变成 UNKNOWN |
| standalone | 现有路径 | 无委派 | 现有路径 |

### 30.4 测试（先红）

进程内多调度器，不进 `workflow_dispatch` 的三节点 e2e。先写、在实现之前失败的：

- `TestP262NewLeaseRejectsDelegatedWrite`：更高 epoch 拒绝旧的委派写。这条今天已经绿，留作回归。
- `TestP262OtherNodeDelegatedWriteRejected`：lease 已释放、代写改过 `fence_node` 之后，不是最后 owner 的 `Delegated` 写必须 `ErrFenced`。今天通过。
- `TestP262StoppedLastOwnerOpensAndClosesChain`：最后 owner 打开 `SEGMENT_EMPTY_LEFTOVER`，释放 lease 并清空行 owner，代写一轮之后删掉残留，它必须关掉且健康不是 UNKNOWN。今天是 `errNotResponsible`。
- `TestP262OwnerChangedClosesOldDirectory`：另一节点新 epoch、另一份没有残留的目录，关闭原因必须是 `OWNER_CHANGED`。今天是 `EVAL`。
- `TestP262NodeGoneChainUnknown`：最后 owner 不再评估，心跳 `last_seen` 早于 3×sweep，链 finding 仍开，健康 UNKNOWN。今天代写写成 HEALTHY。
- `TestP262StandaloneChainUnchanged`：standalone 打开再关闭空残留，健康不是 UNKNOWN，没有 `OWNER_CHANGED`。今天已经绿。

实现再补上（依赖新字段，红提交里编译不过）：

- 同一节点新 epoch 的关闭原因是 `EVAL`。
- 代写之后 `chain_owner` 仍是最后 owner，它的委派写成功，别的节点被拒。
- `NodeGone` 时链上的 CRITICAL 不把健康打成 UNHEALTHY；另有 `TASK_FAILED` 时仍是 UNHEALTHY；`UnknownDetail` 是 `node gone`。
- 心跳读失败不置 `NodeGone`。
- `health.node_gone_after_sec: -1` 拒绝启动；缺省为 0。

### 30.5 本步不做

- P2-6.3。
- 共享存储上由 `_health` 持有者读链目录。
- 新迁移、新 finding kind、`HealthSummary` 把 UNKNOWN 算进 `/api/health`。
- §8.9：`_cluster` finding 以 `NODE_GONE` 关闭。
- 改 `.github/workflows`。三节点 e2e 仍是 `workflow_dispatch`。
- 真机 SIGSTOP / kill -9。QA 在非共享存储上停掉最后 owner：`node_gone_after` 之后健康是 UNKNOWN(node gone)，链 finding 仍开；任务在另一节点起来之后旧目录 finding 以 `OWNER_CHANGED` 关闭，旧 owner 的委派写被拒。
