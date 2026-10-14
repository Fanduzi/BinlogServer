# internal/assurance Module

备份链（ADR 0009 阶段 1，设计见 `docs/design/0009-phase1-backup-chain.md`）。只依赖 `internal/binlog`，不做网络 I/O。

## Files
- `chain.go`: `Inputs`（链顺序、全部目录行、任务目录账本、对象清单快照、checkpoint、起点集合、已记录的换源、tasks 规划器已得出的 I3 子类结论）→ `ChainOf` → `Chain`。`Segments()` 按还原顺序给出每段的目录行、本地是否在与大小、对象是否列举与大小、段内容。`Check(now)` 给出 I1–I7 违反：封闭枚举 `Kind`、涉及的段（第一个是首个违反点）、证据和一句说明；按链顺序排列，同样的输入给出同样的结果。`Objects == nil` 时不判定对象相关的 I6；未配置对象存储时不判定 I7；MariaDB 不做 GTID 判定。
  - #348 QA 后：`UploadGrace` 未设置（≤0）时取 `DefaultUploadGrace`（15min）。I5 还覆盖 checkpoint 指向链上最后一个同前缀源文件之后的文件、以及 pos 超过所指封存段末尾（本地大小，否则行大小），位置判定不限 MySQL。新增 `I2_ORDER_INVALID`：调用方给的顺序漏掉本地账本里的段（例如整段回切 stint），或同一前缀内序号后退。链上段既无目录行也无本地文件报 `I6_MISSING_ROW`；本地大小与对象大小都不等于行大小时报两条 `I6_SIZE_MISMATCH`；UPLOADED 行 `object_key` 为空报 `I6_MISSING_OBJECT`；内部 GTID 洞逐个区间报告，只跳过邻段判定已在同一后继段上报过的那个。
- `gtid.go`: I3 的 GTID 洞判定（段内、相邻源文件、换源处），从 `tasks/window.go` 搬来，现在是唯一一份，`Check` 和 `WindowOf` 共用。
- `window.go`: `WindowOf(segs, flavor, objectStorage, startGTID)`，`GET /window` 的唯一推导：最早/最晚 UTC 事件时间、MySQL 已存 GTID 集合和缺口（同一源文件名前缀内缺序号、相邻源文件或换源处的 GTID 洞、段内洞、已存序号中间的空缺、校验和 mismatch、配了对象存储时封存分段 `UPLOAD_FAILED`/`LOCAL_ONLY`、读不到）。起点集合算作已存。字节由调用方读好传入（tasks 经 `OpenTaskSegment`，可能来自另一个进程），损坏截断由调用方加。

## Exports
- `Inputs`、`Row`、`Switch`、`Checkpoint`、`Segment`、`Violation`、`Kind`（`AllKinds`）、`ChainOf`、`Chain.Segments`、`Chain.Check`、`DefaultUploadGrace`。

## Dependencies
- Upstream: 尚无（P1-2 起 `internal/tasks`）。
- Downstream: `internal/binlog`。

## Update Rule
- 新增或改变一种 `Kind` 的判定时，更新本文件、设计文档 §4，并在 `chain_test.go` 加至少两个构造目录用例。
