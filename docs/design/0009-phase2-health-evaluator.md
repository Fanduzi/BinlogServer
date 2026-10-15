# ADR 0009 阶段 2 设计：健康评估器 (Health Evaluator)

状态：设计（2026-10-15），未实现。上位决定：[ADR 0009](../adr/0009-backup-assurance.md)「分阶段实施」第 2 行，epic #326。前置：[阶段 1 备份链](0009-phase1-backup-chain.md)（已发布于 v0.5.64–v0.5.66）。本文回答阶段 2 的**类型、生命周期、触发与清除规则、集群语义、迁移**和 PR 切分；不改变 ADR 的任何决定，只在 §12 记录与 ADR 草图的差异。

## 1. 要解决什么

阶段 0/1 让失败**被看见**：`Scheduler.Report` 写 `task_findings`，`assurance.Chain.Check` 给出 I1–I7。但「谁负责关闭一条 finding」今天没有答案：

| 现状 | 后果 |
|---|---|
| 17 种 `FindingKind`，`Report`/`Resolve` 散落在 14 个调用点（scheduler、runner、metrics、storage_health、watchdog） | 每加一种 kind 就要再找一个「成功路径」去 `Resolve`；漏了就永远 open |
| `SEGMENT_UNREADABLE`、`SEGMENT_OVERLAP` 只在 `storageHealthOf` 里 `Report`，从不 `Resolve` | 条件清除后告警清了，finding 仍 OPEN（#370/#372 QA 已知缺口） |
| `Resolve` 只对「本进程 book 里 open 的 key」生效 | 重启或换 owner 后，存储里的 open 行没人能关（#366：STOPPED 任务的 `DUMP_STALLED` 挂 40s+） |
| `Check` 的 I2/I3/I5/I6/I7 违反没有 finding | 单源 GTID 洞只在 `/window`（#352）；对象丢失、上传迟到无告警 |
| ADR 决定 F「没有数据 = 不健康」没有实现 | 评估停了与「一切正常」在指标上无法区分 |
| 没有通知 | 用户必须盯着 Console |

阶段 2 的单一目标：**每一条 finding 的打开与关闭都由一个评估器根据「此刻的事实」决定**；API、指标、Console、通知读同一份结果。

## 2. 核心决定（摘要）

1. **一个评估器，期望状态调和（desired-state reconcile）**。评估器对一个任务算出「此刻应当 open 的 finding 集合」`Desired`，与已存的 open 集合比较，差集产生 `Open`/`Close` 转移。没有任何代码路径直接写 finding 行。
2. **两类输入，同一个出口**。条件型（链、任务状态、lease）由评估器自己读；事件型（一次写失败、一次读超时）由热路径调用 `Signal`（非阻塞，只记事实，不写 finding），评估器把事实变成 finding。现有 `Report`/`Resolve` 降为 `Signal` 的包装，最后在 P2-4 改为包内私有，并由 lintguard 禁止新调用。
3. **作用域（scope）防误关**。一次评估只关闭它**本轮有完整输入**的 kind；输入不可得（对象清单未列举、目录读失败、meta 不可读）时这些 kind 的状态是 `Unknown`，保持原样并让任务健康变为 `UNKNOWN`。
4. **触发 = 标脏 + 合并**，周期 sweep 只是兜底：所有触发点只把任务标脏并附原因，单 worker 按任务合并执行；sweep 每 60s 把本节点负责的任务全部标脏。触发与 sweep 走同一个 `evaluate(task)`，不存在两套逻辑。
5. **幂等键 (task_id, kind, subject)** 不变；转移带单调 `generation`，存储写入是条件写（新 generation 或同 generation 才覆盖），重复、乱序、重放都收敛到同一行。
6. **每种 kind 绑定一条清除规则**（§6 表）：kind 注册表是封闭的，注册项里写明 invariant、severity、scope 和 `ClearRule`；没有清除规则的 kind 不能注册（编译期表 + 单测）。
7. **集群：评估权跟随任务 lease**。只有持有 `(owner, epoch)` 的节点评估该任务，finding 写入带 `owner_epoch` 条件；换 owner 后新 owner 的第一轮评估接管所有已存 open 行（含旧 owner 的）。无人持有的任务（STOPPED、未分配）由持有 `_health` 集群 lease 的一个节点评估。
8. **健康是 finding 的视图**：`task_health` 表 + `binlog_server_task_healthy{task_id}` 每个任务总有序列；评估超过 3 个周期没跑 = `UNKNOWN` = 不健康。
9. **通知 = 转移的 outbox**：只在转移（打开、升级、关闭）时通知，带去抖；单节点（`_notify` lease）至少一次投递 webhook，按 `(key, generation, transition)` 去重。
10. **迁移只加不改**：`000009` 给 `task_findings` 加可空列、新建 `task_health` 和 `finding_transitions`；`minRequiredSchemaVersion` 保持 6；缺表缺列时退回 v0.5.66 行为并报 `HEALTH_STORE_DEGRADED`。

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

