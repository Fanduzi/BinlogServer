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
| `HEALTH_STALE`（新，集群级） | W | task.state | 某任务 `task_health.evaluated_at` 超过 `health.stale_after`（默认 5×sweep，可配）未更新且该任务应有评估者 | 该任务被评估 |
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
| P2-6 | 集群：评估权跟随任务 lease、`_health` 选评估者、非共享存储委派最后 owner（绑定最后 lease epoch + 节点）、OWNER_CHANGED、ErrFenced 处理、接管调和、`HEALTH_STALE`、节点消失清理 | 两/三节点：冻结 owner 写被拒且新 owner 不被锁死；委派在新 lease 后失效 | `HEALTH_STALE` | P2-4、P2-5、P2-6a |
| P2-7 | API（`/api/alerts` 分页、`/tasks/{id}/health`、mute（key 级 + 任务级，`task_mutes`）、waive、evaluate、standalone takeover）+ `task_muted` 指标+ 指标（`task_healthy` 总有序列、meta 宕机缓存、真实 2s）+ 推荐规则文件；swagger | 「新建任务即有序列」「meta 暂停时 2s 内返回且序列为 0」「评估停 `stale_after` → UNKNOWN」红 | — | P2-6 |
| P2-8 | Console 告警页（静音、豁免操作）、健康徽标、finding 事件按事实时间排序 | UI 测试：告警页数据与 `/api/alerts` 一致 | — | P2-7 |
| P2-9 | 通知器：`_notify` lease、outbox 游标、两种格式（AM webhook 格式 / AM `/api/v2/alerts`）、去抖、flapping、静音、UPGRADE 摘要；`docs/notifications.md`（钉钉、飞书、邮件路由） | 「投递失败重试且不丢」「`_notify` 切换不重复也不丢」「升级首轮只发一条摘要」红 | — | P2-6、P2-7 |

依赖图：P2-1 → P2-2 → P2-3 → P2-4 → P2-5（另需 P2-5a ← P2-3）；P2-2 → P2-6a；{P2-4, P2-5, P2-6a} → P2-6 → P2-7 → {P2-8, P2-9}。阶段 2 完成后发 v0.6.0，schema 下限提到 9，`health.evaluator` 默认 `on`（§15 D1）。

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
| P2-6 | 3 节点：kill -9 owner；SIGSTOP owner 60s（#364）与 2min（#355）后恢复——旧 owner 的 finding/health/任务行写全部被拒，新 owner 不出现 ErrFenced、≤1 sweep 调和完成；STOPPED 任务只由一个 `_health` 节点评估；非共享存储下 STOPPED 任务的链 finding 由最后 owner 打开/关闭、健康不为 UNKNOWN；任务在另一节点重启后旧目录 finding 以 OWNER_CHANGED 关闭、旧 owner 的委派写被拒；kill 最后 owner `node_gone_after` 后 UNKNOWN(node gone)；杀 `_health` 持有者后另一节点接手；load ~120 时 `HEALTH_STALE` 不误报（默认 5×sweep）；meta 慢导致续约迟到（lease 过期但仍是本节点）时评估不停止、续约后转移写入；节点时钟偏移 ±5min 不影响关闭判定；混版本：v0.5.67 不自动退回（§9），验收项为 `on` 期间加入一个 v0.5.66 节点时任务仍由新版本节点接管、恢复数据一致，自动退回 `shadow` 列为后续 P2-6 实现时的验收项 |
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

**规则**：接管 / run start 时，catalog 的 `file_path`（或其目录）必须落在**本节点配置的 `data_dir`** 之下（`filepath.Clean`，两侧能 `EvalSymlinks` 则解析后再比；比较用 `filepath.Rel`，禁止 `..` 逃逸）。可读但落在本 `data_dir` **之外** → 与不可读 / ENOENT 相同：永久 `SEGMENT_NOT_ON_WORKER`（点名该分段），**不得**因可读就继续写入外来绝对路径，也不得在本机另起一套静默目录假装续写。

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

### 24.7 本步不做

- `HEALTH_STALE`（§8 第 10 条）：需要持有者给**有主**任务写 finding，而有主任务的写权在 owner，代写栅栏要求 lease 无人持有，两者冲突；拟改为 `_cluster` 级 finding（subject = 任务 id，`never` 栅栏，持有者写），下一步单独做。
- 链 scope 的代评与非共享存储的委派、`OWNER_CHANGED`、节点消失清理（§13 P2-6 其余部分）。
