# internal/assurance Module

备份链（ADR 0009 阶段 1，设计见 `docs/design/0009-phase1-backup-chain.md`）。只依赖 `internal/binlog`，不做网络 I/O。

## Files
- `chain.go`: `Inputs`（链顺序、全部目录行、任务目录账本、对象清单快照、checkpoint、起点集合、已记录的换源、tasks 规划器已得出的 I3 子类结论）→ `ChainOf` → `Chain`。`Segments()` 按还原顺序给出每段的目录行、本地是否在与大小、对象是否列举与大小、段内容。`Check(now)` 给出 I1–I7 违反：封闭枚举 `Kind`、涉及的段（第一个是首个违反点）、证据和一句说明；按链顺序排列，同样的输入给出同样的结果。`Objects == nil` 时不判定对象相关的 I6；未配置对象存储时不判定 I7；MariaDB 不做 GTID 判定。
- `gtid.go`: I3 的 GTID 洞判定（段内、相邻源文件、换源处），从 `tasks/window.go` 搬来；P1-2 删除 window.go 的副本。

## Exports
- `Inputs`、`Row`、`Switch`、`Checkpoint`、`Segment`、`Violation`、`Kind`（`AllKinds`）、`ChainOf`、`Chain.Segments`、`Chain.Check`。

## Dependencies
- Upstream: 尚无（P1-2 起 `internal/tasks`）。
- Downstream: `internal/binlog`。

## Update Rule
- 新增或改变一种 `Kind` 的判定时，更新本文件、设计文档 §4，并在 `chain_test.go` 加至少两个构造目录用例。