- **`internal/health` 是深模块**：输入是「一个任务此刻的事实」，输出是期望 finding 集合、转移列表和健康结论。不做 I/O，不 import `tasks`/`meta`。删掉它，清除规则会重新散回 14 个调用点——这正是要消灭的状态。
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
    ScopeSignals      Scope = "signals"       // 热路径事实（总是 complete）
    ScopeStore        Scope = "store"         // 元数据库能力（schema、只读）
    ScopeVerify       Scope = "verify"        // 阶段 3 巡检器，阶段 2 预留
)

type ClearRule struct {
    Invariant string // "I4" / "S2" / "lease" ...，给人和告警页看
    // Holds 在本轮事实下判断「该条件已不成立」。Desired 不含该 key 且 Holds 为真才关闭；
    // 对条件型 kind，Holds 通常就是「Desired 里没有」，对信号型 kind 是「成功事实晚于失败事实」。
    Holds func(f *Facts, key Key) bool
    Text  string // 一句话：什么时候会自动关闭（API 的 clears_when）
    // HoldDown: 条件消失后至少持续这么久才关闭（去抖），默认 0。
    HoldDown time.Duration
}

type KindSpec struct {
    Kind     Kind
    Severity Severity
    Scope    Scope
    Clear    ClearRule
    // Derive 从事实算出该 kind 此刻应当 open 的实例（subject → detail/evidence）。
    Derive func(f *Facts) []Instance
    // Segments 为真表示 subject 是段名，阶段 4/5 可按段与时间窗求交。
    SegmentScoped bool
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
}

// Facts 是一个任务在一个时刻的全部输入。字段为 nil 表示该来源本轮不可得。
type Facts struct {
    TaskID     string
    Now        time.Time
    Task       *TaskFacts          // 状态、desired、owner、epoch、run 是否在跑、进入当前状态的时间
    Chain      *ChainFacts         // assurance.Check 与 Health 的结果；nil = 目录或账本不可读
    Objects    bool                // Chain 里的对象判定是否基于已列举清单
    Signals    map[SignalKey]SignalState
    Store      *StoreFacts         // schema 版本、可选列/表是否存在、是否只读
    Complete   map[Scope]bool
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
    LastFailure time.Time
    LastSuccess time.Time
    Failures    int64
    Detail      string
}

type Transition struct {
    Key
    Op         Op       // OPEN | UPDATE | ESCALATE | CLOSE
    Generation int64
    At         time.Time
    Instance   Instance // CLOSE 时为最后一次 open 的内容
}

// Reconcile 是唯一的生命周期函数：纯、幂等。
//   - Desired 有、open 没有          → OPEN
//   - 两边都有，severity 升高        → ESCALATE；detail/evidence 变化 → UPDATE（不通知）
//   - open 有、Desired 没有、scope Complete、Clear.Holds 且已过 HoldDown → CLOSE
//   - scope 不 Complete              → 不动（并计入 Health.Unknown）
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
    Invariant   string    `json:"invariant"`      // 由 Registry 填，不存库
    ClearsWhen  string    `json:"clears_when"`    // 由 Registry 填，不存库
    AckedBy     string    `json:"acked_by,omitempty"`
    AckedAt     *time.Time `json:"acked_at,omitempty"`
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
    // ApplyTransitions 在一个事务里：条件 upsert finding 行、追加 finding_transitions、写 task_health。
    // 条件：stored.owner_epoch <= t.OwnerEpoch AND stored.generation <= t.Generation。
    // 返回 ErrFenced 表示更新的 owner 已写过，本节点应停止评估该任务。
    ApplyTransitions(ctx context.Context, taskID string, ownerEpoch int64, ts []health.Transition, h health.TaskHealth) error
    ListFindings(ctx context.Context, q FindingQuery) ([]health.Finding, error)
    ListHealth(ctx context.Context) ([]health.TaskHealth, error)
    PendingNotifications(ctx context.Context, after int64, limit int) ([]TransitionRow, error)
    AckNotification(ctx context.Context, id int64) error
}
var ErrFenced = errors.New("health write fenced by a newer owner epoch")
```

## 5. 生命周期与触发

### 5.1 一次评估

```
evaluate(task):
  1. 读 lease 视图：本节点不是 (owner, epoch) 持有者且不是 _health 持有者（对无主任务）→ 跳过
  2. 收集 Facts（Task、Chain=ChainOf(Inputs).Check/Health、Signals 快照、Store 能力），标记 Complete
  3. open := store 里该任务的 open 行（进程内缓存，写成功才更新；缓存缺失时读库）
  4. ts, _ := health.Reconcile(open, facts)；h := health.HealthOf(...)
  5. store.ApplyTransitions(epoch, ts, h)；ErrFenced → 丢弃本轮并停止评估该任务直到下一次 LEASE_ACQUIRED
  6. 写失败 → 转移留在进程内 pending（沿用 findingBook 的合并与重试），健康标 UNKNOWN(store)
