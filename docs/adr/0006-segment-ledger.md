# 段账本：一个模块回答「目录里落盘了什么」

Status: Accepted (2026-10-12)。行为冻结的重构，不改任何 API 输出。

「哪些 GTID 真的在磁盘上」是 v0.5.57 re-dump P0 背后的问题。重构前至少六处各自回答：`binlog/storage_consistency.go`（`gtidIntervals`）、`tasks/stored_hole.go`（第二份区间类型 `gtidRanges` 加自己的按路径缓存）、`tasks/window.go`（go-mysql `GTIDSet`）、`tasks/legacy_failback.go`（自己 `ReadDir`、自己扫段、go-mysql 集合）、`binlog/open_tail.go` 的 `ReconcileOpenTail`+`UnionGTIDText`，以及 runner 的 `absorbTail` 和观测的 `withOpenTailGTID` 各自把尾部并进集合。`ScanSegmentGTIDs` 有 4 个调用方。

决定：`internal/binlog/ledger.go` 是唯一回答它的深模块，`internal/binlog/gtid_set.go` 的 `GTIDSet` 是唯一的 GTID 区间类型。

## 接口

```go
func LedgerAt(dir string) *Ledger                 // 不读盘；每次问答新建一个
func (l *Ledger) Segments() ([]LedgerSegment, error)      // 按 seq、epoch、名字排序；目录只读一次
func (l *Ledger) Contents(name string) (SegmentContents, error) // 一段的事件、previous-GTIDs、GTIDSet；按 size+mtime 缓存
func (l *Ledger) Stored() GTIDSet                          // 所有段的事务，不含 previous-GTIDs
func (l *Ledger) Holes(start GTIDSet) GTIDSet              // 已存集合内部缺的序号，减去起点集合
func (l *Ledger) Damage(checkpoint, start, flavor string) (StorageProblem, bool) // 第一个损坏段 + Missing/Restart
func (l *Ledger) Tail() (OpenTail, bool)                   // 可续传的打开段：完整事务、半截事务、截断点
func (l *Ledger) ResumeGTID(flavor, seed string) (OpenTail, string, bool) // seed ∪ 尾部已落盘的部分
func (t OpenTail) Durable() []string                       // 尾部已落盘的 GTID 文本：previous-GTIDs，再是完整事务
func ReadSegmentContents(r io.Reader) (SegmentContents, error) // 不在本地目录的字节（对象存储、目录行）
func StoredHoles(stored, start GTIDSet) GTIDSet
```

`GTIDSet` 是 `map[key][]GTIDRange`（key 为小写 uuid 或 `uuid:tag`），始终有序、合并。两个解析入口保留原来的两种语义：`ParseGTIDSet` 走 go-mysql 校验与规范化（原 `parseGTIDRanges`，window、stored hole、legacy failback 用），`ParseGTIDSetLenient` 不依赖 go-mysql、空串和坏串都返回 false（原 `parseGTIDIntervals`，损坏恢复集合用）。

调用方：runner 续传（`reconcileOpenTail` 用 `Damage`+`Tail`，`absorbTail` 用 `Durable`）、`/checkpoint`（`ResumeGTID`）、storage alert 与 `/replay`/`/window` 截断（`Damage`）、stored hole（`Contents`+`GTIDSet`）、legacy failback（`Segments`+`Contents`）、`/window` 与 `start_gtid_set` 回放（`ReadSegmentContents`+`GTIDSet`）。

## 不在范围内

- runner 运行时的已执行集合 `executedGTID` 仍是 go-mysql 集合：它要支持 MariaDB，记录的是流上的事件，不是盘上的内容。`ResumeGTID` 同理仍用 go-mysql 合并，因为 MariaDB 的 `domain-server-seq` 不是区间。
- `tasks/gtid_executed.go` 对用户给的 executed 集合做的判断仍用 go-mysql；只把扫段换成 `ReadSegmentContents`。
- 对象存储里的段：账本只管本地目录；读对象字节的调用方用 `ReadSegmentContents`，不另写扫描。

## 后果

- 删掉 `gtidIntervals`、`gtidRanges`、`segmentRangesCache`、`scanSegmentFile`、`addSegmentGTIDs`、`ReconcileOpenTail`、`UnionGTIDText`、`DetectStorageProblem`、window 里 go-mysql 的 `eventGTIDSet`/`addGTIDEvents`/`parseGTIDText`；`ScanSegmentGTIDs` 不再导出，只有账本调用它。
- 段内容缓存（原来只给 stored hole 用）现在所有本地读都走：同一路径 size 和 mtime 都没变才复用。损坏检测本身（`segmentDamage`）每次仍读文件。
- 行为冻结的证据：`internal/binlog/testdata/ledger/golden.json` 在 main 上用旧函数录制，33 个真实目录用例逐字节一致；`TestGTIDSetMatchesGoMySQL` 在 5000 组随机集合（含 tag）上对照 go-mysql 的 `Contain` 和并集。
