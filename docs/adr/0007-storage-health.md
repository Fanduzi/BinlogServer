# 存储健康：一个检测器，一个结论

Status: Accepted (2026-10-12)。行为冻结的重构，不改任何 API 输出。

「这个任务目录还能不能放心回放」重构前由三段串起来的检测回答：`storage_alert.go` 的 `storageProblem` 先问 `binlog.Ledger.Damage`，没有再调 `legacyFailbackFor`（自己再建一次账本、自己 `ReadDir` 算缓存签名），再没有调 `storedHoleFor`（又建一次账本）。顺序只写在这一个函数里；`window.go` 和 `replayDamageWarning` 靠 `Detail` 的前缀（`legacy failback:`、`stored gtid hole:`）倒推是哪一类问题。同一次请求最多三次列目录。

决定：`internal/tasks/storage_health.go` 是唯一的存储检测器，给一个带类别的结论。

## 接口

```go
type StorageHealthKind int // StorageOK, StorageDamaged, StorageLegacyFailback, StorageStoredHole
type StorageHealth struct { Kind StorageHealthKind; Problem binlog.StorageProblem }

func (s *Scheduler) storageHealth(task Task, checkpointGTID string) StorageHealth // 一次问答一个账本
func (h StorageHealth) Found() bool
func (h StorageHealth) ReplayWarning() string                       // /replay 的 warning，按类别
func (h StorageHealth) replayCut(files, chosen []BinlogFile) []BinlogFile // 回放与窗口的截断点
```

- 类别按顺序检查，第一个命中的胜出：损坏（`Ledger.Damage`）→ 旧版回切（`legacyFailbackIn`）→ 已存缺口（`storedHoleIn`）。
- `checkpointGTID` 只填损坏类的 `Missing`，从不改变类别；`/window` 和回放传空串。
- 三类都共用同一个 `binlog.Ledger`：目录列一次，段内容缓存共享。旧版回切的缓存签名改用账本的分段列表，不再单独 `ReadDir`。
- 调用方：`AttachStorageAlert`（list/get，损坏类改写 `FAILED` 的旧 `last_error`，旧版回切给 `valid_segments`）、`SelectReplayFilesFor`、`ReplayWarning`、`RecoveryWindow`（按 `Kind` 判断已存缺口，不再看 `Detail` 前缀）。
- runner 的 Start 拒绝仍只看损坏：新增 `Ledger.IsDamaged()` 代替 `Damage("", "", flavor)` 的空参数调用。旧版回切由 Start 修复；已存缺口只读。

## 不在范围内

- `listTaskBinlogFilesOnDisk` 仍是已存缺口和旧版回切 `validBefore` 用的另一份目录清单（带目录行信息），没有并进账本。
- `segmentDamage` 仍对每个文件读两遍（stray rotate 与位点倒退各一遍）；为冻结行为没有合并。
- `legacyFailbackFor` 保留为 Start 修复路径的入口（包装 `legacyFailbackIn`）。

## 后果

- 删掉 `storageProblem`、`replayDamageWarning`；`storedHoleFor` 改名 `storedHoleIn`，`planStoredHole`/`planLegacyFailback` 接收账本而不是目录。
- 行为冻结的证据：golden 回归（14 个真实目录用例，含损坏、旧版回切、已存缺口及损坏优先于缺口）在 main 与本分支上逐字节一致；e2e quick 与账本相关场景、旧版回切升级脚本三种模式结果一致。`storage_health_test.go` 覆盖类别、优先级和三种 warning 文本。