```

步骤 2 的读取（账本有 size+mtime 缓存）每轮 O(段数)；`Chain.Check` 在 1500 段的任务上 < 50ms（P2-3 有基准测试作为 QA 条件）。

### 5.2 触发点

| 触发 | 来源 | Reason |
|---|---|---|
| 段封存 / 封存事实写入 | `sealed_upload.go`、runner seal | SEAL |
| 上传成功、失败、回读校验 | `sealed_upload.go`、`scheduler_retry_upload.go` | UPLOAD |
| 保留期删除本地或对象 | retention | RETENTION |
| 任务状态转移提交 | `commitTransitionLocked` | TRANSITION |
| lease 获取 / 丢失 / 换 epoch | `scheduler_cluster_lease.go` | LEASE_ACQUIRED / LEASE_LOST |
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
- `generation`：每个任务一个单调计数（存于 `task_health.generation`，进程内缓存，接管时从库读 max+1）。转移带 generation；条件写保证旧转移不覆盖新转移。
- 重复 OPEN 只更新 `last_seen`、`evaluated_at`、`count`；重开已关闭的行：`resolved_at=NULL`，`first_seen` 保留最早值，`finding_transitions` 记一条 OPEN（历史不丢，与 000007 语义一致）。
- CLOSE 写 `resolved_at=now`；对已关闭的行再 CLOSE 是空操作（条件 `resolved_at IS NULL`）。
- 信号型 kind 的 `Count` 是失败次数（`SignalState.Failures`），条件型 kind 的 `Count` 是确认 open 的评估轮数。

## 6. Kind → 清除规则

所有已发布的 17 种 kind 加阶段 2 新增的链 kind。Scope 决定何时有资格关闭。

| Kind | Sev | Scope | 打开条件（Derive） | 清除规则（invariant） |
|---|---|---|---|---|
| `SEGMENT_UNREADABLE` | C | chain.local | `Check` 有 `I1_UNREADABLE`，subject=段 | I1：该段可读，或已不在链上（被挪走/替换） |
| `SEGMENT_OVERLAP` | C | chain.local | `Health()`/`Check` 有 `I4_OVERLAP`，subject=后一副本 | I4：同一源文件在链上只剩一个封存副本，且无 GTID/位点相交 |
| `SEGMENT_DAMAGED`（新，原 storage_alert 损坏） | C | chain.local | `I1_REGRESSION` | I1：位点不倒退 |
| `CHAIN_INDEX_GAP`（新） | C | chain.local | `I2_INDEX_GAP` / `I2_SWITCH_UNRECORDED` / `I2_ORDER_INVALID` | I2：序号连续且每次换源有记录 |
| `GTID_HOLE`（新，**#352**） | C | chain.local | `I3_GTID_HOLE`（含单源任务内部洞）/ `I3_STORED_HOLE`，subject=洞后首段 | I3：`B ∪ Stored` 在该点连续（源补齐或洞被确认为源侧跳号并经 ack 豁免，见 §7.4） |
| `LEGACY_FAILBACK`（新） | C | chain.local | `I3_LEGACY_FAILBACK` | I3：规划器不再给出该结论 |
| `CHECKPOINT_AHEAD`（新） | C | chain.local | `I5_CHECKPOINT_AHEAD` | I5：C ≤ 链截断点 |
| `SEGMENT_MISSING`（新） | C | chain.local / chain.objects | `I6_MISSING_LOCAL` / `I6_MISSING_ROW` / `I6_MISSING_OBJECT`(objects) / `I6_SIZE_MISMATCH` | I6：行、本地、对象三处一致；保留期删除标记（**#341**）视为一致 |
| `ORPHAN_ROW`（新） | W | chain.objects | `I6_ORPHAN_ROW` | I6：行有实物，或行带保留期删除标记 |
| `UPLOAD_LATE`（新） | W→C(>24h) | chain.local | `I7_UPLOAD_LATE` | I7：`UPLOADED+match` |
| `UPLOAD_UNVERIFIED` | C | chain.local + signals | 信号失败，或 `I7_UNVERIFIED` | I7：`checksum=match` |
| `UPLOAD_RECORD_FAILED` | C | signals | 信号 | S8：同一段后续上传记录写成功 |
| `DUMP_STALLED` | W | signals + task.state | 信号失败且任务 RUNNING 且本节点有 run | S12：之后收到事件/心跳；**或任务不再 RUNNING / run 不在本节点（#366）** |
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
| `HEALTH_STALE`（新，集群级） | C | task.state | 某任务 `task_health.evaluated_at` 超过 3×sweep 未更新且该任务应有评估者 | 该任务被评估 |
| `HEALTH_STORE_DEGRADED`（新，集群级） | W | store | `000009` 缺失或 ApplyTransitions 连续失败 | 写成功 |

规则：
- 一个 `assurance.Kind` 只映射到一个 finding kind（多对一），映射表在 `health/registry.go`，单测断言 `assurance.AllKinds` 每一项都有映射——以后 assurance 加 kind 而不配 finding 会失败。
- `Chain.Health()` 的结论仍然决定 `/replay` 截断和 `storage_alert`（行为不变）；finding 只是它的持久化视图，二者从同一个 `Chain` 对象取，保证一致（ADR I6「三处一致」的告警面）。
- 段级 kind 的 subject 是段名。段被挪走后它不在链上 → Derive 不产生 → 关闭。这正是今天缺的评估器。

## 7. 对外表面

### 7.1 API

| 路由 | 内容 |
|---|---|
| `GET /api/alerts?state=open|all&severity=&kind=&task_id=&limit=` | 跨任务 finding（含 `_cluster`），每条带 `invariant`、`clears_when`、`segments`、`evaluated_at`、`acked_*`；响应头带 `X-Health-Evaluated-At` |
| `GET /api/tasks/{id}/health` | `TaskHealth` + 该任务 open findings + 不完整 scope |
| `GET /api/tasks/{id}/findings` | 不变（兼容），新增字段只增不删 |
| `GET /api/findings` | 不变，标记为 `/api/alerts` 的别名，一个版本后在文档里废弃 |
| `GET /api/tasks`、`/api/dashboard` | `open_findings` 保留；新增 `health`（status、evaluated_at） |
| `POST /api/alerts/ack` `{task_id,kind,subject,until?}` | 确认：不关闭、不改变健康，只抑制重复通知；`until` 到期自动失效。I3 源侧跳号的豁免是 ack 的一种（带 evidence 快照，evidence 变化时失效） |
| `POST /api/tasks/{id}/health/evaluate` | 立即评估一次，返回结果（受同一 lease 规则约束；非 owner 返回 409 + owner） |

### 7.2 指标

- 保留：`binlog_server_finding_open{task_id,kind,severity}`、`findings_reported_total`、`finding_sink_*`。
- 新增：
  - `binlog_server_task_healthy{task_id}` 0/1，**每个任务总有序列**；UNKNOWN 输出 0，`binlog_server_task_health_status{task_id,status}` 给出四态之一。
  - `binlog_server_task_health_evaluated_ts{task_id}`。
  - `binlog_server_finding_transitions_total{kind,op}`。
  - `binlog_server_health_eval_duration_seconds`（histogram）、`binlog_server_health_eval_lag_seconds{reason}`、`binlog_server_health_eval_errors_total{scope}`。
  - `binlog_server_notify_pending`、`binlog_server_notify_failures_total`。
- 指标从 `task_health` 表读（每次 scrape 一条查询，2s 超时），叠加本进程未写成功的结果；与今天 finding 指标的「store + 进程内」合并方式相同。
- `docs/` 提供推荐 Prometheus 规则：`task_healthy == 0 for 2m`、`time() - task_health_evaluated_ts > 300`。

### 7.3 Console 告警页

- 新页面 `/ui/#/alerts`：按 severity → 任务 → kind 分组；每条显示 detail、「何时自动关闭」、涉及段、首次/最近、确认状态；一键跳到任务的窗口或回放视图。筛选 open / 最近 7 天已关闭。
- 任务列表与任务详情加健康徽标（HEALTHY / DEGRADED / UNHEALTHY / UNKNOWN），UNKNOWN 显示「评估已 N 分钟未运行」。
- 告警页读 `/api/alerts` 与 `/api/tasks/{id}/health`，不自己推导。事件时间线里的 `FINDING_REPORTED/RESOLVED` 按事件时间排序（顺带满足 #368 在告警相关事件上的要求，见 §10）。

