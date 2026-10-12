# 复制一次运行：五个有名字的阶段

Status: Accepted (2026-10-12)。行为冻结的重构，不改任何 API 输出、日志文本或错误码。

`MySQLRunner.run` 重构前是 843 行的一个函数：二十多个局部变量（`currentFile`、`currentPos`、`file`、`writer`、`start`、`gtidFallback`、`atTip`、`syncer`、`activeSyncer`……）被七个闭包（`appendAndPersist`、`persistRaw`、`checkpointAt`、`sealCurrent`、`handleEvent`、`durableDumpStart`、`readEvent`、`reopenGTID`）共享和改写。哪段代码会改哪个变量只能逐行读；没有一个阶段能脱离源库和 dump 单独测试，半同步与异步两个拉流循环各写一遍。

决定：`run` 只把五个阶段串起来（约 100 行），每个阶段一个文件、一个小接口。

## 阶段

| 阶段 | 文件 | 接口 |
|---|---|---|
| 1 续传起点 | `run_start.go` | `resolveRunStart(ctx, task) (*runStart, error)`：源身份、LATEST 锚点、本地续传、接管（目录行 `file_path` / `UPLOADED` 对象）、已执行 GTID 种子与打开尾部对账、1236 回退集合、`STORAGE_INCONSISTENT` 拒绝。还没有 dump。 |
| 2 连接/握手 | `run_dump.go` | `releaseNotedDump`、`openFirstDump`：先 KILL 上次留下的 dump，再开 dump；失败时先看身份再给 1236 文本。 |
| 3 分段追加、rotate、封存 | `run_segment.go` | `openSegment`：`openAt`、`persistRaw`（先 flush 再写 checkpoint 和 OPEN 行，挂起的 format description）、`checkpointAt`、`seal`（换源）、`rotate`（写入或不写 rotate、封存/丢弃/按已上传 epoch 释放、先记下一文件 checkpoint 再上传、打开下一文件）。 |
| 4 事件处理 | `run_events.go` | `runLoop.handleEvent(event)`：异步循环和半同步 `SynchronousEventHandler` 共用。`handleRotate` 是 rotate 的判定（倒退、合成 rotate、第一个 rotate 打开文件）。 |
| 5 重连与错误分类 | `run_dump.go` | `readEvent`（1236 GTID 回退、最多 `maxDumpReconnectAttempts` 次按落盘位点重连）、`durableDumpStart`、`reopenAfterSwitch`、`pump(handle)`（两个循环合成一个）、`streamEnded`（取消返回 nil）、`dumpStillOpen`。 |

`runLoop` 持有 `*runStart`（循环里继续改的 `start`、`gtidFallback`、`atTip`、`sourceServerUUID`）、`*openSegment` 和 `*dumpConn`。`activeSyncer` 与 `syncer` 始终同时赋值，合并为 `dump.syncer`。

## 不在范围内

- 阶段内部顺序、日志文本、错误码、写入顺序（append → flush → checkpoint → OPEN 行；rotate 的 seal 行 → 下一文件 checkpoint → put → open）全部原样搬移。
- `nextEvent`、`openDump`、`sealLocalFile`、`noteDumpIdentity` 等原有辅助函数不动。
- 半同步 handler 与主 goroutine 并发读写 `dump.syncer` 的既有竞态没有改（与重构前相同）。

## 后果

- `run` 从 843 行降到约 100 行；每个阶段可以不连源库单独构造：`run_phases_test.go` 覆盖 `streamEnded`、`durableDumpStart` 的四种来源、合成 rotate 只前移游标、GTID dump 第一个 rotate 打开文件、rotate 倒退/重复是 `STREAM_REGRESSION`、文件打开前 format description 被挂起。
- 行为冻结的证据：全部单测与 `-race`；golden 14 个真实目录用例与 main 逐字节一致；e2e quick、账本集合、换主/kill 浸泡（gtid-redump、source-outage、worker-crash-recovery、semisync 各 3 轮）、旧版回切升级脚本三种模式在 main 与本分支结果一致。