### 7.4 通知

- `finding_transitions` 是 outbox：每个 OPEN / ESCALATE / CLOSE 一行（UPDATE 不写），与 finding 行同一事务写入。
- 通知器：持有 `_notify` 集群 lease 的节点（单机模式即本进程）按 id 递增读 outbox，投递到配置的 sink；阶段 2 只做 **webhook**（JSON，含 key、op、severity、detail、clears_when、链接），格式兼容 Alertmanager webhook 接收端；其他通道（邮件、IM）通过 webhook 桥接，不进二进制。
- 至少一次：投递成功才前移游标（`notify_cursor` 存在 `task_health` 同库的单行表里）；接收端用 `X-BinlogServer-Delivery: <task>/<kind>/<subject>/<generation>/<op>` 去重。
- 去抖：OPEN 默认立即（CRITICAL）或持续 1 个 sweep（WARNING）后通知；CLOSE 受 `ClearRule.HoldDown`；1 小时内同 key 翻转 >3 次合并为一条「flapping」通知。已 ack 的 key 不再通知 OPEN/ESCALATE，仍通知 CLOSE。
- 未配置 webhook 时 outbox 仍写（保留 30 天，sweep 清理），告警页可回看历史。

## 8. 集群模式与 lease 栅栏

基于 [0346](0346-dump-lease-fence.md) 的 lease（`(owner, epoch)`，CAS Renew）：

1. **评估权**：任务被某节点持有（RUNNING/STARTING/STOPPING 且 lease 未过期）→ 只有该节点评估，`ownerEpoch = lease.epoch`。理由：链的本地字节只在 owner 节点上（阶段 1 §8 已知），Signals 也只在 owner 进程里产生。
2. **无主任务**（STOPPED、无 lease、lease 过期且无人接手）：持有集群 lease `_health`（复用 lease 表，task id `_health`）的节点评估，`ownerEpoch = _health.epoch`，scope 仅 `task.state`、`store`、以及本地能读到目录时的 `chain.*`（共享存储部署）；读不到目录时 `chain.*` 为不完整，链 finding 保持原样（不误关），健康为 UNKNOWN(chain)——一个停掉的任务不该被报「健康」。
3. **栅栏写**：`ApplyTransitions` 的每条 upsert 带 `WHERE owner_epoch <= ? AND generation <= ?`（MySQL 用 `INSERT … ON DUPLICATE KEY UPDATE col = IF(VALUES(owner_epoch) >= owner_epoch, VALUES(col), col)`），`task_health` 同理。影响行数显示被拒 → `ErrFenced`。栅栏只依赖库里单调的 epoch，不依赖时钟，主机冻结（#360 的场景）后醒来的旧 owner 写不进去。
4. **接管**：`LEASE_ACQUIRED` → 读该任务全部 open 行（不论谁写的）作为 `open`，下一轮评估 generation = max+1；旧 owner 的信号型 finding（例如 `DUMP_STALLED`）因为新 owner 的 Signals 里没有失败且任务状态/run 归属已变，按清除规则关闭——修复 #366。
5. **丢失**：`LEASE_LOST` → 立即从调度中移除该任务，丢弃 pending 转移（不重试旧 epoch 的写），进程内缓存清空。
6. **集群级 finding**（`_cluster`）：subject 带节点 id，由产生它的节点评估；节点消失后由 `_health` 持有者在节点心跳（`worker_heartbeats`）过期 3 个周期后关闭并记 `NODE_GONE` detail。
7. **HEALTH_STALE**：`_health` 持有者每个 sweep 检查所有任务的 `evaluated_at`，补上「评估者死了但 lease 还没过期」的窗口。

单机模式（无 meta DSN）：lease 视图恒为本进程，`.findings.json` 扩展为同时存 `task_health` 与 outbox（原子写不变）。

## 9. 迁移与兼容

`000009_health.up.sql`（只加，不改已有列语义）：

```sql
ALTER TABLE task_findings
  ADD COLUMN evidence TEXT NULL,
  ADD COLUMN segments JSON NULL,
  ADD COLUMN generation BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN owner_epoch BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN evaluated_at DATETIME(6) NULL,
  ADD COLUMN acked_by VARCHAR(128) NULL,
  ADD COLUMN acked_at DATETIME(6) NULL,
  ADD COLUMN ack_until DATETIME(6) NULL;

CREATE TABLE task_health (
  task_id VARCHAR(64) PRIMARY KEY,
  status VARCHAR(16) NOT NULL,
  reasons JSON NULL,
  evaluated_at DATETIME(6) NOT NULL,
  generation BIGINT NOT NULL,
  owner VARCHAR(128) NOT NULL DEFAULT '',
  owner_epoch BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE finding_transitions (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  task_id VARCHAR(64) NOT NULL, kind VARCHAR(64) NOT NULL, subject VARCHAR(255) NOT NULL,
  op VARCHAR(16) NOT NULL, generation BIGINT NOT NULL, severity VARCHAR(16) NOT NULL,
  detail TEXT NULL, at DATETIME(6) NOT NULL,
  INDEX idx_ft_key (task_id, kind, subject), INDEX idx_ft_at (at)
);
CREATE TABLE notify_cursor (id TINYINT PRIMARY KEY, last_id BIGINT NOT NULL);
```

兼容规则（与 `000007`/`000008` 同一套路）：
- `minRequiredSchemaVersion` 保持 6。缺 `task_findings` → 今天的内存模式；缺 `000009` 的列/表（1054/1146）→ 评估器仍运行、仍做调和，但写入回退到 v0.5.66 的 upsert（无栅栏、无 outbox），报 `HEALTH_STORE_DEGRADED`，每个 sweep 重新探测（**不**像 #353 那样记到重启）。集群模式下缺 `000009` 时在启动日志与 `/api/alerts` 显式说明「栅栏不可用」。
- 已存的 v0.5.66 open 行：`generation=0, owner_epoch=0`，任何新转移都能覆盖；评估器第一轮把它们当作 `open` 调和——条件已清除的 `SEGMENT_*` 在升级后第一轮关闭（发布说明写明「升级后会批量关闭过期告警」）。
- 旧二进制读写跑过 `000009` 的库：新列有默认值或可空，旧 upsert 语句不涉及它们；旧二进制写的行 `owner_epoch` 不变，会被新二进制的下一轮覆盖。混跑集群（滚动升级）期间旧节点无栅栏，发布说明要求滚动升级窗口内不做手工迁移任务。
- `down`：删三张表和新增列；行为回到 v0.5.66。
- Go API：`tasks.Finding`/`FindingKind` 保留为别名到 v0.6.0；`Scheduler.Report/Resolve` 在 P2-4 后变为 `Signal` 的包装并标 Deprecated，lintguard 禁止 `internal/tasks` 外新增调用。

## 10. 吸收哪些 P3

| Issue | 处理 | 落在 |
|---|---|---|
| #352 单源 GTID 洞无 finding | **吸收**：`GTID_HOLE` 由 `I3_GTID_HOLE`/内部洞导出，不分单源多源 | P2-5 |
| #353 000008 down/up 后封存事实写 NULL 无日志 | **吸收**：`SEAL_FACTS_ABSENT` + 每 sweep 重新探测列（Store scope） | P2-5 |
| #366 STOPPED 任务的 DUMP_STALLED 挂 40s+ | **吸收**：清除规则含「任务不 RUNNING / run 不在本节点」，接管时调和 | P2-4 / P2-6 |
| #341 上传模式保留期删除不写 RETENTION_REMOVED | **吸收**：I6 需要保留期删除标记（阶段 1 §4.1 已排到阶段 2）；同一 PR 写事件、失败走 `RETENTION_RECORD_FAILED` 信号 | P2-5 |
| #342 lintguard 覆盖面 | **部分吸收**：新增「`Report/Resolve` 只能在 evaluator 内」规则时一并识别 logger 实例方法；其余条目不在本阶段 | P2-4 |
| #368 事件时间线按写入时间排序 | **部分吸收**：告警页与时间线中的 finding 事件按事件时间排；时间线整体排序随 Console 修 | P2-8 |
| #360 lease 丢失后 STOPPED 覆盖新 owner 行 | **不吸收**，但 P2-2 的「epoch 条件写」原语（`meta.fencedExec`）可供 #360 的任务行写复用；建议紧随 P2-6 单独修 | — |
| #343 只读 meta 下 RUNNING 显示 1–3s | 不吸收（任务状态，不是健康）；健康会在首次写失败信号后 1 轮内变 UNHEALTHY，缩小但不消除该窗口 | — |
| #345 DUMP_STALLED 48–49s 才报 | 不吸收（runner 检测阈值）；评估器不改变检测时机 | — |
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

每个 PR 第一提交是失败测试；每个 PR 独立过 QA 才合并；P2-1 起每个 PR 都不改变 `/window`、`/replay`、`storage_alert` 的字节输出（沿用阶段 1 golden）。

| PR | 内容 | 第一提交（红） | 不做 |
|---|---|---|---|
| P2-0 | 本设计文档 | — | — |
| P2-1 | `internal/health`：`Kind`/`KindSpec`/`Registry`（现有 17 种 + 新 kind，Derive 先只实现现有两种 SEGMENT_*）、`Facts`、`Reconcile`、`Desire`、`HealthOf`；`tasks` 类型别名 | 表驱动：OPEN/UPDATE/ESCALATE/CLOSE/不完整 scope 不关/HoldDown/幂等（对输出再调和得空）；`assurance.AllKinds` 每项有映射 | 不接线 |
| P2-2 | 迁移 `000009` + `meta` 的 `ApplyTransitions`（事务、epoch/generation 条件写、outbox）、`ListHealth`；缺表缺列回退与每轮重探；standalone 文件存储扩展 | 「旧 epoch 写不覆盖新 epoch」「缺 000009 时回退且不记到重启」测试红 | 不接线评估器 |
| P2-3 | `tasks.Evaluator`：脏集合、per-task 串行、sweep、目录指纹、触发点接线（SEAL/UPLOAD/RETENTION/TRANSITION/CATALOG/STARTUP/API）；接管 `SEGMENT_UNREADABLE`/`SEGMENT_OVERLAP`，`storageHealthOf` 不再 `Report` | 「挪走重复副本后 ≤1 sweep finding 关闭」「修好不可读段后关闭」红 | 其他 kind |
| P2-4 | 所有信号型 kind 改走 `Signal`；`TASK_TRANSITION_STALLED`、`DUMP_STALLED` 改为评估器 Derive；`Report/Resolve` 私有化 + lintguard 规则（含 #342 的 logger 方法） | 每种信号 kind：失败→OPEN、成功→CLOSE、重启后由存储行调和关闭；#366 场景（单机版） | 新链 kind |
| P2-5 | 新链 kind：`SEGMENT_DAMAGED`、`CHAIN_INDEX_GAP`、`GTID_HOLE`(#352)、`LEGACY_FAILBACK`、`CHECKPOINT_AHEAD`、`SEGMENT_MISSING`、`ORPHAN_ROW`、`UPLOAD_LATE`、`UPLOAD_UNVERIFIED` 条件化；保留期删除标记 + 事件(#341)；`SEAL_FACTS_ABSENT`(#353) | 每种 kind ≥2 个构造目录用例：打开一次、修复后关闭一次 | 集群 |
| P2-6 | 集群：评估权跟随 lease、`_health` lease 评估无主任务、`ErrFenced` 处理、接管调和、`HEALTH_STALE`、节点消失清理 | 两节点：旧 owner 冻结后写被拒；接管后旧 `DUMP_STALLED` 关闭（#366 集群版） | — |
| P2-7 | API（`/api/alerts`、`/tasks/{id}/health`、ack、evaluate）+ 指标（`task_healthy` 总有序列等）+ 推荐规则文件；swagger | 「新建任务即有 task_healthy 序列」「评估停 3 周期 → UNKNOWN=0」红 | Console |
| P2-8 | Console 告警页、健康徽标、finding 事件按事件时间排序 | UI 测试：告警页数据与 `/api/alerts` 一致 | — |
| P2-9 | 通知器：`_notify` lease、outbox 游标、webhook、去抖、flapping、ack 抑制；配置项与文档 | 「投递失败重试且不丢」「切换 `_notify` 持有者不重复也不丢（按 delivery id）」红 | 非 webhook 通道 |

依赖：P2-1 → P2-2 → P2-3 → {P2-4, P2-5} → P2-6 → P2-7 → {P2-8, P2-9}。阶段 2 完成后发一个版本（建议 v0.6.0，同时把 schema 下限按 000007 注释的计划提到 7，**需 Adrian 决定**，见开放问题）。

## 14. QA 计划（每个 PR）

通用：`go vet`、`go test ./...`、阶段 1 golden（window/replay/health）逐字节一致、e2e quick 全绿；MySQL 8.0 + MariaDB 11.4（链相关 PR 加 10.11）。

| PR | QA 通过条件 |
|---|---|
| P2-1 | 单测覆盖 Reconcile 全部分支；随机性质测试：任意 (open, facts) 序列重放两次结果相同、对输出再调和为空；不完整 scope 下从不产生 CLOSE |
| P2-2 | 在 schema 6/7/8/9 上各跑一次：读写正常、回退路径报 `HEALTH_STORE_DEGRADED`；在线 `migrate down 1` / `up` 后 1 个 sweep 内恢复栅栏写（#353 同型场景）；并发两个写者不同 epoch，库里只留高 epoch；`down` 后 v0.5.66 二进制可读写 |
| P2-3 | 复现 #372 QA：零填充/完整副本 → OPEN；挪走副本 → finding 在 ≤60s（1 sweep）关闭并有 `finding_transitions` CLOSE；改坏一段 → `SEGMENT_UNREADABLE` OPEN，恢复 → CLOSE；重启后状态一致；1500 段任务单轮评估 <50ms（基准）；`storage_alert` 字节不变 |
| P2-4 | 阶段 0 的 S1–S15 注入测试全部改为断言「失败→OPEN，恢复→CLOSE」，且重启进程后由新进程关闭；meta 只读 60s：`METADATA_WRITE_BLOCKED` OPEN→恢复后 CLOSE；`DUMP_STALLED` 在 Stop 后 ≤1 sweep 关闭；lintguard 在 `internal/tasks` 外调用 `Report` 时 CI 失败 |
| P2-5 | #352 复现（`gtid_next` 跳号 363–381，单源）→ `GTID_HOLE` OPEN；MinIO 删对象（Objects 已列举）→ `SEGMENT_MISSING`，未列举时**不**关闭也不打开；上传关闭 >grace → `UPLOAD_LATE`；保留期删除后无 `ORPHAN_ROW` 且有 `RETENTION_REMOVED` 事件；#353 复现后 1 sweep 内 `SEAL_FACTS_ABSENT` OPEN，`up` 后关闭且封存事实恢复写入；ADR 0006/0007 的 47 个目录用例无误报 |
| P2-6 | 3 节点：kill -9 owner、SIGSTOP owner 2min（#360 场景）后恢复——旧 owner 的 finding 写全部 `ErrFenced`，库里无旧 epoch 覆盖；接管后 ≤1 sweep 调和完成；STOPPED 任务由唯一 `_health` 节点评估；杀 `_health` 持有者后另一节点接手；`HEALTH_STALE` 在评估者冻结时出现、恢复后关闭 |
| P2-7 | 每个健康条件单独破坏一次，`/api/alerts`、`task_healthy`、`task_health_status` 在 1 个评估周期内变化（ADR 阶段 2 QA 条件）；`task_healthy` 对每个任务总有序列（含 STOPPED、从未启动）；停掉评估 3 周期 → UNKNOWN；ack 不改变健康；meta 不可用时 scrape 2s 内返回 |
| P2-8 | Console 告警页与 `/api/alerts` 数据一致（20 个构造 finding）；徽标四态；关闭的 finding 7 天内可见；浏览器控制台无错误；中英文 |
| P2-9 | webhook 接收端停 10 分钟后恢复：全部转移按序到达、按 delivery id 无重复；`_notify` 持有者切换时不丢；flapping 合并；ack 抑制 OPEN 但不抑制 CLOSE；未配置 webhook 时 outbox 30 天清理 |

阶段 2 收尾：BinlogServerQA 在 tip 上做全量回归（DBA 还原、failback、只读 meta、kill -9 + adopt、升级自 v0.5.66：已存过期 `SEGMENT_*` 在第一轮关闭）后发版。

## 15. 开放问题

1. **schema 下限**：阶段 2 是否随 v0.6.0 把 `minRequiredSchemaVersion` 提到 7（000007 注释的计划）甚至 9？提到 9 可去掉无栅栏回退路径，降低集群混跑风险。
2. **无主任务的链评估**：非共享存储部署里，STOPPED 任务的目录只在最后的 owner 节点上。本设计让它 UNKNOWN(chain)；是否接受「停掉的任务显示 UNKNOWN」，还是让最后 owner 在没有 lease 时也评估其本地目录（需要额外的「目录持有者」概念）？
3. **GTID_HOLE 的豁免**：源侧主动跳号（#352 的 `gtid_next`）是否允许通过 ack 永久豁免，还是只允许临时 ack（`until`）？
4. **通知通道**：阶段 2 只做 webhook（Alertmanager 兼容）是否足够，还是需要内建邮件/钉钉/飞书？
5. **UPLOAD_LATE 升级阈值** 24h 是否合适，是否做成配置。
